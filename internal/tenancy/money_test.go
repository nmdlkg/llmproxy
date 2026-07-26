package tenancy

import "testing"

func TestFormatNanoUSD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		nanoUSD int64
		want    string
	}{
		{nanoUSD: 0, want: "$0.00"},
		{nanoUSD: 75, want: "$0.000000075"},
		{nanoUSD: 2_500_000_000, want: "$2.50"},
		{nanoUSD: 9_223_372_036_854_775_807, want: "$9223372036.854775807"},
		{nanoUSD: -1_250_000_000, want: "-$1.25"},
	}
	for _, test := range tests {
		if got := FormatNanoUSD(test.nanoUSD); got != test.want {
			t.Errorf("FormatNanoUSD(%d) = %q, want %q", test.nanoUSD, got, test.want)
		}
	}
}
