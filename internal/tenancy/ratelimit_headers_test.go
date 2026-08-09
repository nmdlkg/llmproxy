package tenancy

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseRateLimitHeaders(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	anthropicReset := now.Add(20 * time.Minute)
	httpReset := now.Add(45 * time.Second)
	tests := []struct {
		name      string
		headers   http.Header
		want      RateLimitHeaders
		wantValid bool
	}{
		{
			name: "anthropic unix reset",
			headers: testHeaders(map[string]string{
				"anthropic-ratelimit-unified-remaining": "80",
				"anthropic-ratelimit-unified-limit":     "100",
				"anthropic-ratelimit-unified-reset":     strconv.FormatInt(anthropicReset.Unix(), 10),
			}),
			want: RateLimitHeaders{
				Remaining: 80,
				Limit:     100,
				ResetAt:   anthropicReset,
				Source:    "anthropic-unified",
			},
			wantValid: true,
		},
		{
			name: "anthropic RFC3339 reset",
			headers: testHeaders(map[string]string{
				"Anthropic-Ratelimit-Unified-Remaining": "4",
				"Anthropic-Ratelimit-Unified-Limit":     "10",
				"Anthropic-Ratelimit-Unified-Reset":     anthropicReset.Format(time.RFC3339),
			}),
			want: RateLimitHeaders{
				Remaining: 4,
				Limit:     10,
				ResetAt:   anthropicReset,
				Source:    "anthropic-unified",
			},
			wantValid: true,
		},
		{
			name: "openai token duration preferred",
			headers: testHeaders(map[string]string{
				"x-ratelimit-remaining-tokens":   "900",
				"x-ratelimit-limit-tokens":       "1000",
				"x-ratelimit-reset-tokens":       "6m0s",
				"x-ratelimit-remaining-requests": "2",
				"x-ratelimit-limit-requests":     "10",
				"x-ratelimit-reset-requests":     "1s",
			}),
			want: RateLimitHeaders{
				Remaining: 900,
				Limit:     1000,
				ResetAt:   now.Add(6 * time.Minute),
				Source:    "openai-tokens",
			},
			wantValid: true,
		},
		{
			name: "openai request duration",
			headers: testHeaders(map[string]string{
				"x-ratelimit-remaining-requests": "3",
				"x-ratelimit-limit-requests":     "50",
				"x-ratelimit-reset-requests":     "1h2m3s",
			}),
			want: RateLimitHeaders{
				Remaining: 3,
				Limit:     50,
				ResetAt:   now.Add(time.Hour + 2*time.Minute + 3*time.Second),
				Source:    "openai-requests",
			},
			wantValid: true,
		},
		{
			name: "remaining without reset supports configured fallback",
			headers: http.Header{
				"x-ratelimit-remaining-tokens": []string{"12"},
				"x-ratelimit-limit-tokens":     []string{"20"},
			},
			want: RateLimitHeaders{
				Remaining: 12,
				Limit:     20,
				Source:    "openai-tokens",
			},
			wantValid: true,
		},
		{
			name: "retry after seconds",
			headers: testHeaders(map[string]string{
				"retry-after": "15",
			}),
			want: RateLimitHeaders{
				ResetAt: now.Add(15 * time.Second),
				Source:  "retry-after",
			},
			wantValid: true,
		},
		{
			name: "retry after fills missing provider reset",
			headers: testHeaders(map[string]string{
				"x-ratelimit-remaining-tokens": "0",
				"x-ratelimit-limit-tokens":     "100",
				"retry-after":                  "15",
			}),
			want: RateLimitHeaders{
				Remaining: 0,
				Limit:     100,
				ResetAt:   now.Add(15 * time.Second),
				Source:    "openai-tokens+retry-after",
			},
			wantValid: true,
		},
		{
			name: "retry after HTTP date",
			headers: testHeaders(map[string]string{
				"Retry-After": httpReset.Format(http.TimeFormat),
			}),
			want: RateLimitHeaders{
				ResetAt: httpReset,
				Source:  "retry-after",
			},
			wantValid: true,
		},
		{
			name:      "absent",
			headers:   nil,
			wantValid: false,
		},
		{
			name: "unknown",
			headers: testHeaders(map[string]string{
				"x-provider-quota": "12",
			}),
			wantValid: false,
		},
		{
			name: "garbage",
			headers: testHeaders(map[string]string{
				"x-ratelimit-remaining-tokens": "many",
				"x-ratelimit-limit-tokens":     "-1",
				"x-ratelimit-reset-tokens":     "soon",
				"retry-after":                  "later",
			}),
			wantValid: false,
		},
		{
			name:    "codex captured primary window",
			headers: capturedCodexHeaders(),
			want: RateLimitHeaders{
				Remaining:      7100,
				Limit:          10000,
				ResetAt:        time.Unix(1786498132, 0).UTC(),
				WindowDuration: 7 * 24 * time.Hour,
				Source:         "codex-primary",
			},
			wantValid: true,
		},
		{
			name: "codex empty reset at falls back to reset after",
			headers: http.Header{
				"x-CoDeX-pRiMaRy-UsEd-PeRcEnT":        []string{"42"},
				"x-CoDeX-pRiMaRy-ReSeT-aT":            []string{""},
				"x-CoDeX-pRiMaRy-ReSeT-aFtEr-SeCoNdS": []string{"90"},
				"x-CoDeX-pRiMaRy-WiNdOw-MiNuTeS":      []string{"60"},
			},
			want: RateLimitHeaders{
				Remaining:      5800,
				Limit:          10000,
				ResetAt:        now.Add(90 * time.Second),
				WindowDuration: time.Hour,
				Source:         "codex-primary",
			},
			wantValid: true,
		},
		{
			name: "codex invalid reset at falls back to reset after",
			headers: testHeaders(map[string]string{
				"X-Codex-Primary-Used-Percent":        "12.34",
				"X-Codex-Primary-Reset-At":            "not-a-timestamp",
				"X-Codex-Primary-Reset-After-Seconds": "120",
				"X-Codex-Primary-Window-Minutes":      "30",
			}),
			want: RateLimitHeaders{
				Remaining:      8766,
				Limit:          10000,
				ResetAt:        now.Add(2 * time.Minute),
				WindowDuration: 30 * time.Minute,
				Source:         "codex-primary",
			},
			wantValid: true,
		},
		{
			name: "codex shorter secondary window preferred",
			headers: testHeaders(map[string]string{
				"X-Codex-Primary-Used-Percent":          "25",
				"X-Codex-Primary-Reset-After-Seconds":   "600",
				"X-Codex-Primary-Window-Minutes":        "10080",
				"X-Codex-Secondary-Used-Percent":        "75",
				"X-Codex-Secondary-Reset-After-Seconds": "300",
				"X-Codex-Secondary-Window-Minutes":      "60",
			}),
			want: RateLimitHeaders{
				Remaining:      2500,
				Limit:          10000,
				ResetAt:        now.Add(5 * time.Minute),
				WindowDuration: time.Hour,
				Source:         "codex-secondary",
			},
			wantValid: true,
		},
		{
			name: "codex secondary used when primary is unusable",
			headers: testHeaders(map[string]string{
				"X-Codex-Primary-Used-Percent":          "25",
				"X-Codex-Primary-Reset-At":              "",
				"X-Codex-Primary-Reset-After-Seconds":   "0",
				"X-Codex-Primary-Window-Minutes":        "0",
				"X-Codex-Secondary-Used-Percent":        "40",
				"X-Codex-Secondary-Reset-After-Seconds": "300",
				"X-Codex-Secondary-Window-Minutes":      "60",
			}),
			want: RateLimitHeaders{
				Remaining:      6000,
				Limit:          10000,
				ResetAt:        now.Add(5 * time.Minute),
				WindowDuration: time.Hour,
				Source:         "codex-secondary",
			},
			wantValid: true,
		},
		{
			name: "codex zero percent",
			headers: testHeaders(map[string]string{
				"X-Codex-Primary-Used-Percent":        "0",
				"X-Codex-Primary-Reset-After-Seconds": "60",
				"X-Codex-Primary-Window-Minutes":      "5",
			}),
			want: RateLimitHeaders{
				Remaining:      10000,
				Limit:          10000,
				ResetAt:        now.Add(time.Minute),
				WindowDuration: 5 * time.Minute,
				Source:         "codex-primary",
			},
			wantValid: true,
		},
		{
			name: "codex one hundred percent",
			headers: testHeaders(map[string]string{
				"X-Codex-Primary-Used-Percent":        "100",
				"X-Codex-Primary-Reset-After-Seconds": "60",
				"X-Codex-Primary-Window-Minutes":      "5",
			}),
			want: RateLimitHeaders{
				Remaining:      0,
				Limit:          10000,
				ResetAt:        now.Add(time.Minute),
				WindowDuration: 5 * time.Minute,
				Source:         "codex-primary",
			},
			wantValid: true,
		},
		{
			name: "codex both windows unusable",
			headers: testHeaders(map[string]string{
				"X-Codex-Primary-Used-Percent":          "29",
				"X-Codex-Primary-Reset-At":              "",
				"X-Codex-Primary-Reset-After-Seconds":   "0",
				"X-Codex-Primary-Window-Minutes":        "0",
				"X-Codex-Secondary-Used-Percent":        "0",
				"X-Codex-Secondary-Reset-At":            "",
				"X-Codex-Secondary-Reset-After-Seconds": "0",
				"X-Codex-Secondary-Window-Minutes":      "0",
			}),
			wantValid: false,
		},
		{
			name: "codex garbage values",
			headers: testHeaders(map[string]string{
				"X-Codex-Primary-Used-Percent":          "many",
				"X-Codex-Primary-Reset-At":              "never",
				"X-Codex-Primary-Reset-After-Seconds":   "later",
				"X-Codex-Primary-Window-Minutes":        "weekly",
				"X-Codex-Secondary-Used-Percent":        "101",
				"X-Codex-Secondary-Reset-At":            "-1",
				"X-Codex-Secondary-Reset-After-Seconds": "-1",
				"X-Codex-Secondary-Window-Minutes":      "-1",
			}),
			wantValid: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, valid := ParseRateLimitHeaders(test.headers, now)
			if valid != test.wantValid {
				t.Fatalf("ParseRateLimitHeaders() valid = %v, want %v; result=%#v", valid, test.wantValid, got)
			}
			if !test.wantValid {
				return
			}
			if got.Remaining != test.want.Remaining ||
				got.Limit != test.want.Limit ||
				got.Source != test.want.Source ||
				got.WindowDuration != test.want.WindowDuration ||
				!got.ResetAt.Equal(test.want.ResetAt) {
				t.Fatalf("ParseRateLimitHeaders() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParsedCodexRateLimitDrivesEffectivePriority(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	parsed, ok := ParseRateLimitHeaders(testHeaders(map[string]string{
		"X-Codex-Primary-Used-Percent":        "29",
		"X-Codex-Primary-Reset-After-Seconds": "600",
		"X-Codex-Primary-Window-Minutes":      "60",
	}), now)
	if !ok {
		t.Fatal("ParseRateLimitHeaders() valid = false, want true")
	}
	window := QuotaWindow{
		WindowEnd:  parsed.ResetAt,
		UsedUnits:  parsed.Limit - parsed.Remaining,
		LimitUnits: parsed.Limit,
	}
	if got := float64(window.UsedUnits) / float64(window.LimitUnits); got != 0.29 {
		t.Fatalf("parsed usage ratio = %v, want 0.29", got)
	}
	if got := EffectivePriority(5, window, now, Balancing{
		UrgencyHorizon: 30 * time.Minute,
		HighWater:      0.30,
		UrgencyBonus:   2,
	}); got != 7 {
		t.Fatalf("EffectivePriority() below high water = %d, want 7", got)
	}
	if got := EffectivePriority(5, window, now, Balancing{
		UrgencyHorizon: 30 * time.Minute,
		HighWater:      0.29,
		UrgencyBonus:   2,
	}); got != 5 {
		t.Fatalf("EffectivePriority() at high water = %d, want 5", got)
	}
}

func capturedCodexHeaders() http.Header {
	return testHeaders(map[string]string{
		"X-Codex-Active-Limit":                         "premium",
		"X-Codex-Plan-Type":                            "prolite",
		"X-Codex-Primary-Used-Percent":                 "29",
		"X-Codex-Primary-Reset-At":                     "1786498132",
		"X-Codex-Primary-Reset-After-Seconds":          "487213",
		"X-Codex-Primary-Window-Minutes":               "10080",
		"X-Codex-Primary-Over-Secondary-Limit-Percent": "0",
		"X-Codex-Secondary-Used-Percent":               "0",
		"X-Codex-Secondary-Reset-At":                   "",
		"X-Codex-Secondary-Reset-After-Seconds":        "0",
		"X-Codex-Secondary-Window-Minutes":             "0",
		"X-Codex-Credits-Balance":                      "0",
		"X-Codex-Credits-Has-Credits":                  "False",
		"X-Codex-Credits-Unlimited":                    "False",
		"X-Codex-Bengalfox-Limit-Name":                 "GPT-5.3-Codex-Spark",
	})
}

func testHeaders(values map[string]string) http.Header {
	headers := make(http.Header, len(values))
	for name, value := range values {
		headers.Set(name, value)
	}
	return headers
}
