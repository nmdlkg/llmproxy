package config

// Fork-owned automatic model routing configuration. Kept out of
// config_types.go so upstream merges do not conflict with fork declarations.

// AutoRoutingConfig configures prompt-difficulty-based model selection for the "auto"
// model name, backed by a vLLM semantic-router sidecar classification API.
//
// When disabled, unreachable, or below the confidence threshold, "auto" resolves through
// the pre-existing registry path (newest available model) unchanged.
type AutoRoutingConfig struct {
	// Enabled turns on classification-driven auto model selection.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// RouterURL is the semantic-router apiserver base URL, e.g. http://127.0.0.1:8080.
	RouterURL string `yaml:"router-url,omitempty" json:"router-url,omitempty"`

	// TimeoutMS bounds the classification call so it can never stall a request.
	TimeoutMS int `yaml:"timeout-ms,omitempty" json:"timeout-ms,omitempty"`

	// MinConfidence is the minimum classifier confidence required to use its category.
	MinConfidence float64 `yaml:"min-confidence,omitempty" json:"min-confidence,omitempty"`

	// CacheTTLSeconds caches classification results keyed by prompt hash.
	CacheTTLSeconds int `yaml:"cache-ttl-seconds,omitempty" json:"cache-ttl-seconds,omitempty"`

	// MaxPromptChars truncates the classified prompt text.
	MaxPromptChars int `yaml:"max-prompt-chars,omitempty" json:"max-prompt-chars,omitempty"`

	// DefaultTier is used when classification is unavailable or low-confidence.
	DefaultTier string `yaml:"default-tier,omitempty" json:"default-tier,omitempty"`

	// FallbackModel is used for quota-exhausted users and as a last resort.
	FallbackModel string `yaml:"fallback-model,omitempty" json:"fallback-model,omitempty"`

	// QuotaFallback downgrades over-quota users to FallbackModel instead of returning 429.
	QuotaFallback bool `yaml:"quota-fallback,omitempty" json:"quota-fallback,omitempty"`

	// Tiers maps a tier label to its candidate models, in preference order.
	Tiers map[string]AutoRoutingTier `yaml:"tiers,omitempty" json:"tiers,omitempty"`

	// Categories maps a semantic-router category to a tier label.
	Categories map[string]string `yaml:"categories,omitempty" json:"categories,omitempty"`
}

// AutoRoutingTier lists the candidate models for one difficulty tier.
type AutoRoutingTier struct {
	// Models are candidate model names in preference order; the first with a live
	// registration wins.
	Models []string `yaml:"models" json:"models"`
}
