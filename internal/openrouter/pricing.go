package openrouter

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// KnownValue carries a benchmark value together with whether the upstream field
// was present and valid. Unknown values are represented as Value=0, Known=false;
// a measured zero is Value=0, Known=true.
type KnownValue struct {
	Value float64 `json:"value"`
	Known bool    `json:"known"`
}

// KnownNanoUSD carries an exact nano-USD amount together with whether the
// upstream field was present and valid. Unknown values are represented as
// NanoUSD=0, Known=false; an explicitly free price is NanoUSD=0, Known=true.
type KnownNanoUSD struct {
	NanoUSD int64 `json:"nano_usd"`
	Known   bool  `json:"known"`
}

// ModelPricing is the exact OpenRouter cost record for one model.
type ModelPricing struct {
	OpenRouterID                     string       `json:"openrouter_id"`
	ContextLength                    int64        `json:"context_length,omitempty"`
	SupportedParameters              []string     `json:"supported_parameters,omitempty"`
	PromptNanoUSDPerToken            KnownNanoUSD `json:"prompt_nano_usd_per_token"`
	CompletionNanoUSDPerToken        KnownNanoUSD `json:"completion_nano_usd_per_token"`
	InternalReasoningNanoUSDPerToken KnownNanoUSD `json:"internal_reasoning_nano_usd_per_token"`
	InputCacheReadNanoUSDPerToken    KnownNanoUSD `json:"input_cache_read_nano_usd_per_token"`
	InputCacheWriteNanoUSDPerToken   KnownNanoUSD `json:"input_cache_write_nano_usd_per_token"`
	RequestNanoUSD                   KnownNanoUSD `json:"request_nano_usd"`
	ImageNanoUSD                     KnownNanoUSD `json:"image_nano_usd"`
	WebSearchNanoUSD                 KnownNanoUSD `json:"web_search_nano_usd"`
}

func parsePricingResponse(data []byte) (map[string]ModelPricing, error) {
	rows, errRows := decodeDataRows(data)
	if errRows != nil {
		return nil, fmt.Errorf("decode pricing response: %w", errRows)
	}

	pricing := make(map[string]ModelPricing, len(rows))
	for _, row := range rows {
		var raw map[string]json.RawMessage
		if errRow := json.Unmarshal(row, &raw); errRow != nil {
			continue
		}
		id, ok := parseJSONString(raw["id"])
		if !ok || strings.TrimSpace(id) == "" {
			continue
		}

		record := ModelPricing{
			OpenRouterID:        id,
			ContextLength:       parseJSONInt64(raw["context_length"]),
			SupportedParameters: parseJSONStringSlice(raw["supported_parameters"]),
		}

		var rawPricing map[string]json.RawMessage
		if len(raw["pricing"]) > 0 {
			_ = json.Unmarshal(raw["pricing"], &rawPricing)
		}
		record.PromptNanoUSDPerToken = parseUSDString(rawPricing["prompt"])
		record.CompletionNanoUSDPerToken = parseUSDString(rawPricing["completion"])
		record.InternalReasoningNanoUSDPerToken = parseUSDString(rawPricing["internal_reasoning"])
		record.InputCacheReadNanoUSDPerToken = parseUSDString(rawPricing["input_cache_read"])
		record.InputCacheWriteNanoUSDPerToken = parseUSDString(rawPricing["input_cache_write"])
		record.RequestNanoUSD = parseUSDString(rawPricing["request"])
		record.ImageNanoUSD = parseUSDString(rawPricing["image"])
		record.WebSearchNanoUSD = parseUSDString(rawPricing["web_search"])
		pricing[id] = record
	}

	if len(pricing) == 0 {
		return nil, fmt.Errorf("pricing response contains no identifiable models")
	}
	return pricing, nil
}

func parseUSDString(raw json.RawMessage) KnownNanoUSD {
	value, ok := parseJSONString(raw)
	if !ok {
		return KnownNanoUSD{}
	}
	parsed, errParse := USDPerTokenToNanoUSD(value)
	if errParse != nil {
		return KnownNanoUSD{}
	}
	return KnownNanoUSD{NanoUSD: parsed, Known: true}
}

// USDPerTokenToNanoUSD converts an OpenRouter USD-per-token decimal string to
// nano-USD per token using exact base-10 arithmetic and round-to-nearest, with
// half values rounded upward. Missing and invalid strings return an error so
// callers cannot confuse them with an explicitly priced zero.
func USDPerTokenToNanoUSD(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, "eE") {
		return 0, fmt.Errorf("invalid non-negative USD decimal %q", raw)
	}
	if strings.HasPrefix(raw, "+") {
		raw = raw[1:]
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 2 || (parts[0] == "" && (len(parts) == 1 || parts[1] == "")) {
		return 0, fmt.Errorf("invalid USD decimal %q", raw)
	}
	whole := parts[0]
	if whole == "" {
		whole = "0"
	}
	if !allDecimalDigits(whole) {
		return 0, fmt.Errorf("invalid USD decimal %q", raw)
	}
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		if fraction != "" && !allDecimalDigits(fraction) {
			return 0, fmt.Errorf("invalid USD decimal %q", raw)
		}
	}

	const nanoDigits = 9
	if len(whole) > 10 || (len(whole) == 10 && whole > "9223372036") {
		return 0, fmt.Errorf("USD decimal %q overflows nano-USD int64", raw)
	}
	var total int64
	for _, digit := range whole {
		if total > math.MaxInt64/10 {
			return 0, fmt.Errorf("USD decimal %q overflows nano-USD int64", raw)
		}
		total = total*10 + int64(digit-'0')
	}
	if total > math.MaxInt64/1_000_000_000 {
		return 0, fmt.Errorf("USD decimal %q overflows nano-USD int64", raw)
	}
	total *= 1_000_000_000

	kept := fraction
	if len(kept) > nanoDigits {
		kept = kept[:nanoDigits]
	}
	var fractional int64
	for _, digit := range kept {
		fractional = fractional*10 + int64(digit-'0')
	}
	for index := len(kept); index < nanoDigits; index++ {
		fractional *= 10
	}
	if total > math.MaxInt64-fractional {
		return 0, fmt.Errorf("USD decimal %q overflows nano-USD int64", raw)
	}
	total += fractional
	if len(fraction) > nanoDigits && fraction[nanoDigits] >= '5' {
		if total == math.MaxInt64 {
			return 0, fmt.Errorf("USD decimal %q overflows nano-USD int64", raw)
		}
		total++
	}
	return total, nil
}

func allDecimalDigits(value string) bool {
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func parseJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func parseJSONInt64(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err == nil && value >= 0 {
		return value
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return 0
	}
	value, err := strconv.ParseInt(strings.TrimSpace(encoded), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func parseJSONStringSlice(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
