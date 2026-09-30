package optimizer

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// The oracle instance is small enough for exact dynamic programming. Budgets
// are integer requests; every request consumes one unit of every window of its
// account. Account 0 serves both classes; account 1 serves only class 0.
type oracleInstance struct {
	arrivals    []int // class per step (one arrival per step)
	shortCap    [2]int
	shortPeriod [2]int // steps between short resets (first reset at period)
	weekly      [2]int // weekly budget, no reset inside the horizon
	eligible    [2][2]bool
}

type oracleState struct {
	step   int
	short  [2]int
	weekly [2]int
}

func (o oracleInstance) applyResets(s oracleState) oracleState {
	for a := 0; a < 2; a++ {
		if s.step > 0 && s.step%o.shortPeriod[a] == 0 {
			s.short[a] = o.shortCap[a]
		}
	}
	return s
}

func (o oracleInstance) optimal(s oracleState, memo map[oracleState]int) int {
	if s.step == len(o.arrivals) {
		return 0
	}
	if value, ok := memo[s]; ok {
		return value
	}
	s = o.applyResets(s)
	class := o.arrivals[s.step]
	next := s
	next.step++
	best := o.optimal(next, memo) // reject
	for a := 0; a < 2; a++ {
		if !o.eligible[a][class] || s.short[a] <= 0 || s.weekly[a] <= 0 {
			continue
		}
		chosen := next
		chosen.short[a]--
		chosen.weekly[a]--
		best = max(best, 1+o.optimal(chosen, memo))
	}
	memo[s] = best
	return best
}

// policyServed runs the shadow-price policy: at each arrival it prices the
// remaining deterministic problem and picks the feasible account with the
// lowest opportunity cost.
func (o oracleInstance) policyServed() int {
	s := oracleState{short: o.shortCap, weekly: o.weekly}
	served := 0
	for s.step < len(o.arrivals) {
		s = o.applyResets(s)
		class := o.arrivals[s.step]
		input := SimInput{Step: time.Minute, Scenarios: 1, Deterministic: true}
		for a := 0; a < 2; a++ {
			account := SimAccount{Success: 1, Eligible: []bool{o.eligible[a][0], o.eligible[a][1]}}
			// Rollout step 0 is the next arrival, so resets are relative to step+1.
			resetIn := (o.shortPeriod[a] - (s.step+1)%o.shortPeriod[a]) % o.shortPeriod[a]
			account.Windows = []SimWindow{
				{Remaining: float64(s.short[a]), Refill: float64(o.shortCap[a]), ResetIn: time.Duration(resetIn) * time.Minute, Period: time.Duration(o.shortPeriod[a]) * time.Minute, PerRequest: []float64{1, 1}},
				{Remaining: float64(s.weekly[a]), Refill: float64(o.weekly[a]), ResetIn: -1, PerRequest: []float64{1, 1}},
			}
			input.Accounts = append(input.Accounts, account)
		}
		// Demand excludes the current arrival: the choice is priced against the future.
		for _, future := range o.arrivals[s.step+1:] {
			row := []float64{0, 0}
			row[future] = 1
			input.Demand = append(input.Demand, row)
		}
		chosen, bestCost := -1, math.Inf(1)
		if len(input.Demand) > 0 {
			prices, _ := ShadowPrices(input, 1)
			for a := 0; a < 2; a++ {
				if !o.eligible[a][class] || s.short[a] <= 0 || s.weekly[a] <= 0 {
					continue
				}
				if cost := prices[a][0] + prices[a][1]; cost < bestCost {
					chosen, bestCost = a, cost
				}
			}
		} else {
			for a := 0; a < 2 && chosen < 0; a++ {
				if o.eligible[a][class] && s.short[a] > 0 && s.weekly[a] > 0 {
					chosen = a
				}
			}
		}
		if chosen >= 0 {
			s.short[chosen]--
			s.weekly[chosen]--
			served++
		}
		s.step++
	}
	return served
}

func TestShadowPricePolicyGapToDPOracle(t *testing.T) {
	instances := []oracleInstance{
		{arrivals: []int{0, 0, 1, 0, 1, 1, 0, 1}, shortCap: [2]int{2, 2}, shortPeriod: [2]int{2, 3}, weekly: [2]int{3, 8}, eligible: [2][2]bool{{true, true}, {true, false}}},
		{arrivals: []int{0, 0, 0, 1, 1, 0, 1, 0, 1}, shortCap: [2]int{3, 1}, shortPeriod: [2]int{3, 2}, weekly: [2]int{4, 5}, eligible: [2][2]bool{{true, true}, {true, false}}},
		{arrivals: []int{0, 1, 0, 1, 0, 0, 0, 1, 1, 0}, shortCap: [2]int{1, 2}, shortPeriod: [2]int{1, 4}, weekly: [2]int{5, 6}, eligible: [2][2]bool{{true, true}, {true, false}}},
	}
	totalGap := 0
	for i, instance := range instances {
		start := oracleState{short: instance.shortCap, weekly: instance.weekly}
		optimal := instance.optimal(start, map[oracleState]int{})
		policy := instance.policyServed()
		roundRobin := instance.roundRobinServed()
		t.Logf("instance %d: dp-optimal=%d shadow-price=%d round-robin=%d", i, optimal, policy, roundRobin)
		if policy > optimal {
			t.Fatal(fmt.Sprintf("policy %d exceeds the exact optimum %d: oracle is wrong", policy, optimal))
		}
		totalGap += optimal - policy
	}
	if totalGap > 1 {
		t.Fatalf("shadow-price policy gap to DP oracle = %d requests, want <= 1", totalGap)
	}
}

func (o oracleInstance) roundRobinServed() int {
	s := oracleState{short: o.shortCap, weekly: o.weekly}
	served, cursor := 0, 0
	for s.step < len(o.arrivals) {
		s = o.applyResets(s)
		class := o.arrivals[s.step]
		for i := 0; i < 2; i++ {
			a := (cursor + i) % 2
			if o.eligible[a][class] && s.short[a] > 0 && s.weekly[a] > 0 {
				s.short[a]--
				s.weekly[a]--
				served++
				cursor = a + 1
				break
			}
		}
		s.step++
	}
	return served
}

func BenchmarkEnginePick(b *testing.B) {
	for _, count := range []int{32, 200, 500} {
		b.Run(fmt.Sprintf("candidates-%d", count), func(b *testing.B) {
			cfg, _ := Defaults().Normalize()
			clock := &fakeClock{t: testEpoch}
			engine := NewEngine(cfg, clock.now)
			candidates := make([]Candidate, 0, count)
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("auth-%03d", i)
				candidates = append(candidates, Candidate{ID: id, Provider: "codex", Account: "acct-" + id})
			}
			engine.Pick("gpt-5", candidates, "")
			for i, candidate := range candidates {
				engine.RecordUsage(Usage{AuthID: candidate.ID, Provider: "codex", Model: "gpt-5", Generate: true, Tokens: 1000, CompletedAt: clock.now(),
					Headers: codexHeaders(float64(i), testEpoch.Add(time.Duration(i)*10*time.Minute), float64(i), testEpoch.Add(48*time.Hour))})
			}
			engine.Recompute()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				engine.Pick("gpt-5", candidates, "")
				if i%64 == 0 {
					engine.mu.Lock()
					engine.reservations = map[string][]reservationGroup{}
					engine.reserved = map[string]float64{}
					engine.mu.Unlock()
				}
			}
		})
	}
}
