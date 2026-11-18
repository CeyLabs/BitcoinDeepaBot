package lnbits

import (
	"errors"
	"fmt"
	"strings"
)

// ErrPaymentNotVerified means lnbits did not confirm the payment. The caller
// must not act on it: the claim came from outside and could be forged.
var ErrPaymentNotVerified = errors.New("payment not verified by lnbits")

// VerifyIncomingPayment asks lnbits whether paymentHash is a settled payment
// INTO wallet w, and returns the amount lnbits recorded, in msat.
//
// It queries with the receiving wallet's own invoice key. lnbits only returns
// the payment details to the wallet that owns the payment, so a payment that
// belongs to another wallet comes back without details and fails the check.
//
// Only positive evidence counts. A webhook body is not evidence: anyone who can
// reach the webhook port can post one, so callers must use the amount returned
// here and ignore the amount in the request.
func (c Client) VerifyIncomingPayment(w Wallet, paymentHash string) (amountMsat int64, err error) {
	if !ValidPaymentHash(paymentHash) {
		return 0, fmt.Errorf("%w: malformed payment hash", ErrPaymentNotVerified)
	}
	p, err := c.Payment(w, paymentHash)
	if err != nil {
		// A 4xx (unknown hash, bad key) is lnbits saying no. Anything else is
		// a transport problem and says nothing either way.
		if IsPaymentRejected(err) {
			return 0, fmt.Errorf("%w: %v", ErrPaymentNotVerified, err)
		}
		return 0, err
	}
	return verifyIncoming(p, w.ID, paymentHash)
}

// verifyIncoming checks an lnbits payment lookup against the wallet and hash
// the caller expects.
func verifyIncoming(p LNbitsPayment, walletID, paymentHash string) (int64, error) {
	if state := classifyPayment(p); state != PaymentStateSettled {
		return 0, fmt.Errorf("%w: payment is %s", ErrPaymentNotVerified, state)
	}
	if walletID == "" || p.Details.WalletID != walletID {
		return 0, fmt.Errorf("%w: payment does not belong to this wallet", ErrPaymentNotVerified)
	}
	if !strings.EqualFold(p.Details.PaymentHash, paymentHash) {
		return 0, fmt.Errorf("%w: payment hash mismatch", ErrPaymentNotVerified)
	}
	if p.Details.Amount <= 0 {
		return 0, fmt.Errorf("%w: not an incoming payment", ErrPaymentNotVerified)
	}
	return p.Details.Amount, nil
}
