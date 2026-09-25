package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/otelusage/otelspec"
)

// SanitizeOTelConfig trims OTLP settings, applies safe defaults, and rejects
// enabled configurations that cannot export. It does not synthesize an absent
// section because absence enables the deprecated environment fallback.
func (cfg *Config) SanitizeOTelConfig() error {
	if cfg == nil || cfg.OTel == nil {
		return nil
	}

	o := cfg.OTel
	o.Endpoint = strings.TrimSpace(o.Endpoint)
	o.ExportInterval = strings.TrimSpace(o.ExportInterval)
	o.ServiceName = strings.TrimSpace(o.ServiceName)
	o.ServiceVersion = strings.TrimSpace(o.ServiceVersion)
	o.Environment = strings.TrimSpace(o.Environment)

	switch interval, errParse := time.ParseDuration(o.ExportInterval); {
	case o.ExportInterval == "", errParse != nil:
		o.ExportInterval = otelspec.DefaultExportInterval.String()
	case interval <= 0:
		return fmt.Errorf("otel.export-interval must be positive")
	}
	if o.ServiceName == "" {
		o.ServiceName = otelspec.DefaultServiceName
	}
	if o.Enabled && o.Endpoint == "" {
		return fmt.Errorf("otel.endpoint is required when otel.enabled is true")
	}

	resourceAttributes, errAttributes := otelspec.NormalizeResourceAttributes(o.ResourceAttributes)
	if errAttributes != nil {
		return fmt.Errorf("sanitize otel.resource-attributes: %w", errAttributes)
	}
	o.ResourceAttributes = resourceAttributes
	return nil
}
