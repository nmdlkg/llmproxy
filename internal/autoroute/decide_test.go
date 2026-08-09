package autoroute

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/openrouter"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	cfg := config.AutoRoutingConfig{
		MinConfidence: 0.5,
		DefaultTier:   "balanced",
		FallbackModel: "fallback",
		Tiers: map[string]config.AutoRoutingTier{
			"cheap":    {Models: []string{"cheap-dead", "cheap-live"}},
			"balanced": {Models: []string{"balanced-live"}},
			"strong":   {Models: []string{"strong-dead", "strong-live"}},
		},
		Categories: map[string]string{
			"general": "cheap",
			"code":    "strong",
		},
	}
	live := map[string]bool{
		"cheap-live":    true,
		"balanced-live": true,
		"strong-live":   true,
		"fallback":      true,
	}
	available := func(model string) bool { return live[model] }

	tests := []struct {
		name           string
		classification Classification
		forcedFallback bool
		mutate         func(map[string]bool)
		want           string
	}{
		{
			name:           "category maps to tier and first live model",
			classification: Classification{Category: "code", Confidence: 0.9, Valid: true},
			want:           "strong-live",
		},
		{
			name:           "low confidence uses default tier",
			classification: Classification{Category: "code", Confidence: 0.49, Valid: true},
			want:           "balanced-live",
		},
		{
			name:           "unknown category uses default tier",
			classification: Classification{Category: "unknown", Confidence: 0.9, Valid: true},
			want:           "balanced-live",
		},
		{
			name:           "forced fallback bypasses classification",
			classification: Classification{Category: "code", Confidence: 0.9, Valid: true},
			forcedFallback: true,
			want:           "fallback",
		},
		{
			name:           "empty tier falls back",
			classification: Classification{Category: "general", Confidence: 0.9, Valid: true},
			mutate: func(models map[string]bool) {
				models["cheap-live"] = false
			},
			want: "fallback",
		},
		{
			name:           "unavailable fallback returns no decision",
			classification: Classification{Category: "general", Confidence: 0.9, Valid: true},
			mutate: func(models map[string]bool) {
				models["cheap-live"] = false
				models["fallback"] = false
			},
			want: "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			testLive := make(map[string]bool, len(live))
			for model, isLive := range live {
				testLive[model] = isLive
			}
			if tt.mutate != nil {
				tt.mutate(testLive)
			}
			got := Decide(cfg, tt.classification, tt.forcedFallback, func(model string) bool {
				return testLive[model]
			}, nil)
			if got != tt.want {
				t.Fatalf("Decide() = %q, want %q", got, tt.want)
			}
		})
	}

	if got := Decide(cfg, Classification{}, false, available, nil); got != "balanced-live" {
		t.Fatalf("Decide(invalid classification) = %q, want default-tier model", got)
	}
}

func TestDecideModelRanking(t *testing.T) {
	t.Parallel()

	cfg := config.AutoRoutingConfig{
		MinConfidence: 0.5,
		DefaultTier:   "balanced",
		FallbackModel: "fallback",
		Tiers: map[string]config.AutoRoutingTier{
			"balanced": {Models: []string{"configured-first", "configured-second"}},
			"strong":   {Models: []string{"strong-first", "strong-second"}},
		},
		Categories: map[string]string{"code": "strong"},
	}
	allLive := func(string) bool { return true }
	code := Classification{Category: "code", Confidence: 0.9, Valid: true}

	if got := Decide(cfg, code, false, allLive, nil); got != "strong-first" {
		t.Fatalf("Decide(nil ranker) = %q, want configured order strong-first", got)
	}
	if got := Decide(cfg, code, false, allLive, func(string, []string) []string {
		return nil
	}); got != "strong-first" {
		t.Fatalf("Decide(empty ranking) = %q, want configured order strong-first", got)
	}

	var rankedCandidates []string
	reorder := func(category string, candidates []string) []string {
		if category != "code" {
			t.Fatalf("ranker category = %q, want code", category)
		}
		rankedCandidates = append([]string(nil), candidates...)
		return []string{"strong-second", "strong-first"}
	}
	if got := Decide(cfg, code, false, allLive, reorder); got != "strong-second" {
		t.Fatalf("Decide(reordered) = %q, want strong-second", got)
	}
	if want := []string{"strong-first", "strong-second"}; !equalStrings(rankedCandidates, want) {
		t.Fatalf("ranked candidates = %v, want selected tier only %v", rankedCandidates, want)
	}

	secondUnavailable := func(model string) bool {
		return model == "strong-first" || model == "fallback"
	}
	if got := Decide(cfg, code, false, secondUnavailable, func(string, []string) []string {
		return []string{"strong-second", "strong-first"}
	}); got != "strong-first" {
		t.Fatalf("Decide(unavailable ranked model) = %q, want strong-first", got)
	}

	if got := Decide(cfg, code, false, allLive, func(string, []string) []string {
		return []string{"foreign-model"}
	}); got != "strong-first" {
		t.Fatalf("Decide(foreign ranked model) = %q, want configured tier model strong-first", got)
	}

	if got := Decide(cfg, code, false, secondUnavailable, func(string, []string) []string {
		return []string{"strong-second"}
	}); got != "strong-first" {
		t.Fatalf("Decide(only unavailable ranked models) = %q, want configured order strong-first", got)
	}
}

func TestDecideRankingDoesNotChangeFallbackPaths(t *testing.T) {
	t.Parallel()

	cfg := config.AutoRoutingConfig{
		DefaultTier:   "missing",
		FallbackModel: "fallback",
	}
	var rankerCalls atomic.Int32
	ranker := func(string, []string) []string {
		rankerCalls.Add(1)
		return []string{"foreign-model"}
	}
	available := func(string) bool { return true }

	if got := Decide(cfg, Classification{}, false, available, ranker); got != "fallback" {
		t.Fatalf("Decide(missing tier) = %q, want fallback", got)
	}
	if got := Decide(cfg, Classification{}, true, available, ranker); got != "fallback" {
		t.Fatalf("Decide(forced fallback) = %q, want fallback", got)
	}
	if got := rankerCalls.Load(); got != 0 {
		t.Fatalf("ranker calls on fallback paths = %d, want 0", got)
	}
}

func TestModelRankerConfigurationAndTaskMapping(t *testing.T) {
	t.Parallel()

	if got := modelRanker(&config.SDKConfig{}); got != nil {
		t.Fatal("modelRanker(empty benchmark source) is non-nil")
	}
	if got := modelRanker(&config.SDKConfig{OpenRouter: config.OpenRouterConfig{
		Enabled:         true,
		BenchmarkSource: "artificial-analysis",
	}}); got == nil {
		t.Fatal("modelRanker(configured benchmark source) is nil")
	}
	if got := openRouterModelRanker(config.OpenRouterConfig{Enabled: true, BenchmarkSource: "unsupported"}, openrouter.Snapshot{}); got != nil {
		t.Fatal("openRouterModelRanker(unsupported source) is non-nil")
	}
	// openrouter.enabled is the master switch: a configured benchmark source
	// must NOT reorder candidates while the catalog is disabled, because
	// disabled is documented as leaving routing behaviour unchanged.
	if got := openRouterModelRanker(config.OpenRouterConfig{
		Enabled:         false,
		BenchmarkSource: "artificial-analysis",
	}, openrouter.Snapshot{}); got != nil {
		t.Fatal("openRouterModelRanker(disabled catalog) is non-nil; enabled must gate ranking")
	}

	tests := []struct {
		category string
		want     openrouter.TaskType
	}{
		{category: "code", want: openrouter.TaskCoding},
		{category: "software-development", want: openrouter.TaskCoding},
		{category: "debugging", want: openrouter.TaskCoding},
		{category: "agentic", want: openrouter.TaskAgentic},
		{category: "tool-use", want: openrouter.TaskAgentic},
		{category: "math", want: openrouter.TaskIntelligence},
		{category: "", want: openrouter.TaskIntelligence},
	}
	for _, tt := range tests {
		if got := taskTypeForCategory(tt.category); got != tt.want {
			t.Errorf("taskTypeForCategory(%q) = %q, want %q", tt.category, got, tt.want)
		}
	}
}

func TestOpenRouterModelRankerPrefersScorePerDollarAndFallsBackToRaw(t *testing.T) {
	t.Parallel()

	snapshot := openrouter.Snapshot{
		Pricing: map[string]openrouter.ModelPricing{
			"vendor/high-quality": {
				PromptNanoUSDPerToken:     openrouter.KnownNanoUSD{NanoUSD: 10, Known: true},
				CompletionNanoUSDPerToken: openrouter.KnownNanoUSD{NanoUSD: 10, Known: true},
			},
			"vendor/high-value": {
				PromptNanoUSDPerToken:     openrouter.KnownNanoUSD{NanoUSD: 1, Known: true},
				CompletionNanoUSDPerToken: openrouter.KnownNanoUSD{NanoUSD: 1, Known: true},
			},
		},
		Quality: map[string]openrouter.ModelQuality{
			"vendor/high-quality": {
				CodingIndex: openrouter.KnownValue{Value: 100, Known: true},
			},
			"vendor/high-value": {
				CodingIndex: openrouter.KnownValue{Value: 90, Known: true},
			},
			"vendor/unpriced": {
				CodingIndex: openrouter.KnownValue{Value: 110, Known: true},
			},
		},
	}
	cfg := config.OpenRouterConfig{
		Enabled:         true,
		BenchmarkSource: "artificial-analysis",
		ModelMap: map[string]string{
			"high-quality": "vendor/high-quality",
			"high-value":   "vendor/high-value",
			"unpriced":     "vendor/unpriced",
		},
	}
	ranker := openRouterModelRanker(cfg, snapshot)
	got := ranker("code", []string{"high-quality", "unpriced", "high-value"})
	want := []string{"high-value", "high-quality", "unpriced"}
	if !equalStrings(got, want) {
		t.Fatalf("score-per-dollar order = %v, want %v", got, want)
	}

	snapshot.Pricing = nil
	ranker = openRouterModelRanker(cfg, snapshot)
	got = ranker("code", []string{"high-quality", "unpriced", "high-value"})
	want = []string{"unpriced", "high-quality", "high-value"}
	if !equalStrings(got, want) {
		t.Fatalf("raw-score fallback order = %v, want %v", got, want)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestResolverCacheHitAndExpiry(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	var calls atomic.Int32
	resolver := &Resolver{
		cache: newDecisionCache(func() time.Time { return now }),
		classify: func(context.Context, config.AutoRoutingConfig, *config.SDKConfig, string) Classification {
			calls.Add(1)
			return Classification{Category: "code", Confidence: 0.9, Valid: true}
		},
		modelAvailable: func(model string) bool { return model == "strong-live" },
	}
	cfg := config.AutoRoutingConfig{
		Enabled:         true,
		MinConfidence:   0.5,
		CacheTTLSeconds: 10,
		DefaultTier:     "balanced",
		Tiers: map[string]config.AutoRoutingTier{
			"strong": {Models: []string{"strong-live"}},
		},
		Categories: map[string]string{"code": "strong"},
	}

	if got := resolver.Resolve(context.Background(), cfg, nil, "same prompt"); got != "strong-live" {
		t.Fatalf("first Resolve() = %q, want strong-live", got)
	}
	if got := resolver.Resolve(context.Background(), cfg, nil, "same prompt"); got != "strong-live" {
		t.Fatalf("cached Resolve() = %q, want strong-live", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("classify calls after cache hit = %d, want 1", got)
	}

	now = now.Add(11 * time.Second)
	if got := resolver.Resolve(context.Background(), cfg, nil, "same prompt"); got != "strong-live" {
		t.Fatalf("expired Resolve() = %q, want strong-live", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("classify calls after expiry = %d, want 2", got)
	}
}

func TestResolverInvalidClassificationAndForcedFallback(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	resolver := &Resolver{
		cache: newDecisionCache(time.Now),
		classify: func(context.Context, config.AutoRoutingConfig, *config.SDKConfig, string) Classification {
			calls.Add(1)
			return Classification{}
		},
		modelAvailable: func(model string) bool { return model == "fallback" },
	}
	cfg := config.AutoRoutingConfig{
		Enabled:       true,
		FallbackModel: "fallback",
	}

	if got := resolver.Resolve(context.Background(), cfg, nil, "prompt"); got != "" {
		t.Fatalf("invalid classification Resolve() = %q, want no decision", got)
	}
	if got := resolver.Resolve(WithForcedFallback(context.Background()), cfg, nil, "prompt"); got != "fallback" {
		t.Fatalf("forced fallback Resolve() = %q, want fallback", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("classify calls = %d, want 1 because forced fallback bypasses sidecar", got)
	}
	if !ForcedFallback(WithForcedFallback(context.Background())) {
		t.Fatal("ForcedFallback() = false, want true")
	}
	if ForcedFallback(context.Background()) {
		t.Fatal("ForcedFallback(background) = true, want false")
	}
}

func TestGlobalModelAvailableUsesRegistryAvailability(t *testing.T) {
	modelID := "autoroute-live-model-test"
	clientID := "autoroute-live-client-test"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(clientID, "autoroute-test", []*registry.ModelInfo{{ID: modelID}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	if !globalModelAvailable(modelID) {
		t.Fatalf("globalModelAvailable(%q) = false, want true", modelID)
	}
	if globalModelAvailable("autoroute-missing-model-test") {
		t.Fatal("globalModelAvailable(missing) = true, want false")
	}
}
