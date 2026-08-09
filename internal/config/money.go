package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	nanoUSDPerUSD              int64 = 1_000_000_000
	nanoUSDPerTokenPerUSDPer1M int64 = 1_000
)

// USDLimit is a human-configured USD amount stored internally as nano-USD.
// YAML and JSON use ordinary decimal USD values, while Go code sees an exact
// integer suitable for accumulation and quota comparison.
type USDLimit int64

// NanoUSD returns the exact internal nano-USD value.
func (value USDLimit) NanoUSD() int64 {
	return int64(value)
}

// UnmarshalYAML converts a decimal USD scalar to nano-USD without binary
// floating-point.
func (value *USDLimit) UnmarshalYAML(node *yaml.Node) error {
	parsed, errParse := parseScaledDecimal(node.Value, nanoUSDPerUSD)
	if errParse != nil {
		return fmt.Errorf("parse USD amount %q: %w", node.Value, errParse)
	}
	*value = USDLimit(parsed)
	return nil
}

// MarshalYAML renders the internal nano-USD value as a decimal USD number.
func (value USDLimit) MarshalYAML() (any, error) {
	return decimalYAMLNode(int64(value), nanoUSDPerUSD), nil
}

// UnmarshalJSON converts a decimal JSON number to nano-USD exactly.
func (value *USDLimit) UnmarshalJSON(data []byte) error {
	parsed, errParse := parseJSONScaledDecimal(data, nanoUSDPerUSD)
	if errParse != nil {
		return fmt.Errorf("parse USD amount: %w", errParse)
	}
	*value = USDLimit(parsed)
	return nil
}

// MarshalJSON renders the internal nano-USD value as a decimal USD number.
func (value USDLimit) MarshalJSON() ([]byte, error) {
	return []byte(formatScaledDecimal(int64(value), nanoUSDPerUSD)), nil
}

// USDPerMillionTokens is a human-configured $/1M-token price stored internally
// as nano-USD per token. The conversion is exact:
//
//	nano-USD/token = USD/1M tokens * 1,000
type USDPerMillionTokens int64

// NanoUSDPerToken returns the exact internal per-token price.
func (value USDPerMillionTokens) NanoUSDPerToken() int64 {
	return int64(value)
}

// UnmarshalYAML converts a decimal $/1M-token scalar to nano-USD per token.
func (value *USDPerMillionTokens) UnmarshalYAML(node *yaml.Node) error {
	parsed, errParse := parseScaledDecimal(node.Value, nanoUSDPerTokenPerUSDPer1M)
	if errParse != nil {
		return fmt.Errorf("parse $/1M token price %q: %w", node.Value, errParse)
	}
	*value = USDPerMillionTokens(parsed)
	return nil
}

// MarshalYAML renders the internal per-token price in conventional $/1M units.
func (value USDPerMillionTokens) MarshalYAML() (any, error) {
	return decimalYAMLNode(int64(value), nanoUSDPerTokenPerUSDPer1M), nil
}

// UnmarshalJSON converts a decimal JSON number in $/1M units exactly.
func (value *USDPerMillionTokens) UnmarshalJSON(data []byte) error {
	parsed, errParse := parseJSONScaledDecimal(data, nanoUSDPerTokenPerUSDPer1M)
	if errParse != nil {
		return fmt.Errorf("parse $/1M token price: %w", errParse)
	}
	*value = USDPerMillionTokens(parsed)
	return nil
}

// MarshalJSON renders the internal per-token price in $/1M units.
func (value USDPerMillionTokens) MarshalJSON() ([]byte, error) {
	return []byte(formatScaledDecimal(int64(value), nanoUSDPerTokenPerUSDPer1M)), nil
}

func parseJSONScaledDecimal(data []byte, scale int64) (int64, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return 0, fmt.Errorf("value must be a decimal number")
	}
	var number json.Number
	if errDecode := json.Unmarshal(data, &number); errDecode != nil {
		return 0, fmt.Errorf("decode decimal number: %w", errDecode)
	}
	return parseScaledDecimal(number.String(), scale)
}

// parseScaledDecimal multiplies a base-10 decimal by an integer power-of-ten
// scale and rounds to nearest, with half values rounded away from zero. It does
// not use float64, so every representable result is deterministic.
func parseScaledDecimal(raw string, scale int64) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || scale <= 0 {
		return 0, fmt.Errorf("invalid decimal")
	}

	negative := false
	switch raw[0] {
	case '-':
		negative = true
		raw = raw[1:]
	case '+':
		raw = raw[1:]
	}
	if raw == "" || strings.ContainsAny(raw, "eE") {
		return 0, fmt.Errorf("value must use plain decimal notation")
	}

	parts := strings.Split(raw, ".")
	if len(parts) > 2 || (parts[0] == "" && (len(parts) == 1 || parts[1] == "")) {
		return 0, fmt.Errorf("invalid decimal")
	}
	wholeText := parts[0]
	if wholeText == "" {
		wholeText = "0"
	}
	if !decimalDigits(wholeText) {
		return 0, fmt.Errorf("invalid decimal")
	}
	fractionText := ""
	if len(parts) == 2 {
		fractionText = parts[1]
		if fractionText != "" && !decimalDigits(fractionText) {
			return 0, fmt.Errorf("invalid decimal")
		}
	}

	digits := scaleDigits(scale)
	if digits < 0 {
		return 0, fmt.Errorf("scale must be a power of ten")
	}
	whole, errWhole := strconv.ParseUint(wholeText, 10, 64)
	if errWhole != nil || whole > uint64(math.MaxInt64)/uint64(scale) {
		return 0, fmt.Errorf("decimal overflows int64")
	}
	scaled := whole * uint64(scale)

	kept := fractionText
	if len(kept) > digits {
		kept = kept[:digits]
	}
	if kept != "" {
		fraction, errFraction := strconv.ParseUint(kept, 10, 64)
		if errFraction != nil {
			return 0, fmt.Errorf("invalid decimal")
		}
		for index := len(kept); index < digits; index++ {
			fraction *= 10
		}
		scaled += fraction
	}

	if len(fractionText) > digits && fractionText[digits] >= '5' {
		scaled++
	}
	positiveLimit := uint64(math.MaxInt64)
	negativeLimit := positiveLimit + 1
	if (!negative && scaled > positiveLimit) || (negative && scaled > negativeLimit) {
		return 0, fmt.Errorf("decimal overflows int64")
	}
	if negative {
		if scaled == negativeLimit {
			return math.MinInt64, nil
		}
		return -int64(scaled), nil
	}
	return int64(scaled), nil
}

func decimalDigits(value string) bool {
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func scaleDigits(scale int64) int {
	digits := 0
	for scale > 1 && scale%10 == 0 {
		scale /= 10
		digits++
	}
	if scale != 1 {
		return -1
	}
	return digits
}

func decimalYAMLNode(value, scale int64) *yaml.Node {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!float",
		Value: formatScaledDecimal(value, scale),
	}
}

func formatScaledDecimal(value, scale int64) string {
	negative := value < 0
	magnitude := uint64(value)
	if negative {
		magnitude = uint64(-(value + 1)) + 1
	}
	whole := magnitude / uint64(scale)
	fraction := magnitude % uint64(scale)
	digits := scaleDigits(scale)
	if fraction == 0 {
		if negative {
			return "-" + strconv.FormatUint(whole, 10) + ".0"
		}
		return strconv.FormatUint(whole, 10) + ".0"
	}
	fractionText := fmt.Sprintf("%0*d", digits, fraction)
	fractionText = strings.TrimRight(fractionText, "0")
	result := strconv.FormatUint(whole, 10) + "." + fractionText
	if negative {
		return "-" + result
	}
	return result
}
