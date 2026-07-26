package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSanitizeOpenRouterConfigFromParseAndLoad(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", " env-key ")
	data := []byte(`
openrouter:
  enabled: true
  base-url: " https://example.test/api/ "
  refresh-interval: invalid
  model-map:
    " gpt-4o ": " openai/gpt-4o "
    "": "ignored/model"
  cost-basis: OPENROUTER
  benchmark-source: ARTIFICIAL-ANALYSIS
`)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	parsed, errParse := ParseConfigBytes(data)
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	loaded, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}

	for name, cfg := range map[string]*Config{"parse": parsed, "load": loaded} {
		t.Run(name, func(t *testing.T) {
			got := cfg.OpenRouter
			if !got.Enabled {
				t.Fatal("Enabled = false, want true")
			}
			if got.APIKey != "env-key" {
				t.Fatalf("APIKey = %q, want env-key", got.APIKey)
			}
			if got.BaseURL != "https://example.test/api" {
				t.Fatalf("BaseURL = %q", got.BaseURL)
			}
			if got.RefreshInterval != "6h" {
				t.Fatalf("RefreshInterval = %q, want 6h", got.RefreshInterval)
			}
			if !reflect.DeepEqual(got.ModelMap, map[string]string{"gpt-4o": "openai/gpt-4o"}) {
				t.Fatalf("ModelMap = %#v", got.ModelMap)
			}
			if got.CostBasis != "openrouter" {
				t.Fatalf("CostBasis = %q, want openrouter", got.CostBasis)
			}
			if got.BenchmarkSource != "artificial-analysis" {
				t.Fatalf("BenchmarkSource = %q, want artificial-analysis", got.BenchmarkSource)
			}
		})
	}
}

func TestSanitizeOpenRouterConfigSafeDefaults(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	cfg := &Config{SDKConfig: SDKConfig{OpenRouter: OpenRouterConfig{
		RefreshInterval: "0s",
		CostBasis:       "raw-dollars",
		BenchmarkSource: "unknown",
	}}}
	cfg.SanitizeOpenRouterConfig()

	if cfg.OpenRouter.Enabled {
		t.Fatal("Enabled changed to true")
	}
	if cfg.OpenRouter.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("BaseURL = %q", cfg.OpenRouter.BaseURL)
	}
	if cfg.OpenRouter.RefreshInterval != "6h" {
		t.Fatalf("RefreshInterval = %q", cfg.OpenRouter.RefreshInterval)
	}
	if cfg.OpenRouter.CostBasis != "static" {
		t.Fatalf("CostBasis = %q", cfg.OpenRouter.CostBasis)
	}
	if cfg.OpenRouter.BenchmarkSource != "" {
		t.Fatalf("BenchmarkSource = %q, want off", cfg.OpenRouter.BenchmarkSource)
	}
}
