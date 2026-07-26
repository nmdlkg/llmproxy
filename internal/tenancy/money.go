package tenancy

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatNanoUSD renders an exact nano-USD amount for API and UI responses.
// It keeps at least cents for ordinary values and preserves additional
// precision when the amount is below one cent.
func FormatNanoUSD(nanoUSD int64) string {
	negative := nanoUSD < 0
	magnitude := uint64(nanoUSD)
	if negative {
		magnitude = uint64(-(nanoUSD + 1)) + 1
	}

	const nanoPerUSD uint64 = 1_000_000_000
	whole := magnitude / nanoPerUSD
	fraction := magnitude % nanoPerUSD
	fractionText := fmt.Sprintf("%09d", fraction)
	fractionText = strings.TrimRight(fractionText, "0")
	if len(fractionText) < 2 {
		fractionText += strings.Repeat("0", 2-len(fractionText))
	}

	result := "$" + strconv.FormatUint(whole, 10) + "." + fractionText
	if negative {
		return "-$" + strings.TrimPrefix(result, "$")
	}
	return result
}
