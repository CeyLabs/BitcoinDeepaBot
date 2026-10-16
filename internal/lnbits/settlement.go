package lnbits

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter enforces a shared rate limit of 150 req/min (safely below LNbits'
// 200/min limit) across every caller that polls lnbits in a loop.
var RateLimiter = rate.NewLimiter(rate.Every(400*time.Millisecond), 1)

// rateLimitGrace lets a settlement check wait this long for a rate limit slot
// past its own deadline, so a zero-timeout lookup still gets its one check.
const rateLimitGrace = 2 * time.Second

var paymentHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidPaymentHash reports whether s is a lowercase hex sha256 payment hash.
// Anything else must not reach an lnbits URL.
func ValidPaymentHash(s string) bool {
	return paymentHashPattern.MatchString(s)
}

// PaymentState describes what lnbits knows about an outgoing payment.
//
// A 2xx from POST /api/v1/payments only means lnbits accepted the payment and
// handed it to its funding backend. It says nothing about whether the sats
// actually reached the destination node, so every outgoing payment has to be
// followed up with a settlement check before it is reported as successful.
type PaymentState int

const (
	// PaymentStateUnknown means lnbits could not be reached, or answered in a
	// shape we cannot interpret. Never report this as success or failure.
	PaymentStateUnknown PaymentState = iota
	// PaymentStatePending means the payment is still in flight.
	PaymentStatePending
	// PaymentStateSettled means the preimage is in: the sats left the node.
	PaymentStateSettled
	// PaymentStateFailed means lnbits resolved the payment without a preimage
	// and the sats are back in the wallet.
	PaymentStateFailed
)

func (s PaymentState) String() string {
	switch s {
	case PaymentStatePending:
		return "pending"
	case PaymentStateSettled:
		return "settled"
	case PaymentStateFailed:
		return "failed"
	}
	return "unknown"
}

// PaymentSettlement is the outcome of following an outgoing payment.
type PaymentSettlement struct {
	State    PaymentState
	Fee      int64  // routing fee in sats, set for settled payments
	Preimage string // proof of payment, set for settled payments
	Err      error  // last lookup error, if any
}

// Settlement checks are polled rather than streamed because the lnbits SSE feed
// only carries incoming payments. Each check makes lnbits re-query its funding
// source (check_transaction_status -> check_payment_status), so the interval
// backs off to keep a long wait well inside the lnbits request rate limit.
const (
	settlementPollInitial = 500 * time.Millisecond
	settlementPollMax     = 3 * time.Second
	settlementPollFactor  = 1.5
)

// WaitForOutgoingPayment polls lnbits until paymentHash reaches a terminal
// state or timeout expires.
//
// A PaymentStatePending or PaymentStateUnknown result means the payment may
// still settle later: it must not be reported to the caller as either a success
// or a failure, and the sats must not be treated as returned.
func (c Client) WaitForOutgoingPayment(w Wallet, paymentHash string, timeout time.Duration) PaymentSettlement {
	if !ValidPaymentHash(paymentHash) {
		return PaymentSettlement{State: PaymentStateUnknown, Err: errors.New("no valid payment hash to check")}
	}

	last := PaymentSettlement{State: PaymentStateUnknown}
	deadline := time.Now().Add(timeout)
	interval := settlementPollInitial
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline)+rateLimitGrace)
		err := RateLimiter.Wait(ctx)
		cancel()
		if err != nil {
			if last.Err == nil {
				last.Err = err
			}
			return last
		}

		payment, err := c.Payment(w, paymentHash)
		if err != nil {
			// This includes a 404 "Payment does not exist". lnbits marks a
			// failed payment FAILED and keeps the row, so a missing record is
			// not evidence that the sats came back — only that we cannot see
			// the payment. Stay unsure rather than claim a refund.
			last = PaymentSettlement{State: PaymentStateUnknown, Err: err}
		} else {
			switch state := classifyPayment(payment); state {
			case PaymentStateSettled:
				return PaymentSettlement{
					State:    PaymentStateSettled,
					Fee:      settlementFeeSats(payment),
					Preimage: settlementPreimage(payment),
				}
			case PaymentStateFailed:
				return PaymentSettlement{State: PaymentStateFailed}
			default:
				last = PaymentSettlement{State: state}
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return last
		}
		if remaining > interval {
			remaining = interval
		}
		time.Sleep(remaining)
		if interval = time.Duration(float64(interval) * settlementPollFactor); interval > settlementPollMax {
			interval = settlementPollMax
		}
	}
}

// classifyPayment maps an lnbits payment lookup onto a PaymentState.
//
// Every state is reported only on positive evidence, and anything else is
// PaymentStateUnknown. That matters in both directions: calling an in-flight
// payment "failed" would tell a user their sats came back when they have not,
// and calling it "settled" is the bug this whole check exists to prevent.
//
// lnbits answers GET /api/v1/payments/{hash} in several shapes:
//
//	{"paid": true,  "preimage": "..", "details": {..}}   settled
//	{"paid": false, "status": "failed", "details": {..}} failed
//	{"paid": false, "status": "pending", "details": {..}} in flight
//	{"paid": false, "preimage": ".."}                     in flight, unauthorised
//
// The last shape carries no status at all, which is why a missing status is
// never read as a failure.
func classifyPayment(p LNbitsPayment) PaymentState {
	if p.Paid {
		return PaymentStateSettled
	}
	// lnbits PaymentState is one of "success", "pending", "failed". Older
	// versions send no status and describe the payment with Details.Pending.
	switch strings.ToLower(firstNonEmpty(p.Status, p.Details.Status)) {
	case "success":
		return PaymentStateSettled
	case "failed":
		return PaymentStateFailed
	case "pending":
		return PaymentStatePending
	}
	if p.Details.Pending {
		return PaymentStatePending
	}
	return PaymentStateUnknown
}

// settlementFeeSats returns the routing fee in sats. lnbits reports Payment.fee
// in millisats, and signs it negative on some versions.
func settlementFeeSats(p LNbitsPayment) int64 {
	fee := p.Details.Fee
	if fee == 0 {
		fee = p.Fee
	}
	if fee < 0 {
		fee = -fee
	}
	return fee / 1000
}

// settlementPreimage returns the payment preimage, treating the all-zero
// placeholder lnbits reports for unsettled payments as absent.
func settlementPreimage(p LNbitsPayment) string {
	preimage := firstNonEmpty(p.Preimage, p.Details.Preimage)
	if strings.Trim(preimage, "0") == "" {
		return ""
	}
	return preimage
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
