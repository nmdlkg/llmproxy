package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/autoroute"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// Fork seam: automatic model routing and forced quota fallback. Kept out of
// handlers_routing.go, handlers_execution.go, and handlers_stream.go so
// upstream execution edits do not conflict; the execution entry points call
// resolveForkModel (or resolveAutoRoutedModel for token counting) before
// applyModelRouter, and the original requested model is preserved by the
// callers for metadata.

// resolveAutoRoute is the testable resolver seam for classification-driven routing.
var resolveAutoRoute = autoroute.Resolve

// forcedQuotaFallback reports whether tenancy quota middleware forced the
// configured fallback model for this request.
func forcedQuotaFallback(ctx context.Context) bool {
	return autoroute.ForcedFallback(ctx)
}

// resolveForkModel resolves "auto" and forced-fallback model names and fails
// closed when a forced fallback model cannot be routed by any provider.
func (h *BaseAPIHandler) resolveForkModel(ctx context.Context, entryProtocol, modelName string, rawJSON []byte) (string, *interfaces.ErrorMessage) {
	modelName = h.resolveAutoRoutedModel(ctx, entryProtocol, modelName, rawJSON)
	if errFallback := forcedFallbackUnavailableError(ctx, modelName); errFallback != nil {
		return modelName, errFallback
	}
	return modelName, nil
}

// forcedFallbackRouteDecision discards a prepared plugin model route when the
// forced quota fallback applies, so the fallback model is always executed.
func forcedFallbackRouteDecision(ctx context.Context, decision modelRouteDecision) modelRouteDecision {
	if forcedQuotaFallback(ctx) {
		return modelRouteDecision{}
	}
	return decision
}

// singleErrorChan returns a closed channel carrying one error message.
func singleErrorChan(errMsg *interfaces.ErrorMessage) <-chan *interfaces.ErrorMessage {
	errChan := make(chan *interfaces.ErrorMessage, 1)
	errChan <- errMsg
	close(errChan)
	return errChan
}

func (h *BaseAPIHandler) resolveAutoRoutedModel(ctx context.Context, entryProtocol, modelName string, rawJSON []byte) string {
	if h == nil || h.Cfg == nil {
		return modelName
	}
	if h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return modelName
	}

	suffix := thinking.ParseSuffix(modelName)
	if autoroute.ForcedFallback(ctx) {
		fallbackModel := strings.TrimSpace(h.Cfg.AutoRouting.FallbackModel)
		if fallbackModel == "" {
			return ""
		}
		if suffix.HasSuffix {
			return fmt.Sprintf("%s(%s)", fallbackModel, suffix.RawSuffix)
		}
		return fallbackModel
	}
	if !h.Cfg.AutoRouting.Enabled {
		return modelName
	}
	if suffix.ModelName != "auto" && !strings.HasPrefix(suffix.ModelName, "auto:") {
		return modelName
	}

	prompt := autoroute.ExtractPrompt(entryProtocol, rawJSON, h.Cfg.AutoRouting.MaxPromptChars)
	resolvedModel := resolveAutoRoute(ctx, h.Cfg.AutoRouting, h.Cfg, prompt)
	if resolvedModel == "" {
		return modelName
	}
	if suffix.HasSuffix {
		return fmt.Sprintf("%s(%s)", resolvedModel, suffix.RawSuffix)
	}
	return resolvedModel
}

// forcedFallbackUnavailableError prevents a quota fallback request from
// silently retrying the original model when the configured fallback cannot be
// routed by any provider.
func forcedFallbackUnavailableError(ctx context.Context, modelName string) *interfaces.ErrorMessage {
	if !autoroute.ForcedFallback(ctx) {
		return nil
	}
	baseModel := thinking.ParseSuffix(modelName).ModelName
	if len(util.GetProviderName(baseModel)) > 0 {
		return nil
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      fmt.Errorf("quota fallback model %q is unavailable", modelName),
	}
}
