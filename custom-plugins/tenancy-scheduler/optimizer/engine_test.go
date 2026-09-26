package optimizer

import (
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

var testEpoch = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC) // Monday

func newTestEngine(t *testing.T, mutate func(*Config)) (*Engine, *fakeClock) {
	t.Helper()
	cfg := Defaults()
	cfg.Scenarios = 8
	cfg.Step = time.Hour
	if mutate != nil {
		mutate(&cfg)
	}
	normalized, errNormalize := cfg.Normalize()
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	clock := &fakeClock{t: testEpoch}
	return NewEngine(normalized, clock.now), clock
}

// codexHeaders renders a Codex response carrying both windows. Percentages are
// used percentages; resets are absolute times.
func codexHeaders(shortUsed float64, shortReset time.Time, weeklyUsed float64, weeklyReset time.Time) http.Header {
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", strconv.FormatFloat(shortUsed, 'f', 2, 64))
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-primary-reset-at", strconv.FormatInt(shortReset.Unix(), 10))
	headers.Set("x-codex-secondary-used-percent", strconv.FormatFloat(weeklyUsed, 'f', 2, 64))
	headers.Set("x-codex-secondary-window-minutes", "10080")
	headers.Set("x-codex-secondary-reset-at", strconv.FormatInt(weeklyReset.Unix(), 10))
	return headers
}

func codexCandidates(ids ...string) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{ID: id, Provider: "codex", Account: "acct-" + id})
	}
	return out
}

func TestEngineLearnsConsumptionThroughRounding(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	engine.Pick("gpt-5", codexCandidates("a"), "")
	shortReset, weeklyReset := testEpoch.Add(4*time.Hour), testEpoch.Add(6*24*time.Hour)
	// Integer percentages: 0.4% per 1000-token request is only visible every few requests.
	used := 0.0
	for i := 0; i < 50; i++ {
		clock.advance(time.Minute)
		used += 0.4
		engine.RecordUsage(Usage{
			AuthID: "a", Provider: "codex", Model: "gpt-5", Generate: true, Tokens: 1000, CompletedAt: clock.now(),
			Headers: codexHeaders(math.Floor(used), shortReset, 1, weeklyReset),
		})
	}
	window := engine.windows[WindowKey{Account: "acct-a", Provider: "codex", Scope: "default", Kind: "5h"}.String()]
	rate, ok := window.Estimator.Rate()
	if !ok {
		t.Fatal("rate not learned")
	}
	// 40 bp per 1000 tokens; the first interval is the creation observation.
	if perRequest := rate * 1000; perRequest < 35 || perRequest > 45 {
		t.Fatalf("learned per-request consumption = %.2f bp, want about 40", perRequest)
	}
}

func TestEngineResetStartsNewGenerationWithoutLearningOrDoubleCounting(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	engine.Pick("gpt-5", codexCandidates("a"), "")
	firstReset := testEpoch.Add(time.Hour)
	weeklyReset := testEpoch.Add(5 * 24 * time.Hour)
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", Model: "gpt-5", Tokens: 1000, CompletedAt: clock.now(), Headers: codexHeaders(90, firstReset, 10, weeklyReset)})
	key := WindowKey{Account: "acct-a", Provider: "codex", Scope: "default", Kind: "5h"}.String()
	engine.Pick("gpt-5", codexCandidates("a"), "") // reservation on generation 0
	clock.advance(2 * time.Hour)
	engine.RecordUsage(Usage{AuthID: "b-unrelated", Provider: "codex", Model: "gpt-5", Tokens: 1000, CompletedAt: clock.now()})
	engine.Pick("gpt-5", codexCandidates("a"), "") // second reservation, still generation 0
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", Model: "gpt-5", Tokens: 1000, CompletedAt: clock.now(), Headers: codexHeaders(1, clock.now().Add(5*time.Hour), 11, weeklyReset)})
	window := engine.windows[key]
	if window.Generation != 1 {
		t.Fatalf("generation = %d, want 1", window.Generation)
	}
	if _, ok := window.Estimator.Rate(); ok {
		t.Fatal("a reset boundary must not produce a consumption sample")
	}
	engine.mu.Lock()
	reserved := engine.reservedLocked()[key]
	engine.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("old-generation reservation depletes the new window: %.2f", reserved)
	}
}

func TestEngineIgnoresOutOfOrderObservations(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	engine.Pick("gpt-5", codexCandidates("a"), "")
	reset, weekly := testEpoch.Add(3*time.Hour), testEpoch.Add(5*24*time.Hour)
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now().Add(time.Minute), Headers: codexHeaders(50, reset, 10, weekly)})
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(20, reset, 10, weekly)})
	window := engine.windows[WindowKey{Account: "acct-a", Provider: "codex", Scope: "default", Kind: "5h"}.String()]
	if window.Remaining != 5000 {
		t.Fatalf("remaining = %v, older snapshot overwrote newer one", window.Remaining)
	}
}

func TestEngineReservationsReleaseFIFOAndExpire(t *testing.T) {
	engine, clock := newTestEngine(t, func(c *Config) { c.ReservationTTL = 10 * time.Minute })
	engine.Pick("gpt-5", codexCandidates("a"), "")
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(10, testEpoch.Add(3*time.Hour), 10, testEpoch.Add(5*24*time.Hour))})
	engine.Pick("gpt-5", codexCandidates("a"), "")
	clock.advance(5 * time.Minute)
	engine.Pick("gpt-5", codexCandidates("a"), "")
	if got := len(engine.reservations["a"]); got != 2 {
		t.Fatalf("reservations = %d, want 2", got)
	}
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now()})
	if got := engine.reservations["a"]; len(got) != 1 || !got[0].at.Equal(clock.now()) {
		t.Fatalf("FIFO release removed the wrong reservation: %+v", got)
	}
	clock.advance(11 * time.Minute)
	engine.mu.Lock()
	engine.expireReservationsLocked(clock.now())
	engine.mu.Unlock()
	if len(engine.reservations) != 0 {
		t.Fatalf("orphan reservation not expired: %+v", engine.reservations)
	}
}

func TestEngineConcurrentPicksDoNotShareApparentCapacity(t *testing.T) {
	engine, clock := newTestEngine(t, func(c *Config) { c.PriorRequestsPerWindow = 100 })
	engine.Pick("gpt-5", codexCandidates("a", "b"), "")
	weekly := testEpoch.Add(5 * 24 * time.Hour)
	// a has room for one prior-sized request (100 bp) at the 95% chance bound.
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(97, testEpoch.Add(4*time.Hour), 10, weekly)})
	engine.RecordUsage(Usage{AuthID: "b", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(50, testEpoch.Add(4*time.Hour), 10, weekly)})
	engine.reservations = map[string][]reservationGroup{}
	picks := map[string]int{}
	for i := 0; i < 6; i++ {
		picks[engine.Pick("gpt-5", codexCandidates("a", "b"), "").AuthID]++
	}
	if picks["a"] > 1 {
		t.Fatalf("in-flight reservations ignored: %v", picks)
	}
}

func TestEngineChanceConstraintRejectsExhaustedWindow(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	engine.Pick("gpt-5", codexCandidates("a", "b"), "")
	weekly := testEpoch.Add(5 * 24 * time.Hour)
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(10, testEpoch.Add(time.Hour), 99.99, weekly)})
	engine.RecordUsage(Usage{AuthID: "b", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(60, testEpoch.Add(4*time.Hour), 40, weekly)})
	for i := 0; i < 4; i++ {
		if decision := engine.Pick("gpt-5", codexCandidates("a", "b"), ""); decision.AuthID != "b" || !decision.Admissible {
			t.Fatalf("decision = %+v, want admissible b", decision)
		}
	}
}

func TestEngineUnobservedAccountIsNeitherFreeNorExcluded(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	engine.prices.Store(&PriceSnapshot{Prices: map[string]float64{
		WindowKey{Account: "acct-a", Provider: "codex", Scope: "default", Kind: "5h"}.String(): 0.001,
	}})
	engine.Pick("gpt-5", codexCandidates("a", "u"), "")
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(0, testEpoch.Add(4*time.Hour), 0, testEpoch.Add(5*24*time.Hour))})
	engine.reservations = map[string][]reservationGroup{}
	picks := map[string]int{}
	for i := 0; i < 4; i++ {
		picks[engine.Pick("gpt-5", codexCandidates("a", "u"), "").AuthID]++
	}
	// Equal opportunity cost, so exact ties rotate between both.
	if picks["a"] == 0 || picks["u"] == 0 {
		t.Fatalf("unknown account handling not neutral: %v", picks)
	}
}

func TestEngineShadowReservesExecutedChoice(t *testing.T) {
	engine, _ := newTestEngine(t, nil)
	decision := engine.Pick("gpt-5", codexCandidates("a", "b"), "b")
	stats := engine.Shadow()
	if stats.Picks != 1 || (decision.AuthID == "b") != (stats.Agreements == 1) {
		t.Fatalf("shadow stats = %+v, decision = %+v", stats, decision)
	}
	if len(engine.reservations["b"]) != 1 || len(engine.reservations["a"]) != 0 {
		t.Fatalf("shadow mode must reserve the executed credential: %+v", engine.reservations)
	}
}

func TestEngineStatePersistsRoundTrip(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	engine.Pick("gpt-5", codexCandidates("a"), "")
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", Model: "gpt-5", Generate: true, Tokens: 500, CompletedAt: clock.now(), Headers: codexHeaders(10, testEpoch.Add(time.Hour), 20, testEpoch.Add(48*time.Hour))})
	path := filepath.Join(t.TempDir(), "state.json")
	if errSave := engine.Save(path, "shadow"); errSave != nil {
		t.Fatal(errSave)
	}
	restored, _ := newTestEngine(t, nil)
	if errLoad := restored.Load(path); errLoad != nil {
		t.Fatal(errLoad)
	}
	if len(restored.windows) != 2 || restored.identities["a"] != "acct-a" || restored.classes["codex/gpt-5"] == nil {
		t.Fatalf("restored state incomplete: windows=%d identities=%v", len(restored.windows), restored.identities)
	}
	if errWrite := writeFileAtomic(path, []byte(`{"version":99}`)); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errLoad := restored.Load(path); errLoad == nil {
		t.Fatal("unsupported version must be rejected")
	}
}

func TestEnginePruneDropsRemovedAccounts(t *testing.T) {
	engine, clock := newTestEngine(t, func(c *Config) { c.AccountTTL = time.Hour })
	engine.Pick("gpt-5", codexCandidates("a"), "")
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(10, testEpoch.Add(time.Hour), 20, testEpoch.Add(48*time.Hour))})
	clock.advance(2 * time.Hour)
	engine.Prune()
	if len(engine.windows) != 0 || len(engine.accounts) != 0 || len(engine.health) != 0 {
		t.Fatalf("phantom capacity retained: windows=%d accounts=%d", len(engine.windows), len(engine.accounts))
	}
}

// seedDemand feeds steady hourly arrivals per class so the forecast has history.
func seedDemand(engine *Engine, clock *fakeClock, perHour map[string]int, hours int) {
	for h := 0; h < hours; h++ {
		engine.mu.Lock()
		for class, count := range perHour {
			forecast := engine.demand[class]
			if forecast == nil {
				forecast = &DemandForecast{}
				engine.demand[class] = forecast
			}
			for i := 0; i < count; i++ {
				forecast.Record(clock.now())
			}
		}
		engine.mu.Unlock()
		clock.advance(time.Hour)
	}
}

func TestEnginePrefersSpareWeeklyWhenShortResetIsImminentButWeeklyScarce(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	engine.Pick("gpt-5", codexCandidates("a", "b"), "")
	// Only a can serve the special model, so its weekly allowance has future value.
	engine.Pick("special", codexCandidates("a"), "")
	seedDemand(engine, clock, map[string]int{"codex/gpt-5": 20, "codex/special": 5}, 3)
	now := clock.now()
	// a: 5h resets in 5 minutes with 90% left, but weekly is 97% used for 6 days.
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: now, Headers: codexHeaders(10, now.Add(5*time.Minute), 97, now.Add(6*24*time.Hour))})
	// b: ordinary state with ample weekly capacity.
	engine.RecordUsage(Usage{AuthID: "b", Provider: "codex", CompletedAt: now, Headers: codexHeaders(40, now.Add(3*time.Hour), 30, now.Add(4*24*time.Hour))})
	engine.Pick("gpt-5", codexCandidates("a", "b"), "")
	engine.reservations = map[string][]reservationGroup{}
	engine.Recompute()
	t.Logf("prices=%v", engine.Prices().Prices)
	if decision := engine.Pick("gpt-5", codexCandidates("a", "b"), ""); decision.AuthID != "b" {
		t.Fatalf("decision = %+v, want b to preserve a's scarce weekly capacity; prices=%v", decision, engine.Prices().Prices)
	}
}

func TestEngineUsesExpiringShortCapacityWhenWeeklyIsAmple(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	engine.Pick("gpt-5", codexCandidates("a", "b"), "")
	// Demand exceeds the short windows, so short capacity is contested.
	seedDemand(engine, clock, map[string]int{"codex/gpt-5": 200}, 3)
	now := clock.now()
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: now, Headers: codexHeaders(10, now.Add(5*time.Minute), 5, now.Add(2*24*time.Hour))})
	engine.RecordUsage(Usage{AuthID: "b", Provider: "codex", CompletedAt: now, Headers: codexHeaders(90, now.Add(3*time.Hour), 10, now.Add(4*24*time.Hour))})
	engine.Pick("gpt-5", codexCandidates("a", "b"), "")
	engine.reservations = map[string][]reservationGroup{}
	engine.Recompute()
	t.Logf("prices=%v", engine.Prices().Prices)
	if decision := engine.Pick("gpt-5", codexCandidates("a", "b"), ""); decision.AuthID != "a" {
		t.Fatalf("decision = %+v, want a (use-it-or-lose-it); prices=%v", decision, engine.Prices().Prices)
	}
}
