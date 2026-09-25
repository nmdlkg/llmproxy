package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func modelIDs(models []*ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model != nil {
			ids = append(ids, model.ID)
		}
	}
	return ids
}

func restoreEmbeddedCatalog(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if errLoad := loadModelsFromBytes(embeddedModelsJSON, "embed"); errLoad != nil {
			t.Fatalf("restore embedded catalog: %v", errLoad)
		}
	})
}

// TestForkModelOverlayAppliedToEmbeddedCatalog covers the embedded and
// --local-model path: every overlay entry is appended to the end of its section
// in overlay order, and upstream entries keep their original order.
func TestForkModelOverlayAppliedToEmbeddedCatalog(t *testing.T) {
	restoreEmbeddedCatalog(t)
	overlay, errOverlay := parseForkModelOverlay(forkModelsOverlayJSON)
	if errOverlay != nil {
		t.Fatalf("parse overlay: %v", errOverlay)
	}
	if len(overlay) == 0 {
		t.Fatal("fork overlay is empty")
	}
	var upstream staticModelsJSON
	if errDecode := json.Unmarshal(embeddedModelsJSON, &upstream); errDecode != nil {
		t.Fatalf("decode upstream catalog: %v", errDecode)
	}
	if errLoad := loadModelsFromBytes(embeddedModelsJSON, "embed"); errLoad != nil {
		t.Fatalf("loadModelsFromBytes: %v", errLoad)
	}
	merged := catalogSections(getModels())
	upstreamSections := catalogSections(&upstream)
	for section, entries := range overlay {
		got := modelIDs(*merged[section])
		want := append(modelIDs(*upstreamSections[section]), modelIDs(entries)...)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s = %v, want %v", section, got, want)
		}
	}
}

func TestForkModelOverlayCodexPlanExposure(t *testing.T) {
	restoreEmbeddedCatalog(t)
	if errLoad := loadModelsFromBytes(embeddedModelsJSON, "embed"); errLoad != nil {
		t.Fatalf("loadModelsFromBytes: %v", errLoad)
	}
	contains := func(models []*ModelInfo, id string) bool {
		for _, model := range models {
			if model != nil && model.ID == id {
				return true
			}
		}
		return false
	}
	checks := []struct {
		name   string
		models []*ModelInfo
		want   []string
		absent []string
	}{
		{name: "free", models: GetCodexFreeModels(), want: []string{"gpt-5.4-mini"}, absent: []string{"gpt-5.4", "gpt-5.3-codex-spark"}},
		{name: "team", models: GetCodexTeamModels(), want: []string{"gpt-5.4", "gpt-5.4-mini"}, absent: []string{"gpt-5.3-codex-spark"}},
		{name: "plus", models: GetCodexPlusModels(), want: []string{"gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini"}},
		{name: "pro", models: GetCodexProModels(), want: []string{"gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini"}},
		{name: "antigravity", models: GetAntigravityModels(), want: []string{"gemini-3-flash-agent", "gemini-3.5-flash-low", "gemini-3.5-flash-extra-low"}},
	}
	for _, check := range checks {
		for _, id := range check.want {
			if !contains(check.models, id) {
				t.Errorf("%s catalog missing %s", check.name, id)
			}
		}
		for _, id := range check.absent {
			if contains(check.models, id) {
				t.Errorf("%s catalog unexpectedly contains %s", check.name, id)
			}
		}
	}
}

func TestForkModelOverlayUpstreamEntriesWin(t *testing.T) {
	data := &staticModelsJSON{
		CodexPro: []*ModelInfo{{ID: "GPT-5.4", DisplayName: "upstream"}, {ID: "other"}},
	}
	overlay := map[string][]*ModelInfo{
		"codex-pro":     {{ID: "gpt-5.4", DisplayName: "fork"}, {ID: "fork-only"}, {ID: "FORK-ONLY"}, {ID: " "}, nil},
		"not-a-section": {{ID: "ignored"}},
	}
	mergeModelOverlay(data, overlay, "test")
	if got := strings.Join(modelIDs(data.CodexPro), ","); got != "GPT-5.4,other,fork-only" {
		t.Fatalf("codex-pro = %s", got)
	}
	if data.CodexPro[0].DisplayName != "upstream" {
		t.Fatalf("upstream entry replaced: %+v", data.CodexPro[0])
	}
	if errValidate := validateModelSection("codex-pro", data.CodexPro); errValidate != nil {
		t.Fatalf("merged section invalid: %v", errValidate)
	}
}

func TestForkModelOverlayMalformedIsIgnored(t *testing.T) {
	original := forkModelsOverlayJSON
	t.Cleanup(func() { forkModelsOverlayJSON = original })
	forkModelsOverlayJSON = []byte(`{not json`)
	data := &staticModelsJSON{Claude: []*ModelInfo{{ID: "c"}}}
	applyForkModelOverlay(data, "test")
	if len(data.Claude) != 1 {
		t.Fatalf("catalog changed by malformed overlay: %v", modelIDs(data.Claude))
	}
}

func TestForkModelOverlayDoesNotShareEntriesBetweenLoads(t *testing.T) {
	first := &staticModelsJSON{}
	second := &staticModelsJSON{}
	applyForkModelOverlay(first, "first")
	applyForkModelOverlay(second, "second")
	if len(first.CodexPro) == 0 || len(second.CodexPro) == 0 {
		t.Fatal("overlay did not add codex-pro entries")
	}
	if first.CodexPro[0] == second.CodexPro[0] {
		t.Fatal("overlay entries are shared between catalog loads")
	}
}

// TestForkModelOverlayAppliedToRemoteRefresh covers the remote updater path:
// an upstream catalog fetched at runtime still carries the fork entries.
func TestForkModelOverlayAppliedToRemoteRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(embeddedModelsJSON)
	}))
	defer server.Close()
	originalURLs := modelsURLs
	t.Cleanup(func() { modelsURLs = originalURLs })
	modelsURLs = []string{server.URL}

	parsed, url := fetchModelsFromRemote(context.Background())
	if parsed == nil || url != server.URL {
		t.Fatalf("fetchModelsFromRemote() = %v, %q", parsed, url)
	}
	found := false
	for _, model := range parsed.CodexPro {
		if model != nil && model.ID == "gpt-5.4" {
			found = true
		}
	}
	if !found {
		t.Fatalf("remote catalog missing overlay entry: %v", modelIDs(parsed.CodexPro))
	}
}
