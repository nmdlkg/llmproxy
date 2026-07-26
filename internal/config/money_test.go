package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTenancyMoneyConfigUsesExactNanoUSD(t *testing.T) {
	t.Parallel()

	cfg, errParse := ParseConfigBytes([]byte(`
openrouter:
  cost-basis: openrouter
tenancy:
  quota:
    base-usd:
      default: 5.0
      rejected: -1.0
    contribution-usd:
      codex:
        pro: 1.25
        rejected: -0.5
    model-price-overrides:
      model-a:
        prompt: 2.50
        completion: 10.0
        reasoning: 10.0
        cache-read: 0.075
        cache-write: 3.125
      rejected:
        prompt: -1.0
      free:
        prompt: 0
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}

	if got := cfg.Tenancy.Quota.BaseUSD["default"].NanoUSD(); got != 5_000_000_000 {
		t.Fatalf("base default = %d nano-USD, want 5000000000", got)
	}
	if _, ok := cfg.Tenancy.Quota.BaseUSD["rejected"]; ok {
		t.Fatal("negative base-usd entry was not rejected")
	}
	if got := cfg.Tenancy.Quota.ContributionUSD["codex"]["pro"].NanoUSD(); got != 1_250_000_000 {
		t.Fatalf("contribution = %d nano-USD, want 1250000000", got)
	}
	if _, ok := cfg.Tenancy.Quota.ContributionUSD["codex"]["rejected"]; ok {
		t.Fatal("negative contribution-usd entry was not rejected")
	}

	price := cfg.Tenancy.Quota.ModelPriceOverrides["model-a"]
	assertNanoUSDPerToken(t, "prompt", price.Prompt, 2500)
	assertNanoUSDPerToken(t, "completion", price.Completion, 10_000)
	assertNanoUSDPerToken(t, "reasoning", price.Reasoning, 10_000)
	assertNanoUSDPerToken(t, "cache-read", price.CacheRead, 75)
	assertNanoUSDPerToken(t, "cache-write", price.CacheWrite, 3125)
	if _, ok := cfg.Tenancy.Quota.ModelPriceOverrides["rejected"]; ok {
		t.Fatal("all-negative model override was not rejected")
	}
	free := cfg.Tenancy.Quota.ModelPriceOverrides["free"].Prompt
	assertNanoUSDPerToken(t, "explicit free prompt", free, 0)

	if cfg.Tenancy.Pricing.CostBasis != "openrouter" {
		t.Fatalf("runtime tenancy cost basis = %q, want openrouter", cfg.Tenancy.Pricing.CostBasis)
	}

	encoded, errMarshal := yaml.Marshal(cfg.Tenancy.Quota)
	if errMarshal != nil {
		t.Fatalf("yaml.Marshal() error = %v", errMarshal)
	}
	text := string(encoded)
	for _, expected := range []string{
		"default: 5.0",
		"prompt: 2.5",
		"cache-read: 0.075",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("marshaled YAML missing %q:\n%s", expected, text)
		}
	}
}

func assertNanoUSDPerToken(t *testing.T, name string, value *USDPerMillionTokens, want int64) {
	t.Helper()
	if value == nil {
		t.Fatalf("%s price is unknown, want %d nano-USD/token", name, want)
	}
	if got := value.NanoUSDPerToken(); got != want {
		t.Fatalf("%s price = %d nano-USD/token, want %d", name, got, want)
	}
}
