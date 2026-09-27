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
	e.purgeResetReservationsLocked(now)
	reserved := e.reservedLocked()

	classes := make([]string, 0, len(e.demand))
	for class := range e.demand {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	classSeen := make(map[string]time.Time, len(classes))
	for _, class := range classes {
		if forecast := e.demand[class]; forecast != nil {
			classSeen[class] = maxTime(classSeen[class], forecast.LastSeen)
		}
		if stats := e.classes[class]; stats != nil {
			classSeen[class] = maxTime(classSeen[class], stats.LastSeen)
		}
	}
	if len(classes) > 256 {
		sort.SliceStable(classes, func(i, j int) bool {
			return classSeen[classes[i]].After(classSeen[classes[j]])
		})
		classes = classes[:256]
		sort.Strings(classes)
	}
	simClassOrder := append([]string(nil), classes...)
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

	configuredStep := e.cfg.Step
	if configuredStep <= 0 {
		configuredStep = 30 * time.Minute
	}
	maxHorizon := e.cfg.MaxHorizon
	if maxHorizon <= 0 {
		maxHorizon = 8 * 24 * time.Hour
	}
	if maxHorizon > 30*24*time.Hour {
		maxHorizon = 30 * 24 * time.Hour
	}
	horizon := configuredStep
	var simAccounts []SimAccount
	var keys [][]string
	var accountSeen []time.Time
	windowCount := 0
	shortestPeriod := time.Duration(0)
	longestShort := configuredStep
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
				horizon = maxDuration(horizon, window.ResetIn+configuredStep)
			}
			for class, index := range classIndex {
				if e.scopedWindowAppliesLocked(view.key, class) {
					window.PerRequest[index], _ = e.consumptionLocked(view.window, view.capacity, class)
				}
			}
			if view.period > 0 && (shortestPeriod == 0 || view.period < shortestPeriod) {
				shortestPeriod = view.period
			}
			if view.period > 0 && view.period < 7*24*time.Hour {
				longestShort = maxDuration(longestShort, view.period)
			}
			simAccount.Windows = append(simAccount.Windows, window)
			accountKeys = append(accountKeys, view.key)
		}
		windowCount += len(simAccount.Windows)
		simAccounts = append(simAccounts, simAccount)
		keys = append(keys, accountKeys)
		accountSeen = append(accountSeen, state.LastSeen)
	}
	if horizon > maxHorizon {
		horizon = maxHorizon
	}

	step, scenarios := configuredStep, e.cfg.Scenarios
	if scenarios <= 0 {
		scenarios = 1
	}
	if scenarios > 64 {
		scenarios = 64
	}
	maxWork := e.cfg.MaxWork
	if maxWork <= 0 || maxWork > 1e8 {
		maxWork = 5e7
	}
	maxStep := maxHorizon
	if shortestPeriod > 0 {
		maxStep = maxDuration(configuredStep, shortestPeriod/2)
		if maxStep > maxHorizon {
			maxStep = maxHorizon
		}
	}
	work := func(h time.Duration, s time.Duration, scenarioCount, accountCount, windows, classCount int) float64 {
		steps := math.Ceil(float64(maxDuration(h, s)) / float64(s))
		// Two passes: first-pass prices, then priced continuation.
		return 2 * float64(windows+1) * float64(scenarioCount) * steps *
			float64(max(accountCount, 1)) * float64(max(classCount, 1))
	}
	for work(horizon, step, scenarios, len(simAccounts), windowCount, len(classes)) > maxWork && scenarios > 2 {
		next := scenarios / 2
		if next < 2 {
			next = 2
		}
		if next == scenarios {
			break
		}
		scenarios = next
	}
	for work(horizon, step, scenarios, len(simAccounts), windowCount, len(classes)) > maxWork && step < maxStep {
		next := step * 2
		if next > maxStep {
			next = maxStep
		}
		if next == step {
			break
		}
		step = next
	}
	minimumHorizon := maxDuration(configuredStep, longestShort)
	if minimumHorizon > maxHorizon {
		minimumHorizon = maxHorizon
	}
	for work(horizon, step, scenarios, len(simAccounts), windowCount, len(classes)) > maxWork && horizon > minimumHorizon {
		next := horizon / 2
		if next < minimumHorizon {
			next = minimumHorizon
		}
		if next == horizon {
			break
		}
		horizon = next
	}
	for work(horizon, step, scenarios, len(simAccounts), windowCount, len(classes)) > maxWork && len(simAccounts) > 1 {
		oldest := 0
		for i := 1; i < len(accountSeen); i++ {
			if accountSeen[i].Before(accountSeen[oldest]) {
				oldest = i
			}
		}
		windowCount -= len(simAccounts[oldest].Windows)
		simAccounts = append(simAccounts[:oldest], simAccounts[oldest+1:]...)
		keys = append(keys[:oldest], keys[oldest+1:]...)
		accountSeen = append(accountSeen[:oldest], accountSeen[oldest+1:]...)
	}
	const maxDemandCells = 20000
	for work(horizon, step, scenarios, len(simAccounts), windowCount, len(classes)) > maxWork && step < maxStep {
		next := step * 2
		if next > maxStep {
			next = maxStep
		}
		if next == step {
			break
		}
		step = next
	}
	for int(math.Ceil(float64(maxDuration(horizon, step))/float64(step)))*len(classes) > maxDemandCells && horizon > minimumHorizon {
		horizon /= 2
		if horizon < minimumHorizon {
			horizon = minimumHorizon
		}
	}
	steps := int(math.Ceil(float64(maxDuration(horizon, step)) / float64(step)))
	if steps < 1 {
		steps = 1
	}
	if steps*len(classes) > maxDemandCells && len(classes) > 1 {
		keep := max(1, maxDemandCells/steps)
		if keep < len(classes) {
			sort.SliceStable(classes, func(i, j int) bool {
				return classSeen[classes[i]].After(classSeen[classes[j]])
			})
			classes = classes[:keep]
			sort.Strings(classes)
			classIndex = make(map[string]int, len(classes))
			for i, class := range classes {
				classIndex[class] = i
			}
			oldClassIndex := make(map[string]int, len(simClassOrder))
			for i, class := range simClassOrder {
				oldClassIndex[class] = i
			}
			for a := range simAccounts {
				oldEligible := simAccounts[a].Eligible
				newEligible := make([]bool, len(classes))
				for i, class := range classes {
					if oldIndex, ok := oldClassIndex[class]; ok && oldIndex < len(oldEligible) {
						newEligible[i] = oldEligible[oldIndex]
					}
				}
				simAccounts[a].Eligible = newEligible
				for w := range simAccounts[a].Windows {
					oldPerRequest := simAccounts[a].Windows[w].PerRequest
					newPerRequest := make([]float64, len(classes))
					for i, class := range classes {
						if oldIndex, ok := oldClassIndex[class]; ok && oldIndex < len(oldPerRequest) {
							newPerRequest[i] = oldPerRequest[oldIndex]
						}
					}
					simAccounts[a].Windows[w].PerRequest = newPerRequest
				}
			}
			simClassOrder = append([]string(nil), classes...)
		}
	}
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
