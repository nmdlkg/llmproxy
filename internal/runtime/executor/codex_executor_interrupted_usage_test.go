package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestCodexExecutorStreamAccountsClientCancellation verifies that a stream
// cancelled by the client before response.completed still publishes one
// successful attempt with output estimated from the streamed deltas.
func TestCodexExecutorStreamAccountsClientCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking about it"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"hello world"}` + "\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	const alias = "codex-interrupted-usage-test"
	capture := &codexResponseModelUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), codexResponseModelNoopUsagePlugin{}) })

	ctx, cancel := context.WithCancel(coreusage.WithRequestedModelAlias(context.Background(), alias))
	defer cancel()
	auth := &cliproxyauth.Auth{ID: "codex-auth", Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	result, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	received := 0
	for range result.Chunks {
		received++
		if received == 2 {
			cancel()
			break
		}
	}

	record := capture.await(t)
	if record.Failed {
		t.Fatalf("client cancellation must not be recorded as an upstream failure: %+v", record.Fail)
	}
	if record.Detail.OutputTokens <= 0 {
		t.Fatalf("output tokens = %d, want a positive estimate", record.Detail.OutputTokens)
	}
	if record.Detail.InputTokens != 0 {
		t.Fatalf("input tokens = %d, want 0 for an interrupted attempt", record.Detail.InputTokens)
	}
}
