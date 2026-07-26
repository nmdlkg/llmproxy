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
				!got.ResetAt.Equal(test.want.ResetAt) {
				t.Fatalf("ParseRateLimitHeaders() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func testHeaders(values map[string]string) http.Header {
	headers := make(http.Header, len(values))
	for name, value := range values {
		headers.Set(name, value)
	}
	return headers
}
