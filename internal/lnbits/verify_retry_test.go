package lnbits

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRetryTransient(t *testing.T) {
	transient := errors.New("connection refused")
	delays := []time.Duration{time.Millisecond, time.Millisecond}

	t.Run("succeeds after a transport error", func(t *testing.T) {
		calls := 0
		got, err := retryTransient(func() (int64, error) {
			calls++
			if calls == 1 {
				return 0, transient
			}
			return 21000, nil
		}, delays, time.Second)
		if err != nil || got != 21000 || calls != 2 {
			t.Fatalf("got %d, %v after %d calls; want 21000, nil after 2", got, err, calls)
		}
	})

	t.Run("never retries a definite rejection", func(t *testing.T) {
		calls := 0
		_, err := retryTransient(func() (int64, error) {
			calls++
			return 0, fmt.Errorf("%w: other wallet", ErrPaymentNotVerified)
		}, delays, time.Second)
		if !errors.Is(err, ErrPaymentNotVerified) || calls != 1 {
			t.Fatalf("got %v after %d calls; want ErrPaymentNotVerified after 1", err, calls)
		}
	})

	t.Run("gives up after the last delay", func(t *testing.T) {
		calls := 0
		_, err := retryTransient(func() (int64, error) { calls++; return 0, transient }, delays, time.Second)
		if !errors.Is(err, transient) || calls != 3 {
			t.Fatalf("got %v after %d calls; want transport error after 3", err, calls)
		}
	})

	t.Run("no new attempt once the budget is spent", func(t *testing.T) {
		calls := 0
		_, err := retryTransient(func() (int64, error) { calls++; return 0, transient },
			[]time.Duration{time.Hour}, 10*time.Millisecond)
		if !errors.Is(err, transient) || calls != 1 {
			t.Fatalf("got %v after %d calls; want transport error after 1", err, calls)
		}
	})

	t.Run("does not retry success", func(t *testing.T) {
		calls := 0
		if _, err := retryTransient(func() (int64, error) { calls++; return 1, nil }, delays, time.Second); err != nil || calls != 1 {
			t.Fatalf("got %v after %d calls", err, calls)
		}
	})
}
