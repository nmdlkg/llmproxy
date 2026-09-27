package optimizer

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func scopedCodexHeaders(used float64, reset time.Time) http.Header {
	headers := http.Header{}
	headers.Set("x-codex-bengalfox-primary-used-percent", formatFloat(used))
	headers.Set("x-codex-bengalfox-primary-window-minutes", "300")
	headers.Set("x-codex-bengalfox-primary-reset-at", formatInt(float64(reset.Unix())))
	return headers
}

func formatFloat(value float64) string {
	return strconvFormatFloat(value)
}

func TestScopedWindowOnlyBlocksAssociatedClass(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	candidate := Candidate{ID: "a", Provider: "codex", Account: "acct-a"}
	reset := clock.now().Add(4 * time.Hour)
	engine.Pick("gpt-5", []Candidate{candidate}, "")
	first := codexHeaders(0, reset, 0, clock.now().Add(7*24*time.Hour))
	for key, values := range scopedCodexHeaders(0, reset) {
		first[key] = values
	}
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", Model: "gpt-5", Headers: first, CompletedAt: clock.now()})
	if decision := engine.Pick("gpt-5", []Candidate{candidate}, ""); !decision.Admissible {
		t.Fatalf("unassociated scoped window blocked gpt-5: %+v", decision)
	}

	second := codexHeaders(0, reset, 0, clock.now().Add(7*24*time.Hour))
	for key, values := range scopedCodexHeaders(100, reset) {
		second[key] = values
	}
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", Model: "gpt-5", Headers: second, CompletedAt: clock.now()})
	if decision := engine.Pick("gpt-5", []Candidate{candidate}, ""); decision.Admissible {
		t.Fatalf("associated exhausted scoped window did not block gpt-5: %+v", decision)
	}
}

func TestResetWindowPurgesOldReservations(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	candidate := codexCandidates("a")[0]
	engine.Pick("gpt-5", []Candidate{candidate}, "")
	engine.RecordUsage(Usage{AuthID: "a", Provider: "codex", CompletedAt: clock.now(), Headers: codexHeaders(0, clock.now().Add(time.Hour), 0, clock.now().Add(7*24*time.Hour))})
	engine.Pick("gpt-5", []Candidate{candidate}, "")
	clock.advance(2 * time.Hour)
	decision := engine.Pick("gpt-5", []Candidate{candidate}, "")
	if !decision.Admissible {
		t.Fatalf("old-generation reservation still reduced refilled window: %+v", decision)
	}
	key := WindowKey{Account: "acct-a", Provider: "codex", Scope: "default", Kind: "5h"}.String()
	if got := len(engine.reservations["a"]); got != 1 {
		t.Fatalf("reset left %d reservation groups, want only the new reservation", got)
	}
	if got := engine.reservations["a"][0].items[0].generation; got != engine.windows[key].Generation {
		t.Fatalf("reservation generation = %d, want current generation %d", got, engine.windows[key].Generation)
	}
}

func TestCountedWindowsUseNativeUnits(t *testing.T) {
	engine, _ := newTestEngine(t, nil)
	engine.classes["claude/test"] = &ClassStats{MeanTokens: 123, SqTokens: 123 * 123, Count: 1}
	requests, requestSD := engine.consumptionLocked(&Window{Unit: UnitRequests}, 100, "claude/test")
	if requests != 1 || requestSD != 0 {
		t.Fatalf("request window consumption = (%v, %v), want (1, 0)", requests, requestSD)
	}
	tokens, _ := engine.consumptionLocked(&Window{Unit: UnitTokens}, 10000, "claude/test")
	if tokens != 123 {
		t.Fatalf("token window consumption = %v, want class mean 123", tokens)
	}
}

func TestClassOfCollapsesDatedSuffix(t *testing.T) {
	if got := ClassOf("openai", "gpt-5-20260927"); got != "openai/gpt-5" {
		t.Fatalf("dated class = %q", got)
	}
	if got := ClassOf("openai", "gpt-5-2026-09-27"); got != "openai/gpt-5" {
		t.Fatalf("ISO dated class = %q", got)
	}
}

func TestParseSecondsFromRejectsExcessiveReset(t *testing.T) {
	if _, ok := parseSecondsFrom("34560001", testEpoch); ok {
		t.Fatal("reset-after beyond 400 days was accepted")
	}
	if _, ok := parseSecondsFrom("34560000", testEpoch); !ok {
		t.Fatal("400-day reset-after was rejected")
	}
}

func TestLoadDropsNilStateEntriesAndMarksCorruption(t *testing.T) {
	engine, _ := newTestEngine(t, nil)
	path := filepath.Join(t.TempDir(), "state.json")
	raw := `{"version":1,"windows":[null],"pooled":{"p":null},"classes":{"c":null},"demand":{"d":null},"health":{"h":null},"accounts":{"x":null},"scoped_classes":{"w":null}}`
	if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errLoad := engine.Load(path); errLoad != nil {
		t.Fatal(errLoad)
	}
	if len(engine.windows) != 0 || len(engine.pooled) != 0 || len(engine.classes) != 0 || len(engine.demand) != 0 || len(engine.health) != 0 || len(engine.accounts) != 0 {
		t.Fatalf("nil state entries survived load: windows=%d pooled=%d classes=%d demand=%d health=%d accounts=%d", len(engine.windows), len(engine.pooled), len(engine.classes), len(engine.demand), len(engine.health), len(engine.accounts))
	}
	if errWrite := os.WriteFile(path, []byte(`{"version":1`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errLoad := engine.Load(path); !errors.Is(errLoad, ErrCorruptState) {
		t.Fatalf("decode error = %v, want ErrCorruptState", errLoad)
	}
	if errWrite := os.WriteFile(path, []byte(`{"version":2}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errLoad := engine.Load(path); !errors.Is(errLoad, ErrCorruptState) {
		t.Fatalf("version error = %v, want ErrCorruptState", errLoad)
	}
}

func TestSavedStateDoesNotContainAuthID(t *testing.T) {
	engine, clock := newTestEngine(t, nil)
	authID := "private-user@example.com"
	engine.Pick("gpt-5", []Candidate{{ID: authID, Provider: "codex"}}, "")
	path := filepath.Join(t.TempDir(), "state.json")
	if errSave := engine.Save(path, "shadow"); errSave != nil {
		t.Fatal(errSave)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(raw), authID) {
		t.Fatalf("saved state contains clear auth ID %q: %s", authID, raw)
	}
	_ = clock
}

func TestBuildSimInputBoundsStepForManyAccounts(t *testing.T) {
	engine, clock := newTestEngine(t, func(c *Config) { c.MaxWork = 30e6 })
	candidates := make([]Candidate, 200)
	for i := range candidates {
		candidates[i] = Candidate{ID: "a" + strconvItoa(i), Provider: "codex", Account: "acct-" + strconvItoa(i)}
	}
	engine.Pick("gpt-5", candidates, "")
	for _, candidate := range candidates {
		engine.RecordUsage(Usage{AuthID: candidate.ID, Provider: "codex", Model: "gpt-5", CompletedAt: clock.now(), Headers: codexHeaders(10, clock.now().Add(5*time.Hour), 10, clock.now().Add(7*24*time.Hour))})
	}
	input, _, accounts := engine.buildSimInput(clock.now())
	if accounts != 200 {
		t.Fatalf("account reduction occurred before the configured work bound: %d", accounts)
	}
	if input.Step > 150*time.Minute {
		t.Fatalf("step = %s, exceeded half of the shortest 5h window", input.Step)
	}
	if len(input.Demand)*1 > 20000 {
		t.Fatalf("demand cells exceeded cap: %d", len(input.Demand))
	}
}

func strconvItoa(value int) string {
	return formatInt(float64(value))
}

func strconvFormatFloat(value float64) string {
	return fmt.Sprintf("%g", value)
}
