package optimizer

import (
	"math"
	"sort"
	"time"
)

// Recompute runs a bounded receding-horizon rollout on a copy of the current
// belief and publishes per-window shadow prices. It never runs in the pick path.
func (e *Engine) Recompute() *PriceSnapshot {
	now := e.now()
	input, keys, accounts := e.buildSimInput(now)
	snapshot := &PriceSnapshot{ComputedAt: now, Prices: make(map[string]float64), Steps: len(input.Demand), Accounts: accounts}
	if len(input.Accounts) > 0 && len(input.Demand) > 0 {
		prices, base := ShadowPrices(input, e.cfg.PerturbRequests)
		snapshot.Baseline = base
		for a := range prices {
			for w, price := range prices[a] {
				snapshot.Prices[keys[a][w]] = price
			}
		}
	}
	e.prices.Store(snapshot)
	return snapshot
}

func (e *Engine) buildSimInput(now time.Time) (SimInput, [][]string, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireReservationsLocked(now)
	reserved := e.reservedLocked()

	classes := make([]string, 0, len(e.demand))
	for class := range e.demand {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	classIndex := make(map[string]int, len(classes))
	for i, class := range classes {
		classIndex[class] = i
	}

	accountIDs := make([]string, 0, len(e.accounts))
	for account, state := range e.accounts {
		if now.Sub(state.LastSeen) <= e.cfg.ActiveAccountWindow {
			accountIDs = append(accountIDs, account)
		}
	}
	sort.Strings(accountIDs)

	horizon := e.cfg.Step
	var simAccounts []SimAccount
	var keys [][]string
	windowCount := 0
	for _, account := range accountIDs {
		state := e.accounts[account]
		simAccount := SimAccount{Eligible: make([]bool, len(classes)), Success: e.accountSuccessLocked(account)}
		for class, seen := range state.Classes {
			if index, ok := classIndex[class]; ok && now.Sub(seen) <= e.cfg.ActiveAccountWindow {
				simAccount.Eligible[index] = true
			}
		}
		var accountKeys []string
		for _, view := range e.accountWindowsLocked(account, state.Provider, now) {
			window := SimWindow{
				Remaining:  math.Max(0, view.remaining-reserved[view.key]),
				Refill:     view.capacity,
				ResetIn:    -1,
				Period:     view.period,
				PerRequest: make([]float64, len(classes)),
			}
			if view.stale && view.window.ResetAt.After(now) {
				window.Remaining = math.Max(0, window.Remaining-view.obsSD)
			}
			if !view.resetAt.IsZero() {
				window.ResetIn = maxDuration(0, view.resetAt.Sub(now))
				horizon = maxDuration(horizon, window.ResetIn+e.cfg.Step)
			}
			for class, index := range classIndex {
				window.PerRequest[index], _ = e.consumptionLocked(view.window, view.capacity, class)
			}
			simAccount.Windows = append(simAccount.Windows, window)
			accountKeys = append(accountKeys, view.key)
		}
		windowCount += len(simAccount.Windows)
		simAccounts = append(simAccounts, simAccount)
		keys = append(keys, accountKeys)
	}
	if horizon > e.cfg.MaxHorizon {
		horizon = e.cfg.MaxHorizon
	}

	step, scenarios := e.cfg.Step, e.cfg.Scenarios
	work := func() float64 {
		// Two passes: first-pass prices, then priced continuation.
		return 2 * float64(windowCount+1) * float64(scenarios) * math.Ceil(float64(horizon)/float64(step)) *
			float64(max(len(simAccounts), 1)) * float64(max(len(classes), 1))
	}
	for work() > e.cfg.MaxWork && scenarios > 4 {
		scenarios /= 2
	}
	for work() > e.cfg.MaxWork && step < horizon {
		step *= 2
	}
	steps := int(math.Ceil(float64(horizon) / float64(step)))
	demand := make([][]float64, steps)
	for s := range demand {
		demand[s] = make([]float64, len(classes))
		at := now.Add(time.Duration(s) * step)
		for c, class := range classes {
			demand[s][c] = e.demand[class].Rate(at, now) * step.Hours()
		}
	}
	return SimInput{
		Accounts:      simAccounts,
		Demand:        demand,
		Step:          step,
		Scenarios:     scenarios,
		Seed:          e.cfg.Seed,
		DemandCV:      e.cfg.DemandCV,
		ConsumptionCV: e.cfg.ConsumptionCV,
		TerminalValue: e.cfg.TerminalValue,
	}, keys, len(simAccounts)
}

// accountSuccessLocked averages the health of the credentials of an account.
func (e *Engine) accountSuccessLocked(account string) float64 {
	var sum float64
	var count int
	for authID, identity := range e.identities {
		if identity != account {
			continue
		}
		sum += e.health[authID].SuccessProbability()
		count++
	}
	if count == 0 {
		return 1
	}
	return sum / float64(count)
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
