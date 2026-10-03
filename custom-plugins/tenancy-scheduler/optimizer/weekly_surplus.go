package optimizer

import (
	"math"
	"time"
)

const (
	minClaudeWeeklyWeight = 0.5
	maxClaudeWeeklyWeight = 5.0
)

// weeklySurplusLocked measures allowance ahead of a uniform weekly burn rate.
// Every applicable weekly window must have fresh data; the tightest gap wins.
// Unknown, expired, or stale observations never earn a scheduling bonus.
func (e *Engine) weeklySurplusLocked(views []windowView, class string, reserved map[string]float64, now time.Time) float64 {
	gap := 1.0
	found := false
	for _, view := range views {
		if view.window.Unit != UnitPercentBP || view.period != 7*24*time.Hour || !e.scopedWindowAppliesLocked(view.key, class) {
			continue
		}
		if view.stale || view.capacity <= 0 || !view.window.ResetAt.After(now) {
			return 0
		}
		remaining := math.Max(0, math.Min(1, (view.remaining-reserved[view.key])/view.capacity))
		timeLeft := math.Min(1, float64(view.window.ResetAt.Sub(now))/float64(view.period))
		gap = math.Min(gap, math.Max(0, remaining-timeLeft))
		found = true
	}
	if !found {
		return 0
	}
	return gap
}

func claudeWeeklyWeight(gap, coefficient float64) float64 {
	weight := 1 + math.Max(0, gap)*math.Max(0, coefficient)
	return math.Min(maxClaudeWeeklyWeight, math.Max(minClaudeWeeklyWeight, weight))
}
