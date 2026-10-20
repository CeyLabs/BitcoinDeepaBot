package lnbits

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	decodepay "github.com/fiatjaf/ln-decodepay"
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

// InvoiceExpiresAt returns when a decoded bolt11 invoice stops being payable.
func InvoiceExpiresAt(inv decodepay.Bolt11) time.Time {
	return time.Unix(int64(inv.CreatedAt)+int64(inv.Expiry), 0)
}

// InvoiceExpired reports whether a decoded bolt11 invoice is past its expiry.
// Decodepay accepts expired invoices, so callers have to check this themselves.
func InvoiceExpired(inv decodepay.Bolt11) bool {
	return time.Now().After(InvoiceExpiresAt(inv))
}

// IsPaymentRejected reports whether a Pay error is lnbits refusing the payment
// with a 4xx, which means it never left the wallet. A transport error or a 5xx
// does not say that: the connection may have dropped after lnbits started the
// payment, so the payment hash has to be checked before calling it failed.
func IsPaymentRejected(err error) bool {
	var lerr Error
	return errors.As(err, &lerr) && lerr.StatusCode >= 400 && lerr.StatusCode < 500
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
			last = settlementOf(payment)
			if last.State == PaymentStateSettled || last.State == PaymentStateFailed {
				return last
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

// PriorPayment looks paymentHash up in w once, so a caller can tell whether the
// wallet already paid (or is still paying) an invoice before paying it again.
// found is false only when lnbits answers 404, i.e. the wallet has no payment
// for this hash. Any other lookup error is returned: it says nothing about
// whether a payment exists.
func (c Client) PriorPayment(w Wallet, paymentHash string) (settlement PaymentSettlement, found bool, err error) {
	if !ValidPaymentHash(paymentHash) {
		return PaymentSettlement{}, false, errors.New("no valid payment hash to check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rateLimitGrace)
	defer cancel()
	if err := RateLimiter.Wait(ctx); err != nil {
		return PaymentSettlement{}, false, err
	}

	payment, err := c.Payment(w, paymentHash)
	var lerr Error
	if errors.As(err, &lerr) && lerr.StatusCode == http.StatusNotFound {
		return PaymentSettlement{}, false, nil
	}
	if err != nil {
		return PaymentSettlement{}, false, err
	}
	return settlementOf(payment), true, nil
}

// settlementOf turns one lnbits payment lookup into a PaymentSettlement.
func settlementOf(p LNbitsPayment) PaymentSettlement {
	state := classifyPayment(p)
	if state != PaymentStateSettled {
		return PaymentSettlement{State: state}
	}
	return PaymentSettlement{
		State:    PaymentStateSettled,
		Fee:      settlementFeeSats(p),
		Preimage: settlementPreimage(p),
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
