package config

import (
	"strings"

	log "github.com/sirupsen/logrus"
)

// Tenancy defaults. Kept here so both the file loader and the byte parser
// converge on identical values.
const (
	defaultTenancyValidationInterval = "1h"
	// DefaultTenancyQuotaWindow is one week. Go's duration parser has no day
	// unit, so this must be expressed in hours. Exported so the tenancy package
	// uses the same value for its own fallback and the two cannot drift.
	DefaultTenancyQuotaWindow    = "168h"
	defaultTenancyUrgencyHorizon = "30m"
	defaultTenancyHighWater      = 0.9
	defaultTenancyUrgencyBonus   = 1
)

// SanitizeTenancyConfig trims tenancy values and applies defaults for unset fields.
// It never enables tenancy implicitly: Enabled must be set explicitly.
func (cfg *Config) SanitizeTenancyConfig() {
	if cfg == nil {
		return
	}
	t := &cfg.Tenancy
	t.UserPanel.GitHubRepository = strings.TrimSpace(t.UserPanel.GitHubRepository)
	if t.UserPanel.GitHubRepository == "" {
		t.UserPanel.GitHubRepository = DefaultUserPanelGitHubRepository
	}
	t.UserPanel.PinnedVersion = strings.TrimSpace(t.UserPanel.PinnedVersion)
	t.DBPath = strings.TrimSpace(t.DBPath)
	t.ValidationInterval = strings.TrimSpace(t.ValidationInterval)
	if t.ValidationInterval == "" {
		t.ValidationInterval = defaultTenancyValidationInterval
	}

	t.Quota.Window = strings.TrimSpace(t.Quota.Window)
	if t.Quota.Window == "" {
		t.Quota.Window = DefaultTenancyQuotaWindow
	}
	t.Quota.BaseUSD = normalizeUSDLimits(t.Quota.BaseUSD, "tenancy.quota.base-usd")
	t.Quota.ModelPriceOverrides = normalizeModelPriceOverrides(t.Quota.ModelPriceOverrides)
	t.Quota.ProviderWindows = normalizeLowerKeyedString(t.Quota.ProviderWindows)
	if len(t.Quota.ContributionUSD) > 0 {
		contributions := make(map[string]map[string]USDLimit, len(t.Quota.ContributionUSD))
		for rawProvider, plans := range t.Quota.ContributionUSD {
			provider := strings.ToLower(strings.TrimSpace(rawProvider))
			if provider == "" {
				continue
			}
			normalized := normalizeUSDLimits(
				plans,
				"tenancy.quota.contribution-usd."+provider,
			)
			if len(normalized) == 0 {
				continue
			}
			contributions[provider] = normalized
		}
		if len(contributions) == 0 {
			contributions = nil
		}
		t.Quota.ContributionUSD = contributions
	}

	t.Balancing.UrgencyHorizon = strings.TrimSpace(t.Balancing.UrgencyHorizon)
	if t.Balancing.UrgencyHorizon == "" {
		t.Balancing.UrgencyHorizon = defaultTenancyUrgencyHorizon
	}
	if t.Balancing.HighWater <= 0 || t.Balancing.HighWater > 1 {
		t.Balancing.HighWater = defaultTenancyHighWater
	}
	if t.Balancing.UrgencyBonus <= 0 {
		t.Balancing.UrgencyBonus = defaultTenancyUrgencyBonus
	}
}

func normalizeUSDLimits(in map[string]USDLimit, field string) map[string]USDLimit {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]USDLimit, len(in))
	for rawKey, value := range in {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		if key == "" {
			continue
		}
		if value < 0 {
			log.WithFields(log.Fields{
				"field": field,
				"key":   key,
			}).Warn("config: rejected negative USD amount")
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeModelPriceOverrides(in map[string]ModelPriceOverride) map[string]ModelPriceOverride {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ModelPriceOverride, len(in))
	for rawModel, price := range in {
		model := strings.ToLower(strings.TrimSpace(rawModel))
		if model == "" {
			continue
		}
		price.Prompt = nonNegativeModelPrice(model, "prompt", price.Prompt)
		price.Completion = nonNegativeModelPrice(model, "completion", price.Completion)
		price.Reasoning = nonNegativeModelPrice(model, "reasoning", price.Reasoning)
		price.CacheRead = nonNegativeModelPrice(model, "cache-read", price.CacheRead)
		price.CacheWrite = nonNegativeModelPrice(model, "cache-write", price.CacheWrite)
		if price.Prompt == nil &&
			price.Completion == nil &&
			price.Reasoning == nil &&
			price.CacheRead == nil &&
			price.CacheWrite == nil {
			continue
		}
		out[model] = price
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nonNegativeModelPrice(model, category string, value *USDPerMillionTokens) *USDPerMillionTokens {
	if value == nil || *value >= 0 {
		return value
	}
	log.WithFields(log.Fields{
		"model":    model,
		"category": category,
	}).Warn("config: rejected negative model price override")
	return nil
}

func normalizeLowerKeyedString(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for rawKey, value := range in {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
