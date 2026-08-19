package tenancy

import (
	"context"
	"testing"
	"time"
)

// seedAggregateUsage writes a deterministic ledger spanning three UTC days for
// two users so the aggregation queries can be checked exactly.
func seedAggregateUsage(t *testing.T, store *SQLiteStore, base time.Time) {
	t.Helper()
	entries := []UsageEntry{
		{UserID: "user-a", AuthID: "auth-1", Provider: "codex", Model: "gpt-5.6", CostNanoUSD: 100, InputTokens: 10, OutputTokens: 5, OccurredAt: base},
		{UserID: "user-a", AuthID: "auth-1", Provider: "codex", Model: "gpt-5.6", CostNanoUSD: 200, InputTokens: 20, OutputTokens: 7, OccurredAt: base.Add(time.Hour)},
		{UserID: "user-a", AuthID: "auth-2", Provider: "claude", Model: "claude-opus-5", CostNanoUSD: 400, InputTokens: 40, OutputTokens: 9, Failed: true, OccurredAt: base.Add(24 * time.Hour)},
		{UserID: "user-b", AuthID: "auth-3", Provider: "codex", Model: "gpt-5.6", CostNanoUSD: 900, InputTokens: 90, OutputTokens: 3, OccurredAt: base.Add(48 * time.Hour)},
	}
	if errAppend := store.AppendUsage(context.Background(), entries); errAppend != nil {
		t.Fatalf("AppendUsage() error = %v", errAppend)
	}
}

func TestUsageByModelIsolatesUsersAndCountsFailures(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC)
	seedAggregateUsage(t, store, base)

	stats, errStats := store.UsageByModel(context.Background(), "user-a", base.Add(-time.Hour), base.Add(72*time.Hour))
	if errStats != nil {
		t.Fatalf("UsageByModel() error = %v", errStats)
	}
	if len(stats) != 2 {
		t.Fatalf("UsageByModel() returned %d rows, want 2", len(stats))
	}
	byModel := make(map[string]UsageModelStat, len(stats))
	for _, stat := range stats {
		if stat.Model == "gpt-5.6" && stat.Provider != "codex" {
			t.Errorf("provider = %q, want codex", stat.Provider)
		}
		byModel[stat.Model] = stat
	}
	codex := byModel["gpt-5.6"]
	if codex.CostNanoUSD != 300 || codex.Attempts != 2 || codex.FailedAttempts != 0 {
		t.Errorf("gpt-5.6 stat = %+v, want cost 300 attempts 2 failed 0", codex)
	}
	if codex.InputTokens != 30 || codex.OutputTokens != 12 {
		t.Errorf("gpt-5.6 tokens = %d/%d, want 30/12", codex.InputTokens, codex.OutputTokens)
	}
	opus := byModel["claude-opus-5"]
	if opus.CostNanoUSD != 400 || opus.Attempts != 1 || opus.FailedAttempts != 1 {
		t.Errorf("claude-opus-5 stat = %+v, want cost 400 attempts 1 failed 1", opus)
	}
	// user-b's row must never leak into user-a's breakdown.
	for _, stat := range stats {
		if stat.CostNanoUSD == 900 {
			t.Error("UsageByModel() leaked another user's ledger row")
		}
	}
}

func TestUsageByDayBucketsIntoUTCDays(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC)
	seedAggregateUsage(t, store, base)

	stats, errStats := store.UsageByDay(context.Background(), "user-a", base.Add(-time.Hour), base.Add(72*time.Hour))
	if errStats != nil {
		t.Fatalf("UsageByDay() error = %v", errStats)
	}
	if len(stats) != 2 {
		t.Fatalf("UsageByDay() returned %d buckets, want 2", len(stats))
	}
	if !stats[0].Day.Before(stats[1].Day) {
		t.Errorf("UsageByDay() days not ascending: %v then %v", stats[0].Day, stats[1].Day)
	}
	firstDay := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	if !stats[0].Day.Equal(firstDay) {
		t.Errorf("first bucket = %v, want %v", stats[0].Day, firstDay)
	}
	if stats[0].CostNanoUSD != 300 || stats[0].Attempts != 2 {
		t.Errorf("first bucket = %+v, want cost 300 attempts 2", stats[0])
	}
	if stats[1].FailedAttempts != 1 {
		t.Errorf("second bucket failed = %d, want 1", stats[1].FailedAttempts)
	}
}

func TestUsageByDayBucketsPreEpochTimestampByUTCDate(t *testing.T) {
	store := newTestStore(t)
	occurredAt := time.Date(1969, 12, 31, 23, 30, 0, 0, time.UTC)
	if errAppend := store.AppendUsage(context.Background(), []UsageEntry{{
		UserID: "pre-epoch", AuthID: "auth", Provider: "codex", Model: "gpt-5.6",
		OccurredAt: occurredAt,
	}}); errAppend != nil {
		t.Fatalf("AppendUsage() error = %v", errAppend)
	}
	stats, errStats := store.UsageByDay(context.Background(), "pre-epoch", occurredAt.Add(-time.Hour), occurredAt.Add(time.Hour))
	if errStats != nil {
		t.Fatalf("UsageByDay() error = %v", errStats)
	}
	if len(stats) != 1 {
		t.Fatalf("UsageByDay() returned %d rows, want 1", len(stats))
	}
	want := time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC)
	if !stats[0].Day.Equal(want) {
		t.Errorf("pre-epoch bucket = %v, want %v", stats[0].Day, want)
	}
}

func TestUsageByDayExcludesRowsOutsideRange(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC)
	seedAggregateUsage(t, store, base)

	// [since, until) must exclude the boundary row at until.
	stats, errStats := store.UsageByDay(context.Background(), "user-a", base, base.Add(24*time.Hour))
	if errStats != nil {
		t.Fatalf("UsageByDay() error = %v", errStats)
	}
	if len(stats) != 1 {
		t.Fatalf("UsageByDay() returned %d buckets, want 1", len(stats))
	}
	if stats[0].Attempts != 2 {
		t.Errorf("attempts = %d, want 2", stats[0].Attempts)
	}
}

func TestUsageByUserAggregatesEveryUser(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC)
	seedAggregateUsage(t, store, base)

	stats, errStats := store.UsageByUser(context.Background(), base.Add(-time.Hour), base.Add(72*time.Hour))
	if errStats != nil {
		t.Fatalf("UsageByUser() error = %v", errStats)
	}
	byUser := make(map[string]UsageUserStat, len(stats))
	for _, stat := range stats {
		byUser[stat.UserID] = stat
	}
	if got := byUser["user-a"]; got.CostNanoUSD != 700 || got.Attempts != 3 || got.FailedAttempts != 1 {
		t.Errorf("user-a = %+v, want cost 700 attempts 3 failed 1", got)
	}
	if got := byUser["user-b"]; got.CostNanoUSD != 900 || got.Attempts != 1 {
		t.Errorf("user-b = %+v, want cost 900 attempts 1", got)
	}
}

func TestUsageAggregatesReturnEmptyWithoutRows(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC()
	models, errModels := store.UsageByModel(context.Background(), "ghost", now.Add(-time.Hour), now)
	if errModels != nil {
		t.Fatalf("UsageByModel() error = %v", errModels)
	}
	if len(models) != 0 {
		t.Errorf("UsageByModel() = %d rows, want 0", len(models))
	}
	days, errDays := store.UsageByDay(context.Background(), "ghost", now.Add(-time.Hour), now)
	if errDays != nil {
		t.Fatalf("UsageByDay() error = %v", errDays)
	}
	if len(days) != 0 {
		t.Errorf("UsageByDay() = %d buckets, want 0", len(days))
	}
}
