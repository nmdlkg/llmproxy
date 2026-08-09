package config

import "sync"

var (
	modelPricingAuditorMu sync.RWMutex
	modelPricingAuditor   func(*Config)
)

// RegisterModelPricingAuditor installs the catalog-owned pricing coverage
// audit invoked after both tenancy and OpenRouter config are sanitized.
func RegisterModelPricingAuditor(auditor func(*Config)) {
	modelPricingAuditorMu.Lock()
	modelPricingAuditor = auditor
	modelPricingAuditorMu.Unlock()
}

func auditModelPricing(cfg *Config) {
	modelPricingAuditorMu.RLock()
	auditor := modelPricingAuditor
	modelPricingAuditorMu.RUnlock()
	if auditor != nil {
		auditor(cfg)
	}
}
