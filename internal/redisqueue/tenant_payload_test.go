package redisqueue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestUsageQueuePayloadCarriesTenantAttribution(t *testing.T) {
	withEnabledQueue(t, func() {
		ctx := constant.WithTenant(context.Background(), "user-42", "pro")
		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(ctx, coreusage.Record{
			Provider:    "codex",
			Model:       "gpt-5.6-sol",
			APIKey:      "super-secret-key",
			RequestedAt: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:      coreusage.Detail{InputTokens: 5, OutputTokens: 6, TotalTokens: 11},
		})

		payload := popSinglePayload(t)
		requireStringField(t, payload, "user_id", "user-42")
		requireStringField(t, payload, "user_tier", "pro")
	})
}

func TestUsageQueuePayloadNeverSerializesAPIKey(t *testing.T) {
	withEnabledQueue(t, func() {
		const secret = "super-secret-key"
		plugin := &usageQueuePlugin{}
		// Source deliberately mirrors the API key: resolveUsageSource can fall
		// back to raw key material, which must not reach the queue.
		plugin.HandleUsage(context.Background(), coreusage.Record{
			Provider:    "codex",
			Model:       "gpt-5.6-sol",
			APIKey:      secret,
			Source:      secret,
			RequestedAt: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})

		raw := popSingleRawPayload(t)
		if strings.Contains(string(raw), secret) {
			t.Fatalf("queue payload leaked API key material: %s", raw)
		}
	})
}

func TestUsageQueuePayloadWithholdsUpstreamAPIKeySource(t *testing.T) {
	withEnabledQueue(t, func() {
		const upstreamKey = "sk-upstream-provider-key"
		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(context.Background(), coreusage.Record{
			Provider:    "openai",
			Model:       "gpt-5.4",
			AuthType:    "apikey",
			APIKey:      "sk-client-key",
			Source:      upstreamKey,
			RequestedAt: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})

		raw := popSingleRawPayload(t)
		if strings.Contains(string(raw), upstreamKey) {
			t.Fatalf("queue payload leaked the upstream API key: %s", raw)
		}
	})
}

func TestUsageQueuePayloadPublishesUnattributedRequests(t *testing.T) {
	withEnabledQueue(t, func() {
		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(context.Background(), coreusage.Record{
			Provider:    "codex",
			Model:       "gpt-5.6-sol",
			RequestedAt: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:      coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})

		payload := popSinglePayload(t)
		requireMissingField(t, payload, "user_id")
		requireMissingField(t, payload, "user_tier")
		requireStringField(t, payload, "model", "gpt-5.6-sol")
	})
}

// popSingleRawPayload returns the queued payload bytes so tests can assert on
// the exact serialized form rather than decoded fields.
func popSingleRawPayload(t *testing.T) []byte {
	t.Helper()
	items := PopOldest(10)
	if len(items) != 1 {
		t.Fatalf("PopOldest() items = %d, want 1", len(items))
	}
	return items[0]
}

func TestSanitizeSourceKeepsNonKeySources(t *testing.T) {
	if got := sanitizeSource("user@example.com", "secret", "oauth"); got != "user@example.com" {
		t.Errorf("sanitizeSource(email) = %q, want the email", got)
	}
	if got := sanitizeSource("secret", "secret", "oauth"); got != "" {
		t.Errorf("sanitizeSource(key) = %q, want empty", got)
	}
	if got := sanitizeSource("  ", "secret", "oauth"); got != "" {
		t.Errorf("sanitizeSource(blank) = %q, want empty", got)
	}
}

// TestSanitizeSourceDropsUpstreamAPIKeySource covers the api-key credential
// case: resolveUsageSource returns the upstream provider key, which never
// matches the client key and must still be withheld.
func TestSanitizeSourceDropsUpstreamAPIKeySource(t *testing.T) {
	if got := sanitizeSource("sk-upstream-provider-key", "sk-client-key", "apikey"); got != "" {
		t.Errorf("sanitizeSource(upstream key) = %q, want empty", got)
	}
	if got := sanitizeSource("sk-upstream-provider-key", "", "APIKEY"); got != "" {
		t.Errorf("sanitizeSource(upstream key, mixed case) = %q, want empty", got)
	}
}
