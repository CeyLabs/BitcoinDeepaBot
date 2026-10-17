package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/gorilla/mux"
)

// Invoices from the BOLT11 spec examples. Their signatures are valid, so they
// decode, but they are long expired and can never be paid.
const (
	// 2500u = 250,000 sats.
	testInvoice250k = "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jsxqzpuaztrnwngzn3kdzw5hydlzf03qdgm2hdq27cqv3agm2awhz5se903vruatfhq77w3ls4evs3ch9zw97j25emudupq63nyw24cg27h2rspfj9srp"
	// No amount.
	testInvoiceAmountless = "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaq8rkx3yf5tcsyz3d73gafnh3cax9rn449d9p5uxz9ezhhypd0elx87sjle52x86fux2ypatgddc6k63n7erqz25le42c4u4ecky03ylcqca784w"
)

// authenticated returns r as WalletHMACMiddleware would pass it on.
func authenticated(r *http.Request, wallet string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), "authenticated_wallet", wallet))
}

// These cases are all rejected before the handler touches the bot, lnbits or
// the memo cache, so a zero Service is enough.
func TestSendRejectsBadRequests(t *testing.T) {
	withTestWallets(t)
	internal.Configuration.API.Send.MinAmount = 1
	internal.Configuration.API.Send.MaxAmount = 1000

	tests := []struct {
		name     string
		body     string
		wallet   string
		wantBody string
	}{
		{name: "invalid json", body: `{`, wallet: "testwallet", wantBody: "Invalid JSON request"},
		{name: "not authenticated", body: `{"to":"bob","amount":10}`, wantBody: "Authentication failed"},
		{name: "unknown wallet", body: `{"to":"bob","amount":10}`, wallet: "ghost", wantBody: "Invalid wallet configuration"},
		{name: "missing to", body: `{"amount":10}`, wallet: "testwallet", wantBody: "Missing 'to' field"},
		{
			name:     "invoice that does not decode",
			body:     `{"to":"lnbc1notarealinvoice"}`,
			wallet:   "testwallet",
			wantBody: "Invalid Lightning invoice",
		},
		{
			name:     "amountless invoice",
			body:     `{"to":"` + testInvoiceAmountless + `"}`,
			wallet:   "testwallet",
			wantBody: "amountless invoices are not supported",
		},
		{
			// The request amount is ignored: the invoice asks for 250k sats,
			// which is over the 1000 sat limit.
			name:     "invoice over max amount",
			body:     `{"to":"` + testInvoice250k + `","amount":1}`,
			wallet:   "testwallet",
			wantBody: "Amount cannot exceed",
		},
		{
			name:     "invoice with lightning: prefix and upper case",
			body:     `{"to":"LIGHTNING:` + strings.ToUpper(testInvoice250k) + `"}`,
			wallet:   "testwallet",
			wantBody: "Amount cannot exceed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/send", strings.NewReader(tt.body))
			if tt.wallet != "" {
				r = authenticated(r, tt.wallet)
			}
			rec := httptest.NewRecorder()
			Service{}.Send(rec, r)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestSendRejectsInvoiceUnderMinAmount(t *testing.T) {
	withTestWallets(t)
	internal.Configuration.API.Send.MinAmount = 300_000
	internal.Configuration.API.Send.MaxAmount = 1_000_000

	r := authenticated(httptest.NewRequest("POST", "/api/v1/send",
		strings.NewReader(`{"to":"`+testInvoice250k+`","amount":500000}`)), "testwallet")
	rec := httptest.NewRecorder()
	Service{}.Send(rec, r)

	if !strings.Contains(rec.Body.String(), "Amount must be greater than") {
		t.Errorf("body = %q, want a min amount error", rec.Body.String())
	}
}

func TestSendPaymentStatusRejectsBadRequests(t *testing.T) {
	withTestWallets(t)
	validHash := strings.Repeat("ab", 32)

	tests := []struct {
		name     string
		hash     string
		wallet   string
		wantBody string
	}{
		{name: "empty hash", hash: "", wallet: "testwallet", wantBody: "Invalid payment_hash"},
		{name: "short hash", hash: "abc123", wallet: "testwallet", wantBody: "Invalid payment_hash"},
		{name: "non hex hash", hash: strings.Repeat("zz", 32), wallet: "testwallet", wantBody: "Invalid payment_hash"},
		{name: "path injection", hash: strings.Repeat("a", 60) + "/../", wallet: "testwallet", wantBody: "Invalid payment_hash"},
		// A valid hash passes validation (upper case is normalised) and the
		// request then fails on authentication.
		{name: "not authenticated", hash: strings.ToUpper(validHash), wantBody: "Authentication failed"},
		{name: "unknown wallet", hash: validHash, wallet: "ghost", wantBody: "Invalid wallet configuration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/send/payment/x", nil)
			r = mux.SetURLVars(r, map[string]string{"payment_hash": tt.hash})
			if tt.wallet != "" {
				r = authenticated(r, tt.wallet)
			}
			rec := httptest.NewRecorder()
			Service{}.SendPaymentStatus(rec, r)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}
