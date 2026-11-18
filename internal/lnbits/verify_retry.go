package lnbits

import (
	"errors"
	"time"
)

// verifyRetryDelays are the pauses between attempts when lnbits cannot be
// reached. Short, because the caller is answering an HTTP request.
var verifyRetryDelays = []time.Duration{250 * time.Millisecond, time.Second}

// VerifyIncomingPaymentWithRetry is VerifyIncomingPayment with a few retries
// on transport errors, within budget.
//
// lnbits does not retry a webhook that fails, so one dropped lookup would lose
// the callback for good (a paid group ticket with no invite link, for example).
// A definite "not verified" answer is never retried, and no new attempt starts
// once the budget is spent.
func (c Client) VerifyIncomingPaymentWithRetry(w Wallet, paymentHash string, budget time.Duration) (int64, error) {
	return retryTransient(func() (int64, error) {
		return c.VerifyIncomingPayment(w, paymentHash)
	}, verifyRetryDelays, budget)
}

func retryTransient(fn func() (int64, error), delays []time.Duration, budget time.Duration) (int64, error) {
	deadline := time.Now().Add(budget)
	amount, err := fn()
	for _, d := range delays {
		if err == nil || errors.Is(err, ErrPaymentNotVerified) {
			return amount, err
		}
		if time.Now().Add(d).After(deadline) {
			break
		}
		time.Sleep(d)
		amount, err = fn()
	}
	return amount, err
}
