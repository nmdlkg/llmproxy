package tenancy

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

const defaultQuotaCacheTTL = 3 * time.Second

// quotaStoreErrorRetryAfter is advertised to clients when the quota store cannot
// be read. Check fails closed in that case, so this is kept short to keep a
// transient store error from looking like a long quota exhaustion.
const quotaStoreErrorRetryAfter = 5 * time.Second

// Credential is the dependency-light quota contribution input supplied by the
// caller after it has filtered credentials to those owned by and shared from a
// user.
type Credential struct {
	AuthID   string
	Provider string
	PlanTier string
}

// CredentialResolver returns the owned and shared credentials that contribute
// to one user's quota.
type CredentialResolver func(userID string) ([]Credential, error)

// Quota calculates rolling per-user limits and usage.
type Quota struct {
	store       Store
	cfg         config.TenancyQuota
	window      time.Duration
	cacheTTL    time.Duration
	credentials CredentialResolver
	now         func() time.Time

	mu    sync.Mutex
	cache map[string]quotaSnapshot
}

type quotaSnapshot struct {
	limit     int64
	used      int64
	oldest    time.Time
	expiresAt time.Time
}

// NewQuota creates a quota calculator with a short in-memory cache.
func NewQuota(store Store, cfg config.TenancyQuota, credentials CredentialResolver) (*Quota, error) {
	if store == nil {
		return nil, fmt.Errorf("tenancy quota: store is nil")
	}
	windowText := strings.TrimSpace(cfg.Window)
	if windowText == "" {
		// Mirror the config sanitizer default so a caller that builds
		// TenancyQuota directly (SDK embedders, tests) gets the same window as
		// a YAML-loaded config instead of silently falling back to a shorter one.
		windowText = config.DefaultTenancyQuotaWindow
	}
	window, errParse := time.ParseDuration(windowText)
	if errParse != nil || window <= 0 {
		if errParse == nil {
			errParse = fmt.Errorf("duration must be positive")
		}
		return nil, fmt.Errorf("tenancy quota: parse window %q: %w", windowText, errParse)
	}
	return &Quota{
		store:       store,
		cfg:         cfg,
		window:      window,
		cacheTTL:    defaultQuotaCacheTTL,
		credentials: credentials,
		now:         time.Now,
		cache:       make(map[string]quotaSnapshot),
	}, nil
}

// LimitFor computes the configured base plus credential contributions.
func LimitFor(user User, credentials []Credential, cfg config.TenancyQuota) int64 {
	tier := strings.ToLower(strings.TrimSpace(user.Tier))
	base, ok := cfg.BaseUSD[tier]
	if !ok {
		base = cfg.BaseUSD["default"]
	}

	limit := base.NanoUSD()
	seen := make(map[string]struct{}, len(credentials))
	for _, credential := range credentials {
		authID := strings.TrimSpace(credential.AuthID)
		if authID != "" {
			if _, exists := seen[authID]; exists {
				continue
			}
			seen[authID] = struct{}{}
		}
		provider := strings.ToLower(strings.TrimSpace(credential.Provider))
		plans := cfg.ContributionUSD[provider]
		if len(plans) == 0 {
			continue
		}
		planTier := strings.ToLower(strings.TrimSpace(credential.PlanTier))
		contribution, exists := plans[planTier]
		if !exists {
			contribution = plans["default"]
		}
		if contribution.NanoUSD() > 0 && limit > math.MaxInt64-contribution.NanoUSD() {
			return math.MaxInt64
		}
		limit += contribution.NanoUSD()
	}
	return limit
}

// Limit returns the current quota limit for a user.
func (q *Quota) Limit(userID string) (int64, error) {
	snapshot, errLoad := q.load(userID)
	if errLoad != nil {
		return 0, errLoad
	}
	return snapshot.limit, nil
}

// Used returns exact nano-USD consumed in the rolling quota window.
func (q *Quota) Used(userID string) (int64, error) {
	snapshot, errLoad := q.load(userID)
	if errLoad != nil {
		return 0, errLoad
	}
	return snapshot.used, nil
}

// Check reports whether a request is allowed and, when denied, how long until
// the oldest in-window usage expires.
//
// Store failures fail CLOSED: an unreadable quota store denies the request
// rather than silently disabling enforcement. The returned retryAfter is
// deliberately short so a transient store error degrades into a brief retry
// rather than an outage. Callers that want availability over enforcement must
// opt out explicitly; do not change this default without saying so in the
// tenancy docs.
func (q *Quota) Check(userID string) (allowed bool, retryAfter time.Duration) {
	snapshot, errLoad := q.load(userID)
	if errLoad != nil {
		log.WithError(errLoad).WithField("user_id", userID).Error("tenancy quota: check failed, denying request (fail-closed)")
		return false, quotaStoreErrorRetryAfter
	}
	if snapshot.used < snapshot.limit {
		return true, 0
	}

	now := q.now().UTC()
	if snapshot.oldest.IsZero() {
		return false, q.window
	}
	retryAfter = snapshot.oldest.Add(q.window).Sub(now)
	if retryAfter <= 0 {
		retryAfter = time.Second
	}
	return false, retryAfter
}

// Invalidate removes cached quota results. With no IDs it clears the cache.
func (q *Quota) Invalidate(userIDs ...string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(userIDs) == 0 {
		clear(q.cache)
		return
	}
	for _, userID := range userIDs {
		delete(q.cache, userID)
	}
}

func (q *Quota) load(userID string) (quotaSnapshot, error) {
	now := q.now().UTC()
	q.mu.Lock()
	if snapshot, ok := q.cache[userID]; ok && now.Before(snapshot.expiresAt) {
		q.mu.Unlock()
		return snapshot, nil
	}
	q.mu.Unlock()

	user, errUser := q.store.GetUser(userID)
	if errUser != nil {
		return quotaSnapshot{}, fmt.Errorf("tenancy quota: get user: %w", errUser)
	}
	var credentials []Credential
	if q.credentials != nil {
		var errCredentials error
		credentials, errCredentials = q.credentials(userID)
		if errCredentials != nil {
			return quotaSnapshot{}, fmt.Errorf("tenancy quota: resolve credentials: %w", errCredentials)
		}
	}
	since := now.Add(-q.window)
	used, errUsed := q.store.UsedUnits(context.Background(), userID, since)
	if errUsed != nil {
		return quotaSnapshot{}, fmt.Errorf("tenancy quota: sum usage: %w", errUsed)
	}
	oldest, _, errOldest := q.store.OldestUserUsage(context.Background(), userID, since)
	if errOldest != nil {
		return quotaSnapshot{}, fmt.Errorf("tenancy quota: find oldest usage: %w", errOldest)
	}
	snapshot := quotaSnapshot{
		limit:     LimitFor(*user, credentials, q.cfg),
		used:      used,
		oldest:    oldest,
		expiresAt: now.Add(q.cacheTTL),
	}
	q.mu.Lock()
	q.cache[userID] = snapshot
	q.mu.Unlock()
	return snapshot, nil
}
