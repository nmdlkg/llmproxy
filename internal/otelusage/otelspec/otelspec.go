// Package otelspec holds dependency-free OpenTelemetry usage export defaults and
// validation shared by configuration parsing and the exporter. It must not import
// SDK usage packages so internal/config can depend on it without an import cycle.
package otelspec

import (
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultExportInterval keeps proxy metrics reasonably fresh without
	// turning collector availability into a request-path concern.
	DefaultExportInterval = 15 * time.Second

	// DefaultServiceName is the OTel service.name used when none is configured.
	DefaultServiceName = "llmproxy"
)

// NormalizeResourceAttributes trims resource attributes, drops empty entries,
// and rejects attributes reserved by the exporter or that could carry secrets.
func NormalizeResourceAttributes(attributes map[string]string) (map[string]string, error) {
	if len(attributes) == 0 {
		return nil, nil
	}
	resourceAttributes := make(map[string]string, len(attributes))
	for key, value := range attributes {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		if forbiddenAttributeName(key) {
			return nil, fmt.Errorf("otel usage: resource attribute %q is forbidden", key)
		}
		resourceAttributes[key] = value
	}
	if len(resourceAttributes) == 0 {
		return nil, nil
	}
	return resourceAttributes, nil
}

func forbiddenAttributeName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "service.name", "service.version", "deployment.environment", "host.name",
		"auth_id", "auth_index", "api_key", "request_id", "alias":
		return true
	default:
		return false
	}
}
