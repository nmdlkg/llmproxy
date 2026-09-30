package optimizer

import (
	"net/http"
	"testing"
	"time"
)

func findObservation(t *testing.T, observations []Observation, scope, kind string) Observation {
	t.Helper()
	for _, observation := range observations {
		if observation.Scope == scope && observation.Kind == kind {
			return observation
		}
	}
	t.Fatalf("window %s/%s not found in %+v", scope, kind, observations)
	return Observation{}
}

func TestParseWindowsKeepsSimultaneousCodexWindows(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "12")
	headers.Set("X-Codex-Primary-Window-Minutes", "300")
	headers.Set("X-Codex-Primary-Reset-After-Seconds", "600")
	headers.Set("X-Codex-Secondary-Used-Percent", "97.5")
	headers.Set("X-Codex-Secondary-Window-Minutes", "10080")
	headers.Set("X-Codex-Secondary-Reset-At", "1788000000")
	headers.Set("X-Codex-Bengalfox-Primary-Used-Percent", "3")
	headers.Set("X-Codex-Bengalfox-Primary-Reset-After-Seconds", "60")

	observations := ParseWindows("Codex", headers, now)
	if len(observations) != 3 {
		t.Fatalf("observations = %d, want 3: %+v", len(observations), observations)
	}
	short := findObservation(t, observations, "default", "5h")
	if short.Remaining != 8800 || short.Capacity != 10000 || !short.ResetAt.Equal(now.Add(10*time.Minute)) || short.Resolution != 100 {
		t.Fatalf("short window = %+v", short)
	}
	weekly := findObservation(t, observations, "default", "7d")
	if weekly.Remaining != 250 || weekly.Duration != 7*24*time.Hour || weekly.Resolution != 10 {
		t.Fatalf("weekly window = %+v", weekly)
	}
	scoped := findObservation(t, observations, "bengalfox", "unknown-primary")
	if scoped.Remaining != 9700 {
		t.Fatalf("scoped window = %+v", scoped)
	}
}

func TestParseWindowsAnthropicUnifiedAndCountedFamilies(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.25")
	headers.Set("anthropic-ratelimit-unified-5h-reset", "1788000000")
	headers.Set("anthropic-ratelimit-unified-7d_opus-utilization", "1.2")
	headers.Set("anthropic-ratelimit-unified-7d_opus-reset", "1788500000")
	headers.Set("x-ratelimit-remaining-tokens", "900")
	headers.Set("x-ratelimit-limit-tokens", "1000")
	headers.Set("x-ratelimit-reset-tokens", "6s")
	headers.Set("x-ratelimit-remaining-requests", "not-a-number")
	headers.Set("x-ratelimit-reset-requests", "1s")

	observations := ParseWindows("claude", headers, now)
	if len(observations) != 3 {
		t.Fatalf("observations = %d: %+v", len(observations), observations)
	}
	if got := findObservation(t, observations, "default", "5h"); got.Remaining != 7500 || got.Duration != 5*time.Hour {
		t.Fatalf("5h = %+v", got)
	}
	if got := findObservation(t, observations, "opus", "7d"); got.Remaining != 0 {
		t.Fatalf("opus weekly utilization must clamp at 100%%: %+v", got)
	}
	if got := findObservation(t, observations, "tokens", "minute"); got.Remaining != 900 || got.Capacity != 1000 || got.Unit != UnitTokens {
		t.Fatalf("tokens = %+v", got)
	}
}

func TestParseWindowsRejectsIncompleteSignals(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "140")
	headers.Set("X-Codex-Primary-Reset-After-Seconds", "10")
	headers.Set("X-Codex-Secondary-Used-Percent", "10")
	headers.Set("Retry-After", "30")
	if observations := ParseWindows("codex", headers, time.Now()); len(observations) != 0 {
		t.Fatalf("expected no observations, got %+v", observations)
	}
}
