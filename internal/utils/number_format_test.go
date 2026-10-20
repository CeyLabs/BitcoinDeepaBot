package utils

import "testing"

func TestAmountWithFeeReserve(t *testing.T) {
	tests := []struct{ amount, want int64 }{
		{0, 0},
		{98, 100}, // exactly 2% of 100 left over
		{99, 102}, // rounds up, never under-reserves
		{1, 2},
		{980_000_000, 1_000_000_000},
		{980_000_001, 1_000_000_002},
	}
	for _, tt := range tests {
		if got := AmountWithFeeReserve(tt.amount); got != tt.want {
			t.Errorf("AmountWithFeeReserve(%d) = %d, want %d", tt.amount, got, tt.want)
		}
		// The reserve must leave at least 2%: amount <= 98% of the result.
		if got := AmountWithFeeReserve(tt.amount); tt.amount*100 > got*98 {
			t.Errorf("AmountWithFeeReserve(%d) = %d leaves less than 2%%", tt.amount, got)
		}
	}
}
