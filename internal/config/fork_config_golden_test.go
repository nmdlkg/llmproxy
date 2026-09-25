package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// forkConfigSnapshot captures every fork-owned config section, including the
// runtime-only Tenancy.Pricing copy that JSON tags would otherwise hide.
type forkConfigSnapshot struct {
	Tenancy        TenancyConfig     `json:"tenancy"`
	TenancyPricing OpenRouterConfig  `json:"tenancy-pricing"`
	AutoRouting    AutoRoutingConfig `json:"auto-routing"`
	OpenRouter     OpenRouterConfig  `json:"openrouter"`
	OTel           *OTelConfig       `json:"otel"`
}

func snapshotForkConfig(cfg *Config) forkConfigSnapshot {
	return forkConfigSnapshot{
		Tenancy:        cfg.Tenancy,
		TenancyPricing: cfg.Tenancy.Pricing,
		AutoRouting:    cfg.AutoRouting,
		OpenRouter:     cfg.OpenRouter,
		OTel:           cfg.OTel,
	}
}

// TestForkConfigNormalizationGolden pins the parsed fork configuration so that
// moving fork types and normalizers out of upstream files cannot change
// behavior. Regenerate only for intentional changes with
// UPDATE_FORK_CONFIG_GOLDEN=1 go test ./internal/config -run ForkConfigNormalizationGolden.
func TestForkConfigNormalizationGolden(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	fixtures := map[string]string{
		"config-example": filepath.Join("..", "..", "config.example.yaml"),
		"fork-enabled":   filepath.Join("testdata", "fork_sections_enabled.yaml"),
		"fork-absent":    filepath.Join("testdata", "fork_sections_absent.yaml"),
	}
	for name, path := range fixtures {
		t.Run(name, func(t *testing.T) {
			data, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatalf("read fixture: %v", errRead)
			}
			parsed, errParse := ParseConfigBytes(data)
			if errParse != nil {
				t.Fatalf("ParseConfigBytes: %v", errParse)
			}
			tempPath := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(tempPath, data, 0o600); errWrite != nil {
				t.Fatalf("write temp config: %v", errWrite)
			}
			loaded, errLoad := LoadConfigOptional(tempPath, false)
			if errLoad != nil {
				t.Fatalf("LoadConfigOptional: %v", errLoad)
			}
			parsedSnapshot := snapshotForkConfig(parsed)
			if loadedSnapshot := snapshotForkConfig(loaded); !reflect.DeepEqual(parsedSnapshot, loadedSnapshot) {
				t.Fatalf("loader and byte parser diverged:\nparse=%+v\nload=%+v", parsedSnapshot, loadedSnapshot)
			}

			got, errMarshal := json.MarshalIndent(parsedSnapshot, "", "  ")
			if errMarshal != nil {
				t.Fatalf("marshal snapshot: %v", errMarshal)
			}
			got = append(got, '\n')
			goldenPath := filepath.Join("testdata", name+".golden.json")
			if os.Getenv("UPDATE_FORK_CONFIG_GOLDEN") == "1" {
				if errWrite := os.WriteFile(goldenPath, got, 0o644); errWrite != nil {
					t.Fatalf("write golden: %v", errWrite)
				}
			}
			want, errGolden := os.ReadFile(goldenPath)
			if errGolden != nil {
				t.Fatalf("read golden: %v", errGolden)
			}
			var wantSnapshot forkConfigSnapshot
			if errDecode := json.Unmarshal(want, &wantSnapshot); errDecode != nil {
				t.Fatalf("decode golden: %v", errDecode)
			}
			var gotSnapshot forkConfigSnapshot
			if errDecode := json.Unmarshal(got, &gotSnapshot); errDecode != nil {
				t.Fatalf("decode snapshot: %v", errDecode)
			}
			if !reflect.DeepEqual(wantSnapshot, gotSnapshot) {
				t.Fatalf("fork config changed:\nwant=%s\ngot=%s", want, got)
			}
		})
	}
}

// TestForkConfigInvalidOTelIsRejectedByBothLoaders keeps OTel validation on
// both parsing entry points.
func TestForkConfigInvalidOTelIsRejectedByBothLoaders(t *testing.T) {
	data := []byte("otel:\n  enabled: true\n  endpoint: \"\"\n")
	if _, errParse := ParseConfigBytes(data); errParse == nil {
		t.Fatal("ParseConfigBytes accepted enabled OTel without endpoint")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	if _, errLoad := LoadConfigOptional(path, false); errLoad == nil {
		t.Fatal("LoadConfigOptional accepted enabled OTel without endpoint")
	}
}

// TestForkConfigWriteBackOmitsUserPanelDefault verifies that saving a config
// loaded without a user-panel section does not persist the injected default
// repository, and that reloading the saved file yields the same fork sections.
func TestForkConfigWriteBackOmitsUserPanelDefault(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("port: 8317\ntenancy:\n  enabled: true\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	loaded, errLoad := LoadConfigOptional(path, false)
	if errLoad != nil {
		t.Fatalf("load config: %v", errLoad)
	}
	if loaded.Tenancy.UserPanel.GitHubRepository != DefaultUserPanelGitHubRepository {
		t.Fatalf("user panel repository = %q", loaded.Tenancy.UserPanel.GitHubRepository)
	}
	if errSave := SaveConfigPreserveComments(path, loaded); errSave != nil {
		t.Fatalf("save config: %v", errSave)
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read saved config: %v", errRead)
	}
	if strings.Contains(string(saved), DefaultUserPanelGitHubRepository) {
		t.Fatalf("saved config persisted the default user panel repository:\n%s", saved)
	}
	reloaded, errReload := LoadConfigOptional(path, false)
	if errReload != nil {
		t.Fatalf("reload config: %v", errReload)
	}
	if !reflect.DeepEqual(snapshotForkConfig(loaded), snapshotForkConfig(reloaded)) {
		t.Fatalf("reload changed fork sections:\nbefore=%+v\nafter=%+v", snapshotForkConfig(loaded), snapshotForkConfig(reloaded))
	}
}
