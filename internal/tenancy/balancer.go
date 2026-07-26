package tenancy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Balancing is the parsed reset-window priority policy.
type Balancing struct {
	UrgencyHorizon time.Duration
	HighWater      float64
	UrgencyBonus   int
}

// ParseBalancing parses duration-bearing balancing config.
func ParseBalancing(cfg config.TenancyBalancing) (Balancing, error) {
	horizonText := cfg.UrgencyHorizon
	if horizonText == "" {
		horizonText = "30m"
	}
	horizon, errParse := time.ParseDuration(horizonText)
	if errParse != nil || horizon <= 0 {
		if errParse == nil {
			errParse = fmt.Errorf("duration must be positive")
		}
		return Balancing{}, fmt.Errorf("tenancy balancing: parse urgency horizon %q: %w", horizonText, errParse)
	}
	highWater := cfg.HighWater
	if highWater <= 0 || highWater > 1 {
		highWater = 0.9
	}
	urgencyBonus := cfg.UrgencyBonus
	if urgencyBonus <= 0 {
		urgencyBonus = 1
	}
	return Balancing{
		UrgencyHorizon: horizon,
		HighWater:      highWater,
		UrgencyBonus:   urgencyBonus,
	}, nil
}

// EffectivePriority returns the operator priority plus an urgency bonus when
// the active window resets strictly inside the horizon and is strictly below
// the high-water ratio.
func EffectivePriority(base int, window QuotaWindow, now time.Time, balancing Balancing) int {
	if balancing.UrgencyHorizon <= 0 || balancing.UrgencyBonus == 0 {
		return base
	}
	timeToReset := window.WindowEnd.Sub(now)
	if timeToReset <= 0 || timeToReset >= balancing.UrgencyHorizon {
		return base
	}
	if window.LimitUnits <= 0 || balancing.HighWater <= 0 {
		return base
	}
	if float64(window.UsedUnits)/float64(window.LimitUnits) >= balancing.HighWater {
		return base
	}
	return base + balancing.UrgencyBonus
}

// BasePriorityResolver returns the operator-set priority for an auth.
type BasePriorityResolver func(authID string) int

// Balancer computes changed effective priorities from durable quota windows.
type Balancer struct {
	store        Store
	balancing    Balancing
	basePriority BasePriorityResolver

	mu   sync.Mutex
	last map[string]int
}

// NewBalancer creates a quota-window-aware priority calculator.
func NewBalancer(store Store, balancing Balancing, basePriority BasePriorityResolver) *Balancer {
	return &Balancer{
		store:        store,
		balancing:    balancing,
		basePriority: basePriority,
		last:         make(map[string]int),
	}
}

// Recompute returns only effective priorities that changed since the previous
// call. The first call returns every auth with a recorded quota window.
func (b *Balancer) Recompute(now time.Time) (map[string]int, error) {
	if b == nil || b.store == nil {
		return nil, fmt.Errorf("tenancy balancing: store is nil")
	}
	windows, errWindows := b.store.ListQuotaWindows(context.Background())
	if errWindows != nil {
		return nil, fmt.Errorf("tenancy balancing: list quota windows: %w", errWindows)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	current := make(map[string]int, len(windows)+len(b.last))
	for _, window := range windows {
		base := b.resolveBasePriority(window.AuthID)
		effective := EffectivePriority(base, window, now, b.balancing)
		if existing, ok := current[window.AuthID]; !ok || effective > existing {
			current[window.AuthID] = effective
		}
	}
	for authID := range b.last {
		if _, ok := current[authID]; !ok {
			current[authID] = b.resolveBasePriority(authID)
		}
	}

	changed := make(map[string]int)
	for authID, effective := range current {
		previous, existed := b.last[authID]
		if !existed || previous != effective {
			changed[authID] = effective
		}
	}
	b.last = current
	return changed, nil
}

func (b *Balancer) resolveBasePriority(authID string) int {
	if b.basePriority == nil {
		return 0
	}
	return b.basePriority(authID)
}
