package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSanitizeOTelConfigFromParseAndLoad(t *testing.T) {
	data := []byte(`
otel:
  enabled: true
  endpoint: " http://collector.tailnet:4328 "
  export-interval: invalid
  service-name: " "
  service-version: " v1.2.3 "
  environment: " staging "
  resource-attributes:
    " region ": " ap-northeast-2 "
    empty: " "
`)

	for name, cfg := range parseAndLoadOTelConfig(t, data) {
		t.Run(name, func(t *testing.T) {
			if cfg.OTel == nil {
				t.Fatal("OTel = nil, want present section")
			}
			if !cfg.OTel.Enabled {
				t.Fatal("Enabled = false, want true")
			}
			if cfg.OTel.Endpoint != "http://collector.tailnet:4328" {
				t.Fatalf("Endpoint = %q", cfg.OTel.Endpoint)
			}
			if cfg.OTel.ExportInterval != "15s" {
				t.Fatalf("ExportInterval = %q, want 15s", cfg.OTel.ExportInterval)
			}
			if cfg.OTel.ServiceName != "llmproxy" {
				t.Fatalf("ServiceName = %q, want llmproxy", cfg.OTel.ServiceName)
			}
			if cfg.OTel.ServiceVersion != "v1.2.3" {
				t.Fatalf("ServiceVersion = %q, want v1.2.3", cfg.OTel.ServiceVersion)
			}
			if cfg.OTel.Environment != "staging" {
				t.Fatalf("Environment = %q, want staging", cfg.OTel.Environment)
			}
			wantAttributes := map[string]string{"region": "ap-northeast-2"}
			if !reflect.DeepEqual(cfg.OTel.ResourceAttributes, wantAttributes) {
				t.Fatalf("ResourceAttributes = %#v, want %#v", cfg.OTel.ResourceAttributes, wantAttributes)
			}
		})
	}
}

func TestSanitizeOTelConfigRejectsInvalidSettings(t *testing.T) {
	tests := map[string]struct {
		yaml      string
		errorText string
	}{
		"enabled without endpoint": {
			yaml: `
otel:
  enabled: true
`,
			errorText: "otel.endpoint is required",
		},
		"zero interval": {
			yaml: `
otel:
  export-interval: "0s"
`,
			errorText: "otel.export-interval must be positive",
		},
		"negative interval": {
			yaml: `
otel:
  export-interval: "-1s"
`,
			errorText: "otel.export-interval must be positive",
		},
		"reserved resource attribute": {
			yaml: `
otel:
  resource-attributes:
    service.name: override
`,
			errorText: `resource attribute "service.name" is forbidden`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			data := []byte(test.yaml)
			if _, errParse := ParseConfigBytes(data); errParse == nil || !strings.Contains(errParse.Error(), test.errorText) {
				t.Fatalf("ParseConfigBytes() error = %v, want containing %q", errParse, test.errorText)
			}

			path := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
				t.Fatalf("WriteFile() error = %v", errWrite)
			}
			if _, errLoad := LoadConfig(path); errLoad == nil || !strings.Contains(errLoad.Error(), test.errorText) {
				t.Fatalf("LoadConfig() error = %v, want containing %q", errLoad, test.errorText)
			}
		})
	}
}

func TestSanitizeOTelConfigPreservesAbsentSection(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("debug: false\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.OTel != nil {
		t.Fatalf("OTel = %#v, want nil for absent section", cfg.OTel)
	}
}

func parseAndLoadOTelConfig(t *testing.T, data []byte) map[string]*Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	parsed, errParse := ParseConfigBytes(data)
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	loaded, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	return map[string]*Config{"parse": parsed, "load": loaded}
}
