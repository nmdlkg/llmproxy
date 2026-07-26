package tenancy

import (
	"context"
	"testing"
	"time"
)

func TestEffectivePriority(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	balancing := Balancing{
		UrgencyHorizon: 30 * time.Minute,
		HighWater:      0.9,
		UrgencyBonus:   2,
	}
	tests := []struct {
		name   string
		window QuotaWindow
		want   int
	}{
		{
			name:   "inside horizon below high water",
			window: QuotaWindow{WindowEnd: now.Add(29 * time.Minute), UsedUnits: 89, LimitUnits: 100},
			want:   7,
		},
		{
			name:   "exactly at horizon has no bonus",
			window: QuotaWindow{WindowEnd: now.Add(30 * time.Minute), UsedUnits: 89, LimitUnits: 100},
			want:   5,
		},
		{
			name:   "exactly at high water has no bonus",
			window: QuotaWindow{WindowEnd: now.Add(29 * time.Minute), UsedUnits: 90, LimitUnits: 100},
			want:   5,
		},
		{
			name:   "past reset has no bonus",
			window: QuotaWindow{WindowEnd: now.Add(-time.Second), UsedUnits: 10, LimitUnits: 100},
			want:   5,
		},
		{
			name:   "reset now has no bonus",
			window: QuotaWindow{WindowEnd: now, UsedUnits: 10, LimitUnits: 100},
			want:   5,
		},
		{
			name:   "unknown limit has no bonus",
			window: QuotaWindow{WindowEnd: now.Add(time.Minute), UsedUnits: 0, LimitUnits: 0},
			want:   5,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := EffectivePriority(5, test.window, now, balancing); got != test.want {
				t.Fatalf("EffectivePriority() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestBalancerRecomputeReturnsOnlyChanges(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	window := QuotaWindow{
		AuthID:      "auth-1",
		Provider:    "codex",
		WindowStart: now.Add(-time.Hour),
		WindowEnd:   now.Add(10 * time.Minute),
		UsedUnits:   50,
		LimitUnits:  100,
		Source:      "test",
		UpdatedAt:   now,
	}
	if errUpsert := store.UpsertQuotaWindow(context.Background(), window); errUpsert != nil {
		t.Fatalf("UpsertQuotaWindow() error = %v", errUpsert)
	}

	balancer := NewBalancer(
		store,
		Balancing{UrgencyHorizon: 30 * time.Minute, HighWater: 0.9, UrgencyBonus: 1},
		func(string) int { return 3 },
	)
	changed, errRecompute := balancer.Recompute(now)
	if errRecompute != nil {
		t.Fatalf("Recompute() error = %v", errRecompute)
	}
	if got := changed["auth-1"]; got != 4 {
		t.Fatalf("Recompute() priority = %d, want 4", got)
	}
	changed, errRecompute = balancer.Recompute(now.Add(time.Minute))
	if errRecompute != nil {
		t.Fatalf("Recompute() second call error = %v", errRecompute)
	}
	if len(changed) != 0 {
		t.Fatalf("Recompute() unchanged = %#v, want empty", changed)
	}
	changed, errRecompute = balancer.Recompute(now.Add(11 * time.Minute))
	if errRecompute != nil {
		t.Fatalf("Recompute() after reset error = %v", errRecompute)
	}
	if got := changed["auth-1"]; got != 3 {
		t.Fatalf("Recompute() after reset priority = %d, want 3", got)
	}
}

func TestQuotaWindowUpsertPreservesKnownLimit(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	initial := QuotaWindow{
		AuthID:      "auth-1",
		Provider:    "codex",
		WindowStart: now.Add(-time.Hour),
		WindowEnd:   now.Add(time.Hour),
		UsedUnits:   80,
		LimitUnits:  100,
		Source:      "openai-tokens",
		UpdatedAt:   now,
	}
	if errUpsert := store.UpsertQuotaWindow(context.Background(), initial); errUpsert != nil {
		t.Fatalf("UpsertQuotaWindow(initial) error = %v", errUpsert)
	}
	retryOnly := initial
	retryOnly.WindowEnd = now.Add(30 * time.Second)
	retryOnly.UsedUnits = 0
	retryOnly.LimitUnits = 0
	retryOnly.Source = "retry-after"
	if errUpsert := store.UpsertQuotaWindow(context.Background(), retryOnly); errUpsert != nil {
		t.Fatalf("UpsertQuotaWindow(retry-only) error = %v", errUpsert)
	}

	windows, errList := store.ListQuotaWindows(context.Background())
	if errList != nil {
		t.Fatalf("ListQuotaWindows() error = %v", errList)
	}
	if len(windows) != 1 || windows[0].UsedUnits != 80 || windows[0].LimitUnits != 100 {
		t.Fatalf("ListQuotaWindows() = %#v, want preserved 80/100", windows)
	}
	if windows[0].Source != "retry-after" || !windows[0].WindowEnd.Equal(retryOnly.WindowEnd) {
		t.Fatalf("ListQuotaWindows() reset metadata = %#v, want retry-after update", windows[0])
	}
}
