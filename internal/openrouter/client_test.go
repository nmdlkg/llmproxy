package openrouter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestFetchPricingParsesStringCostsDefensively(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"data": [
				{
					"id": "vendor/free",
					"context_length": 1234,
					"supported_parameters": ["tools"],
					"pricing": {
						"prompt": "0",
						"completion": "0.0000025",
						"internal_reasoning": "garbage",
						"input_cache_read": "0",
						"request": "0.01",
						"image": "0.002",
						"web_search": 1
					}
				},
				{"id": "vendor/missing", "pricing": {}}
			]
		}`)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.OpenRouter.BaseURL = server.URL
	got, err := NewClient(context.Background(), cfg).FetchPricing(context.Background())
	if err != nil {
		t.Fatalf("FetchPricing() error = %v", err)
	}

	tests := []struct {
		name  string
		value KnownNanoUSD
		want  KnownNanoUSD
	}{
		{name: "known free prompt", value: got["vendor/free"].PromptNanoUSDPerToken, want: KnownNanoUSD{Known: true}},
		{name: "known completion", value: got["vendor/free"].CompletionNanoUSDPerToken, want: KnownNanoUSD{NanoUSD: 2500, Known: true}},
		{name: "garbage reasoning", value: got["vendor/free"].InternalReasoningNanoUSDPerToken, want: KnownNanoUSD{}},
		{name: "known free cache read", value: got["vendor/free"].InputCacheReadNanoUSDPerToken, want: KnownNanoUSD{Known: true}},
		{name: "known per request", value: got["vendor/free"].RequestNanoUSD, want: KnownNanoUSD{NanoUSD: 10_000_000, Known: true}},
		{name: "known per image", value: got["vendor/free"].ImageNanoUSD, want: KnownNanoUSD{NanoUSD: 2_000_000, Known: true}},
		{name: "wrong web search shape", value: got["vendor/free"].WebSearchNanoUSD, want: KnownNanoUSD{}},
		{name: "missing prompt", value: got["vendor/missing"].PromptNanoUSDPerToken, want: KnownNanoUSD{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.want {
				t.Fatalf("value = %+v, want %+v", tt.value, tt.want)
			}
		})
	}

	if got["vendor/free"].ContextLength != 1234 {
		t.Fatalf("ContextLength = %d, want 1234", got["vendor/free"].ContextLength)
	}
	if len(got["vendor/free"].SupportedParameters) != 1 || got["vendor/free"].SupportedParameters[0] != "tools" {
		t.Fatalf("SupportedParameters = %v, want [tools]", got["vendor/free"].SupportedParameters)
	}
}

func TestFetchBenchmarksPreservesMissingIndices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/benchmarks" {
			t.Errorf("path = %q, want /benchmarks", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want bearer test key", got)
		}
		if got := r.URL.Query().Get("source"); got != "artificial-analysis" {
			t.Errorf("source = %q, want artificial-analysis", got)
		}
		if got := r.URL.Query().Get("task_type"); got != "coding" {
			t.Errorf("task_type = %q, want coding", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"data": [{
				"model_permaslug": "vendor/model",
				"display_name": "Model",
				"intelligence_index": 0,
				"agentic_index": "41.5"
			}]
		}`)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.OpenRouter.BaseURL = server.URL
	cfg.OpenRouter.APIKey = "test-key"
	got, err := NewClient(context.Background(), cfg).FetchBenchmarks(context.Background(), BenchmarkQuery{
		Source:     BenchmarkArtificialAnalysis,
		TaskType:   TaskCoding,
		MaxResults: 100,
	})
	if err != nil {
		t.Fatalf("FetchBenchmarks() error = %v", err)
	}

	record := got["vendor/model"]
	if record.IntelligenceIndex != (KnownValue{Value: 0, Known: true}) {
		t.Fatalf("IntelligenceIndex = %+v, want known zero", record.IntelligenceIndex)
	}
	if record.CodingIndex.Known {
		t.Fatalf("CodingIndex = %+v, want unknown", record.CodingIndex)
	}
	if record.AgenticIndex != (KnownValue{Value: 41.5, Known: true}) {
		t.Fatalf("AgenticIndex = %+v, want 41.5", record.AgenticIndex)
	}
	if record.Elo.Known || record.WinRate.Known {
		t.Fatalf("design metrics = elo %+v, win rate %+v; want unknown", record.Elo, record.WinRate)
	}
}

func TestFetchBenchmarksWithoutKeyReturnsUnauthorized(t *testing.T) {
	cfg := &config.Config{}
	cfg.OpenRouter.BaseURL = "http://127.0.0.1:1"
	_, err := NewClient(context.Background(), cfg).FetchBenchmarks(context.Background(), BenchmarkQuery{
		Source: BenchmarkArtificialAnalysis,
	})
	if !errors.Is(err, ErrBenchmarkAPIKeyRequired) {
		t.Fatalf("error = %v, want ErrBenchmarkAPIKeyRequired", err)
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error type = %T, want *HTTPStatusError", err)
	}
	if statusErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", statusErr.StatusCode)
	}
}

func TestFetchDesignArenaMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("arena"); got != "models" {
			t.Errorf("arena = %q, want models", got)
		}
		_, _ = fmt.Fprint(w, `{
			"data": [{
				"model_permaslug": "vendor/designer",
				"elo": 1234.5,
				"win_rate": 0.61,
				"avg_generation_time_ms": 4321,
				"arena": "models",
				"category": "web"
			}]
		}`)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.OpenRouter.BaseURL = server.URL
	cfg.OpenRouter.APIKey = "test-key"
	got, err := NewClient(context.Background(), cfg).FetchBenchmarks(context.Background(), BenchmarkQuery{
		Source: BenchmarkDesignArena,
		Arena:  "models",
	})
	if err != nil {
		t.Fatalf("FetchBenchmarks() error = %v", err)
	}
	record := got["vendor/designer"]
	if record.Elo != (KnownValue{Value: 1234.5, Known: true}) {
		t.Fatalf("Elo = %+v", record.Elo)
	}
	if record.WinRate != (KnownValue{Value: 0.61, Known: true}) {
		t.Fatalf("WinRate = %+v", record.WinRate)
	}
	if record.AvgGenerationTimeMS != (KnownValue{Value: 4321, Known: true}) {
		t.Fatalf("AvgGenerationTimeMS = %+v", record.AvgGenerationTimeMS)
	}
	if record.Arena != "models" || record.Category != "web" {
		t.Fatalf("metadata = arena %q category %q", record.Arena, record.Category)
	}
}
