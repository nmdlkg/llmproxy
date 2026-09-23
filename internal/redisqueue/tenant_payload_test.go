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
			Provider:         "codex",
			Model:            "gpt-5.6-sol",
			APIKey:           "super-secret-key",
			SourceProvenance: coreusage.SourceProvenanceIdentifier,
			RequestedAt:      time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:           coreusage.Detail{InputTokens: 5, OutputTokens: 6, TotalTokens: 11},
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
			Provider:         "codex",
			Model:            "gpt-5.6-sol",
			APIKey:           secret,
			Source:           secret,
			SourceProvenance: coreusage.SourceProvenanceSecret,
			RequestedAt:      time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:           coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
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
			Provider:         "openai",
			Model:            "gpt-5.4",
			AuthType:         "apikey",
			APIKey:           "sk-client-key",
			Source:           upstreamKey,
			SourceProvenance: coreusage.SourceProvenanceSecret,
			RequestedAt:      time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:           coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
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

func TestSanitizeSourceKeepsIdentifiersAndFailsClosed(t *testing.T) {
	if got := sanitizeSource("user@example.com", coreusage.SourceProvenanceIdentifier); got != "user@example.com" {
		t.Errorf("sanitizeSource(email) = %q, want the email", got)
	}
	if got := sanitizeSource("secret", coreusage.SourceProvenanceSecret); got != "" {
		t.Errorf("sanitizeSource(key) = %q, want empty", got)
	}
	if got := sanitizeSource("secret", coreusage.SourceProvenanceUnknown); got != "" {
		t.Errorf("sanitizeSource(unknown) = %q, want empty", got)
	}
	if got := sanitizeSource("  ", coreusage.SourceProvenanceIdentifier); got != "" {
		t.Errorf("sanitizeSource(blank) = %q, want empty", got)
	}
}

// TestSanitizeSourceDropsUpstreamAPIKeySource covers the api-key credential
// case: resolveUsageSource returns the upstream provider key, which never
// matches the client key and must still be withheld.
func TestSanitizeSourceDropsUpstreamAPIKeySource(t *testing.T) {
	if got := sanitizeSource("sk-upstream-provider-key", coreusage.SourceProvenanceSecret); got != "" {
		t.Errorf("sanitizeSource(upstream key) = %q, want empty", got)
	}
	if got := sanitizeSource("sk-upstream-provider-key", coreusage.SourceProvenanceUnknown); got != "" {
		t.Errorf("sanitizeSource(upstream key, unknown) = %q, want empty", got)
	}
}

func TestUsageQueuePayloadWithholdsOAuthClassifiedAPIKey(t *testing.T) {
	withEnabledQueue(t, func() {
		const secret = "oauth-carried-api-key"
		(&usageQueuePlugin{}).HandleUsage(context.Background(), coreusage.Record{
			Provider: "openai", Model: "gpt-5.4", Source: secret,
			SourceProvenance: coreusage.SourceProvenanceSecret,
			RequestedAt:      time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:           coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})
		if raw := popSingleRawPayload(t); strings.Contains(string(raw), secret) {
			t.Fatalf("queue payload leaked OAuth-carried API key: %s", raw)
		}
	})
}

func TestUsageQueuePayloadKeepsVertexProjectID(t *testing.T) {
	withEnabledQueue(t, func() {
		const projectID = "vertex-project-123"
		(&usageQueuePlugin{}).HandleUsage(context.Background(), coreusage.Record{
			Provider: "vertex", Model: "gemini-2.5", Source: projectID,
			SourceProvenance: coreusage.SourceProvenanceIdentifier,
			RequestedAt:      time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:           coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})
		requireStringField(t, popSinglePayload(t), "source", projectID)
	})
}

func TestUsageQueuePayloadKeepsOAuthEmail(t *testing.T) {
	withEnabledQueue(t, func() {
		const email = "oauth@example.com"
		(&usageQueuePlugin{}).HandleUsage(context.Background(), coreusage.Record{
			Provider: "claude", Model: "claude-opus-5", Source: email,
			SourceProvenance: coreusage.SourceProvenanceIdentifier,
			RequestedAt:      time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
			Detail:           coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		})
		requireStringField(t, popSinglePayload(t), "source", email)
	})
}
