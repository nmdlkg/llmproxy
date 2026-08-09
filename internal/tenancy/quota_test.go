package tenancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestLimitFor(t *testing.T) {
	t.Parallel()

	cfg := config.TenancyQuota{
		BaseUSD: map[string]config.USDLimit{
			"default": 100,
			"staff":   200,
		},
		ContributionUSD: map[string]map[string]config.USDLimit{
			"codex": {
				"default": 10,
				"plus":    50,
			},
			"claude": {
				"default": 30,
			},
		},
	}
	tests := []struct {
		name        string
		user        User
		credentials []Credential
		want        int64
	}{
		{
			name: "tier and explicit contribution tiers",
			user: User{Tier: "staff"},
			credentials: []Credential{
				{AuthID: "codex-1", Provider: "codex", PlanTier: "plus"},
				{AuthID: "claude-1", Provider: "claude", PlanTier: "default"},
			},
			want: 280,
		},
		{
			name: "user and plan tier fallbacks",
			user: User{Tier: "unknown"},
			credentials: []Credential{
				{AuthID: "codex-1", Provider: "CODEX", PlanTier: "unknown"},
				{AuthID: "claude-1", Provider: "claude"},
			},
			want: 140,
		},
		{
			name: "unknown provider contributes nothing",
			user: User{Tier: "default"},
			credentials: []Credential{
				{AuthID: "gemini-1", Provider: "gemini", PlanTier: "pro"},
			},
			want: 100,
		},
		{
			name: "duplicate auth contributes once",
			user: User{Tier: "default"},
			credentials: []Credential{
				{AuthID: "codex-1", Provider: "codex", PlanTier: "plus"},
				{AuthID: "codex-1", Provider: "codex", PlanTier: "plus"},
			},
			want: 150,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := LimitFor(test.user, test.credentials, cfg); got != test.want {
				t.Fatalf("LimitFor() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestLedgerAggregationAndWindowRollover(t *testing.T) {
	store := newTestStore(t)
	user := &User{
		ID:    "user-1",
		Email: "person@example.com",
		Role:  RoleUser,
		Tier:  "default",
	}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	entries := []UsageEntry{
		{UserID: user.ID, AuthID: "auth-1", Provider: "codex", CostNanoUSD: 450, OccurredAt: now.Add(-2 * time.Hour)},
		{UserID: user.ID, AuthID: "auth-1", Provider: "codex", CostNanoUSD: 625, OccurredAt: now.Add(-30 * time.Minute)},
		{UserID: user.ID, AuthID: "auth-2", Provider: "claude", CostNanoUSD: 375, OccurredAt: now.Add(-10 * time.Minute)},
	}
	if errAppend := store.AppendUsage(context.Background(), entries); errAppend != nil {
		t.Fatalf("AppendUsage() error = %v", errAppend)
	}
	directUsed, errUsed := store.UsedUnits(context.Background(), user.ID, now.Add(-time.Hour))
	if errUsed != nil {
		t.Fatalf("UsedUnits() error = %v", errUsed)
	}
	if directUsed != 1000 {
		t.Fatalf("UsedUnits() = %d, want 1000 nano-USD", directUsed)
	}

	quota, errQuota := NewQuota(store, config.TenancyQuota{
		Window:  "1h",
		BaseUSD: map[string]config.USDLimit{"default": 1000},
	}, nil)
	if errQuota != nil {
		t.Fatalf("NewQuota() error = %v", errQuota)
	}
	clock := now
	quota.now = func() time.Time { return clock }
	quota.cacheTTL = 0

	used, errQuotaUsed := quota.Used(user.ID)
	if errQuotaUsed != nil {
		t.Fatalf("Quota.Used() error = %v", errQuotaUsed)
	}
	if used != 1000 {
		t.Fatalf("Quota.Used() = %d, want 1000 nano-USD", used)
	}
	allowed, retryAfter := quota.Check(user.ID)
	if allowed {
		t.Fatal("Quota.Check() allowed usage exactly at the limit")
	}
	if retryAfter != 30*time.Minute {
		t.Fatalf("Quota.Check() retryAfter = %v, want 30m", retryAfter)
	}

	clock = now.Add(31 * time.Minute)
	quota.Invalidate(user.ID)
	used, errQuotaUsed = quota.Used(user.ID)
	if errQuotaUsed != nil {
		t.Fatalf("Quota.Used() after partial rollover error = %v", errQuotaUsed)
	}
	if used != 375 {
		t.Fatalf("Quota.Used() after partial rollover = %d, want 375 nano-USD", used)
	}
	allowed, retryAfter = quota.Check(user.ID)
	if !allowed || retryAfter != 0 {
		t.Fatalf("Quota.Check() after partial rollover = (%v, %v), want (true, 0)", allowed, retryAfter)
	}

	clock = now.Add(51 * time.Minute)
	quota.Invalidate(user.ID)
	used, errQuotaUsed = quota.Used(user.ID)
	if errQuotaUsed != nil {
		t.Fatalf("Quota.Used() after complete rollover error = %v", errQuotaUsed)
	}
	if used != 0 {
		t.Fatalf("Quota.Used() after complete rollover = %v, want 0", used)
	}
}

// failingStore embeds a real store but makes the usage sum fail, so Check hits
// its store-error path.
type failingStore struct {
	Store
	err error
}

func (s failingStore) UsedUnits(context.Context, string, time.Time) (int64, error) {
	return 0, s.err
}

// TestQuotaCheckFailsClosedOnStoreError pins the fail-closed contract: an
// unreadable quota store must deny the request rather than silently disabling
// enforcement.
func TestQuotaCheckFailsClosedOnStoreError(t *testing.T) {
	store := newTestStore(t)
	user := &User{ID: "u_failclosed", Email: "failclosed@example.com", Role: "user", Tier: "default"}
	if err := store.CreateUser(user); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	cfg := config.TenancyQuota{Window: "24h", BaseUSD: map[string]config.USDLimit{"default": 1000}}
	quota, err := NewQuota(failingStore{Store: store, err: errors.New("database is locked")}, cfg, nil)
	if err != nil {
		t.Fatalf("NewQuota() error = %v", err)
	}

	allowed, retryAfter := quota.Check(user.ID)
	if allowed {
		t.Fatal("Quota.Check() allowed the request despite a store error; must fail closed")
	}
	if retryAfter != quotaStoreErrorRetryAfter {
		t.Fatalf("Quota.Check() retryAfter = %v, want %v", retryAfter, quotaStoreErrorRetryAfter)
	}
}
