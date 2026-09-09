package constant

import (
	"context"
	"strings"
)

// Harness identifies the client tool family that originated a request. The
// values intentionally mirror the tool.family resource attribute that harness
// clients already declare to the OTel collector, so proxy-side and client-side
// telemetry share one vocabulary.
const (
	HarnessCodex      = "codex"
	HarnessClaudeCode = "claude-code"
	HarnessOpenCode   = "opencode"
	HarnessUnknown    = "unknown"
)

type harnessKey struct{}

// ClassifyHarness maps a client User-Agent to a bounded harness label. Unknown
// or absent agents collapse to HarnessUnknown so metric cardinality stays closed
// regardless of what a client sends.
func ClassifyHarness(userAgent string) string {
	agent := strings.ToLower(strings.TrimSpace(userAgent))
	if agent == "" {
		return HarnessUnknown
	}
	switch {
	case strings.HasPrefix(agent, "claude-cli"):
		return HarnessClaudeCode
	case strings.HasPrefix(agent, "opencode"):
		return HarnessOpenCode
	case strings.HasPrefix(agent, "codex"):
		return HarnessCodex
	}
	return HarnessUnknown
}

// WithHarness stores the resolved harness label on the context.
func WithHarness(ctx context.Context, harness string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	harness = strings.TrimSpace(harness)
	if harness == "" {
		harness = HarnessUnknown
	}
	return context.WithValue(ctx, harnessKey{}, harness)
}

// GetHarness returns the harness label recorded for the request, or
// HarnessUnknown when the request was not classified.
func GetHarness(ctx context.Context) string {
	if ctx == nil {
		return HarnessUnknown
	}
	if harness, ok := ctx.Value(harnessKey{}).(string); ok && harness != "" {
		return harness
	}
	return HarnessUnknown
}

type tenantKey struct{}

// Tenant carries the minimal tenant identity that non-tenancy packages need for
// telemetry attribution. It lives here so low-level sinks can read it without
// importing internal/tenancy, which would create an import cycle.
type Tenant struct {
	// ID is the stable tenancy user identifier.
	ID string
	// Tier is the tenancy quota tier of the user.
	Tier string
}

// WithTenant stores the tenant identity used for telemetry attribution.
func WithTenant(ctx context.Context, id string, tier string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, tenantKey{}, Tenant{ID: id, Tier: strings.TrimSpace(tier)})
}

// TenantFromContext returns the tenant identity recorded for telemetry.
func TenantFromContext(ctx context.Context) (Tenant, bool) {
	if ctx == nil {
		return Tenant{}, false
	}
	tenant, ok := ctx.Value(tenantKey{}).(Tenant)
	return tenant, ok && tenant.ID != ""
}
