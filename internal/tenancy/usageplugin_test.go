package tenancy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/openrouter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestCalculateWeightedTokensUsesV2PriceCategories(t *testing.T) {
	t.Parallel()

	record := usage.Record{
		Provider: "codex",
		Detail: usage.Detail{TokenBreakdown: usage.TokenBreakdown{
			SchemaVersion: usage.TokenAccountingSchemaVersion,
			Quality:       usage.TokenAccountingQualityComplete,
			TotalTokens:   25,
			Input: usage.TokenInputBreakdown{
				TotalTokens:      16,
				UncachedTokens:   10,
				CacheReadTokens:  4,
				CacheWriteTokens: 2,
			},
			Output: usage.TokenOutputBreakdown{
				TotalTokens:        9,
				NonReasoningTokens: 6,
				ReasoningTokens:    3,
			},
		}},
	}
	pricing := openrouter.ModelPricing{
		PromptNanoUSDPerToken:            openrouter.KnownNanoUSD{NanoUSD: 100, Known: true},
		CompletionNanoUSDPerToken:        openrouter.KnownNanoUSD{NanoUSD: 200, Known: true},
		InternalReasoningNanoUSDPerToken: openrouter.KnownNanoUSD{NanoUSD: 300, Known: true},
		InputCacheReadNanoUSDPerToken:    openrouter.KnownNanoUSD{NanoUSD: 75, Known: true},
		InputCacheWriteNanoUSDPerToken:   openrouter.KnownNanoUSD{NanoUSD: 50, Known: true},
	}

	// 10*100 + 4*75 + 2*50 + 6*200 + 3*300 = 3500 nano-USD.
	if got := CalculateWeightedTokens(record, pricing); got != 3500 {
		t.Fatalf("CalculateWeightedTokens() = %d, want 3500 nano-USD", got)
	}
}

func TestCalculateWeightedTokensCheapestPriceDoesNotRoundToZero(t *testing.T) {
	t.Parallel()

	record := usage.Record{
		Detail: usage.Detail{TokenBreakdown: usage.TokenBreakdown{
			SchemaVersion: usage.TokenAccountingSchemaVersion,
			Quality:       usage.TokenAccountingQualityComplete,
			TotalTokens:   1,
			Input: usage.TokenInputBreakdown{
				TotalTokens:     1,
				CacheReadTokens: 1,
			},
		}},
	}
	pricing := openrouter.ModelPricing{
		InputCacheReadNanoUSDPerToken: openrouter.KnownNanoUSD{NanoUSD: 75, Known: true},
	}
	if got := CalculateWeightedTokens(record, pricing); got != 75 {
		t.Fatalf("one cache-read token cost = %d nano-USD, want 75", got)
	}
}

func TestCalculateWeightedTokensUnclassifiedUsesPromptAndReasoningFallsBack(t *testing.T) {
	t.Parallel()

	record := usage.Record{
		Detail: usage.Detail{TokenBreakdown: usage.TokenBreakdown{
			SchemaVersion: usage.TokenAccountingSchemaVersion,
			Quality:       usage.TokenAccountingQualityInconsistent,
			TotalTokens:   10,
			Output: usage.TokenOutputBreakdown{
				TotalTokens:     3,
				ReasoningTokens: 3,
			},
			UnclassifiedTokens: 7,
		}},
	}
	pricing := openrouter.ModelPricing{
		PromptNanoUSDPerToken:     openrouter.KnownNanoUSD{NanoUSD: 100, Known: true},
		CompletionNanoUSDPerToken: openrouter.KnownNanoUSD{NanoUSD: 200, Known: true},
	}
	if got := CalculateWeightedTokens(record, pricing); got != 1300 {
		t.Fatalf("unclassified/reasoning fallback cost = %d, want 1300 nano-USD", got)
	}
}

func TestModelPricingForStaticUsesExactOverrideThenDefaultFloor(t *testing.T) {
	t.Parallel()

	cfg := config.TenancyConfig{
		Pricing: config.OpenRouterConfig{CostBasis: "static"},
		Quota: config.TenancyQuota{ModelPriceOverrides: map[string]config.ModelPriceOverride{
			"*": {
				Prompt:     modelPrice(10),
				Completion: modelPrice(20),
				CacheRead:  modelPrice(5),
			},
			"model-a": {
				Prompt: modelPrice(100),
			},
		}},
	}
	pricing := ModelPricingFor("MODEL-A", cfg)
	if pricing.PromptNanoUSDPerToken.NanoUSD != 100 ||
		pricing.CompletionNanoUSDPerToken.NanoUSD != 20 ||
		pricing.InputCacheReadNanoUSDPerToken.NanoUSD != 5 {
		t.Fatalf("effective static pricing = %+v", pricing)
	}
}

func TestOpenRouterPricingWinsAndOverridesFillGaps(t *testing.T) {
	t.Parallel()

	effective := openrouter.ModelPricing{
		PromptNanoUSDPerToken:     openrouter.KnownNanoUSD{NanoUSD: 100, Known: true},
		CompletionNanoUSDPerToken: openrouter.KnownNanoUSD{NanoUSD: 200, Known: true},
	}
	overlayKnownCatalogPricing(&effective, openrouter.ModelPricing{
		OpenRouterID:              "vendor/model-a",
		PromptNanoUSDPerToken:     openrouter.KnownNanoUSD{NanoUSD: 2500, Known: true},
		CompletionNanoUSDPerToken: openrouter.KnownNanoUSD{},
	})

	if effective.PromptNanoUSDPerToken.NanoUSD != 2500 {
		t.Fatalf("OpenRouter prompt did not win: %+v", effective.PromptNanoUSDPerToken)
	}
	if effective.CompletionNanoUSDPerToken.NanoUSD != 200 {
		t.Fatalf("override did not fill missing completion: %+v", effective.CompletionNanoUSDPerToken)
	}
}

func TestUsagePluginLedgerAndConfiguredWindowFallback(t *testing.T) {
	store := newTestStore(t)
	user := &User{
		ID:    "user-1",
		Email: "person@example.com",
		Role:  RoleUser,
		Tier:  "default",
	}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-20 * time.Minute)
	if errAppend := store.AppendUsage(context.Background(), []UsageEntry{{
		UserID:      user.ID,
		AuthID:      "auth-1",
		Provider:    "codex",
		Model:       "gpt-old",
		CostNanoUSD: 2,
		OccurredAt:  earlier,
	}}); errAppend != nil {
		t.Fatalf("AppendUsage() seed error = %v", errAppend)
	}

	plugin := NewUsagePlugin(
		store,
		config.TenancyConfig{
			Pricing: config.OpenRouterConfig{CostBasis: "static"},
			Quota: config.TenancyQuota{
				ModelPriceOverrides: map[string]config.ModelPriceOverride{
					"*": {
						Prompt:     modelPrice(3),
						Completion: modelPrice(6),
						Reasoning:  modelPrice(9),
						CacheRead:  modelPrice(1),
						CacheWrite: modelPrice(2),
					},
				},
				ProviderWindows: map[string]string{"codex": "1h"},
			},
		},
		func(context.Context, usage.Record) (*User, error) {
			return user, nil
		},
	)
	plugin.now = func() time.Time { return now }
	t.Cleanup(func() {
		if errClose := plugin.Close(); errClose != nil {
			t.Errorf("UsagePlugin.Close() error = %v", errClose)
		}
	})

	plugin.HandleUsage(context.Background(), usage.Record{
		Provider:    "codex",
		Model:       "gpt-5",
		AuthID:      "auth-1",
		RequestedAt: now,
		Detail: usage.Detail{
			TokenBreakdown: usage.TokenBreakdown{
				SchemaVersion: usage.TokenAccountingSchemaVersion,
				Quality:       usage.TokenAccountingQualityComplete,
				TotalTokens:   15,
				Input: usage.TokenInputBreakdown{
					TotalTokens:    10,
					UncachedTokens: 10,
				},
				Output: usage.TokenOutputBreakdown{
					TotalTokens:        5,
					NonReasoningTokens: 3,
					ReasoningTokens:    2,
				},
			},
			InputTokens:  10,
			OutputTokens: 5,
		},
		ResponseHeaders: http.Header{
			"x-ratelimit-remaining-tokens": []string{"20"},
			"x-ratelimit-limit-tokens":     []string{"100"},
		},
	})
	if errFlush := plugin.Flush(context.Background()); errFlush != nil {
		t.Fatalf("UsagePlugin.Flush() error = %v", errFlush)
	}

	used, errUsed := store.UsedUnits(context.Background(), user.ID, now.Add(-time.Hour))
	if errUsed != nil {
		t.Fatalf("UsedUnits() error = %v", errUsed)
	}
	// Seed 2 + (10*3 + 3*6 + 2*9) = 68 nano-USD.
	if used != 68 {
		t.Fatalf("UsedUnits() = %d, want 68 nano-USD", used)
	}

	windows, errWindows := store.ListQuotaWindows(context.Background())
	if errWindows != nil {
		t.Fatalf("ListQuotaWindows() error = %v", errWindows)
	}
	if len(windows) != 1 {
		t.Fatalf("ListQuotaWindows() length = %d, want 1", len(windows))
	}
	window := windows[0]
	if !window.WindowStart.Equal(earlier) {
		t.Fatalf("window start = %v, want %v", window.WindowStart, earlier)
	}
	if !window.WindowEnd.Equal(earlier.Add(time.Hour)) {
		t.Fatalf("window end = %v, want %v", window.WindowEnd, earlier.Add(time.Hour))
	}
	if window.UsedUnits != 80 || window.LimitUnits != 100 {
		t.Fatalf("window usage = %d/%d, want 80/100", window.UsedUnits, window.LimitUnits)
	}
	if window.Source != "openai-tokens:provider-window" {
		t.Fatalf("window source = %q, want configured fallback source", window.Source)
	}
}
