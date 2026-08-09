package openrouter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestNewCatalogLoadsIndependentEmbeddedFallback(t *testing.T) {
	catalog, err := NewCatalog()
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	first := catalog.Snapshot()
	if first.Source != "embedded" {
		t.Fatalf("Source = %q, want embedded", first.Source)
	}
	if len(first.Pricing) == 0 || len(first.Quality) == 0 {
		t.Fatalf("embedded snapshot is incomplete: pricing=%d quality=%d", len(first.Pricing), len(first.Quality))
	}

	delete(first.Pricing, "openai/gpt-4o")
	second := catalog.Snapshot()
	if _, ok := second.Pricing["openai/gpt-4o"]; !ok {
		t.Fatal("mutating returned snapshot changed catalog state")
	}
}

func TestRefreshFailureRetainsPreviousSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"vendor/new","pricing":{"prompt":"1","completion":"2"}}]}`)
		case "/benchmarks":
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := NewCatalog()
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	before := catalog.Snapshot()
	cfg := &config.Config{}
	cfg.OpenRouter.BaseURL = server.URL
	cfg.OpenRouter.APIKey = "bad-key"
	cfg.OpenRouter.BenchmarkSource = string(BenchmarkArtificialAnalysis)

	err = catalog.refresh(context.Background(), NewClient(context.Background(), cfg), cfg.OpenRouter)
	if err == nil {
		t.Fatal("refresh() error = nil, want benchmark failure")
	}
	after := catalog.Snapshot()
	if after.Source != before.Source || len(after.Pricing) != len(before.Pricing) {
		t.Fatalf("snapshot changed after failed refresh: before=%q/%d after=%q/%d",
			before.Source, len(before.Pricing), after.Source, len(after.Pricing))
	}
	if _, ok := after.Pricing["vendor/new"]; ok {
		t.Fatal("partial pricing was published after benchmark failure")
	}
}

func TestRefreshSuccessAtomicallyReplacesSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"vendor/new","pricing":{"prompt":"1","completion":"2"}}]}`)
		case "/benchmarks":
			_, _ = fmt.Fprint(w, `{"data":[{"model_permaslug":"vendor/new","coding_index":77}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := NewCatalog()
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	cfg := &config.Config{}
	cfg.OpenRouter.BaseURL = server.URL
	cfg.OpenRouter.APIKey = "test-key"
	cfg.OpenRouter.BenchmarkSource = string(BenchmarkArtificialAnalysis)
	if errRefresh := catalog.refresh(context.Background(), NewClient(context.Background(), cfg), cfg.OpenRouter); errRefresh != nil {
		t.Fatalf("refresh() error = %v", errRefresh)
	}

	after := catalog.Snapshot()
	if after.Source != "openrouter-models+artificial-analysis" {
		t.Fatalf("Source = %q, want combined remote source", after.Source)
	}
	if len(after.Pricing) != 1 || len(after.Quality) != 1 {
		t.Fatalf("remote snapshot sizes = pricing %d quality %d, want 1/1", len(after.Pricing), len(after.Quality))
	}
	if !after.Quality["vendor/new"].CodingIndex.Known {
		t.Fatal("remote benchmark score is unknown")
	}
}

func TestStartUpdaterDisabledAndLocalOnlyDoNotFetch(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	tests := []struct {
		name          string
		enabled       bool
		disableRemote bool
	}{
		{name: "disabled config", enabled: false, disableRemote: false},
		{name: "local model", enabled: true, disableRemote: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog, err := NewCatalog()
			if err != nil {
				t.Fatalf("NewCatalog() error = %v", err)
			}
			cfg := &config.Config{}
			cfg.OpenRouter.Enabled = tt.enabled
			cfg.OpenRouter.BaseURL = server.URL
			catalog.StartUpdater(context.Background(), UpdaterOptions{
				Config:        cfg,
				LocalModels:   []string{"gpt-4o"},
				DisableRemote: tt.disableRemote,
			})
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("network requests = %d, want 0", got)
	}
}

func TestStartUpdaterFetchesImmediately(t *testing.T) {
	requested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"vendor/model","pricing":{"prompt":"1","completion":"1"}}]}`)
	}))
	defer server.Close()

	catalog, err := NewCatalog()
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	cfg := &config.Config{}
	cfg.OpenRouter.Enabled = true
	cfg.OpenRouter.BaseURL = server.URL
	cfg.OpenRouter.RefreshInterval = "1h"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	catalog.StartUpdater(ctx, UpdaterOptions{Config: cfg})
	select {
	case <-requested:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("startup refresh did not fetch immediately")
	}
}
