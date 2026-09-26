package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/autoroute"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestForcedFallbackSkipsEveryModelRouterTarget(t *testing.T) {
	tests := []struct {
		name     string
		response pluginapi.ModelRouteResponse
	}{
		{
			name: "provider only",
			response: pluginapi.ModelRouteResponse{
				Handled: true, TargetKind: pluginapi.ModelRouteTargetProvider, Target: "quota-fallback-test",
			},
		},
		{
			name: "explicit provider model",
			response: pluginapi.ModelRouteResponse{
				Handled: true, TargetKind: pluginapi.ModelRouteTargetProvider, Target: "quota-fallback-test", TargetModel: "quota-original",
			},
		},
		{
			name: "plugin executor",
			response: pluginapi.ModelRouteResponse{
				Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: "quota-plugin-executor",
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, executor := newForcedFallbackRouterTestHandler(t, fmt.Sprintf("forced-router-%d", index))
			host := &handlerRouterOnlyTestHost{hasRouters: true}
			host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
				return test.response, true
			}
			handler.SetModelRouterHost(host)

			ctx := autoroute.WithForcedFallback(context.Background())
			_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "quota-original", []byte(`{"model":"quota-original"}`), "")
			if errMsg != nil {
				t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
			}
			if host.called {
				t.Fatal("model router was called during forced quota fallback")
			}
			if executor.calls != 1 || executor.request.Model != "quota-fallback" {
				t.Fatalf("executor calls/model = %d/%q, want 1/quota-fallback", executor.calls, executor.request.Model)
			}
		})
	}
}

func TestForcedFallbackIgnoresPreparedStreamModelRoute(t *testing.T) {
	handler, executor := newForcedFallbackRouterTestHandler(t, "forced-prepared-router")
	host := &handlerRouterOnlyTestHost{hasRouters: true}
	host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
		return pluginapi.ModelRouteResponse{
			Handled: true, TargetKind: pluginapi.ModelRouteTargetProvider,
			Target: "quota-fallback-test", TargetModel: "quota-original",
		}, true
	}
	handler.SetModelRouterHost(host)
	body := []byte(`{"model":"quota-original","stream":true}`)
	ctx, routed := handler.PrepareStreamModelRoute(context.Background(), "openai", "quota-original", body)
	if !routed {
		t.Fatal("PrepareStreamModelRoute() did not create the override fixture")
	}
	ctx = autoroute.WithForcedFallback(ctx)

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "quota-original", body, "")
	for range dataChan {
	}
	if errMsg := <-errChan; errMsg != nil {
		t.Fatalf("ExecuteStreamWithAuthManager() error = %+v", errMsg)
	}
	if executor.calls != 1 || executor.request.Model != "quota-fallback" {
		t.Fatalf("executor calls/model = %d/%q, want 1/quota-fallback", executor.calls, executor.request.Model)
	}
}

func newForcedFallbackRouterTestHandler(t *testing.T, authID string) (*BaseAPIHandler, *autoRouteCaptureExecutor) {
	t.Helper()
	executor := &autoRouteCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	credential := &coreauth.Auth{
		ID:       authID,
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
	}
	if _, errRegister := manager.Register(context.Background(), credential); errRegister != nil {
		t.Fatalf("manager.Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{
		{ID: "quota-original"},
		{ID: "quota-fallback"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(credential.ID)
	})
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{AutoRouting: internalconfig.AutoRoutingConfig{
		FallbackModel: "quota-fallback",
	}}, manager), executor
}
