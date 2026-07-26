package openrouter

import (
	"encoding/json"
	"testing"
)

func TestUSDPerTokenToNanoUSD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want int64
	}{
		{raw: "0.0000025", want: 2500},
		{raw: "0.000000075", want: 75},
		{raw: "0", want: 0},
		{raw: "0.0000000005", want: 1},
		{raw: "0.0000000004", want: 0},
	}
	for _, test := range tests {
		test := test
		t.Run(test.raw, func(t *testing.T) {
			t.Parallel()
			got, errConvert := USDPerTokenToNanoUSD(test.raw)
			if errConvert != nil {
				t.Fatalf("USDPerTokenToNanoUSD(%q) error = %v", test.raw, errConvert)
			}
			if got != test.want {
				t.Fatalf("USDPerTokenToNanoUSD(%q) = %d, want %d", test.raw, got, test.want)
			}
		})
	}
}

func TestParseUSDStringPreservesUnknownAndExplicitZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
		want KnownNanoUSD
	}{
		{name: "missing", want: KnownNanoUSD{}},
		{name: "null", raw: json.RawMessage(`null`), want: KnownNanoUSD{}},
		{name: "unparseable", raw: json.RawMessage(`"not-a-price"`), want: KnownNanoUSD{}},
		{name: "negative", raw: json.RawMessage(`"-0.1"`), want: KnownNanoUSD{}},
		{name: "explicit zero", raw: json.RawMessage(`"0"`), want: KnownNanoUSD{Known: true}},
		{name: "cheapest realistic", raw: json.RawMessage(`"0.000000075"`), want: KnownNanoUSD{NanoUSD: 75, Known: true}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := parseUSDString(test.raw); got != test.want {
				t.Fatalf("parseUSDString(%s) = %+v, want %+v", test.raw, got, test.want)
			}
		})
	}
}
