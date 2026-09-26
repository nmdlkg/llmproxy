package optimizer

import (
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

// SimWindow is one account window in a rollout. Offsets are relative to the
// decision epoch.
type SimWindow struct {
	Remaining float64
	// Refill is the budget restored at each reset.
	Refill float64
	// ResetIn is the first reset offset; negative means no known reset.
	ResetIn time.Duration
	// Period repeats resets after the first one; zero means a single reset.
	Period time.Duration
	// PerRequest is the mean native consumption per request of each class.
	PerRequest []float64
}

// SimAccount is one upstream account in a rollout.
type SimAccount struct {
	Windows []SimWindow
	// Eligible marks the demand classes this account can serve.
	Eligible []bool
	// Success is the probability that an assigned request succeeds.
	Success float64
}

// SimInput is an immutable description of a rollout problem.
type SimInput struct {
	Accounts []SimAccount
	// Demand holds the expected arrivals per step and class.
	Demand [][]float64
	Step   time.Duration
	// Scenarios is the number of common-random-number demand scenarios.
	Scenarios int
	Seed      uint64
	// DemandCV and ConsumptionCV are scenario-level multiplicative uncertainty.
	DemandCV      float64
	ConsumptionCV float64
	// Deterministic uses expected arrivals directly (oracle comparisons).
	Deterministic bool
	// Prices, when set, makes the continuation policy assign each arrival to
	// the account with the lowest priced consumption (one policy-improvement
	// step over the heuristic continuation). Indexed like Accounts/Windows.
	Prices [][]float64
	// TerminalValue values each request-equivalent of capacity whose window
	// does not reset before the horizon. It stops a short lookahead from
	// treating weekly capacity as free.
	TerminalValue float64
}

// SimResult summarizes the expected outcome of the baseline continuation policy.
type SimResult struct {
	Value    float64
	Served   float64
	Unserved float64
}

type perturbation struct {
	account, window int
	delta           float64
}

// Rollout estimates the expected value of the current budgets under the
// earliest-binding-reset continuation policy.
func Rollout(input SimInput) SimResult {
	return rollout(input, nil)
}

// ShadowPrices estimates the marginal future value of one native unit in each
// window by finite differences of the rollout value under common random
// numbers. prices[a][w] is in request-value per native unit and is never
// negative. A second pass re-evaluates with a continuation policy that uses
// the first-pass prices (approximate policy iteration).
func ShadowPrices(input SimInput, perturbRequests float64) ([][]float64, SimResult) {
	prices, _ := shadowPricesOnce(input, perturbRequests)
	improved := input
	improved.Prices = prices
	return shadowPricesOnce(improved, perturbRequests)
}

func shadowPricesOnce(input SimInput, perturbRequests float64) ([][]float64, SimResult) {
	base := rollout(input, nil)
	prices := make([][]float64, len(input.Accounts))
	for a, account := range input.Accounts {
		prices[a] = make([]float64, len(account.Windows))
		for w, window := range account.Windows {
			mean := meanPerRequest(window.PerRequest, input.Demand)
			if window.Remaining <= 0 || mean <= 0 {
				continue
			}
			delta := math.Min(window.Remaining, math.Max(perturbRequests, 1)*mean)
			reduced := rollout(input, &perturbation{account: a, window: w, delta: delta})
			prices[a][w] = math.Max(0, (base.Value-reduced.Value)/delta)
		}
	}
	return prices, base
}

func meanPerRequest(perRequest []float64, demand [][]float64) float64 {
	var weighted, total float64
	for c, value := range perRequest {
		var classDemand float64
		for _, step := range demand {
			if c < len(step) {
				classDemand += step[c]
			}
		}
		weighted += value * (classDemand + 1e-9)
		total += classDemand + 1e-9
	}
	if total == 0 {
		return 0
	}
	return weighted / total
}

type simWindowState struct {
	remaining float64
	nextReset float64 // in steps; +Inf when none
	period    float64 // in steps; 0 when none
	refill    float64
	perReq    []float64
	resetOnce bool
}

func rollout(input SimInput, perturb *perturbation) SimResult {
	steps := len(input.Demand)
	scenarios := input.Scenarios
	if scenarios <= 0 {
		scenarios = 1
	}
	if steps == 0 || len(input.Accounts) == 0 || input.Step <= 0 {
		return SimResult{}
	}
	stepLen := float64(input.Step)
	var total SimResult
	states := make([][]simWindowState, len(input.Accounts))
	order := make([]int, len(input.Accounts))
	keys := make([]float64, len(input.Accounts))
	costs := make([]float64, len(input.Accounts))
	flexibility := make([]int, len(input.Accounts))
	for a, account := range input.Accounts {
		for _, eligible := range account.Eligible {
			if eligible {
				flexibility[a]++
			}
		}
	}
	for scenario := 0; scenario < scenarios; scenario++ {
		rng := rand.New(rand.NewPCG(input.Seed, uint64(scenario)+1))
		demandScale := lognormal(rng, input.DemandCV)
		for a, account := range input.Accounts {
			states[a] = states[a][:0]
			consumptionScale := lognormal(rng, input.ConsumptionCV)
			for w, window := range account.Windows {
				state := simWindowState{
					remaining: window.Remaining,
					nextReset: math.Inf(1),
					refill:    window.Refill,
				}
				if perturb != nil && perturb.account == a && perturb.window == w {
					state.remaining = math.Max(0, state.remaining-perturb.delta)
				}
				if window.ResetIn >= 0 {
					state.nextReset = float64(window.ResetIn) / stepLen
				}
				if window.Period > 0 {
					state.period = float64(window.Period) / stepLen
				}
				state.perReq = make([]float64, len(window.PerRequest))
				for c, value := range window.PerRequest {
					state.perReq[c] = value * consumptionScale
				}
				states[a] = append(states[a], state)
			}
		}
		var served, unserved, value float64
		for step := 0; step < steps; step++ {
			now := float64(step)
			for a := range states {
				for w := range states[a] {
					state := &states[a][w]
					for state.nextReset <= now {
						state.remaining = state.refill
						state.resetOnce = true
						if state.period <= 0 {
							state.nextReset = math.Inf(1)
							break
						}
						state.nextReset += state.period
					}
				}
			}
			for c, expected := range input.Demand[step] {
				arrivals := expected * demandScale
				if !input.Deterministic {
					arrivals = poisson(rng, arrivals)
				}
				if arrivals <= 0 {
					continue
				}
				candidates := order[:0]
				for a, account := range input.Accounts {
					if c < len(account.Eligible) && account.Eligible[c] {
						candidates = append(candidates, a)
						keys[a] = bindingReset(states[a], c)
						costs[a] = pricedCost(input.Prices, a, states[a], c)
					}
				}
				// Lowest priced consumption first; then less flexible accounts,
				// preserving accounts that can serve other classes; then the
				// capacity that expires soonest.
				sort.SliceStable(candidates, func(i, j int) bool {
					x, y := candidates[i], candidates[j]
					if math.Abs(costs[x]-costs[y]) > 1e-12 {
						return costs[x] < costs[y]
					}
					if flexibility[x] != flexibility[y] {
						return flexibility[x] < flexibility[y]
					}
					return keys[x] < keys[y]
				})
				for _, a := range candidates {
					if arrivals <= 0 {
						break
					}
					capacity := requestCapacity(states[a], c)
					if capacity <= 0 {
						continue
					}
					assigned := math.Min(arrivals, capacity)
					for w := range states[a] {
						if c < len(states[a][w].perReq) {
							states[a][w].remaining -= assigned * states[a][w].perReq[c]
						}
					}
					arrivals -= assigned
					success := input.Accounts[a].Success
					served += assigned
					value += assigned * success
				}
				unserved += arrivals
			}
		}
		if input.TerminalValue > 0 {
			value += input.TerminalValue * terminalCapacity(states, input.Demand)
		}
		total.Served += served
		total.Unserved += unserved
		total.Value += value
	}
	n := float64(scenarios)
	return SimResult{Value: total.Value / n, Served: total.Served / n, Unserved: total.Unserved / n}
}

func pricedCost(prices [][]float64, a int, states []simWindowState, c int) float64 {
	if a >= len(prices) {
		return 0
	}
	var cost float64
	for w, state := range states {
		if w < len(prices[a]) && c < len(state.perReq) {
			cost += prices[a][w] * state.perReq[c]
		}
	}
	return cost
}

// bindingReset orders accounts by the reset of the window that limits their
// capacity for class c: capacity that expires soonest is used first.
func bindingReset(states []simWindowState, c int) float64 {
	binding, limit := math.Inf(1), math.Inf(1)
	for _, state := range states {
		if c >= len(state.perReq) || state.perReq[c] <= 0 {
			continue
		}
		capacity := state.remaining / state.perReq[c]
		if capacity < limit {
			limit, binding = capacity, state.nextReset
		}
	}
	return binding
}

func requestCapacity(states []simWindowState, c int) float64 {
	capacity := math.Inf(1)
	for _, state := range states {
		if c >= len(state.perReq) || state.perReq[c] <= 0 {
			continue
		}
		capacity = math.Min(capacity, math.Max(0, state.remaining)/state.perReq[c])
	}
	if math.IsInf(capacity, 1) {
		return 0
	}
	return capacity
}

// terminalCapacity counts request-equivalents left in windows that never
// reset inside the horizon, capped by each account's other windows.
func terminalCapacity(states [][]simWindowState, demand [][]float64) float64 {
	var total float64
	for _, windows := range states {
		capacity := math.Inf(1)
		carried := false
		for _, state := range windows {
			mean := meanPerRequest(state.perReq, demand)
			if mean <= 0 {
				continue
			}
			if !state.resetOnce {
				carried = true
			}
			capacity = math.Min(capacity, math.Max(0, state.remaining)/mean)
		}
		if carried && !math.IsInf(capacity, 1) {
			total += capacity
		}
	}
	return total
}

func lognormal(rng *rand.Rand, cv float64) float64 {
	if cv <= 0 {
		return 1
	}
	sigma2 := math.Log1p(cv * cv)
	return math.Exp(rng.NormFloat64()*math.Sqrt(sigma2) - sigma2/2)
}

func poisson(rng *rand.Rand, mean float64) float64 {
	switch {
	case mean <= 0:
		return 0
	case mean > 50:
		return math.Max(0, math.Round(mean+rng.NormFloat64()*math.Sqrt(mean)))
	}
	limit, product, count := math.Exp(-mean), rng.Float64(), 0.0
	for product > limit {
		product *= rng.Float64()
		count++
	}
	return count
}
