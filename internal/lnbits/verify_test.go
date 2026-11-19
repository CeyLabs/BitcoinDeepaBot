package lnbits

import (
	"errors"
	"strings"
	"testing"
)

func TestVerifyIncoming(t *testing.T) {
	const wallet = "w1"
	hash := strings.Repeat("ab", 32)

	settled := func(mod func(*LNbitsPayment)) LNbitsPayment {
		p := LNbitsPayment{
			Paid:    true,
			Details: Payment{WalletID: wallet, PaymentHash: hash, Amount: 21000, Status: "success"},
		}
		if mod != nil {
			mod(&p)
		}
		return p
	}

	tests := []struct {
		name    string
		p       LNbitsPayment
		wallet  string
		want    int64
		wantErr bool
	}{
		{name: "settled incoming payment", p: settled(nil), wallet: wallet, want: 21000},
		{name: "hash compare ignores case", p: settled(func(p *LNbitsPayment) { p.Details.PaymentHash = strings.ToUpper(hash) }), wallet: wallet, want: 21000},
		{name: "pending is rejected", p: settled(func(p *LNbitsPayment) { p.Paid = false; p.Details.Status = "pending" }), wallet: wallet, wantErr: true},
		{name: "failed is rejected", p: settled(func(p *LNbitsPayment) { p.Paid = false; p.Details.Status = "failed" }), wallet: wallet, wantErr: true},
		{name: "unknown state is rejected", p: LNbitsPayment{}, wallet: wallet, wantErr: true},
		// lnbits answers {"paid": true} with no details when the key does not
		// own the payment. That must never pass.
		{name: "paid without details is rejected", p: LNbitsPayment{Paid: true}, wallet: wallet, wantErr: true},
		{name: "other wallet is rejected", p: settled(func(p *LNbitsPayment) { p.Details.WalletID = "w2" }), wallet: wallet, wantErr: true},
		{name: "empty expected wallet is rejected", p: settled(func(p *LNbitsPayment) { p.Details.WalletID = "" }), wallet: "", wantErr: true},
		{name: "hash mismatch is rejected", p: settled(func(p *LNbitsPayment) { p.Details.PaymentHash = strings.Repeat("cd", 32) }), wallet: wallet, wantErr: true},
		{name: "outgoing payment is rejected", p: settled(func(p *LNbitsPayment) { p.Details.Amount = -21000 }), wallet: wallet, wantErr: true},
		{name: "zero amount is rejected", p: settled(func(p *LNbitsPayment) { p.Details.Amount = 0 }), wallet: wallet, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := verifyIncoming(tt.p, tt.wallet, hash)
			if tt.wantErr {
				if !errors.Is(err, ErrPaymentNotVerified) {
					t.Fatalf("want ErrPaymentNotVerified, got amount=%d err=%v", got, err)
				}
				if got != 0 {
					t.Fatalf("rejected payment returned amount %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("amount = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestVerifyIncomingPaymentRejectsMalformedHash(t *testing.T) {
	c := Client{}
	for _, h := range []string{"", "xyz", strings.Repeat("ab", 31), strings.Repeat("AB", 32), "../../etc/passwd"} {
		if _, err := c.VerifyIncomingPayment(Wallet{ID: "w1"}, h); !errors.Is(err, ErrPaymentNotVerified) {
			t.Fatalf("hash %q: want ErrPaymentNotVerified, got %v", h, err)
		}
	}
}
