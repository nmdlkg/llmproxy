package autoroute

import "testing"

func TestExtractPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		protocol   string
		body       string
		maxChars   int
		wantPrompt string
	}{
		{
			name:       "openai string content selects last user message",
			protocol:   "openai",
			body:       `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"last"}]}`,
			maxChars:   4000,
			wantPrompt: "last",
		},
		{
			name:       "claude content parts",
			protocol:   "claude",
			body:       `{"messages":[{"role":"user","content":[{"type":"text","text":"alpha"},{"type":"image","source":{"type":"base64"}},{"type":"text","text":"beta"}]}]}`,
			maxChars:   4000,
			wantPrompt: "alpha\nbeta",
		},
		{
			name:       "openai mixed content parts",
			protocol:   "openai-response",
			body:       `{"messages":[{"role":"user","content":["alpha",{"type":"text","text":"beta"}]}]}`,
			maxChars:   4000,
			wantPrompt: "alpha\nbeta",
		},
		{
			name:       "gemini parts joined from last user content",
			protocol:   "gemini",
			body:       `{"contents":[{"role":"user","parts":[{"text":"first"}]},{"role":"model","parts":[{"text":"reply"}]},{"role":"user","parts":[{"text":"last one"},{"inlineData":{"mimeType":"image/png"}},{"text":"last two"}]}]}`,
			maxChars:   4000,
			wantPrompt: "last one\nlast two",
		},
		{
			name:       "gemini omitted role is accepted",
			protocol:   "gemini-interactions",
			body:       `{"contents":[{"parts":[{"text":"hello"}]}]}`,
			maxChars:   4000,
			wantPrompt: "hello",
		},
		{
			name:       "unicode truncation counts characters",
			protocol:   "openai",
			body:       `{"messages":[{"role":"user","content":"가나다라마"}]}`,
			maxChars:   3,
			wantPrompt: "가나다",
		},
		{
			name:       "empty body",
			protocol:   "openai",
			body:       "",
			maxChars:   4000,
			wantPrompt: "",
		},
		{
			name:       "malformed body",
			protocol:   "openai",
			body:       `{"messages":`,
			maxChars:   4000,
			wantPrompt: "",
		},
		{
			name:       "missing messages",
			protocol:   "claude",
			body:       `{"prompt":"hello"}`,
			maxChars:   4000,
			wantPrompt: "",
		},
		{
			name:       "no user message",
			protocol:   "openai",
			body:       `{"messages":[{"role":"assistant","content":"hello"}]}`,
			maxChars:   4000,
			wantPrompt: "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractPrompt(tt.protocol, []byte(tt.body), tt.maxChars); got != tt.wantPrompt {
				t.Fatalf("ExtractPrompt() = %q, want %q", got, tt.wantPrompt)
			}
		})
	}
}
