package tenancy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// usdPerMillion builds the config price type used by model-price-overrides.
func usdPerMillion(value config.USDPerMillionTokens) *config.USDPerMillionTokens {
	return &value
}

// TestModelPricingForCatalogOverridesWildcardFallback pins the precedence that
// makes the "*" override a fallback for unpriced models rather than a floor:
// once the catalog knows a price it wins, even against an explicit override.
func TestModelPricingForCatalogOverridesWildcardFallback(t *testing.T) {
	const model = "gpt-4o"
	cfg := config.TenancyConfig{}
	cfg.Pricing.Enabled = true
	cfg.Pricing.CostBasis = "openrouter"
	cfg.Pricing.ModelMap = map[string]string{model: "openai/gpt-4o"}
	cfg.Quota.ModelPriceOverrides = map[string]config.ModelPriceOverride{
		"*": {Prompt: usdPerMillion(999)},
	}

	pricing := ModelPricingFor(model, cfg)
	if !pricing.PromptNanoUSDPerToken.Known {
		t.Fatal("prompt price is unknown; the catalog should have priced this model")
	}
	if pricing.PromptNanoUSDPerToken.NanoUSD == config.USDPerMillionTokens(999).NanoUSDPerToken() {
		t.Fatal("wildcard override won over a known catalog price; precedence regressed")
	}
	if pricing.PromptNanoUSDPerToken.NanoUSD != 2500 {
		t.Fatalf("prompt price = %d nano-USD, want the catalog 2500", pricing.PromptNanoUSDPerToken.NanoUSD)
	}
}

// TestModelPricingForWildcardCoversUnmappedModel shows the wildcard still keeps
// an unmatched local model from being silently free.
func TestModelPricingForWildcardCoversUnmappedModel(t *testing.T) {
	cfg := config.TenancyConfig{}
	cfg.Pricing.Enabled = true
	cfg.Pricing.CostBasis = "openrouter"
	cfg.Quota.ModelPriceOverrides = map[string]config.ModelPriceOverride{
		"*": {Prompt: usdPerMillion(1), Completion: usdPerMillion(2)},
	}

	pricing := ModelPricingFor("local-model-with-no-catalog-entry", cfg)
	if !pricing.PromptNanoUSDPerToken.Known || pricing.PromptNanoUSDPerToken.NanoUSD == 0 {
		t.Fatalf("unmapped model prompt price = %+v, want the non-zero wildcard", pricing.PromptNanoUSDPerToken)
	}
	if !pricing.CompletionNanoUSDPerToken.Known || pricing.CompletionNanoUSDPerToken.NanoUSD == 0 {
		t.Fatalf("unmapped model completion price = %+v, want the non-zero wildcard", pricing.CompletionNanoUSDPerToken)
	}
}

// TestModelPricingForExactOverrideBeatsWildcard keeps the documented
// exact-name-then-wildcard order for the static path.
func TestModelPricingForExactOverrideBeatsWildcard(t *testing.T) {
	cfg := config.TenancyConfig{}
	cfg.Pricing.CostBasis = "static"
	cfg.Quota.ModelPriceOverrides = map[string]config.ModelPriceOverride{
		"*":        {Prompt: usdPerMillion(1)},
		"my-model": {Prompt: usdPerMillion(7)},
	}

	pricing := ModelPricingFor("my-model", cfg)
	if pricing.PromptNanoUSDPerToken.NanoUSD != config.USDPerMillionTokens(7).NanoUSDPerToken() {
		t.Fatalf("prompt price = %d, want the exact override", pricing.PromptNanoUSDPerToken.NanoUSD)
	}
}
