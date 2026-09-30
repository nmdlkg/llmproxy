package optimizer

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTelemetryScopesShadowFallbacksAndHistory(t *testing.T) {
	e, clock := newTestEngine(t, nil)
	e.StartEpoch("shadow", false)
	e.Pick("gpt-5", codexCandidates("a"), "a")
	e.RecordUsage(Usage{AuthID: "a", Provider: "codex", Headers: codexHeaders(100, clock.now().Add(time.Hour), 100, clock.now().Add(7*24*time.Hour))})
	e.Pick("gpt-5", codexCandidates("a"), "a")
	shadow := e.Shadow()
	if shadow.Picks != 2 || shadow.Fallbacks != 1 || shadow.Agreements != 2 {
		t.Fatalf("shadow counters = %+v", shadow)
	}
	prior := e.TelemetrySnapshot()
	clock.advance(time.Second)
	e.StartEpoch("optimizer", false)
	e.Pick("gpt-5", codexCandidates("a"), "")
	current := e.TelemetrySnapshot()
	if e.Shadow().Fallbacks != 0 || e.Shadow().Picks != 0 || current.InadmissibleDecisions != 1 {
		t.Fatalf("optimizer picks leaked into shadow: shadow=%+v telemetry=%+v", e.Shadow(), current)
	}
	if current.EpochID == prior.EpochID || current.ConfigFingerprint != prior.ConfigFingerprint || current.CarriedReservations != 1 {
		t.Fatalf("epoch policy identity = %+v; prior = %+v", current, prior)
	}
	if history := e.TelemetryHistory(); len(history) != 1 || history[0].Mode != "shadow" || history[0].Decisions != 2 {
		t.Fatalf("history = %+v", history)
	}
	for range maxTelemetryHistory + 3 {
		e.StartEpoch("optimizer", false)
	}
	if got := len(e.TelemetryHistory()); got != maxTelemetryHistory {
		t.Fatalf("history length = %d", got)
	}
}

func TestTelemetryUsageOutcomesHistogramsAndBoundary(t *testing.T) {
	e, clock := newTestEngine(t, nil)
	e.StartEpoch("shadow", false)
	e.Pick("gpt-5", codexCandidates("a"), "a")
	e.RecordUsage(Usage{AuthID: "a", Provider: "codex", RequestedAt: clock.now(), Latency: 100 * time.Millisecond, TTFT: 5 * time.Millisecond,
		Headers: codexHeaders(10, clock.now().Add(time.Hour), 10, clock.now().Add(7*24*time.Hour))})
	e.RecordUsage(Usage{AuthID: "a", Provider: "codex", RequestedAt: clock.now(), Failed: true, StatusCode: 429, Latency: 301 * time.Second})
	e.RecordUsage(Usage{AuthID: "a", Provider: "codex", RequestedAt: clock.now(), Failed: true, StatusCode: 503, Latency: -1})
	e.RecordUsage(Usage{AuthID: "a", Provider: "codex", RequestedAt: clock.now().Add(-time.Second), Failed: true, StatusCode: 429, Latency: time.Second})
	tel := e.TelemetrySnapshot()
	if tel.UsageRecords != 3 || tel.UsageSuccesses != 1 || tel.UsageFailures != 2 || tel.Usage429 != 1 || tel.BoundaryUsage != 1 || tel.UsageWithQuota != 1 || tel.UsageWithoutQuota != 2 {
		t.Fatalf("outcomes = %+v", tel)
	}
	if tel.ReservationsCreated != 1 || tel.ReservationsReleased != 1 || tel.UnmatchedUsage != 2 {
		t.Fatalf("lifecycle = %+v", tel)
	}
	if tel.Latency.Samples != 2 || tel.Latency.Counts[0] != 1 || tel.Latency.Counts[len(tel.Latency.Counts)-1] != 1 || tel.Latency.SumMS != 301100 || tel.TTFT.Samples != 1 {
		t.Fatalf("histograms = %+v, %+v", tel.Latency, tel.TTFT)
	}
	tel.Latency.Counts[0] = 99
	if e.TelemetrySnapshot().Latency.Counts[0] != 1 {
		t.Fatal("snapshot histogram aliases engine state")
	}
	e.StartEpoch("optimizer", false)
	history := e.TelemetryHistory()
	history[0].Latency.Counts[0] = 99
	if e.TelemetryHistory()[0].Latency.Counts[0] != 1 {
		t.Fatal("history histogram aliases engine state")
	}
}

func TestTelemetryReservationExpirationAndReset(t *testing.T) {
	e, clock := newTestEngine(t, func(c *Config) { c.ReservationTTL = time.Minute })
	e.StartEpoch("optimizer", false)
	e.Pick("gpt-5", codexCandidates("unknown"), "")
	clock.advance(time.Second)
	e.Pick("gpt-5", codexCandidates("unknown"), "")
	if len(e.reservations["unknown"]) != 2 {
		t.Fatal("unobserved attempts must survive reset sweep until usage/TTL")
	}
	clock.advance(time.Minute)
	e.expireReservationsLocked(clock.now())
	if tel := e.TelemetrySnapshot(); tel.ReservationsExpired != 2 || tel.ReservationsInvalidated != 0 {
		t.Fatalf("expiration = %+v", tel)
	}
	e.RecordUsage(Usage{AuthID: "a", Provider: "codex", Headers: codexHeaders(10, clock.now().Add(30*time.Second), 10, clock.now().Add(30*time.Second))})
	e.Pick("gpt-5", []Candidate{{ID: "a", Provider: "codex"}}, "")
	clock.advance(31 * time.Second)
	e.purgeResetReservationsLocked(clock.now())
	if tel := e.TelemetrySnapshot(); tel.ReservationsInvalidated != 1 || tel.ReservationsExpired != 2 {
		t.Fatalf("reset invalidation = %+v", tel)
	}
}

func TestTelemetryReservationEpochAndPruning(t *testing.T) {
	e, clock := newTestEngine(t, func(c *Config) { c.AccountTTL = time.Minute })
	e.StartEpoch("shadow", false)
	e.Pick("gpt-5", codexCandidates("a"), "a")
	oldStart := clock.now()
	clock.advance(time.Second)
	e.StartEpoch("optimizer", false)
	e.RecordUsage(Usage{AuthID: "a", RequestedAt: oldStart})
	if tel := e.TelemetrySnapshot(); tel.BoundaryUsage != 1 || tel.PriorEpochReservationsRemoved != 1 || tel.ReservationsReleased != 0 {
		t.Fatalf("old epoch completion = %+v", tel)
	}
	e.Pick("gpt-5", codexCandidates("a"), "")
	e.RecordUsage(Usage{AuthID: "a", RequestedAt: oldStart})
	if tel := e.TelemetrySnapshot(); tel.ReservationsReleased != 0 || len(e.reservations["a"]) != 1 {
		t.Fatalf("old completion consumed current epoch reservation: %+v", tel)
	}
	clock.advance(2 * time.Minute)
	e.Prune()
	if tel := e.TelemetrySnapshot(); tel.ReservationsPruned != 1 {
		t.Fatalf("pruning = %+v", tel)
	}
}

func TestTelemetryCoverageAndFreshness(t *testing.T) {
	e, clock := newTestEngine(t, func(c *Config) { c.StaleAfter = time.Minute })
	e.StartEpoch("optimizer", false)
	e.Pick("gpt-5", codexCandidates("a", "u"), "")
	e.RecordUsage(Usage{AuthID: "a", Provider: "codex", Headers: codexHeaders(10, clock.now().Add(time.Hour), 10, clock.now().Add(7*24*time.Hour))})
	clock.advance(2 * time.Minute)
	e.Pick("gpt-5", codexCandidates("a"), "")
	tel := e.TelemetrySnapshot()
	if tel.UnknownCandidateDecisions != 1 || tel.UnpricedDecisions != 2 || tel.StaleWindowDecisions != 1 || tel.KnownAccounts != 1 || tel.StaleWindows != 2 || tel.UnlearnedWindows != 2 || tel.OldestObservationAge != 120 {
		t.Fatalf("coverage = %+v", tel)
	}
	e.prices.Store(&PriceSnapshot{ComputedAt: clock.now().Add(-2 * time.Minute), Prices: map[string]float64{}})
	e.Pick("gpt-5", codexCandidates("a"), "")
	if e.TelemetrySnapshot().StalePriceDecisions != 1 {
		t.Fatal("stale price decision was not recorded")
	}
}

func TestTelemetryFingerprintIncludesSchedulingCadence(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	e.StartEpoch("shadow", false, time.Minute, 5*time.Minute)
	first := e.TelemetrySnapshot().ConfigFingerprint
	e.StartEpoch("shadow", false, 2*time.Minute, 5*time.Minute)
	second := e.TelemetrySnapshot().ConfigFingerprint
	if first == second {
		t.Fatal("recompute cadence must distinguish telemetry cohorts")
	}
}

func TestTelemetryStateRoundTripRestartAndLegacyCompatibility(t *testing.T) {
	e, clock := newTestEngine(t, nil)
	e.StartEpoch("shadow", false)
	e.Pick("gpt-5", codexCandidates("a"), "a")
	e.RecordUsage(Usage{AuthID: "a", Latency: time.Second})
	prior := e.TelemetrySnapshot()
	path := filepath.Join(t.TempDir(), "state.json")
	if errSave := e.Save(path, "shadow"); errSave != nil {
		t.Fatal(errSave)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var saved map[string]json.RawMessage
	if errDecode := json.Unmarshal(raw, &saved); errDecode != nil {
		t.Fatal(errDecode)
	}
	var fields map[string]json.RawMessage
	if errDecode := json.Unmarshal(saved["telemetry"], &fields); errDecode != nil {
		t.Fatal(errDecode)
	}
	for _, name := range []string{"schema_version", "epoch_id", "mode", "started_at", "config_fingerprint", "usage_records", "usage_successes", "usage_failures", "usage_429", "boundary_usage", "latency", "ttft", "decisions"} {
		if _, exists := fields[name]; !exists {
			t.Fatalf("saved telemetry lacks report field %s", name)
		}
	}
	clock.advance(time.Second)
	restored := NewEngine(e.cfg, clock.now)
	if errLoad := restored.Load(path); errLoad != nil {
		t.Fatal(errLoad)
	}
	restored.StartEpoch("shadow", false)
	current := restored.TelemetrySnapshot()
	if current.EpochID == prior.EpochID || current.Decisions != 0 || restored.Shadow().Picks != 0 || len(restored.reservations) != 0 || len(restored.TelemetryHistory()) != 1 || restored.TelemetryHistory()[0].Latency.Samples != 1 {
		t.Fatalf("restart telemetry = %+v, history = %+v", current, restored.TelemetryHistory())
	}
	if errWrite := os.WriteFile(path, []byte(`{"version":1,"shadow":{"picks":99,"inadmissible_fallbacks":50}}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errLoad := restored.Load(path); errLoad != nil {
		t.Fatal(errLoad)
	}
	restored.StartEpoch("shadow", false)
	if restored.Shadow().Picks != 0 {
		t.Fatal("legacy mode-mixed counters contaminated the new epoch")
	}
}

func TestTelemetryActualSnapshotsAcceptedByReport(t *testing.T) {
	python, errFind := exec.LookPath("python3")
	if errFind != nil {
		t.Skip("python3 is required for offline report integration")
	}
	e, clock := newTestEngine(t, nil)
	e.StartEpoch("shadow", false)
	paths := []string{filepath.Join(t.TempDir(), "start.json"), filepath.Join(t.TempDir(), "end.json")}
	if errSave := e.Save(paths[0], "shadow"); errSave != nil {
		t.Fatal(errSave)
	}
	clock.advance(time.Minute)
	e.Pick("gpt-5", codexCandidates("a"), "a")
	e.RecordUsage(Usage{AuthID: "a", RequestedAt: clock.now(), Latency: time.Second, TTFT: time.Millisecond})
	if errSave := e.Save(paths[1], "shadow"); errSave != nil {
		t.Fatal(errSave)
	}
	cmd := exec.Command(python, "-c", `import json, sys
sys.path.insert(0, "../tools")
from rollout_report import interval
data = interval(json.load(open(sys.argv[1])), json.load(open(sys.argv[2])))
assert data["counts"]["usage_successes"] == 1
assert data["latency"]["mean_ms"] == 1000
assert data["ttft"]["samples"] == 1
print("accepted")`, paths[0], paths[1])
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, errRun := cmd.CombinedOutput()
	if errRun != nil || !strings.Contains(string(output), "accepted") {
		t.Fatalf("report rejected actual Save snapshots: %v\n%s", errRun, output)
	}
}
