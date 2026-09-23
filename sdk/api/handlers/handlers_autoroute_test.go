package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/autoroute"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
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
		emptyFallback       bool
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
			name:             "Home mode skips forced fallback",
			model:            "auto",
			enabled:          true,
			homeEnabled:      true,
			forcedFallback:   true,
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
			name:             "non-auto model passes through when disabled",
			model:            "gpt-existing(high)",
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
			name:             "forced fallback bypasses disabled auto routing and explicit model",
			model:            "gpt-existing",
			forcedFallback:   true,
			want:             "fallback-live",
			wantResolveCalls: 0,
		},
		{
			name:             "forced fallback bypasses enabled auto routing and explicit model",
			model:            "gpt-existing",
			enabled:          true,
			forcedFallback:   true,
			want:             "fallback-live",
			wantResolveCalls: 0,
		},
		{
			name:             "forced fallback preserves thinking suffix",
			model:            "gpt-existing(high)",
			forcedFallback:   true,
			want:             "fallback-live(high)",
			wantResolveCalls: 0,
		},
		{
			name:             "forced fallback without configured model fails closed",
			model:            "gpt-existing",
			forcedFallback:   true,
			emptyFallback:    true,
			want:             "",
			wantResolveCalls: 0,
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
			if tt.emptyFallback {
				cfg.AutoRouting.FallbackModel = ""
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

func TestGetContextWithCancelPreservesQuotaAndTenantValues(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	user := &tenancy.User{ID: "user-1", Email: "tenant@example.com"}
	requestContext := tenancy.WithUser(context.Background(), user)
	requestContext = autoroute.WithForcedFallback(requestContext)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestContext)

	handler := NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	executionContext, cancel := handler.GetContextWithCancel(nil, ginContext, context.Background())
	defer cancel()
	if !autoroute.ForcedFallback(executionContext) {
		t.Fatal("execution context lost forced fallback marker")
	}
	resolvedUser, ok := tenancy.UserFromContext(executionContext)
	if !ok || resolvedUser.ID != user.ID {
		t.Fatalf("execution context user = %#v, %t; want user-1", resolvedUser, ok)
	}
}

func TestGetContextWithCancelClassifiesHarnessFromUserAgent(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("User-Agent", "claude-cli/2.1.63")
	ginContext.Request = request

	handler := NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	executionContext, cancel := handler.GetContextWithCancel(nil, ginContext, context.Background())
	defer cancel()
	if got := constant.GetHarness(executionContext); got != constant.HarnessClaudeCode {
		t.Fatalf("GetHarness() = %q, want %q", got, constant.HarnessClaudeCode)
	}
}

type autoRouteCaptureExecutor struct {
	request coreexecutor.Request
	options coreexecutor.Options
	calls   int
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
	e.calls++
	e.request = req
	e.options = opts
}

func TestForcedFallbackUnavailableDoesNotExecuteOriginalModel(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("auto-routing-enabled=%t", enabled), func(t *testing.T) {
			const originalModel = "quota-original-live"
			executor := &autoRouteCaptureExecutor{}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			auth := &coreauth.Auth{
				ID:       "quota-original-auth",
				Provider: executor.Identifier(),
				Status:   coreauth.StatusActive,
			}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("manager.Register() error = %v", errRegister)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: originalModel}})
			t.Cleanup(func() {
				registry.GetGlobalRegistry().UnregisterClient(auth.ID)
			})

			handler := NewBaseAPIHandlers(&config.SDKConfig{AutoRouting: config.AutoRoutingConfig{
				Enabled:       enabled,
				FallbackModel: "quota-fallback-unavailable",
			}}, manager)
			ctx := autoroute.WithForcedFallback(context.Background())
			_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", originalModel, nil, "")
			if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway {
				t.Fatalf("execution error = %#v, want 502 for unavailable fallback", errMsg)
			}
			if executor.calls != 0 {
				t.Fatalf("original model executor calls = %d, want 0", executor.calls)
			}
		})
	}
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
