package constant

import (
	"context"
	"testing"
)

func TestClassifyHarness(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		want      string
	}{
		{name: "claude code", userAgent: "claude-cli/2.1.63 (external, cli)", want: HarnessClaudeCode},
		{name: "opencode", userAgent: "opencode/1.18.18", want: HarnessOpenCode},
		{name: "codex rust cli", userAgent: "codex_cli_rs/0.5.0", want: HarnessCodex},
		{name: "codex tui", userAgent: "codex-tui/1.2.3", want: HarnessCodex},
		{name: "codex desktop", userAgent: "Codex Desktop/1.0", want: HarnessCodex},
		{name: "mixed case", userAgent: "Claude-CLI/9.9", want: HarnessClaudeCode},
		{name: "empty", userAgent: "", want: HarnessUnknown},
		{name: "whitespace", userAgent: "   ", want: HarnessUnknown},
		{name: "generic client", userAgent: "curl/8.5.0", want: HarnessUnknown},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ClassifyHarness(testCase.userAgent); got != testCase.want {
				t.Errorf("ClassifyHarness(%q) = %q, want %q", testCase.userAgent, got, testCase.want)
			}
		})
	}
}

func TestHarnessContextRoundTrip(t *testing.T) {
	ctx := WithHarness(context.Background(), HarnessCodex)
	if got := GetHarness(ctx); got != HarnessCodex {
		t.Errorf("GetHarness() = %q, want %q", got, HarnessCodex)
	}
}

func TestGetHarnessDefaultsToUnknown(t *testing.T) {
	if got := GetHarness(context.Background()); got != HarnessUnknown {
		t.Errorf("GetHarness(bare) = %q, want %q", got, HarnessUnknown)
	}
	//lint:ignore SA1012 verifying the nil-context guard is part of the contract.
	if got := GetHarness(nil); got != HarnessUnknown {
		t.Errorf("GetHarness(nil) = %q, want %q", got, HarnessUnknown)
	}
}

func TestWithHarnessNormalizesEmpty(t *testing.T) {
	ctx := WithHarness(context.Background(), "  ")
	if got := GetHarness(ctx); got != HarnessUnknown {
		t.Errorf("GetHarness() = %q, want %q", got, HarnessUnknown)
	}
}

func TestTenantContextRoundTrip(t *testing.T) {
	ctx := WithTenant(context.Background(), "user-1", "pro")
	tenant, ok := TenantFromContext(ctx)
	if !ok {
		t.Fatal("TenantFromContext() ok = false, want true")
	}
	if tenant.ID != "user-1" || tenant.Tier != "pro" {
		t.Errorf("TenantFromContext() = %+v, want {user-1 pro}", tenant)
	}
}

func TestWithTenantIgnoresEmptyID(t *testing.T) {
	ctx := WithTenant(context.Background(), "   ", "pro")
	if _, ok := TenantFromContext(ctx); ok {
		t.Error("TenantFromContext() ok = true, want false for empty id")
	}
}
