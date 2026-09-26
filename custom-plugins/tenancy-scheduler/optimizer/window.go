// Package optimizer contains the pure quota observation, forecasting, valuation,
// and selection logic of the tenancy scheduler. It has no native ABI dependency.
package optimizer

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Unit is the native budget unit reported by a provider for a window.
type Unit string

const (
	// UnitPercentBP is a provider-normalized budget in basis points (10000 = 100%).
	UnitPercentBP Unit = "percent_bp"
	// UnitTokens is an absolute token budget.
	UnitTokens Unit = "tokens"
	// UnitRequests is an absolute request budget.
	UnitRequests Unit = "requests"
	// UnitOpaque is a budget whose unit is not documented by the provider.
	UnitOpaque Unit = "opaque"
)

const percentScale = 10000

// WindowKey identifies one quota window of one upstream account. The window
// instance is tracked separately through Window.Generation.
type WindowKey struct {
	Account  string `json:"account"`
	Provider string `json:"provider"`
	Scope    string `json:"scope"`
	Kind     string `json:"kind"`
}

func (k WindowKey) String() string {
	return k.Account + "|" + k.Provider + "|" + k.Scope + "|" + k.Kind
}

// Observation is one window snapshot parsed from an upstream response.
type Observation struct {
	Provider string
	Scope    string
	Kind     string
	Unit     Unit
	// Capacity is the window size in Unit, or zero when unknown.
	Capacity float64
	// Remaining is the unconsumed amount in Unit.
	Remaining float64
	// ResetAt is the replenishment time, or zero when unknown.
	ResetAt time.Time
	// Duration is the window length, or zero when unknown.
	Duration time.Duration
	// Resolution is the reporting granularity in Unit (rounding uncertainty).
	Resolution float64
	Source     string
}

var (
	codexUsedPercentPattern   = regexp.MustCompile(`^x-codex-(?:(.+)-)?(primary|secondary)-used-percent$`)
	anthropicUtilPattern      = regexp.MustCompile(`^anthropic-ratelimit-unified-([a-z0-9_]+)-utilization$`)
	anthropicWindowKindPrefix = regexp.MustCompile(`^([0-9]+[hd])(?:_(.+))?$`)
)

// ParseWindows extracts every quota window carried by response headers. Unlike
// a single-window parser it keeps short, weekly, and scoped windows together,
// because they are simultaneous constraints on the same account.
func ParseWindows(provider string, headers http.Header, now time.Time) []Observation {
	if len(headers) == 0 {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	lower := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		lower[strings.ToLower(key)] = strings.TrimSpace(values[0])
	}
	var out []Observation
	out = append(out, parseCodexWindows(provider, lower, now)...)
	out = append(out, parseAnthropicUnifiedWindows(provider, lower)...)
	out = append(out, parseCountedWindows(provider, lower, now)...)
	return out
}

func parseCodexWindows(provider string, headers map[string]string, now time.Time) []Observation {
	var out []Observation
	for name, value := range headers {
		match := codexUsedPercentPattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		limitName, slot := match[1], match[2]
		prefix := strings.TrimSuffix(name, "used-percent")
		percent, errParse := strconv.ParseFloat(value, 64)
		if errParse != nil || math.IsNaN(percent) || percent < 0 || percent > 100 {
			continue
		}
		resetAt, okReset := parseAbsoluteTime(headers[prefix+"reset-at"])
		if !okReset {
			resetAt, okReset = parseSecondsFrom(headers[prefix+"reset-after-seconds"], now)
		}
		if !okReset {
			continue
		}
		duration := parseMinutes(headers[prefix+"window-minutes"])
		kind := durationKind(duration)
		if kind == "" {
			// Primary/secondary names do not identify a duration.
			kind = "unknown-" + slot
		}
		scope := "default"
		if limitName != "" {
			scope = strings.TrimPrefix(limitName, "additional-")
		}
		used := math.Round(percent * percentScale / 100)
		out = append(out, Observation{
			Provider:   provider,
			Scope:      scope,
			Kind:       kind,
			Unit:       UnitPercentBP,
			Capacity:   percentScale,
			Remaining:  percentScale - used,
			ResetAt:    resetAt,
			Duration:   duration,
			Resolution: percentResolution(value),
			Source:     "codex-" + slot,
		})
	}
	return out
}

func parseAnthropicUnifiedWindows(provider string, headers map[string]string) []Observation {
	var out []Observation
	for name, value := range headers {
		match := anthropicUtilPattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		utilization, errParse := strconv.ParseFloat(value, 64)
		if errParse != nil || math.IsNaN(utilization) || utilization < 0 {
			continue
		}
		utilization = math.Min(utilization, 1)
		resetAt, okReset := parseAbsoluteTime(headers["anthropic-ratelimit-unified-"+match[1]+"-reset"])
		if !okReset {
			continue
		}
		kind, scope := match[1], "default"
		var duration time.Duration
		if parts := anthropicWindowKindPrefix.FindStringSubmatch(match[1]); parts != nil {
			kind = parts[1]
			duration = parseKindDuration(parts[1])
			if parts[2] != "" {
				scope = parts[2]
			}
		}
		used := math.Round(utilization * percentScale)
		out = append(out, Observation{
			Provider:   provider,
			Scope:      scope,
			Kind:       kind,
			Unit:       UnitPercentBP,
			Capacity:   percentScale,
			Remaining:  percentScale - used,
			ResetAt:    resetAt,
			Duration:   duration,
			Resolution: 1,
			Source:     "anthropic-unified-" + match[1],
		})
	}
	return out
}

type countedFamily struct {
	remaining, limit, reset string
	unit                    Unit
	scope, kind, source     string
	parseReset              func(string, time.Time) (time.Time, bool)
}

var countedFamilies = []countedFamily{
	{"anthropic-ratelimit-unified-remaining", "anthropic-ratelimit-unified-limit", "anthropic-ratelimit-unified-reset", UnitOpaque, "default", "unified", "anthropic-unified", absoluteReset},
	{"anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-limit", "anthropic-ratelimit-tokens-reset", UnitTokens, "tokens", "minute", "anthropic-tokens", absoluteReset},
	{"anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-requests-limit", "anthropic-ratelimit-requests-reset", UnitRequests, "requests", "minute", "anthropic-requests", absoluteReset},
	{"x-ratelimit-remaining-tokens", "x-ratelimit-limit-tokens", "x-ratelimit-reset-tokens", UnitTokens, "tokens", "minute", "openai-tokens", durationReset},
	{"x-ratelimit-remaining-requests", "x-ratelimit-limit-requests", "x-ratelimit-reset-requests", UnitRequests, "requests", "minute", "openai-requests", durationReset},
}

func parseCountedWindows(provider string, headers map[string]string, now time.Time) []Observation {
	var out []Observation
	for _, family := range countedFamilies {
		remaining, okRemaining := parseNonNegative(headers[family.remaining])
		limit, okLimit := parseNonNegative(headers[family.limit])
		resetAt, okReset := family.parseReset(headers[family.reset], now)
		// Remaining is required: a reset or limit alone carries no budget.
		if !okRemaining || !okReset {
			continue
		}
		observation := Observation{
			Provider:   provider,
			Scope:      family.scope,
			Kind:       family.kind,
			Unit:       family.unit,
			Remaining:  remaining,
			ResetAt:    resetAt,
			Resolution: 1,
			Source:     family.source,
		}
		if okLimit && limit >= remaining {
			observation.Capacity = limit
		}
		out = append(out, observation)
	}
	return out
}

func absoluteReset(value string, _ time.Time) (time.Time, bool) { return parseAbsoluteTime(value) }

func durationReset(value string, now time.Time) (time.Time, bool) {
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

func parseAbsoluteTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if seconds, errParse := strconv.ParseInt(value, 10, 64); errParse == nil && seconds >= 0 {
		return time.Unix(seconds, 0).UTC(), true
	}
	parsed, errParse := time.Parse(time.RFC3339, value)
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func parseSecondsFrom(value string, now time.Time) (time.Time, bool) {
	seconds, ok := parseNonNegative(value)
	if !ok || seconds > float64(math.MaxInt32)*1000 {
		return time.Time{}, false
	}
	return now.Add(time.Duration(seconds) * time.Second), true
}

func parseMinutes(value string) time.Duration {
	minutes, ok := parseNonNegative(value)
	if !ok || minutes <= 0 || minutes > 60*24*366 {
		return 0
	}
	return time.Duration(minutes) * time.Minute
}

func parseNonNegative(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	parsed, errParse := strconv.ParseInt(value, 10, 64)
	if errParse != nil || parsed < 0 {
		return 0, false
	}
	return float64(parsed), true
}

func percentResolution(value string) float64 {
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		return percentScale / 100
	}
	decimals := len(value) - dot - 1
	return math.Max(1, percentScale/100/math.Pow(10, float64(decimals)))
}

// durationKind renders a window length as a stable kind such as "5h" or "7d".
func durationKind(duration time.Duration) string {
	if duration <= 0 {
		return ""
	}
	minutes := int64(duration / time.Minute)
	switch {
	case minutes%(24*60) == 0:
		return fmt.Sprintf("%dd", minutes/(24*60))
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func parseKindDuration(kind string) time.Duration {
	if len(kind) < 2 {
		return 0
	}
	count, errParse := strconv.Atoi(kind[:len(kind)-1])
	if errParse != nil || count <= 0 {
		return 0
	}
	switch kind[len(kind)-1] {
	case 'h':
		return time.Duration(count) * time.Hour
	case 'd':
		return time.Duration(count) * 24 * time.Hour
	}
	return 0
}
