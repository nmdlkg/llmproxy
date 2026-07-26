package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/autoroute"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type fakeAutoRouteResolver struct {
	classification  autoroute.Classification
	liveModels      map[string]bool
	resolveCalls    int
	classifierCalls int
	lastPrompt      string
}

func (f *fakeAutoRouteResolver) resolve(ctx context.Context, cfg config.AutoRoutingConfig, _ *config.SDKConfig, prompt string) string {
	f.resolveCalls++
	if autoroute.ForcedFallback(ctx) {
		return autoroute.Decide(cfg, autoroute.Classification{}, true, f.modelAvailable)
	}
	if prompt == "" {
		return ""
	}
	f.classifierCalls++
	f.lastPrompt = prompt
	if !f.classification.Valid {
		return ""
	}
	return autoroute.Decide(cfg, f.classification, false, f.modelAvailable)
}

func (f *fakeAutoRouteResolver) modelAvailable(model string) bool {
	return f.liveModels[model]
}

func TestResolveAutoRoutedModel(t *testing.T) {
	originalResolve := resolveAutoRoute
	t.Cleanup(func() {
		resolveAutoRoute = originalResolve
	})

	requestBody := []byte(`{"messages":[{"role":"user","content":"write code"}]}`)
	tests := []struct {
		name                string
		model               string
		enabled             bool
		homeEnabled         bool
		classification      autoroute.Classification
		liveModels          map[string]bool
		forcedFallback      bool
		want                string
		wantResolveCalls    int
		wantClassifierCalls int
		wantPrompt          string
	}{
		{
			name:             "disabled config passes through",
			model:            "auto",
			want:             "auto",
			wantResolveCalls: 0,
		},
		{
			name:             "Home mode passes through",
			model:            "auto",
			enabled:          true,
			homeEnabled:      true,
			want:             "auto",
			wantResolveCalls: 0,
		},
		{
			name:             "non-auto model passes through",
			model:            "gpt-existing(high)",
			enabled:          true,
			want:             "gpt-existing(high)",
			wantResolveCalls: 0,
		},
		{
			name:           "auto resolves to first live tier model",
			model:          "auto",
			enabled:        true,
			classification: autoroute.Classification{Category: "code", Confidence: 0.9, Valid: true},
			liveModels: map[string]bool{
				"strong-live": true,
			},
			want:                "strong-live",
			wantResolveCalls:    1,
			wantClassifierCalls: 1,
			wantPrompt:          "write code",
		},
		{
			name:           "auto thinking suffix is preserved",
			model:          "auto(high)",
			enabled:        true,
			classification: autoroute.Classification{Category: "code", Confidence: 0.9, Valid: true},
			liveModels: map[string]bool{
				"strong-live": true,
			},
			want:                "strong-live(high)",
			wantResolveCalls:    1,
			wantClassifierCalls: 1,
			wantPrompt:          "write code",
		},
		{
			name:           "auto profile triggers routing",
			model:          "auto:quality",
			enabled:        true,
			classification: autoroute.Classification{Category: "code", Confidence: 0.9, Valid: true},
			liveModels: map[string]bool{
				"strong-live": true,
			},
			want:                "strong-live",
			wantResolveCalls:    1,
			wantClassifierCalls: 1,
			wantPrompt:          "write code",
		},
		{
			name:                "no decision leaves legacy auto untouched",
			model:               "auto",
			enabled:             true,
			classification:      autoroute.Classification{},
			want:                "auto",
			wantResolveCalls:    1,
			wantClassifierCalls: 1,
			wantPrompt:          "write code",
		},
		{
			name:             "forced fallback bypasses classification",
			model:            "auto",
			enabled:          true,
			liveModels:       map[string]bool{"fallback-live": true},
			forcedFallback:   true,
			want:             "fallback-live",
			wantResolveCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeResolver := &fakeAutoRouteResolver{
				classification: tt.classification,
				liveModels:     tt.liveModels,
			}
			resolveAutoRoute = fakeResolver.resolve

			cfg := &config.SDKConfig{
				AutoRouting: config.AutoRoutingConfig{
					Enabled:       tt.enabled,
					MinConfidence: 0.5,
					DefaultTier:   "balanced",
					FallbackModel: "fallback-live",
					Tiers: map[string]config.AutoRoutingTier{
						"balanced": {Models: []string{"balanced-live"}},
						"strong":   {Models: []string{"strong-dead", "strong-live"}},
					},
					Categories: map[string]string{"code": "strong"},
				},
			}
			var manager *coreauth.Manager
			if tt.homeEnabled {
				manager = coreauth.NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
			}
			handler := NewBaseAPIHandlers(cfg, manager)

			ctx := context.Background()
			if tt.forcedFallback {
				ctx = autoroute.WithForcedFallback(ctx)
			}
			got := handler.resolveAutoRoutedModel(ctx, "openai", tt.model, requestBody)
			if got != tt.want {
				t.Fatalf("resolveAutoRoutedModel() = %q, want %q", got, tt.want)
			}
			if fakeResolver.resolveCalls != tt.wantResolveCalls {
				t.Fatalf("resolver calls = %d, want %d", fakeResolver.resolveCalls, tt.wantResolveCalls)
			}
			if fakeResolver.classifierCalls != tt.wantClassifierCalls {
				t.Fatalf("classifier calls = %d, want %d", fakeResolver.classifierCalls, tt.wantClassifierCalls)
			}
			if fakeResolver.lastPrompt != tt.wantPrompt {
				t.Fatalf("classifier prompt = %q, want %q", fakeResolver.lastPrompt, tt.wantPrompt)
			}
		})
	}
}

type autoRouteCaptureExecutor struct {
	request coreexecutor.Request
	options coreexecutor.Options
}

func (e *autoRouteCaptureExecutor) Identifier() string {
	return "autoroute-handler-test"
}

func (e *autoRouteCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.capture(req, opts)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *autoRouteCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.capture(req, opts)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("stream-ok")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *autoRouteCaptureExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.capture(req, opts)
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":1}`)}, nil
}

func (e *autoRouteCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *autoRouteCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *autoRouteCaptureExecutor) capture(req coreexecutor.Request, opts coreexecutor.Options) {
	e.request = req
	e.options = opts
}

func TestAutoRoutingExecutionEntryPointsPreserveOriginalRequestedModel(t *testing.T) {
	originalResolve := resolveAutoRoute
	t.Cleanup(func() {
		resolveAutoRoute = originalResolve
	})

	const (
		requestedModel = "auto"
		resolvedModel  = "autoroute-handler-live"
	)
	resolveAutoRoute = func(context.Context, config.AutoRoutingConfig, *config.SDKConfig, string) string {
		return resolvedModel
	}

	tests := []struct {
		name   string
		invoke func(*BaseAPIHandler) *interfaces.ErrorMessage
	}{
		{
			name: "non-stream",
			invoke: func(handler *BaseAPIHandler) *interfaces.ErrorMessage {
				_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", requestedModel, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), "")
				return errMsg
			},
		},
		{
			name: "stream",
			invoke: func(handler *BaseAPIHandler) *interfaces.ErrorMessage {
				data, _, errors := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", requestedModel, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), "")
				for range data {
				}
				for errMsg := range errors {
					if errMsg != nil {
						return errMsg
					}
				}
				return nil
			},
		},
		{
			name: "count",
			invoke: func(handler *BaseAPIHandler) *interfaces.ErrorMessage {
				_, _, errMsg := handler.ExecuteCountWithAuthManager(context.Background(), "openai", requestedModel, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), "")
				return errMsg
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &autoRouteCaptureExecutor{}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			auth := &coreauth.Auth{
				ID:       "autoroute-handler-auth-" + tt.name,
				Provider: executor.Identifier(),
				Status:   coreauth.StatusActive,
			}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("manager.Register() error = %v", errRegister)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: resolvedModel}})
			t.Cleanup(func() {
				registry.GetGlobalRegistry().UnregisterClient(auth.ID)
			})

			handler := NewBaseAPIHandlers(&config.SDKConfig{
				AutoRouting: config.AutoRoutingConfig{Enabled: true},
			}, manager)
			if errMsg := tt.invoke(handler); errMsg != nil {
				t.Fatalf("execution error = %+v", errMsg)
			}
			if executor.request.Model != resolvedModel {
				t.Fatalf("executor model = %q, want %q", executor.request.Model, resolvedModel)
			}
			if got := executor.options.Metadata[coreexecutor.RequestedModelMetadataKey]; got != requestedModel {
				t.Fatalf("requested model metadata = %#v, want %q", got, requestedModel)
			}
		})
	}
}
