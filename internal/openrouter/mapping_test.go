package openrouter

import "testing"

func TestResolveModels(t *testing.T) {
	known := []string{
		"openai/gpt-4o",
		"anthropic/claude-sonnet-4-5-20250929",
		"vendor/other-model",
		"alpha/ambiguous-20250101",
		"beta/ambiguous-20250202",
	}

	tests := []struct {
		name     string
		local    string
		explicit map[string]string
		wantID   string
		method   MappingMethod
		resolved bool
	}{
		{
			name:     "explicit wins over heuristic",
			local:    "gpt-4o",
			explicit: map[string]string{"gpt-4o": "vendor/other-model"},
			wantID:   "vendor/other-model",
			method:   MappingExplicit,
			resolved: true,
		},
		{
			name:     "invalid explicit blocks heuristic",
			local:    "gpt-4o",
			explicit: map[string]string{"gpt-4o": "missing/model"},
			wantID:   "missing/model",
			method:   MappingExplicit,
			resolved: false,
		},
		{
			name:     "vendor prefix ignored",
			local:    "gpt-4o",
			wantID:   "openai/gpt-4o",
			method:   MappingHeuristic,
			resolved: true,
		},
		{
			name:     "date suffix ignored",
			local:    "claude-sonnet-4-5",
			wantID:   "anthropic/claude-sonnet-4-5-20250929",
			method:   MappingHeuristic,
			resolved: true,
		},
		{
			name:     "case insensitive",
			local:    "OPENAI/GPT-4O",
			wantID:   "openai/gpt-4o",
			method:   MappingHeuristic,
			resolved: true,
		},
		{
			name:     "ambiguous date match is conservative",
			local:    "ambiguous",
			method:   MappingUnresolved,
			resolved: false,
		},
		{
			name:     "fuzzy family match rejected",
			local:    "gpt-4o-mini",
			method:   MappingUnresolved,
			resolved: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveModels([]string{tt.local}, known, tt.explicit)
			if len(got) != 1 {
				t.Fatalf("len = %d, want 1", len(got))
			}
			if got[0].LocalModel != tt.local {
				t.Fatalf("LocalModel = %q, want unchanged %q", got[0].LocalModel, tt.local)
			}
			if got[0].OpenRouterModel != tt.wantID || got[0].Method != tt.method || got[0].Resolved != tt.resolved {
				t.Fatalf("mapping = %+v, want id=%q method=%q resolved=%v", got[0], tt.wantID, tt.method, tt.resolved)
			}
		})
	}
}
