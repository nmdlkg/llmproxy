package autoroute

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestClientClassifySuccess(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != classifyIntentPath {
			t.Errorf("path = %s, want %s", r.URL.Path, classifyIntentPath)
		}
		var requestBody struct {
			Text string `json:"text"`
		}
		if errDecode := json.NewDecoder(r.Body).Decode(&requestBody); errDecode != nil {
			t.Errorf("decode request: %v", errDecode)
		}
		if requestBody.Text != "classify me" {
			t.Errorf("text = %q, want %q", requestBody.Text, "classify me")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"classification":{"category":"Code","confidence":0.93}}`))
	}))
	defer server.Close()

	client := NewClient(context.Background(), config.AutoRoutingConfig{
		RouterURL: server.URL,
		TimeoutMS: 500,
	}, nil)
	got := client.Classify(context.Background(), "classify me")
	want := Classification{Category: "code", Confidence: 0.93, Valid: true}
	if got != want {
		t.Fatalf("Classify() = %#v, want %#v", got, want)
	}
}

func TestClientClassifyFailuresReturnNoDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		newServer func(t *testing.T) (string, func())
		timeoutMS int
	}{
		{
			name: "timeout",
			newServer: func(t *testing.T) (string, func()) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					time.Sleep(100 * time.Millisecond)
					_, _ = w.Write([]byte(`{"classification":{"category":"code","confidence":0.9}}`))
				}))
				return server.URL, server.Close
			},
			timeoutMS: 20,
		},
		{
			name: "server error",
			newServer: func(t *testing.T) (string, func()) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				}))
				return server.URL, server.Close
			},
			timeoutMS: 500,
		},
		{
			name: "garbage json",
			newServer: func(t *testing.T) (string, func()) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`not-json`))
				}))
				return server.URL, server.Close
			},
			timeoutMS: 500,
		},
		{
			name: "trailing garbage",
			newServer: func(t *testing.T) (string, func()) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`{"classification":{"category":"code","confidence":0.9}} garbage`))
				}))
				return server.URL, server.Close
			},
			timeoutMS: 500,
		},
		{
			name: "malformed classification",
			newServer: func(t *testing.T) (string, func()) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`{"classification":{"category":"","confidence":0.9}}`))
				}))
				return server.URL, server.Close
			},
			timeoutMS: 500,
		},
		{
			name: "unreachable",
			newServer: func(t *testing.T) (string, func()) {
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				url := server.URL
				server.Close()
				return url, func() {}
			},
			timeoutMS: 500,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			routerURL, cleanup := tt.newServer(t)
			defer cleanup()

			client := NewClient(context.Background(), config.AutoRoutingConfig{
				RouterURL: routerURL,
				TimeoutMS: tt.timeoutMS,
			}, nil)
			start := time.Now()
			if got := client.Classify(context.Background(), "prompt"); got.Valid {
				t.Fatalf("Classify() = %#v, want no decision", got)
			}
			if tt.name == "timeout" {
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Fatalf("timeout took %v, want under 1s", elapsed)
				}
			}
		})
	}
}
