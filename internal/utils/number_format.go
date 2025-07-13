package utils

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatFloatWithCommas formats a float64 with thousands separators and two decimal digits.
func FormatFloatWithCommas(value float64) string {
	negative := value < 0
	if negative {
		value = -value
	}
	s := fmt.Sprintf("%.2f", value)
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	fracPart := ""
	if len(parts) > 1 {
		fracPart = parts[1]
	}
	n := len(intPart)
	for i := n - 3; i > 0; i -= 3 {
		intPart = intPart[:i] + "," + intPart[i:]
	}
	if negative {
		intPart = "-" + intPart
	}
	if fracPart != "" {
		return intPart + "." + fracPart
	}
	return intPart
}

// FormatSats formats an integer satoshi value with thousands separators and includes "sat" or "sats".
// If withPlus is true, a "+" sign is added for positive amounts.
func FormatSats(amount int64, withPlus ...bool) string {
	showPlus := false
	if len(withPlus) > 0 {
		showPlus = withPlus[0]
	}

	sign := ""
	if amount < 0 {
		sign = "-"
		amount = -amount
	} else if showPlus && amount > 0 {
		sign = "+"
	}

	s := strconv.FormatInt(amount, 10)
	n := len(s)
	for i := n - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}

	// Add "sat" or "sats" based on the absolute value
	unit := " sat"
	if amount != 1 {
		unit = " sats"
	}

	return sign + s + unit
}
