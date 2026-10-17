package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
)

const testWalletSecret = "test-secret"

// withTestWallets swaps in a single whitelisted wallet for the duration of t.
func withTestWallets(t *testing.T) {
	t.Helper()
	saved := internal.Configuration.API.Send
	t.Cleanup(func() { internal.Configuration.API.Send = saved })

	internal.Configuration.API.Send.TimestampTolerance = 300
	internal.Configuration.API.Send.WhitelistedWallets = map[string]internal.WhitelistedWallet{
		"testwallet": {Username: "alice", HMACSecret: testWalletSecret},
	}
}

// signedRequest builds a request signed the way API clients sign it.
func signedRequest(method, path, body string, timestamp int64, secret string) *http.Request {
	ts := strconv.FormatInt(timestamp, 10)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Timestamp", ts)
	r.Header.Set("X-HMAC-Signature", GenerateHMACSignature(method, path, ts, body, secret))
	return r
}

// serveHMAC runs r through WalletHMACMiddleware and reports the response and
// the wallet the inner handler saw ("" if it was not reached).
func serveHMAC(r *http.Request) (*httptest.ResponseRecorder, string) {
	var wallet string
	next := func(w http.ResponseWriter, r *http.Request) {
		wallet, _ = r.Context().Value("authenticated_wallet").(string)
		if _, ok := RequestStart(r.Context()); !ok {
			wallet = "missing request start"
		}
		w.WriteHeader(http.StatusOK)
	}
	rec := httptest.NewRecorder()
	WalletHMACMiddleware(next)(rec, r)
	return rec, wallet
}

func TestWalletHMACMiddleware(t *testing.T) {
	withTestWallets(t)
	now := time.Now().Unix()

	tests := []struct {
		name     string
		req      func() *http.Request
		wantCode int
		wantBody string
	}{
		{
			name:     "valid signature",
			req:      func() *http.Request { return signedRequest("POST", "/api/v1/send", `{"n":1}`, now, testWalletSecret) },
			wantCode: http.StatusOK,
		},
		{
			name: "timestamp slightly in the future is allowed",
			req: func() *http.Request {
				return signedRequest("POST", "/api/v1/send", `{"n":2}`, now+30, testWalletSecret)
			},
			wantCode: http.StatusOK,
		},
		{
			name: "timestamp too old",
			req: func() *http.Request {
				return signedRequest("POST", "/api/v1/send", `{"n":3}`, now-301, testWalletSecret)
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Request expired",
		},
		{
			name: "timestamp too far in the future",
			req: func() *http.Request {
				return signedRequest("POST", "/api/v1/send", `{"n":4}`, now+301, testWalletSecret)
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Request expired",
		},
		{
			name:     "wrong secret",
			req:      func() *http.Request { return signedRequest("POST", "/api/v1/send", `{"n":5}`, now, "other-secret") },
			wantCode: http.StatusUnauthorized,
			wantBody: "Invalid signature",
		},
		{
			name: "body changed after signing",
			req: func() *http.Request {
				r := signedRequest("POST", "/api/v1/send", `{"amount":1}`, now, testWalletSecret)
				tampered := signedRequest("POST", "/api/v1/send", `{"amount":1000000}`, now, testWalletSecret)
				tampered.Header.Set("X-HMAC-Signature", r.Header.Get("X-HMAC-Signature"))
				return tampered
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Invalid signature",
		},
		{
			name: "path changed after signing",
			req: func() *http.Request {
				r := signedRequest("GET", "/api/v1/userbalance", "", now, testWalletSecret)
				other := httptest.NewRequest("GET", "/api/v1/usertransactions", nil)
				other.Header = r.Header.Clone()
				return other
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Invalid signature",
		},
		{
			name: "missing timestamp",
			req: func() *http.Request {
				r := signedRequest("POST", "/api/v1/send", `{"n":6}`, now, testWalletSecret)
				r.Header.Del("X-Timestamp")
				return r
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Missing timestamp",
		},
		{
			name: "invalid timestamp",
			req: func() *http.Request {
				r := signedRequest("POST", "/api/v1/send", `{"n":7}`, now, testWalletSecret)
				r.Header.Set("X-Timestamp", "yesterday")
				return r
			},
			wantCode: http.StatusBadRequest,
			wantBody: "Invalid timestamp",
		},
		{
			name: "missing signature",
			req: func() *http.Request {
				r := signedRequest("POST", "/api/v1/send", `{"n":8}`, now, testWalletSecret)
				r.Header.Del("X-HMAC-Signature")
				return r
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Missing signature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, wallet := serveHMAC(tt.req())
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantCode == http.StatusOK {
				if wallet != "testwallet" {
					t.Errorf("handler saw wallet %q, want %q", wallet, "testwallet")
				}
				return
			}
			if wallet != "" {
				t.Errorf("handler was reached for a rejected request")
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestWalletHMACMiddlewareRejectsReplay(t *testing.T) {
	withTestWallets(t)
	ts := time.Now().Unix()
	body := `{"to":"bob","amount":100,"memo":"replay-test"}`

	first, _ := serveHMAC(signedRequest("POST", "/api/v1/send", body, ts, testWalletSecret))
	if first.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200 (body %q)", first.Code, first.Body.String())
	}

	// The exact same signed request again, as an attacker or a naive retry
	// would send it.
	replay, wallet := serveHMAC(signedRequest("POST", "/api/v1/send", body, ts, testWalletSecret))
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replay: status = %d, want 401", replay.Code)
	}
	if wallet != "" {
		t.Errorf("handler was reached for a replayed request")
	}
	if !strings.Contains(replay.Body.String(), "Request already processed") {
		t.Errorf("replay body = %q", replay.Body.String())
	}

	// A retry signed with a fresh timestamp is a new request and goes through.
	retry, _ := serveHMAC(signedRequest("POST", "/api/v1/send", body, ts+1, testWalletSecret))
	if retry.Code != http.StatusOK {
		t.Errorf("fresh retry: status = %d, want 200 (body %q)", retry.Code, retry.Body.String())
	}
}

func TestSignatureSetClaim(t *testing.T) {
	s := &signatureSet{seen: make(map[string]time.Time)}

	if !s.claim("a", time.Minute) {
		t.Fatal("first claim of a = false, want true")
	}
	if s.claim("a", time.Minute) {
		t.Fatal("second claim of a = true, want false")
	}
	if !s.claim("b", time.Minute) {
		t.Fatal("claim of b = false, want true")
	}

	// An expired entry is forgotten, so the set cannot grow without bound.
	if !s.claim("old", -time.Second) {
		t.Fatal("claim of old = false, want true")
	}
	if !s.claim("old", time.Minute) {
		t.Error("claim of expired old = false, want true")
	}
}
