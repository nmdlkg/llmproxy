package tenancy

import (
	"context"
	"net/http"
	"strconv"
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

func TestCaptureQuotaWindowPrefersCodexHeaderWindow(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(6 * 24 * time.Hour)
	plugin := NewUsagePlugin(
		store,
		config.TenancyConfig{Quota: config.TenancyQuota{
			ProviderWindows: map[string]string{"codex": "1h"},
		}},
		nil,
	)
	plugin.now = func() time.Time { return now }
	t.Cleanup(func() {
		if errClose := plugin.Close(); errClose != nil {
			t.Errorf("UsagePlugin.Close() error = %v", errClose)
		}
	})

	plugin.captureQuotaWindow(usage.Record{
		Provider: "codex",
		AuthID:   "auth-1",
		ResponseHeaders: testHeaders(map[string]string{
			"X-Codex-Primary-Used-Percent":        "29",
			"X-Codex-Primary-Reset-At":            strconv.FormatInt(resetAt.Unix(), 10),
			"X-Codex-Primary-Reset-After-Seconds": "1",
			"X-Codex-Primary-Window-Minutes":      "10080",
		}),
	}, now)

	windows, errWindows := store.ListQuotaWindows(context.Background())
	if errWindows != nil {
		t.Fatalf("ListQuotaWindows() error = %v", errWindows)
	}
	if len(windows) != 1 {
		t.Fatalf("ListQuotaWindows() length = %d, want 1", len(windows))
	}
	window := windows[0]
	wantStart := resetAt.Add(-7 * 24 * time.Hour)
	if !window.WindowStart.Equal(wantStart) {
		t.Fatalf("window start = %v, want header-derived %v", window.WindowStart, wantStart)
	}
	if !window.WindowEnd.Equal(resetAt) {
		t.Fatalf("window end = %v, want %v", window.WindowEnd, resetAt)
	}
	if window.UsedUnits != 2900 || window.LimitUnits != 10000 {
		t.Fatalf("window usage = %d/%d, want 2900/10000", window.UsedUnits, window.LimitUnits)
	}
	if window.Source != "codex-primary" {
		t.Fatalf("window source = %q, want codex-primary", window.Source)
	}
}

func TestUsagePluginRecordsCredentialValidationOutcomes(t *testing.T) {
	store := newTestStore(t)
	user := &User{ID: "user-1", Email: "person@example.com", Role: RoleUser, Tier: "default"}
	validator := newCredentialValidator(store, func(string) ([]Credential, error) {
		return []Credential{{AuthID: "auth-1", Provider: "codex"}}, nil
	})
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	validator.now = func() time.Time { return now }
	if _, errPreferred := validator.preferredAuthIDs(user.ID, time.Hour); errPreferred != nil {
		t.Fatalf("preferredAuthIDs() error = %v", errPreferred)
	}

	plugin := NewUsagePlugin(
		store,
		config.TenancyConfig{},
		func(context.Context, usage.Record) (*User, error) {
			return user, nil
		},
	)
	plugin.validator = validator
	t.Cleanup(func() {
		if errClose := plugin.Close(); errClose != nil {
			t.Errorf("UsagePlugin.Close() error = %v", errClose)
		}
	})

	plugin.HandleUsage(context.Background(), usage.Record{
		AuthID:      "auth-1",
		Provider:    "codex",
		Failed:      true,
		Fail:        usage.Failure{StatusCode: http.StatusUnauthorized},
		RequestedAt: now,
	})
	failed, errGet := store.GetCredentialValidation(context.Background(), "auth-1")
	if errGet != nil {
		t.Fatalf("GetCredentialValidation(failure) error = %v", errGet)
	}
	if failed.LastOKAt != nil {
		t.Fatalf("failed outcome LastOKAt = %v, want nil", failed.LastOKAt)
	}
	if failed.LastStatus != "401" {
		t.Fatalf("failed outcome LastStatus = %q, want %q", failed.LastStatus, "401")
	}

	successAt := now.Add(time.Minute)
	plugin.HandleUsage(context.Background(), usage.Record{
		AuthID:      "auth-1",
		Provider:    "codex",
		RequestedAt: successAt,
	})
	succeeded, errGet := store.GetCredentialValidation(context.Background(), "auth-1")
	if errGet != nil {
		t.Fatalf("GetCredentialValidation(success) error = %v", errGet)
	}
	if succeeded.LastOKAt == nil || !succeeded.LastOKAt.Equal(successAt) {
		t.Fatalf("successful outcome LastOKAt = %v, want %v", succeeded.LastOKAt, successAt)
	}
	if succeeded.LastStatus != "ok" {
		t.Fatalf("successful outcome LastStatus = %q, want %q", succeeded.LastStatus, "ok")
	}

	laterFailureAt := successAt.Add(time.Minute)
	plugin.HandleUsage(context.Background(), usage.Record{
		AuthID:      "auth-1",
		Provider:    "codex",
		Failed:      true,
		Fail:        usage.Failure{StatusCode: http.StatusForbidden},
		RequestedAt: laterFailureAt,
	})
	laterFailure, errGet := store.GetCredentialValidation(context.Background(), "auth-1")
	if errGet != nil {
		t.Fatalf("GetCredentialValidation(later failure) error = %v", errGet)
	}
	if laterFailure.LastOKAt == nil || !laterFailure.LastOKAt.Equal(successAt) {
		t.Fatalf("later failed outcome LastOKAt = %v, want preserved %v", laterFailure.LastOKAt, successAt)
	}
	if laterFailure.LastStatus != "403" {
		t.Fatalf("later failed outcome LastStatus = %q, want %q", laterFailure.LastStatus, "403")
	}
}

// TestModelPricingForRespectsOpenRouterMasterSwitch pins the master-switch
// contract: openrouter.enabled=false is documented as leaving quota behaviour
// unchanged, so cost-basis: openrouter must degrade to overrides-only instead
// of silently pricing from the embedded snapshot.
func TestModelPricingForRespectsOpenRouterMasterSwitch(t *testing.T) {
	// A model present in the embedded snapshot, so the catalog lookup can succeed.
	const model = "gpt-4o"
	modelMap := map[string]string{model: "openai/gpt-4o"}

	disabled := config.TenancyConfig{}
	disabled.Pricing.Enabled = false
	disabled.Pricing.CostBasis = "openrouter"
	disabled.Pricing.ModelMap = modelMap

	enabled := disabled
	enabled.Pricing.Enabled = true

	gotDisabled := ModelPricingFor(model, disabled)
	if gotDisabled.PromptNanoUSDPerToken.Known {
		t.Fatalf("disabled catalog produced a known prompt price (%d nano-USD); enabled must gate the catalog",
			gotDisabled.PromptNanoUSDPerToken.NanoUSD)
	}

	gotEnabled := ModelPricingFor(model, enabled)
	if !gotEnabled.PromptNanoUSDPerToken.Known {
		t.Fatal("enabled catalog did not price a model present in the embedded snapshot")
	}
	// $2.50 / 1M tokens == 2500 nano-USD per token, exactly.
	if gotEnabled.PromptNanoUSDPerToken.NanoUSD != 2500 {
		t.Fatalf("prompt price = %d nano-USD, want 2500", gotEnabled.PromptNanoUSDPerToken.NanoUSD)
	}
}
