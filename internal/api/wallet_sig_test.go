package api

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
)

// testWalletKey signs requests for "testwallet". A fixed seed keeps it stable.
var testWalletKey = ed25519.NewKeyFromSeed(func() []byte { s := sha256.Sum256([]byte("testwallet")); return s[:] }())

// withTestWallets swaps in a single whitelisted wallet for the duration of t.
func withTestWallets(t *testing.T) {
	t.Helper()
	saved := internal.Configuration.API.Send
	t.Cleanup(func() { internal.Configuration.API.Send = saved })

	internal.Configuration.API.Send.TimestampTolerance = 300
	internal.Configuration.API.Send.WhitelistedWallets = map[string]internal.WhitelistedWallet{
		"testwallet": {Username: "alice", PublicKey: testWalletKey.Public().(ed25519.PublicKey)},
	}
}

var nonceCounter atomic.Int64

func newNonce() string { return "test-nonce-" + strconv.FormatInt(nonceCounter.Add(1), 10) + "-pad" }

// sigReq describes a request signed the way API clients sign it.
type sigReq struct {
	method, path, body, wallet, nonce string
	ts                                int64
	key                               ed25519.PrivateKey
	tamper                            func(r *http.Request)
}

func newSigReq(method, path, body string) sigReq {
	return sigReq{method: method, path: path, body: body, wallet: "testwallet", nonce: newNonce(), ts: time.Now().Unix(), key: testWalletKey}
}

func (s sigReq) build() *http.Request {
	ts := strconv.FormatInt(s.ts, 10)
	r := httptest.NewRequest(s.method, s.path, strings.NewReader(s.body))
	msg := Ed25519SigningString(s.method, r.URL.Path, r.URL.RawQuery, ts, s.nonce, []byte(s.body))
	r.Header.Set("X-Wallet-ID", s.wallet)
	r.Header.Set("X-Timestamp", ts)
	r.Header.Set("X-Nonce", s.nonce)
	r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, []byte(msg))))
	if s.tamper != nil {
		s.tamper(r)
	}
	return r
}

// serveAuth runs r through WalletAuthMiddleware and reports the response and
// the wallet the inner handler saw ("" if it was not reached).
func serveAuth(r *http.Request) (*httptest.ResponseRecorder, string) {
	var wallet string
	next := func(w http.ResponseWriter, r *http.Request) {
		wallet, _ = r.Context().Value("authenticated_wallet").(string)
		if _, ok := RequestStart(r.Context()); !ok {
			wallet = "missing request start"
		}
		w.WriteHeader(http.StatusOK)
	}
	rec := httptest.NewRecorder()
	WalletAuthMiddleware(next)(rec, r)
	return rec, wallet
}

func TestWalletAuthMiddleware(t *testing.T) {
	withTestWallets(t)
	_, otherKey, _ := ed25519.GenerateKey(nil)
	now := time.Now().Unix()
	base := func() sigReq { return newSigReq(http.MethodPost, "/api/v1/send?x=1", `{"amount":5}`) }
	with := func(f func(*sigReq)) func() sigReq { return func() sigReq { s := base(); f(&s); return s } }
	tamper := func(f func(r *http.Request)) func() sigReq { return with(func(s *sigReq) { s.tamper = f }) }

	tests := []struct {
		name     string
		req      func() sigReq
		wantCode int
		wantBody string
	}{
		{"valid", base, http.StatusOK, ""},
		{"get without body", func() sigReq { return newSigReq(http.MethodGet, "/api/v1/referral/lookup?code=abc", "") }, http.StatusOK, ""},
		{"timestamp slightly in the future", with(func(s *sigReq) { s.ts = now + 30 }), http.StatusOK, ""},
		{"wrong key", with(func(s *sigReq) { s.key = otherKey }), http.StatusUnauthorized, "Invalid signature"},
		{"unknown wallet", with(func(s *sigReq) { s.wallet = "nope" }), http.StatusUnauthorized, "Invalid signature"},
		{"expired", with(func(s *sigReq) { s.ts = now - 301 }), http.StatusUnauthorized, "Request expired"},
		{"too far in the future", with(func(s *sigReq) { s.ts = now + 301 }), http.StatusUnauthorized, "Request expired"},
		{"short nonce", with(func(s *sigReq) { s.nonce = "abc" }), http.StatusUnauthorized, "Invalid nonce"},
		{"bad nonce chars", with(func(s *sigReq) { s.nonce = "nonce with spaces!!" }), http.StatusUnauthorized, "Invalid nonce"},
		{"missing timestamp", tamper(func(r *http.Request) { r.Header.Del("X-Timestamp") }), http.StatusUnauthorized, "Invalid timestamp"},
		{"missing signature", tamper(func(r *http.Request) { r.Header.Del("X-Signature") }), http.StatusUnauthorized, "Invalid signature"},
		{"old hmac header only", tamper(func(r *http.Request) {
			r.Header.Del("X-Signature")
			r.Header.Set("X-HMAC-Signature", "deadbeef")
		}), http.StatusUnauthorized, "Invalid signature"},
		{"body changed", tamper(func(r *http.Request) { r.Body = httptest.NewRequest("POST", "/", strings.NewReader(`{"amount":5000}`)).Body }), http.StatusUnauthorized, "Invalid signature"},
		{"query changed", tamper(func(r *http.Request) { r.URL.RawQuery = "x=2" }), http.StatusUnauthorized, "Invalid signature"},
		{"path changed", tamper(func(r *http.Request) { r.URL.Path = "/api/v1/userbalance" }), http.StatusUnauthorized, "Invalid signature"},
		{"method changed", tamper(func(r *http.Request) { r.Method = http.MethodPut }), http.StatusUnauthorized, "Invalid signature"},
		{"nonce changed", tamper(func(r *http.Request) { r.Header.Set("X-Nonce", newNonce()) }), http.StatusUnauthorized, "Invalid signature"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, wallet := serveAuth(tt.req().build())
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantCode == http.StatusOK {
				if wallet != "testwallet" {
					t.Fatalf("handler saw wallet %q", wallet)
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

func TestWalletAuthMiddlewareRejectsReplay(t *testing.T) {
	withTestWallets(t)
	s := newSigReq(http.MethodPost, "/api/v1/send", `{"to":"bob","amount":100}`)
	if rec, _ := serveAuth(s.build()); rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d", rec.Code)
	}
	rec, wallet := serveAuth(s.build())
	if rec.Code != http.StatusUnauthorized || wallet != "" || !strings.Contains(rec.Body.String(), "Request already processed") {
		t.Fatalf("replay: got %d %q, wallet %q", rec.Code, rec.Body.String(), wallet)
	}
	// A retry with a fresh nonce is a new request and goes through.
	s.nonce = newNonce()
	if rec, _ := serveAuth(s.build()); rec.Code != http.StatusOK {
		t.Fatalf("fresh retry: got %d", rec.Code)
	}
}

func TestWalletAuthMiddlewareRejectsLargeBody(t *testing.T) {
	withTestWallets(t)
	s := newSigReq(http.MethodPost, "/api/v1/send", strings.Repeat("a", maxSignedBodySize+1))
	if rec, _ := serveAuth(s.build()); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
}

func withAnalyticsKey(t *testing.T) {
	t.Helper()
	saved := internal.Configuration.API.Analytics
	t.Cleanup(func() { internal.Configuration.API.Analytics = saved })
	internal.Configuration.API.Analytics.TimestampTolerance = 300
	internal.Configuration.API.Analytics.APIKeys = map[string]internal.AnalyticsAPIKey{
		"data-team": {Name: "Data Team", PublicKey: testWalletKey.Public().(ed25519.PublicKey)},
	}
}

func analyticsRequest(path, keyID, nonce string, ts int64, version string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	tsStr := strconv.FormatInt(ts, 10)
	msg := signingString(version, http.MethodGet, r.URL.Path, r.URL.RawQuery, tsStr, nonce, nil)
	r.Header.Set("X-Key-ID", keyID)
	r.Header.Set("X-Timestamp", tsStr)
	r.Header.Set("X-Nonce", nonce)
	r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(testWalletKey, []byte(msg))))
	return r
}

func serveAnalytics(r *http.Request) (int, string) {
	var key string
	rec := httptest.NewRecorder()
	AnalyticsAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		key, _ = r.Context().Value("analytics_api_key").(string)
	})(rec, r)
	return rec.Code, key
}

func TestAnalyticsAuthMiddleware(t *testing.T) {
	withAnalyticsKey(t)
	now := time.Now().Unix()
	path := "/api/v1/analytics/transactions?start_date=2026-11-01&limit=10"

	r := analyticsRequest(path, "data-team", newNonce(), now, analyticsSigVersion)
	if code, key := serveAnalytics(r); code != http.StatusOK || key != "data-team" {
		t.Fatalf("valid: got %d key %q", code, key)
	}
	if code, _ := serveAnalytics(analyticsRequest(path, "data-team", r.Header.Get("X-Nonce"), now, analyticsSigVersion)); code != http.StatusUnauthorized {
		t.Errorf("replay: got %d, want 401", code)
	}

	bad := map[string]*http.Request{
		"unknown key":          analyticsRequest(path, "nope", newNonce(), now, analyticsSigVersion),
		"expired":              analyticsRequest(path, "data-team", newNonce(), now-301, analyticsSigVersion),
		"future":               analyticsRequest(path, "data-team", newNonce(), now+301, analyticsSigVersion),
		"send API signature":   analyticsRequest(path, "data-team", newNonce(), now, ed25519SigVersion),
		"old hmac header only": httptest.NewRequest(http.MethodGet, path, nil),
	}
	changed := analyticsRequest(path, "data-team", newNonce(), now, analyticsSigVersion)
	changed.URL.RawQuery = "start_date=2020-01-01&limit=10"
	bad["query changed"] = changed
	bad["old hmac header only"].Header.Set("X-HMAC-Signature", "deadbeef")
	bad["old hmac header only"].Header.Set("X-Timestamp", strconv.FormatInt(now, 10))
	for name, r := range bad {
		if code, key := serveAnalytics(r); code != http.StatusUnauthorized || key != "" {
			t.Errorf("%s: got %d key %q, want 401", name, code, key)
		}
	}
}

// A wallet signature can't be used on the analytics API, even with the same
// key pair and id.
func TestWalletSignatureRejectedByAnalytics(t *testing.T) {
	withTestWallets(t)
	withAnalyticsKey(t)
	internal.Configuration.API.Analytics.APIKeys["testwallet"] = internal.AnalyticsAPIKey{PublicKey: testWalletKey.Public().(ed25519.PublicKey)}
	r := newSigReq(http.MethodGet, "/api/v1/analytics/transactions", "").build()
	r.Header.Set("X-Key-ID", "testwallet")
	if code, _ := serveAnalytics(r); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", code)
	}
}
