package autoroute

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
			})
			if got != tt.want {
				t.Fatalf("Decide() = %q, want %q", got, tt.want)
			}
		})
	}

	if got := Decide(cfg, Classification{}, false, available); got != "balanced-live" {
		t.Fatalf("Decide(invalid classification) = %q, want default-tier model", got)
	}
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
