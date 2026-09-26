package config

// Fork-owned OpenTelemetry usage export configuration. Kept out of
// config_types.go so upstream merges do not conflict with fork declarations.

// OTelConfig configures process-static OTLP/HTTP usage metric export.
type OTelConfig struct {
	Enabled            bool              `yaml:"enabled" json:"enabled"`
	Endpoint           string            `yaml:"endpoint" json:"endpoint"`
	ExportInterval     string            `yaml:"export-interval" json:"export-interval"`
	ServiceName        string            `yaml:"service-name" json:"service-name"`
	ServiceVersion     string            `yaml:"service-version" json:"service-version"`
	Environment        string            `yaml:"environment" json:"environment"`
	ResourceAttributes map[string]string `yaml:"resource-attributes" json:"resource-attributes"`
}
