package optimizer

import (
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"testing"
	"time"
)

// The evaluation harness drives policies through a synthetic world with true
// (hidden) windows. Policies see only what the host would give them: the
// candidates not in cooldown and integer-percent response headers.

type worldWindow struct {
	remaining float64 // basis points
	period    time.Duration
	nextReset time.Time
	// perRequest is the true mean basis-point cost of one request.
	perRequest float64
}

type worldAccount struct {
	id      string
	short   *worldWindow // nil for weekly-only accounts
	weekly  *worldWindow // nil for short-only accounts
	special bool         // can serve the exclusive model
}

func (a *worldAccount) windows() []*worldWindow {
	var out []*worldWindow
	for _, w := range []*worldWindow{a.short, a.weekly} {
		if w != nil {
			out = append(out, w)
		}
	}
	return out
}

func (a *worldAccount) exhausted() bool {
	for _, w := range a.windows() {
		if w.remaining <= 0 {
			return true
		}
	}
	return false
}

type scenario struct {
	name        string
	accounts    func(start time.Time) []*worldAccount
	ratePerHour float64
	specialFrac float64
	days        int
}

type policy interface {
	name() string
	choose(model string, candidates []*worldAccount, now time.Time) *worldAccount
	observe(account *worldAccount, model string, now time.Time)
}

// observedWindow is what header-based baselines know about an account window.
type observedWindow struct {
	usedFrac float64
	resetAt  time.Time
	duration time.Duration
}

type headerMemory map[string][]observedWindow

func (m headerMemory) record(account *worldAccount) {
	var windows []observedWindow
	for _, w := range account.windows() {
		windows = append(windows, observedWindow{usedFrac: math.Round((10000-w.remaining)/100) / 100, resetAt: w.nextReset, duration: w.period})
	}
	m[account.id] = windows
}

type roundRobin struct{ cursor int }

func (p *roundRobin) name() string { return "round-robin" }
func (p *roundRobin) choose(_ string, c []*worldAccount, _ time.Time) *worldAccount {
	p.cursor++
	return c[p.cursor%len(c)]
}
func (p *roundRobin) observe(*worldAccount, string, time.Time) {}

// legacyBonus reproduces the host rule: +1 priority when the shortest known
// window resets within 30 minutes and is below 90% used; rotate in the top tier.
type legacyBonus struct {
	cursor int
	memory headerMemory
}

func (p *legacyBonus) name() string { return "legacy-30m-90%" }
func (p *legacyBonus) choose(_ string, c []*worldAccount, now time.Time) *worldAccount {
	var urgent []*worldAccount
	for _, account := range c {
		windows := p.memory[account.id]
		if len(windows) == 0 {
			continue
		}
		shortest := windows[0]
		for _, w := range windows[1:] {
			if w.duration < shortest.duration {
				shortest = w
			}
		}
		if left := shortest.resetAt.Sub(now); left > 0 && left < 30*time.Minute && shortest.usedFrac < 0.9 {
			urgent = append(urgent, account)
		}
	}
	if len(urgent) == 0 {
		urgent = c
	}
	p.cursor++
	return urgent[p.cursor%len(urgent)]
}
func (p *legacyBonus) observe(a *worldAccount, _ string, _ time.Time) { p.memory.record(a) }

type earliestReset struct{ memory headerMemory }

func (p *earliestReset) name() string { return "earliest-reset-first" }
func (p *earliestReset) choose(_ string, c []*worldAccount, _ time.Time) *worldAccount {
	best, bestReset := c[0], time.Time{}
	for _, account := range c {
		for _, w := range p.memory[account.id] {
			if bestReset.IsZero() || w.resetAt.Before(bestReset) {
				best, bestReset = account, w.resetAt
			}
		}
	}
	return best
}
func (p *earliestReset) observe(a *worldAccount, _ string, _ time.Time) { p.memory.record(a) }

// bindingGreedy is the forecast-free greedy baseline: prefer the account whose
// capacity-limiting window resets soonest (the rollout continuation policy).
type bindingGreedy struct{ memory headerMemory }

func (p *bindingGreedy) name() string { return "binding-reset-greedy" }
func (p *bindingGreedy) choose(_ string, c []*worldAccount, _ time.Time) *worldAccount {
	best, bestKey := c[0], time.Time{}
	for _, account := range c {
		windows := p.memory[account.id]
		if len(windows) == 0 {
			continue
		}
		binding := windows[0]
		for _, w := range windows[1:] {
			// Weekly percent is worth more requests per point; compare in short-window points.
			if (1-w.usedFrac)*float64(w.duration) < (1-binding.usedFrac)*float64(binding.duration) {
				binding = w
			}
		}
		if bestKey.IsZero() || binding.resetAt.Before(bestKey) {
			best, bestKey = account, binding.resetAt
		}
	}
	return best
}
func (p *bindingGreedy) observe(a *worldAccount, _ string, _ time.Time) { p.memory.record(a) }

type optimizerPolicy struct {
	engine  *Engine
	clock   *fakeClock
	lastRun time.Time
}

func (p *optimizerPolicy) name() string { return "optimizer" }
func (p *optimizerPolicy) choose(model string, c []*worldAccount, now time.Time) *worldAccount {
	p.clock.t = now
	if now.Sub(p.lastRun) >= 30*time.Minute {
		p.engine.Recompute()
		p.lastRun = now
	}
	candidates := make([]Candidate, 0, len(c))
	byID := make(map[string]*worldAccount, len(c))
	for _, account := range c {
		candidates = append(candidates, Candidate{ID: account.id, Provider: "codex", Account: account.id})
		byID[account.id] = account
	}
	return byID[p.engine.Pick(model, candidates, "").AuthID]
}
func (p *optimizerPolicy) observe(a *worldAccount, model string, now time.Time) {
	p.clock.t = now
	headers := http.Header{}
	set := func(slot string, w *worldWindow) {
		used := math.Round((10000 - math.Max(0, w.remaining)) / 100)
		headers["X-Codex-"+slot+"-Used-Percent"] = []string{formatInt(used)}
		headers["X-Codex-"+slot+"-Window-Minutes"] = []string{formatInt(w.period.Minutes())}
		headers["X-Codex-"+slot+"-Reset-At"] = []string{formatInt(float64(w.nextReset.Unix()))}
	}
	if a.short != nil {
		set("Primary", a.short)
	}
	if a.weekly != nil {
		set("Secondary", a.weekly)
	}
	p.engine.RecordUsage(Usage{AuthID: a.id, Provider: "codex", Model: model, Generate: true, Tokens: 1000, CompletedAt: now, Headers: headers})
}

func formatInt(v float64) string { return strconv.FormatInt(int64(v), 10) }

type outcome struct {
	served, rejected int
}

func runScenario(sc scenario, p policy, seed uint64) outcome {
	start := testEpoch
	accounts := sc.accounts(start)
	rng := rand.New(rand.NewPCG(seed, 99))
	var out outcome
	end := start.Add(time.Duration(sc.days) * 24 * time.Hour)
	for now := start; now.Before(end); now = now.Add(time.Minute) {
		for _, account := range accounts {
			for _, w := range account.windows() {
				for !now.Before(w.nextReset) {
					w.remaining = 10000
					w.nextReset = w.nextReset.Add(w.period)
				}
			}
		}
		// Diurnal demand: busier during the day, quiet at night (UTC).
		hour := float64(now.Hour()) + float64(now.Minute())/60
		rate := sc.ratePerHour * (1 + 0.6*math.Sin((hour-8)/24*2*math.Pi)) / 60
		arrivals := int(poisson(rng, rate))
		for i := 0; i < arrivals; i++ {
			model := "gpt-5"
			if rng.Float64() < sc.specialFrac {
				model = "special"
			}
			var candidates []*worldAccount
			for _, account := range accounts {
				if account.exhausted() || (model == "special" && !account.special) {
					continue
				}
				candidates = append(candidates, account)
			}
			sort.Slice(candidates, func(i, j int) bool { return candidates[i].id < candidates[j].id })
			if len(candidates) == 0 {
				out.rejected++
				continue
			}
			chosen := p.choose(model, candidates, now)
			noise := lognormal(rng, 0.3)
			for _, w := range chosen.windows() {
				w.remaining -= w.perRequest * noise
			}
			out.served++
			p.observe(chosen, model, now)
		}
	}
	return out
}

func codexAccount(id string, start time.Time, shortOffset, weeklyOffset time.Duration, shortCost, weeklyCost float64, special bool) *worldAccount {
	account := &worldAccount{id: id, special: special}
	if shortCost > 0 {
		account.short = &worldWindow{remaining: 10000, period: 5 * time.Hour, nextReset: start.Add(shortOffset), perRequest: shortCost}
	}
	if weeklyCost > 0 {
		account.weekly = &worldWindow{remaining: 10000, period: 7 * 24 * time.Hour, nextReset: start.Add(weeklyOffset), perRequest: weeklyCost}
	}
	return account
}

func evaluationSuite() []scenario {
	return []scenario{
		{
			name: "staggered-resets",
			accounts: func(s time.Time) []*worldAccount {
				return []*worldAccount{
					codexAccount("a", s, 1*time.Hour, 2*24*time.Hour, 50, 8, false),
					codexAccount("b", s, 3*time.Hour, 4*24*time.Hour, 50, 8, false),
					codexAccount("c", s, 4*time.Hour, 6*24*time.Hour, 50, 8, false),
				}
			},
			ratePerHour: 40, days: 7,
		},
		{
			name: "exclusive-model-weekly-scarce",
			accounts: func(s time.Time) []*worldAccount {
				a := codexAccount("a", s, 10*time.Minute, 6*24*time.Hour, 40, 6, true)
				a.weekly.remaining = 3000
				return []*worldAccount{a,
					codexAccount("b", s, 2*time.Hour, 3*24*time.Hour, 40, 6, false),
					codexAccount("c", s, 4*time.Hour, 5*24*time.Hour, 40, 6, false),
				}
			},
			ratePerHour: 20, specialFrac: 0.1, days: 7,
		},
		{
			name: "mixed-short-only-and-weekly-only",
			accounts: func(s time.Time) []*worldAccount {
				return []*worldAccount{
					codexAccount("a", s, 2*time.Hour, 0, 60, 0, false),
					codexAccount("b", s, 0, 3*24*time.Hour, 0, 5, false),
					codexAccount("c", s, 30*time.Minute, 5*24*time.Hour, 50, 8, false),
				}
			},
			ratePerHour: 62, days: 7,
		},
	}
}

func newOptimizerPolicy(t *testing.T) *optimizerPolicy {
	engine, clock := newTestEngine(t, func(c *Config) {
		c.Step = time.Hour
		c.Scenarios = 6
		c.PriorRequestsPerWindow = 200
	})
	return &optimizerPolicy{engine: engine, clock: clock}
}

func TestEvaluationSuiteOptimizerVersusBaselines(t *testing.T) {
	if testing.Short() {
		t.Skip("evaluation suite is slow")
	}
	seeds := []uint64{1, 2, 3, 4, 5}
	totals := map[string]int{}
	for _, sc := range evaluationSuite() {
		var diffs []float64
		legacyServed := 0
		for _, seed := range seeds {
			policies := []policy{
				&roundRobin{},
				&legacyBonus{memory: headerMemory{}},
				&earliestReset{memory: headerMemory{}},
				&bindingGreedy{memory: headerMemory{}},
				newOptimizerPolicy(t),
			}
			served := map[string]int{}
			for _, p := range policies {
				result := runScenario(sc, p, seed)
				served[p.name()] = result.served
				totals[p.name()] += result.served
				t.Logf("%-34s seed=%d %-22s served=%6d rejected=%6d", sc.name, seed, p.name(), result.served, result.rejected)
			}
			diffs = append(diffs, float64(served["optimizer"]-served["legacy-30m-90%"]))
			legacyServed += served["legacy-30m-90%"]
		}
		mean, halfWidth := meanCI95(diffs)
		t.Logf("%-34s optimizer-legacy served: mean=%.1f 95%%CI=[%.1f, %.1f]", sc.name, mean, mean-halfWidth, mean+halfWidth)
		// No material regression in any scenario (0.5% of legacy volume).
		if mean < -0.005*float64(legacyServed)/float64(len(seeds)) {
			t.Fatalf("%s: optimizer regresses against legacy by %.1f requests per run", sc.name, -mean)
		}
	}
	for name, served := range totals {
		t.Logf("TOTAL %-22s served=%d", name, served)
	}
	if totals["optimizer"] <= totals["legacy-30m-90%"] {
		t.Fatalf("optimizer served %d <= legacy %d on the mixed-window suite", totals["optimizer"], totals["legacy-30m-90%"])
	}
}

// meanCI95 returns the mean and the 95% half-width using a normal approximation.
func meanCI95(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))
	if len(values) < 2 {
		return mean, 0
	}
	var sq float64
	for _, v := range values {
		sq += (v - mean) * (v - mean)
	}
	return mean, 1.96 * math.Sqrt(sq/float64(len(values)-1)/float64(len(values)))
}
