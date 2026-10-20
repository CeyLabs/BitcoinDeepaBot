package lnbits

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	decodepay "github.com/fiatjaf/ln-decodepay"
)

// The payloads below are the response shapes lnbits returns from
// GET /api/v1/payments/{payment_hash}. lnbits builds them differently for an
// authorised wallet key and for public access, and older versions describe a
// payment with `pending` instead of `status`, so all of those shapes have to
// classify correctly.
func TestClassifyPayment(t *testing.T) {
	tests := []struct {
		name string
		body string
		want PaymentState
	}{
		{
			name: "settled",
			body: `{"paid":true,"preimage":"6f1a2b","details":{"status":"success","pending":false,"fee":-1000}}`,
			want: PaymentStateSettled,
		},
		{
			name: "settled by status alone",
			body: `{"paid":false,"status":"success","details":{"status":"success"}}`,
			want: PaymentStateSettled,
		},
		{
			name: "failed",
			body: `{"paid":false,"status":"failed","details":{"status":"failed","pending":false}}`,
			want: PaymentStateFailed,
		},
		{
			name: "in flight",
			body: `{"paid":false,"status":"pending","preimage":"0000000000000000000000000000000000000000000000000000000000000000","details":{"status":"pending","pending":true}}`,
			want: PaymentStatePending,
		},
		{
			// lnbits omits both details and status on the public response, so
			// an in-flight payment looks identical to a resolved one. It must
			// not be read as a failure.
			name: "in flight, public response without details",
			body: `{"paid":false,"preimage":"0000000000000000000000000000000000000000000000000000000000000000"}`,
			want: PaymentStateUnknown,
		},
		{
			name: "legacy settled without status",
			body: `{"paid":true,"preimage":"6f1a2b","details":{"pending":false,"fee":-2000}}`,
			want: PaymentStateSettled,
		},
		{
			name: "legacy in flight without status",
			body: `{"paid":false,"details":{"pending":true}}`,
			want: PaymentStatePending,
		},
		{
			name: "no evidence either way",
			body: `{"paid":false,"details":{"pending":false}}`,
			want: PaymentStateUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p LNbitsPayment
			if err := json.Unmarshal([]byte(tt.body), &p); err != nil {
				t.Fatalf("could not unmarshal lnbits response: %v", err)
			}
			if got := classifyPayment(p); got != tt.want {
				t.Errorf("classifyPayment() = %v, want %v", got, tt.want)
			}
		})
	}
}

// lnbits reports Payment.fee in millisats, negative for an outgoing payment.
func TestSettlementFeeSats(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int64
	}{
		{name: "negative millisats in details", body: `{"details":{"fee":-1000}}`, want: 1},
		{name: "positive millisats in details", body: `{"details":{"fee":2500}}`, want: 2},
		{name: "top level fee", body: `{"fee":-3000}`, want: 3},
		{name: "no fee", body: `{"details":{}}`, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p LNbitsPayment
			if err := json.Unmarshal([]byte(tt.body), &p); err != nil {
				t.Fatalf("could not unmarshal lnbits response: %v", err)
			}
			if got := settlementFeeSats(p); got != tt.want {
				t.Errorf("settlementFeeSats() = %d, want %d", got, tt.want)
			}
		})
	}
}

// lnbits sends an all-zero preimage for a payment that has not settled, which
// is not proof of payment and must not be reported as one.
func TestSettlementPreimage(t *testing.T) {
	zero := `{"preimage":"0000000000000000000000000000000000000000000000000000000000000000"}`
	var p LNbitsPayment
	if err := json.Unmarshal([]byte(zero), &p); err != nil {
		t.Fatalf("could not unmarshal lnbits response: %v", err)
	}
	if got := settlementPreimage(p); got != "" {
		t.Errorf("settlementPreimage() = %q, want empty for the all-zero placeholder", got)
	}

	var real LNbitsPayment
	if err := json.Unmarshal([]byte(`{"details":{"preimage":"6f1a2b"}}`), &real); err != nil {
		t.Fatalf("could not unmarshal lnbits response: %v", err)
	}
	if got := settlementPreimage(real); got != "6f1a2b" {
		t.Errorf("settlementPreimage() = %q, want %q", got, "6f1a2b")
	}
}

func TestValidPaymentHash(t *testing.T) {
	valid := "0f3a9c1e2b4d5f6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c"
	tests := map[string]bool{
		valid:               true,
		"":                  false,
		valid[:63]:          false,
		valid + "0":         false,
		"../wallet":         false,
		valid[:60] + "%3Fx": false,
		valid[:63] + "g":    false,
	}
	for in, want := range tests {
		if got := ValidPaymentHash(in); got != want {
			t.Errorf("ValidPaymentHash(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestInvoiceExpired(t *testing.T) {
	now := int(time.Now().Unix())
	tests := []struct {
		name string
		inv  decodepay.Bolt11
		want bool
	}{
		{name: "fresh", inv: decodepay.Bolt11{CreatedAt: now, Expiry: 3600}, want: false},
		{name: "past expiry", inv: decodepay.Bolt11{CreatedAt: now - 7200, Expiry: 3600}, want: true},
		{name: "zero expiry", inv: decodepay.Bolt11{CreatedAt: now - 1, Expiry: 0}, want: true},
	}
	for _, tt := range tests {
		if got := InvoiceExpired(tt.inv); got != tt.want {
			t.Errorf("%s: InvoiceExpired() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsPaymentRejected(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "lnbits 400", err: Error{StatusCode: 400, Detail: "Insufficient balance."}, want: true},
		{name: "lnbits 404", err: Error{StatusCode: 404}, want: true},
		{name: "lnbits 500", err: Error{StatusCode: 500}, want: false},
		{name: "lnbits 520 payment error", err: Error{StatusCode: 520}, want: false},
		{name: "transport error", err: errors.New("connection reset by peer"), want: false},
		{name: "wrapped 400", err: fmt.Errorf("pay: %w", Error{StatusCode: 400}), want: true},
	}
	for _, tt := range tests {
		if got := IsPaymentRejected(tt.err); got != tt.want {
			t.Errorf("%s: IsPaymentRejected() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestPriorPayment(t *testing.T) {
	const hash = "0f3a9c1e2b4d5f6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c"
	tests := []struct {
		name      string
		status    int
		body      string
		wantFound bool
		wantState PaymentState
		wantErr   bool
	}{
		{name: "never paid", status: 404, body: `{"detail":"Payment does not exist."}`},
		{name: "settled", status: 200, body: `{"paid":true,"preimage":"6f1a2b"}`, wantFound: true, wantState: PaymentStateSettled},
		{name: "in flight", status: 200, body: `{"paid":false,"status":"pending"}`, wantFound: true, wantState: PaymentStatePending},
		{name: "failed", status: 200, body: `{"paid":false,"status":"failed"}`, wantFound: true, wantState: PaymentStateFailed},
		{name: "lnbits down", status: 502, body: `bad gateway`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/payments/"+hash {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			got, found, err := Client{url: srv.URL}.PriorPayment(Wallet{Inkey: "k"}, hash)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if found != tt.wantFound || got.State != tt.wantState {
				t.Errorf("got (%s, found=%v), want (%s, found=%v)", got.State, found, tt.wantState, tt.wantFound)
			}
		})
	}

	if _, _, err := (Client{}).PriorPayment(Wallet{}, "../wallet"); err == nil {
		t.Error("PriorPayment accepted an invalid payment hash")
	}
}
