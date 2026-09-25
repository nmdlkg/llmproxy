package handlers

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/autoroute"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
)

// Fork seam: request-scoped tenancy values for executor contexts. Most
// handlers build their executor context from context.Background(), so values
// stored only on the gin request context would not reach the scheduler, quota
// fallback, or usage plugins. GetContextWithCancel calls these helpers.

// withForkRequestValues copies the immutable tenant policy values (forced quota
// fallback marker and resolved tenant user) from the inbound request context.
func withForkRequestValues(parentCtx, requestCtx context.Context) context.Context {
	if requestCtx == nil {
		return parentCtx
	}
	if autoroute.ForcedFallback(requestCtx) {
		parentCtx = autoroute.WithForcedFallback(parentCtx)
	}
	if user, ok := tenancy.UserFromContext(requestCtx); ok {
		parentCtx = tenancy.WithUser(parentCtx, user)
	}
	return parentCtx
}

// withForkClientAttribution classifies the originating client harness for usage
// attribution.
func withForkClientAttribution(ctx context.Context, c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return ctx
	}
	return constant.WithHarness(ctx, constant.ClassifyHarness(c.Request.UserAgent()))
}
