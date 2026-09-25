package config

// Fork-owned OpenRouter catalog configuration. Kept out of config_types.go so
// upstream merges do not conflict with fork declarations.

// OpenRouterConfig configures OpenRouter pricing and benchmark catalog refreshes.
// It is disabled by default so existing quota and routing behavior is unchanged.
type OpenRouterConfig struct {
	// Enabled allows the OpenRouter catalog package to load configured data. The
	// caller may still disable remote refreshes for --local-model operation.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// APIKey authorizes the benchmark endpoint. Pricing remains public. When empty,
	// OPENROUTER_API_KEY is used during config normalization.
	APIKey string `yaml:"api-key,omitempty" json:"api-key,omitempty"`

	// BaseURL is the OpenRouter API base URL.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// RefreshInterval controls background catalog refresh frequency.
	RefreshInterval string `yaml:"refresh-interval,omitempty" json:"refresh-interval,omitempty"`

	// ModelMap maps local model names to authoritative OpenRouter model IDs.
	ModelMap map[string]string `yaml:"model-map,omitempty" json:"model-map,omitempty"`

	// CostBasis selects static overrides only or OpenRouter prices with override
	// fallback.
	CostBasis string `yaml:"cost-basis,omitempty" json:"cost-basis,omitempty"`

	// BenchmarkSource selects the benchmark catalog used by automatic routing.
	// Empty disables benchmark-based ranking.
	BenchmarkSource string `yaml:"benchmark-source,omitempty" json:"benchmark-source,omitempty"`
}
