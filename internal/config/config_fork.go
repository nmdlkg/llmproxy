package config

import "fmt"

// Fork configuration hooks. The upstream loaders (LoadConfigOptional and
// ParseConfigBytes) call only these two functions so fork-owned sections can
// evolve without touching upstream files.

// applyForkConfigDefaults seeds fork defaults before YAML unmarshal so that
// absent keys keep their defaults.
func applyForkConfigDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.Tenancy.UserPanel.GitHubRepository = DefaultUserPanelGitHubRepository
}

// normalizeForkConfig validates and normalizes every fork-owned section after
// the upstream sanitization pipeline. Order matters: OpenRouter normalization
// copies its result into Tenancy.Pricing and audits model pricing, so it must
// run after tenancy normalization.
func normalizeForkConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	if errOTel := cfg.SanitizeOTelConfig(); errOTel != nil {
		return fmt.Errorf("invalid OpenTelemetry config: %w", errOTel)
	}
	cfg.SanitizeTenancyConfig()
	cfg.SanitizeAutoRoutingConfig()
	cfg.SanitizeOpenRouterConfig()
	return nil
}
