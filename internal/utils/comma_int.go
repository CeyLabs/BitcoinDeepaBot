package utils

import (
	"fmt"
	"io"
)

type CommaInt int64

func (c CommaInt) Format(f fmt.State, verb rune) {
	switch verb {
	case 'd', 'v':
		io.WriteString(f, FormatIntWithCommas(int64(c)))
	default:
		fmt.Fprintf(f, "%%!%c(%d)", verb, int64(c))
	}
}
