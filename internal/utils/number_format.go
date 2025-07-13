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

// FormatSats formats an integer satoshi value with thousands separators.
func FormatSats(amount int64) string {
	negative := amount < 0
	if negative {
		amount = -amount
	}
	s := strconv.FormatInt(amount, 10)
	n := len(s)
	for i := n - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if negative {
		s = "-" + s
	}
	return s
}
