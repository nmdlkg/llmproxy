package tenancy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	defaultBalancingInterval  = 30 * time.Second
	balancingWarningRateLimit = 5 * time.Minute
)

var errBalancingStopped = errors.New("tenancy balancing: stopped")

type prioritySnapshot struct {
	values map[string]int
}

type balancingRequest struct {
	result chan error
}

// balancingRuntime separates slow priority recomputation from the scheduler's
// lock-held priority lookup.
type balancingRuntime struct {
	authManager *coreauth.Manager
	balancer    *Balancer
	interval    time.Duration

	base      atomic.Pointer[prioritySnapshot]
	effective atomic.Pointer[prioritySnapshot]

	requests chan balancingRequest
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func startBalancingRuntime(
	store Store,
	balancing Balancing,
	authManager *coreauth.Manager,
	interval time.Duration,
) *balancingRuntime {
	if store == nil || authManager == nil {
		return nil
	}
	if interval <= 0 {
		interval = defaultBalancingInterval
	}

	runtime := &balancingRuntime{
		authManager: authManager,
		interval:    interval,
		requests:    make(chan balancingRequest, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	runtime.base.Store(&prioritySnapshot{values: map[string]int{}})
	runtime.effective.Store(&prioritySnapshot{values: map[string]int{}})
	runtime.balancer = NewBalancer(store, balancing, runtime.basePriority)

	// The scheduler invokes this resolver while holding its mutex. The closure
	// therefore performs only an atomic snapshot load and a map lookup; it never
	// calls the auth manager, touches the store, or acquires a lock.
	authManager.SetPriorityResolver(runtime.resolvePriority)
	go runtime.run()
	return runtime
}

func (r *balancingRuntime) run() {
	defer close(r.done)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	lastWarning := time.Time{}
	recompute := func() error {
		errRecompute := r.recompute(time.Now())
		if errRecompute == nil {
			lastWarning = time.Time{}
			return nil
		}
		now := time.Now()
		if lastWarning.IsZero() || now.Sub(lastWarning) >= balancingWarningRateLimit {
			log.WithError(errRecompute).Warn("tenancy balancing: recompute failed; retaining previous priorities")
			lastWarning = now
		}
		return errRecompute
	}

	_ = recompute()
	for {
		select {
		case request := <-r.requests:
			errRecompute := recompute()
			if request.result != nil {
				request.result <- errRecompute
			}
		case <-ticker.C:
			_ = recompute()
		case <-r.stop:
			return
		}
	}
}

func (r *balancingRuntime) recompute(now time.Time) error {
	if r == nil || r.authManager == nil || r.balancer == nil {
		return fmt.Errorf("tenancy balancing: runtime is not initialized")
	}

	previousBase := r.base.Load()
	baseValues := configuredPriorities(r.authManager.List())
	r.base.Store(&prioritySnapshot{values: baseValues})

	changedByBalancer, errRecompute := r.balancer.Recompute(now)
	if errRecompute != nil {
		r.base.Store(previousBase)
		return errRecompute
	}

	previous := r.effective.Load()
	nextValues := make(map[string]int, len(baseValues))
	for authID, basePriority := range baseValues {
		nextValues[authID] = basePriority
		if previous == nil || previousBase == nil {
			continue
		}
		previousEffective, hadEffective := previous.values[authID]
		previousBasePriority, hadBase := previousBase.values[authID]
		if hadEffective && hadBase {
			nextValues[authID] = basePriority + previousEffective - previousBasePriority
		}
	}
	for authID, effectivePriority := range changedByBalancer {
		if _, active := baseValues[authID]; active {
			nextValues[authID] = effectivePriority
		}
	}

	changedAuthIDs := changedPriorities(previous, nextValues)
	r.effective.Store(&prioritySnapshot{values: nextValues})

	// Refresh only after publication. Scheduler upserts resolve priority while
	// holding their mutex, so they must observe the already-published snapshot.
	for _, authID := range changedAuthIDs {
		r.authManager.RefreshSchedulerEntry(authID)
	}
	return nil
}

func (r *balancingRuntime) trigger() {
	if r == nil {
		return
	}
	select {
	case <-r.stop:
		return
	default:
	}
	select {
	case r.requests <- balancingRequest{}:
	default:
	}
}

func (r *balancingRuntime) requestRecompute(ctx context.Context) error {
	if r == nil {
		return errBalancingStopped
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result := make(chan error, 1)
	request := balancingRequest{result: result}
	select {
	case r.requests <- request:
	case <-r.stop:
		return errBalancingStopped
	case <-ctx.Done():
		return fmt.Errorf("tenancy balancing: queue recompute: %w", ctx.Err())
	}
	select {
	case errRecompute := <-result:
		return errRecompute
	case <-r.stop:
		return errBalancingStopped
	case <-ctx.Done():
		return fmt.Errorf("tenancy balancing: wait for recompute: %w", ctx.Err())
	}
}

func (r *balancingRuntime) stopAndUninstall() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stop)
		<-r.done
		r.authManager.SetPriorityResolver(nil)
	})
}

func (r *balancingRuntime) resolvePriority(auth *coreauth.Auth) (int, bool) {
	if r == nil || auth == nil {
		return 0, false
	}
	snapshot := r.effective.Load()
	if snapshot == nil {
		return 0, false
	}
	priority, ok := snapshot.values[auth.ID]
	return priority, ok
}

func (r *balancingRuntime) basePriority(authID string) int {
	if r == nil {
		return 0
	}
	snapshot := r.base.Load()
	if snapshot == nil {
		return 0
	}
	return snapshot.values[authID]
}

func configuredPriorities(auths []*coreauth.Auth) map[string]int {
	priorities := make(map[string]int, len(auths))
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		priorities[auth.ID] = configuredPriority(auth)
	}
	return priorities
}

func configuredPriority(auth *coreauth.Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	priority, errParse := strconv.Atoi(raw)
	if errParse != nil {
		return 0
	}
	return priority
}

func changedPriorities(previous *prioritySnapshot, next map[string]int) []string {
	changed := make([]string, 0, len(next))
	for authID, nextPriority := range next {
		if previous == nil {
			changed = append(changed, authID)
			continue
		}
		previousPriority, existed := previous.values[authID]
		if !existed || previousPriority != nextPriority {
			changed = append(changed, authID)
		}
	}
	if previous == nil {
		return changed
	}
	for authID := range previous.values {
		if _, exists := next[authID]; !exists {
			changed = append(changed, authID)
		}
	}
	return changed
}
