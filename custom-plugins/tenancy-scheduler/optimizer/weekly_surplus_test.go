package optimizer

import (
	"math"
	"testing"
	"time"
)

func TestWeeklySurplusUsesRemainingAndResetFractions(t *testing.T) {
	now := testEpoch
	engine, _ := newTestEngine(t, nil)
	window := &Window{Key: WindowKey{Provider: "claude", Scope: "default", Kind: "7d"}, Unit: UnitPercentBP, Capacity: percentScale, Remaining: 8000, ResetAt: now.Add(3 * 24 * time.Hour), Duration: int64(7 * 24 * time.Hour), ObservedAt: now}
	views := []windowView{{key: window.Key.String(), window: window, remaining: 8000, capacity: percentScale, period: 7 * 24 * time.Hour}}
	got := engine.weeklySurplusLocked(views, "claude/test", nil, now)
	if math.Abs(got-13.0/35.0) > 1e-9 {
		t.Fatalf("surplus = %v, want 13/35", got)
	}
}

func TestWeeklySurplusDoesNotRewardStaleWindows(t *testing.T) {
	now := testEpoch
	engine, _ := newTestEngine(t, nil)
	window := &Window{Key: WindowKey{Provider: "claude", Scope: "default", Kind: "7d"}, Unit: UnitPercentBP, Capacity: percentScale, Remaining: 9000, ResetAt: now.Add(time.Hour), Duration: int64(7 * 24 * time.Hour), ObservedAt: now.Add(-time.Hour)}
	views := []windowView{{key: window.Key.String(), window: window, remaining: 9000, capacity: percentScale, period: 7 * 24 * time.Hour, stale: true}}
	if got := engine.weeklySurplusLocked(views, "claude/test", nil, now); got != 0 {
		t.Fatalf("stale surplus = %v, want 0", got)
	}
}

func TestClaudeWeeklyWeightIsCapped(t *testing.T) {
	if got := claudeWeeklyWeight(1, 100); got != maxClaudeWeeklyWeight {
		t.Fatalf("upper cap = %v, want %v", got, maxClaudeWeeklyWeight)
	}
	if got := claudeWeeklyWeight(-1, 100); got != 1 {
		t.Fatalf("negative gap = %v, want 1", got)
	}
}
