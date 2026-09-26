package api

// forkOptionConfig holds fork-only ServerOption state. It is embedded as a
// single field in serverOptionConfig so upstream option edits do not conflict.
type forkOptionConfig struct {
	tenancyEnabled   bool
	otelUsageEnabled bool
}

// WithTenancyService enables tenancy lifecycle bootstrap from the server's
// loaded configuration. A disabled tenancy config remains a no-op.
func WithTenancyService() ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.fork.tenancyEnabled = true
	}
}

// WithOTelUsage enables startup-only OTLP/HTTP usage metrics bootstrap from
// Config.OTel, with deprecated LLMPROXY_OTEL_* fallback when the section is absent.
func WithOTelUsage() ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.fork.otelUsageEnabled = true
	}
}

// WithOTelUsageFromEnvironment is retained for callers migrating to WithOTelUsage.
// Deprecated: use WithOTelUsage.
func WithOTelUsageFromEnvironment() ServerOption {
	return WithOTelUsage()
}
