package openrouter

import "testing"

func TestBuiltinLocalModelNamesIncludesKnownCatalogGaps(t *testing.T) {
	t.Parallel()

	models := builtinLocalModelNames()
	known := make(map[string]struct{}, len(models))
	for _, model := range models {
		known[model] = struct{}{}
	}
	for _, model := range []string{
		"gpt-5.3-codex-spark",
		"codex-auto-review",
		"imagen-4.0-generate-001",
	} {
		if _, ok := known[model]; !ok {
			t.Errorf("built-in local model audit is missing %q", model)
		}
	}
}
