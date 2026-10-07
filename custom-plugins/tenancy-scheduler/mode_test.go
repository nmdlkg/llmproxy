package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestParsePluginConfig(t *testing.T) {
	cfg, errParse := parsePluginConfig([]byte("enabled: true\npriority: 100\n"))
	if errParse != nil || cfg.Mode != modeLegacy || cfg.AcrossPriorities || cfg.Optimizer.Epsilon != 0.05 {
		t.Fatalf("defaults = %+v, err = %v", cfg, errParse)
	}
	cfg, errParse = parsePluginConfig([]byte(`
mode: Optimizer
state-path: /tmp/state.json
across-priorities: true
optimizer:
  epsilon: 0.01
  step: 15m
  lambda-failure: 0
  recompute-interval: 2m
`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if cfg.Mode != modeOptimizer || !cfg.AcrossPriorities || cfg.Optimizer.Epsilon != 0.01 ||
		cfg.Optimizer.Step != 15*time.Minute || cfg.Optimizer.LambdaFailure != 0 || cfg.RecomputeEvery != 2*time.Minute {
		t.Fatalf("parsed = %+v", cfg)
	}
	for _, bad := range []string{"mode: greedy", "optimizer:\n  step: -1m", "optimizer:\n  epsilon: 0.7", "optimizer:\n  lambda-latency: -1"} {
		if _, errBad := parsePluginConfig([]byte(bad)); errBad == nil {
			t.Fatalf("config %q must be rejected", bad)
		}
	}
}

func TestParsePluginConfigRejectsNonFiniteOptimizerValues(t *testing.T) {
	for _, value := range []string{".nan", ".inf", "-.inf"} {
		if _, errParse := parsePluginConfig([]byte("optimizer:\n  epsilon: " + value + "\n")); errParse == nil {
			t.Fatalf("epsilon %q must be rejected", value)
		}
	}
}

func registerWith(t *testing.T, yaml string) (map[string]bool, error) {
	t.Helper()
	request, _ := json.Marshal(map[string]any{"config_yaml": base64.StdEncoding.EncodeToString([]byte(yaml)), "schema_version": pluginabi.SchemaVersion})
	raw, errCall := handleMethod(pluginabi.MethodPluginRegister, request)
	if errCall != nil {
		return nil, errCall
	}
	var env envelope
	if errDecode := json.Unmarshal(raw, &env); errDecode != nil {
		t.Fatal(errDecode)
	}
	var registration struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if errDecode := json.Unmarshal(env.Result, &registration); errDecode != nil {
		t.Fatal(errDecode)
	}
	return registration.Capabilities, nil
}

func TestRegistrationCapabilitiesFollowMode(t *testing.T) {
	t.Cleanup(func() { sharedScheduler.configure(defaultPluginConfig()) })
	capabilities, errRegister := registerWith(t, "enabled: true\n")
	if errRegister != nil || !capabilities["scheduler"] || capabilities["usage_plugin"] || capabilities["scheduler_across_priorities"] {
		t.Fatalf("legacy capabilities = %v, err = %v", capabilities, errRegister)
	}
	capabilities, errRegister = registerWith(t, "mode: shadow\nacross-priorities: true\n")
	if errRegister != nil || !capabilities["usage_plugin"] || !capabilities["scheduler_across_priorities"] {
		t.Fatalf("shadow capabilities = %v, err = %v", capabilities, errRegister)
	}
	if _, errRegister = registerWith(t, "mode: nope\n"); errRegister == nil {
		t.Fatal("invalid configuration must fail registration so the host keeps its fallback")
	}
}

func candidate(id string, priority int, metadata map[string]any) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Priority: priority, Metadata: metadata}
}

func TestLegacyKeepsTierSemanticsWhenCandidatesSpanPriorities(t *testing.T) {
	var s scheduler
	req := pluginapi.SchedulerPickRequest{Model: "gpt-5", Candidates: []pluginapi.SchedulerAuthCandidate{
		candidate("top-private", 10, map[string]any{"owner_user_id": "u", "shared": false}),
		candidate("low-shared", 0, nil),
	}}
	if got := s.pick(req); got.Handled {
		t.Fatalf("legacy must not promote a lower tier: %+v", got)
	}
}

func TestLegacyAppliesWeightsWithinDefaultPriorityTier(t *testing.T) {
	var s scheduler
	req := pluginapi.SchedulerPickRequest{Model: "gpt-5", Candidates: []pluginapi.SchedulerAuthCandidate{
		candidate("a", 0, nil),
		candidate("b", 0, nil),
	}}
	req.Candidates[0].Attributes = map[string]string{"weight": "3"}
	req.Candidates[1].Attributes = map[string]string{"weight": "1"}
	want := []string{"a", "a", "a", "b"}
	for i, expected := range want {
		if got := s.pick(req); !got.Handled || got.AuthID != expected {
			t.Fatalf("pick %d = %+v, want %s", i, got, expected)
		}
	}
}

func newModeScheduler(t *testing.T, mode string, across bool) *scheduler {
	t.Helper()
	cfg := defaultPluginConfig()
	cfg.Mode = mode
	cfg.AcrossPriorities = across
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	s := &scheduler{}
	s.configure(cfg)
	t.Cleanup(s.shutdown)
	return s
}

func codexUsage(authID string, shortUsed, weeklyUsed int) pluginapi.UsageRecord {
	now := time.Now()
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", strconv.Itoa(shortUsed))
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-primary-reset-after-seconds", "7200")
	headers.Set("x-codex-secondary-used-percent", strconv.Itoa(weeklyUsed))
	headers.Set("x-codex-secondary-window-minutes", "10080")
	headers.Set("x-codex-secondary-reset-after-seconds", "300000")
	return pluginapi.UsageRecord{AuthID: authID, Provider: "codex", Model: "gpt-5", Generate: true, RequestedAt: now, APIKey: "client-secret",
		Detail: pluginapi.UsageDetail{TotalTokens: 1000}, ResponseHeaders: headers}
}

func TestOptimizerModeAvoidsExhaustedAccountAndAcrossPriorities(t *testing.T) {
	s := newModeScheduler(t, modeOptimizer, true)
	req := pluginapi.SchedulerPickRequest{Model: "gpt-5", Candidates: []pluginapi.SchedulerAuthCandidate{
		candidate("top", 10, map[string]any{"account_identity": "acct-top"}),
		candidate("low", 0, nil),
	}}
	s.pick(req)
	s.handleUsage(codexUsage("top", 10, 100))
	s.handleUsage(codexUsage("low", 10, 10))
	for i := 0; i < 3; i++ {
		if got := s.pick(req); !got.Handled || got.AuthID != "low" {
			t.Fatalf("pick = %+v, want low (top's weekly window is exhausted)", got)
		}
		s.handleUsage(codexUsage("low", 10, 10))
	}
}

func TestShadowModeExecutesLegacyAndCheckpointsWithoutSecrets(t *testing.T) {
	s := newModeScheduler(t, modeShadow, false)
	req := pluginapi.SchedulerPickRequest{Model: "gpt-5", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("a", 0, nil), candidate("b", 0, nil)}}
	var legacy scheduler
	for i := 0; i < 4; i++ {
		want := legacy.pick(req).AuthID
		if got := s.pick(req); got.AuthID != want {
			t.Fatalf("shadow pick = %s, want legacy %s", got.AuthID, want)
		}
		s.handleUsage(codexUsage(want, 20, 20))
	}
	if errCheckpoint := s.checkpoint(); errCheckpoint != nil {
		t.Fatal(errCheckpoint)
	}
	raw, errRead := os.ReadFile(s.cfg.StatePath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var state struct {
		Shadow struct {
			Picks uint64 `json:"picks"`
		} `json:"shadow"`
	}
	if errDecode := json.Unmarshal(raw, &state); errDecode != nil || state.Shadow.Picks != 4 {
		t.Fatalf("shadow picks = %d, err = %v", state.Shadow.Picks, errDecode)
	}
	for _, secret := range []string{"client-secret", "x-codex-primary"} {
		if strings.Contains(strings.ToLower(string(raw)), secret) {
			t.Fatalf("state file leaks %q", secret)
		}
	}
}

func TestUsageUsesAliasAndResponseStartObservationTime(t *testing.T) {
	s := newModeScheduler(t, modeOptimizer, false)
	requestedAt := time.Date(2026, 9, 27, 3, 4, 5, 0, time.UTC)
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "10")
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-primary-reset-after-seconds", "7200")
	s.handleUsage(pluginapi.UsageRecord{
		AuthID:          "alias-auth",
		Provider:        "codex",
		Model:           "upstream-model",
		Alias:           "route-alias",
		Generate:        true,
		RequestedAt:     requestedAt,
		TTFT:            3 * time.Second,
		Latency:         45 * time.Minute,
		Detail:          pluginapi.UsageDetail{TotalTokens: 100},
		ResponseHeaders: headers,
	})
	if errCheckpoint := s.checkpoint(); errCheckpoint != nil {
		t.Fatal(errCheckpoint)
	}
	raw, errRead := os.ReadFile(s.cfg.StatePath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var state struct {
		Classes map[string]json.RawMessage `json:"classes"`
		Windows []struct {
			ObservedAt time.Time `json:"observed_at"`
		} `json:"windows"`
	}
	if errDecode := json.Unmarshal(raw, &state); errDecode != nil {
		t.Fatal(errDecode)
	}
	if _, ok := state.Classes["codex/route-alias"]; !ok {
		t.Fatalf("classes = %v, want route alias", state.Classes)
	}
	if _, ok := state.Classes["codex/upstream-model"]; ok {
		t.Fatalf("classes = %v, upstream model must not replace route alias", state.Classes)
	}
	wantObservedAt := requestedAt.Add(3 * time.Second)
	if len(state.Windows) != 1 || !state.Windows[0].ObservedAt.Equal(wantObservedAt) {
		t.Fatalf("observed_at = %v, want %v", state.Windows, wantObservedAt)
	}
}

func TestUsageHandleJSONContract(t *testing.T) {
	s := newModeScheduler(t, modeOptimizer, false)
	previous := sharedScheduler.engine
	sharedScheduler.mu.Lock()
	sharedScheduler.engine = s.engine
	sharedScheduler.mu.Unlock()
	t.Cleanup(func() {
		sharedScheduler.mu.Lock()
		sharedScheduler.engine = previous
		sharedScheduler.mu.Unlock()
	})
	raw, _ := json.Marshal(codexUsage("a", 30, 40))
	response, errCall := handleMethod(pluginabi.MethodUsageHandle, raw)
	if errCall != nil {
		t.Fatal(errCall)
	}
	var env envelope
	if errDecode := json.Unmarshal(response, &env); errDecode != nil || !env.OK {
		t.Fatalf("usage.handle response = %s", response)
	}
	if prices := s.engine.Recompute(); prices == nil {
		t.Fatal("recompute returned nil")
	}
}

func TestReconfigurationScopesTelemetryEpochsAndPolicyFingerprint(t *testing.T) {
	s := newModeScheduler(t, modeShadow, false)
	req := pluginapi.SchedulerPickRequest{Model: "gpt-5", Candidates: []pluginapi.SchedulerAuthCandidate{candidate("a", 0, nil)}}
	s.pick(req)
	s.handleUsage(codexUsage("a", 10, 10))
	initial := s.engine.TelemetrySnapshot()
	engine := s.engine
	cfg := s.cfg
	cfg.Mode = modeOptimizer
	s.configure(cfg)
	current := s.engine.TelemetrySnapshot()
	if s.engine != engine || current.EpochID == initial.EpochID || current.ConfigFingerprint != initial.ConfigFingerprint || current.Decisions != 0 || s.engine.Shadow().Picks != 0 {
		t.Fatalf("mode reconfigure = %+v; prior = %+v", current, initial)
	}
	s.configure(cfg)
	if s.engine.TelemetrySnapshot().EpochID != current.EpochID {
		t.Fatal("identical registration must retain the current epoch")
	}
	cfg.AcrossPriorities = true
	s.configure(cfg)
	across := s.engine.TelemetrySnapshot()
	if across.EpochID == current.EpochID || across.ConfigFingerprint == current.ConfigFingerprint {
		t.Fatal("across-priorities change must rotate the epoch and policy fingerprint")
	}
	cfg.RecomputeEvery = 2 * time.Minute
	s.configure(cfg)
	cadence := s.engine.TelemetrySnapshot()
	if cadence.EpochID == across.EpochID || cadence.ConfigFingerprint == across.ConfigFingerprint {
		t.Fatal("recompute cadence change must rotate the epoch and policy fingerprint")
	}
	if cadence.KnownAccounts != 1 {
		t.Fatal("reconfiguration discarded learned quota state")
	}
	s.shutdown()
	s.configure(cfg)
	restarted := s.engine.TelemetrySnapshot()
	if restarted.EpochID == cadence.EpochID || restarted.ConfigFingerprint != cadence.ConfigFingerprint || restarted.Decisions != 0 {
		t.Fatal("restart must rotate the epoch while preserving the policy fingerprint")
	}
}
