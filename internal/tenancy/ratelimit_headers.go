package tenancy

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitHeaders is the normalized quota-window information extracted from
// provider response headers.
type RateLimitHeaders struct {
	Remaining int64
	Limit     int64
	ResetAt   time.Time
	Source    string
}

// ParseRateLimitHeaders parses known Anthropic, OpenAI, and generic rate-limit
// response headers. It returns ok=false when no trustworthy value is present.
func ParseRateLimitHeaders(headers http.Header, now time.Time) (result RateLimitHeaders, ok bool) {
	if len(headers) == 0 {
		return RateLimitHeaders{}, false
	}
	now = now.UTC()

	if result, ok = parseRateLimitFamily(
		headers,
		"anthropic-ratelimit-unified-remaining",
		"anthropic-ratelimit-unified-limit",
		"anthropic-ratelimit-unified-reset",
		"anthropic-unified",
		func(value string, _ time.Time) (time.Time, bool) {
			return parseAbsoluteReset(value)
		},
		now,
	); ok {
		return applyRetryAfterReset(result, headers, now), true
	}

	if result, ok = parseRateLimitFamily(
		headers,
		"x-ratelimit-remaining-tokens",
		"x-ratelimit-limit-tokens",
		"x-ratelimit-reset-tokens",
		"openai-tokens",
		parseDurationReset,
		now,
	); ok {
		return applyRetryAfterReset(result, headers, now), true
	}

	if result, ok = parseRateLimitFamily(
		headers,
		"x-ratelimit-remaining-requests",
		"x-ratelimit-limit-requests",
		"x-ratelimit-reset-requests",
		"openai-requests",
		parseDurationReset,
		now,
	); ok {
		return applyRetryAfterReset(result, headers, now), true
	}

	retryAfter := strings.TrimSpace(headerValue(headers, "retry-after"))
	if retryAfter == "" {
		return RateLimitHeaders{}, false
	}
	resetAt, valid := parseRetryAfter(retryAfter, now)
	if !valid {
		return RateLimitHeaders{}, false
	}
	return RateLimitHeaders{
		ResetAt: resetAt,
		Source:  "retry-after",
	}, true
}

func applyRetryAfterReset(result RateLimitHeaders, headers http.Header, now time.Time) RateLimitHeaders {
	if !result.ResetAt.IsZero() {
		return result
	}
	retryAfter := strings.TrimSpace(headerValue(headers, "retry-after"))
	if retryAfter == "" {
		return result
	}
	resetAt, valid := parseRetryAfter(retryAfter, now)
	if !valid {
		return result
	}
	result.ResetAt = resetAt
	result.Source += "+retry-after"
	return result
}

type resetParser func(string, time.Time) (time.Time, bool)

func parseRateLimitFamily(
	headers http.Header,
	remainingHeader string,
	limitHeader string,
	resetHeader string,
	source string,
	parseReset resetParser,
	now time.Time,
) (RateLimitHeaders, bool) {
	remainingText := strings.TrimSpace(headerValue(headers, remainingHeader))
	limitText := strings.TrimSpace(headerValue(headers, limitHeader))
	resetText := strings.TrimSpace(headerValue(headers, resetHeader))
	if remainingText == "" && limitText == "" && resetText == "" {
		return RateLimitHeaders{}, false
	}

	var result RateLimitHeaders
	validValues := 0
	if value, valid := parseNonNegativeInt(remainingText); valid {
		result.Remaining = value
		validValues++
	}
	if value, valid := parseNonNegativeInt(limitText); valid {
		result.Limit = value
		validValues++
	}
	if value, valid := parseReset(resetText, now); valid {
		result.ResetAt = value
		validValues++
	}
	if validValues == 0 {
		return RateLimitHeaders{}, false
	}
	result.Source = source
	return result, true
}

func parseNonNegativeInt(value string) (int64, bool) {
	if strings.TrimSpace(value) == "" {
		return 0, false
	}
	parsed, errParse := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if errParse != nil || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

func parseAbsoluteReset(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if unixSeconds, errParse := strconv.ParseInt(value, 10, 64); errParse == nil && unixSeconds >= 0 {
		return time.Unix(unixSeconds, 0).UTC(), true
	}
	parsed, errParse := time.Parse(time.RFC3339, value)
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func parseDurationReset(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	duration, errParse := time.ParseDuration(value)
	if errParse != nil || duration < 0 {
		return time.Time{}, false
	}
	return now.Add(duration), true
}

func parseRetryAfter(value string, now time.Time) (time.Time, bool) {
	if seconds, valid := parseNonNegativeInt(value); valid {
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	parsed, errParse := http.ParseTime(value)
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) || len(values) == 0 {
			continue
		}
		return values[0]
	}
	return ""
}
