package tenancy

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// newScenarioQuota builds a quota calculator with a controllable clock so the
// rolling window can be advanced deterministically.
func newScenarioQuota(t *testing.T, store *SQLiteStore, limitUSD config.USDLimit, window string, now *time.Time) *Quota {
	t.Helper()
	cfg := config.TenancyQuota{Window: window, BaseUSD: map[string]config.USDLimit{"default": limitUSD}}
	quota, errQuota := NewQuota(store, cfg, nil)
	if errQuota != nil {
		t.Fatalf("NewQuota() error = %v", errQuota)
	}
	quota.now = func() time.Time { return *now }
	return quota
}

func createScenarioUser(t *testing.T, store *SQLiteStore, id string) *User {
	t.Helper()
	user := &User{ID: id, Email: id + "@example.com", DisplayName: id, Role: RoleUser, Tier: "default"}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	return user
}

func TestQuotaCheckRetryAfterDerivesFromOldestInWindowRow(t *testing.T) {
	store := newTestStore(t)
	user := createScenarioUser(t, store, "u_retry")
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	quota := newScenarioQuota(t, store, 1, "24h", &now)

	oldest := now.Add(-20 * time.Hour)
	if errAppend := store.AppendUsage(context.Background(), []UsageEntry{
		{UserID: user.ID, AuthID: "a", Provider: "codex", Model: "m", CostNanoUSD: 1_000_000_000, OccurredAt: oldest},
		{UserID: user.ID, AuthID: "a", Provider: "codex", Model: "m", CostNanoUSD: 1_000_000_000, OccurredAt: now.Add(-time.Hour)},
	}); errAppend != nil {
		t.Fatalf("AppendUsage() error = %v", errAppend)
	}

	allowed, retryAfter := quota.Check(user.ID)
	if allowed {
		t.Fatal("Quota.Check() allowed a request past the limit")
	}
	want := oldest.Add(24 * time.Hour).Sub(now)
	if retryAfter != want {
		t.Errorf("retryAfter = %v, want %v (oldest in-window row + window)", retryAfter, want)
	}
}

func TestQuotaSlidingWindowRecoversAsUsageAges(t *testing.T) {
	store := newTestStore(t)
	user := createScenarioUser(t, store, "u_slide")
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	quota := newScenarioQuota(t, store, 1, "24h", &now)

	if errAppend := store.AppendUsage(context.Background(), []UsageEntry{
		{UserID: user.ID, AuthID: "a", Provider: "codex", Model: "m", CostNanoUSD: 1_000_000_000, OccurredAt: now.Add(-23 * time.Hour)},
	}); errAppend != nil {
		t.Fatalf("AppendUsage() error = %v", errAppend)
	}
	if allowed, _ := quota.Check(user.ID); allowed {
		t.Fatal("Quota.Check() allowed a request at the limit")
	}

	// Advance past the window so the row ages out; Invalidate stands in for
	// cache expiry so the test does not sleep.
	now = now.Add(2 * time.Hour)
	quota.Invalidate(user.ID)
	if allowed, retryAfter := quota.Check(user.ID); !allowed {
		t.Fatalf("Quota.Check() still denied after the window slid, retryAfter = %v", retryAfter)
	}
}

func TestQuotaInvalidateDropsCachedSnapshot(t *testing.T) {
	store := newTestStore(t)
	user := createScenarioUser(t, store, "u_cache")
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	quota := newScenarioQuota(t, store, 1, "24h", &now)

	if used, errUsed := quota.Used(user.ID); errUsed != nil || used != 0 {
		t.Fatalf("Used() = %d, %v, want 0, nil", used, errUsed)
	}
	if errAppend := store.AppendUsage(context.Background(), []UsageEntry{
		{UserID: user.ID, AuthID: "a", Provider: "codex", Model: "m", CostNanoUSD: 250_000_000, OccurredAt: now.Add(-time.Minute)},
	}); errAppend != nil {
		t.Fatalf("AppendUsage() error = %v", errAppend)
	}
	// The cached snapshot still reports the pre-append value.
	if used, _ := quota.Used(user.ID); used != 0 {
		t.Fatalf("Used() = %d, want the cached 0 before invalidation", used)
	}
	quota.Invalidate(user.ID)
	if used, _ := quota.Used(user.ID); used != 250_000_000 {
		t.Errorf("Used() after Invalidate = %d, want 250000000", used)
	}
}

func TestQuotaCompositionSeparatesBaseFromContribution(t *testing.T) {
	store := newTestStore(t)
	user := createScenarioUser(t, store, "u_comp")
	cfg := config.TenancyQuota{
		Window:  "168h",
		BaseUSD: map[string]config.USDLimit{"default": 5},
		ContributionUSD: map[string]map[string]config.USDLimit{
			"codex": {"pro": 20},
		},
	}
	credentials := []Credential{{AuthID: "codex-1", Provider: "codex", PlanTier: "pro"}}
	quota, errQuota := NewQuota(store, cfg, func(string) ([]Credential, error) { return credentials, nil })
	if errQuota != nil {
		t.Fatalf("NewQuota() error = %v", errQuota)
	}

	composition, errComposition := quota.Composition(user.ID)
	if errComposition != nil {
		t.Fatalf("Composition() error = %v", errComposition)
	}
	if composition.BaseNanoUSD != config.USDLimit(5).NanoUSD() {
		t.Errorf("base = %d, want %d", composition.BaseNanoUSD, config.USDLimit(5).NanoUSD())
	}
	if composition.ContributionNanoUSD != config.USDLimit(20).NanoUSD() {
		t.Errorf("contribution = %d, want %d", composition.ContributionNanoUSD, config.USDLimit(20).NanoUSD())
	}
	if composition.TotalNanoUSD != composition.BaseNanoUSD+composition.ContributionNanoUSD {
		t.Errorf("total = %d, want base + contribution", composition.TotalNanoUSD)
	}
	if composition.ContributingCredentials != 1 {
		t.Errorf("contributingCredentials = %d, want 1", composition.ContributingCredentials)
	}
}

func TestQuotaCompositionWithoutCredentialsMatchesBase(t *testing.T) {
	store := newTestStore(t)
	user := createScenarioUser(t, store, "u_nocomp")
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	quota := newScenarioQuota(t, store, 7, "168h", &now)

	composition, errComposition := quota.Composition(user.ID)
	if errComposition != nil {
		t.Fatalf("Composition() error = %v", errComposition)
	}
	if composition.ContributionNanoUSD != 0 || composition.ContributingCredentials != 0 {
		t.Errorf("composition = %+v, want no contribution", composition)
	}
	if composition.TotalNanoUSD != composition.BaseNanoUSD {
		t.Errorf("total = %d, want base %d", composition.TotalNanoUSD, composition.BaseNanoUSD)
	}
}
