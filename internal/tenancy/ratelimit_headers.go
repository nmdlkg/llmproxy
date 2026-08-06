package tenancy

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Codex reports ratios rather than token counts, so preserve two decimal
// places as basis-point units for the downstream used/limit calculation.
const codexPercentScale int64 = 10000

// RateLimitHeaders is the normalized quota-window information extracted from
// provider response headers.
type RateLimitHeaders struct {
	Remaining      int64
	Limit          int64
	ResetAt        time.Time
	WindowDuration time.Duration
	Source         string
}

// ParseRateLimitHeaders parses known Anthropic, OpenAI, Codex, and generic
// rate-limit response headers. It returns ok=false when no trustworthy value
// is present.
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

	if result, ok = parseCodexRateLimitHeaders(headers, now); ok {
		return result, true
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

func parseCodexRateLimitHeaders(headers http.Header, now time.Time) (RateLimitHeaders, bool) {
	primary, primaryOK := parseCodexRateLimitWindow(headers, "primary", now)
	secondary, secondaryOK := parseCodexRateLimitWindow(headers, "secondary", now)
	switch {
	case primaryOK && secondaryOK:
		// Capacity in the shorter window expires sooner, so it is the more
		// actionable window for reset-aware balancing. Primary wins ties.
		if secondary.WindowDuration < primary.WindowDuration {
			return secondary, true
		}
		return primary, true
	case primaryOK:
		return primary, true
	case secondaryOK:
		return secondary, true
	default:
		return RateLimitHeaders{}, false
	}
}

func parseCodexRateLimitWindow(headers http.Header, windowName string, now time.Time) (RateLimitHeaders, bool) {
	prefix := "x-codex-" + windowName + "-"
	remaining, limit, validPercent := parseCodexUsedPercent(headerValue(headers, prefix+"used-percent"))
	windowDuration, validWindow := parsePositiveMinutes(headerValue(headers, prefix+"window-minutes"))

	var resetAt time.Time
	validReset := false
	absoluteReset := strings.TrimSpace(headerValue(headers, prefix+"reset-at"))
	if absoluteReset != "" {
		resetAt, validReset = parseAbsoluteReset(absoluteReset)
	}
	if !validReset {
		resetAt, validReset = parseNonNegativeSecondsReset(
			headerValue(headers, prefix+"reset-after-seconds"),
			now,
		)
	}
	if !validPercent || !validWindow || !validReset {
		return RateLimitHeaders{}, false
	}

	return RateLimitHeaders{
		Remaining:      remaining,
		Limit:          limit,
		ResetAt:        resetAt,
		WindowDuration: windowDuration,
		Source:         "codex-" + windowName,
	}, true
}

func parseCodexUsedPercent(value string) (remaining int64, limit int64, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, false
	}
	percent, errParse := strconv.ParseFloat(value, 64)
	if errParse != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
		return 0, 0, false
	}
	used := int64(math.Round(percent * float64(codexPercentScale) / 100))
	return codexPercentScale - used, codexPercentScale, true
}

func parsePositiveMinutes(value string) (time.Duration, bool) {
	minutes, valid := parseNonNegativeInt(value)
	maxDuration := time.Duration(1<<63 - 1)
	if !valid || minutes == 0 || minutes > int64(maxDuration/time.Minute) {
		return 0, false
	}
	return time.Duration(minutes) * time.Minute, true
}

func parseNonNegativeSecondsReset(value string, now time.Time) (time.Time, bool) {
	seconds, valid := parseNonNegativeInt(value)
	maxDuration := time.Duration(1<<63 - 1)
	if !valid || seconds > int64(maxDuration/time.Second) {
		return time.Time{}, false
	}
	return now.Add(time.Duration(seconds) * time.Second), true
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
