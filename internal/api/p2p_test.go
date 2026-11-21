package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/netutil"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/tgoidc"
	"github.com/LightningTipBot/LightningTipBot/internal/tma"
)

var testServiceKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))

// p2pRequest builds a request signed by the Mini App server.
func p2pRequest(method, path, body, initData string, ts time.Time, nonce string, key ed25519.PrivateKey) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if initData != "" {
		r.Header.Set(InitDataHeader, initData)
	}
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	msg := P2PSigningString(method, r.URL.EscapedPath(), r.URL.RawQuery, tsStr, nonce, initData, []byte(body))
	r.Header.Set("X-Timestamp", tsStr)
	r.Header.Set("X-Nonce", nonce)
	r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(msg))))
	return r
}

var p2pNonceN atomic.Int64

func p2pNonce() string { return "p2p-test-nonce-" + strconv.FormatInt(p2pNonceN.Add(1), 10) }

func withP2PConfig(t *testing.T) {
	t.Helper()
	savedTG, savedP2P := internal.Configuration.Telegram, internal.Configuration.API.P2P
	t.Cleanup(func() { internal.Configuration.Telegram, internal.Configuration.API.P2P = savedTG, savedP2P })
	internal.Configuration.Telegram.ApiKey = "123:REAL"
	internal.Configuration.API.P2P.InitDataMaxAge = 3600
	internal.Configuration.API.P2P.ServiceKey = testServiceKey.Public().(ed25519.PublicKey)
}

// serveP2PAuth runs r through AuthNewUser (login, Telegram credential) or
// Auth (X-BDB-User) and reports the status, error code and whether the
// handler was reached.
func serveP2PAuth(r *http.Request, login bool) (int, string, bool) {
	reached := false
	p := &P2PService{}
	h := p.Auth
	if login {
		h = p.AuthNewUser
	}
	rec := httptest.NewRecorder()
	h(func(w http.ResponseWriter, r *http.Request) { reached = true })(rec, r)
	var body P2PErrorBody
	json.NewDecoder(rec.Body).Decode(&body)
	return rec.Code, body.Code, reached
}

func TestP2PAuthRejectsBadInitData(t *testing.T) {
	withP2PConfig(t)
	user := tma.User{ID: 42, FirstName: "Eve"}
	cases := map[string]struct {
		initData string
	}{
		"no init data":      {""},
		"garbage":           {"hello"},
		"other bot's token": {tma.Sign(user, "999:OTHER", time.Now(), nil)},
		"expired":           {tma.Sign(user, "123:REAL", time.Now().Add(-2*time.Hour), nil)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := p2pRequest(http.MethodPost, "/api/v1/init", "{}", tc.initData, time.Now(), p2pNonce(), testServiceKey)
			code, errCode, reached := serveP2PAuth(r, true)
			if code != http.StatusUnauthorized || reached || errCode != "unauthorized" {
				t.Fatalf("got %d %q, reached=%v; want 401 unauthorized", code, errCode, reached)
			}
		})
	}
}

func TestP2PAuthRejectsBadServiceSignature(t *testing.T) {
	withP2PConfig(t)
	initData := tma.Sign(tma.User{ID: 42, FirstName: "Eve"}, "123:REAL", time.Now(), nil)
	other := tma.Sign(tma.User{ID: 43, FirstName: "Mallory"}, "123:REAL", time.Now(), nil)
	_, otherKey, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	base := func() *http.Request {
		return p2pRequest(http.MethodPost, "/api/v1/p2p/send", `{"amount":1}`, initData, now, p2pNonce(), testServiceKey)
	}

	cases := map[string]func() *http.Request{
		"no signature": func() *http.Request { r := base(); r.Header.Del("X-Signature"); return r },
		"wrong key": func() *http.Request {
			return p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, now, p2pNonce(), otherKey)
		},
		"expired": func() *http.Request {
			return p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, now.Add(-301*time.Second), p2pNonce(), testServiceKey)
		},
		"bad nonce": func() *http.Request {
			return p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, now, "short", testServiceKey)
		},
		"body changed":      func() *http.Request { r := base(); r.Body = io.NopCloser(strings.NewReader(`{"amount":9}`)); return r },
		"path changed":      func() *http.Request { r := base(); r.URL.Path = "/api/v1/p2p/invoice/pay"; return r },
		"init data swapped": func() *http.Request { r := base(); r.Header.Set(InitDataHeader, other); return r },
		"no service key": func() *http.Request {
			internal.Configuration.API.P2P.ServiceKey = nil
			return base()
		},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			internal.Configuration.API.P2P.ServiceKey = testServiceKey.Public().(ed25519.PublicKey)
			code, errCode, reached := serveP2PAuth(req(), false)
			if code != http.StatusUnauthorized || reached || errCode != "bad_service_signature" {
				t.Fatalf("got %d %q, reached=%v; want 401 bad_service_signature", code, errCode, reached)
			}
		})
	}
}

func TestP2PAuthIPAllowlist(t *testing.T) {
	withP2PConfig(t)
	savedBot := internal.Configuration.Bot
	t.Cleanup(func() { internal.Configuration.Bot = savedBot })
	internal.Configuration.Bot.TrustedProxyNets = nil
	internal.Configuration.API.P2P.AllowedNets, _ = netutil.ParseNetworks([]string{"203.0.113.7"})
	initData := tma.Sign(tma.User{ID: 42, FirstName: "Eve"}, "123:REAL", time.Now().Add(-2*time.Hour), nil)

	r := p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, time.Now(), p2pNonce(), testServiceKey)
	r.RemoteAddr = "198.51.100.4:1"
	if code, errCode, reached := serveP2PAuth(r, false); code != http.StatusForbidden || errCode != "forbidden" || reached {
		t.Fatalf("outside IP: got %d %q reached=%v, want 403 forbidden", code, errCode, reached)
	}

	// From the allowed IP the request gets past the IP check (and then fails
	// on the expired init data).
	r = p2pRequest(http.MethodPost, "/api/v1/init", "{}", initData, time.Now(), p2pNonce(), testServiceKey)
	r.RemoteAddr = "203.0.113.7:1"
	if code, errCode, _ := serveP2PAuth(r, true); code != http.StatusUnauthorized || errCode != "unauthorized" {
		t.Fatalf("allowed IP: got %d %q, want 401 unauthorized", code, errCode)
	}
}

func TestVerifyP2PServiceSig(t *testing.T) {
	key := testServiceKey.Public().(ed25519.PublicKey)
	initData := "user=x&hash=y"
	r := p2pRequest(http.MethodPost, "/api/v1/p2p/send?a=1", `{"amount":1}`, initData, time.Now(), p2pNonce(), testServiceKey)
	if err := verifyP2PServiceSig(httptest.NewRecorder(), r, key, time.Now()); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if b, _ := io.ReadAll(r.Body); string(b) != `{"amount":1}` {
		t.Fatalf("body not restored: %q", b)
	}
	// The same nonce again is a replay.
	again := p2pRequest(http.MethodPost, "/api/v1/p2p/send?a=1", `{"amount":1}`, initData, time.Now(), r.Header.Get("X-Nonce"), testServiceKey)
	if err := verifyP2PServiceSig(httptest.NewRecorder(), again, key, time.Now()); err == nil {
		t.Fatal("replayed nonce accepted")
	}
	big := p2pRequest(http.MethodPost, "/api/v1/p2p/send", strings.Repeat("a", p2pMaxBodySize+1), initData, time.Now(), p2pNonce(), testServiceKey)
	if err := verifyP2PServiceSig(httptest.NewRecorder(), big, key, time.Now()); err == nil {
		t.Fatal("oversized body accepted")
	}
}

// The path is signed as sent, the nonce outlives its timestamp, and body
// errors are told apart from signature failures.
func TestVerifyP2PServiceSigEdgeCases(t *testing.T) {
	key := testServiceKey.Public().(ed25519.PublicKey)
	initData := "user=x&hash=y"
	now := time.Now()

	esc := p2pRequest(http.MethodGet, "/api/v1/p2p/transfers/a%20b", "", initData, now, p2pNonce(), testServiceKey)
	if err := verifyP2PServiceSig(httptest.NewRecorder(), esc, key, now); err != nil {
		t.Fatalf("escaped path rejected: %v", err)
	}

	late := p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, now.Add(p2pSigMaxSkew*time.Second), p2pNonce(), testServiceKey)
	if err := verifyP2PServiceSig(httptest.NewRecorder(), late, key, now); err != nil {
		t.Fatalf("latest timestamp rejected: %v", err)
	}
	usedSignatures.mu.Lock()
	expiry := usedSignatures.seen["p2p:"+late.Header.Get("X-Nonce")]
	usedSignatures.mu.Unlock()
	if last := time.Unix(now.Unix()+2*p2pSigMaxSkew+1, 0); expiry.Before(last) {
		t.Fatalf("nonce forgotten at %v, before its timestamp expires at %v", expiry, last)
	}

	big := p2pRequest(http.MethodPost, "/api/v1/p2p/send", strings.Repeat("a", p2pMaxBodySize+1), initData, now, p2pNonce(), testServiceKey)
	if err := verifyP2PServiceSig(httptest.NewRecorder(), big, key, now); !errors.Is(err, errP2PBodyTooLarge) {
		t.Fatalf("oversized body: %v", err)
	}
	broken := p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, now, p2pNonce(), testServiceKey)
	broken.Body = io.NopCloser(iotestErrReader{})
	if err := verifyP2PServiceSig(httptest.NewRecorder(), broken, key, now); !errors.Is(err, errP2PBodyRead) {
		t.Fatalf("unreadable body: %v", err)
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(3, time.Minute)
	now := time.Unix(1_000_000, 0)
	for i := 0; i < 3; i++ {
		if !l.allow("a", now) {
			t.Fatalf("event %d refused", i)
		}
	}
	if l.allow("a", now) {
		t.Fatal("4th event within a minute allowed")
	}
	if !l.allow("b", now) {
		t.Fatal("another sender was limited")
	}
	if !l.allow("a", now.Add(61*time.Second)) {
		t.Fatal("still limited after the window")
	}
}

func TestP2PWriteErrStatus(t *testing.T) {
	for code, want := range map[string]int{
		"recipient_not_found":  http.StatusNotFound,
		"sender_not_allowed":   http.StatusForbidden,
		"idempotency_conflict": http.StatusConflict,
		"amount_too_large":     http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		p2pWriteErr(rec, &telegram.P2PError{Code: code, Message: "x"})
		if rec.Code != want {
			t.Errorf("%s: got %d, want %d", code, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	p2pWriteErr(rec, http.ErrBodyNotAllowed)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("unexpected error: got %d, want 500", rec.Code)
	}
}

func TestTransferResponse(t *testing.T) {
	tr := &telegram.P2PTransfer{
		Base: storage.New(storage.ID("p2p-1")), Kind: telegram.P2PKindInternal, Status: telegram.P2PProcessing,
		ToTelegramID: 7, ToUsername: "bob", ToName: "Bob", Amount: 60000, Preimage: "secret",
		IdempotencyKey: "never-shown", InternalPaymentHash: "abc123",
	}
	resp := toTransferResponse(tr)
	if resp.PaymentHash != "abc123" {
		t.Fatalf("internal transfer should show its invoice hash, got %q", resp.PaymentHash)
	}
	if resp.Preimage != "" {
		t.Fatal("preimage shown before completion")
	}
	if resp.To == nil || resp.To.TelegramID != 7 {
		t.Fatalf("bad response: %+v", resp)
	}
	if statusCode(tr) != http.StatusAccepted {
		t.Fatal("processing should be 202")
	}
	b, _ := json.Marshal(resp)
	if json.Valid(b) && contains(string(b), "never-shown") {
		t.Fatal("idempotency key leaked in the response")
	}
	tr.Status = telegram.P2PCompleted
	if r := toTransferResponse(tr); r.Preimage != "secret" || statusCode(tr) != http.StatusOK {
		t.Fatalf("completed response wrong: %+v", r)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// withTestOIDC turns on Telegram login with a test signing key served as a
// JWKS, and returns that key.
func withTestOIDC(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	jwks, _ := json.Marshal(map[string]interface{}{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "kid": "k1",
		"x": base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
	}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(jwks) }))
	saved := tgoidc.Default
	tgoidc.Default = &tgoidc.KeySet{URL: srv.URL, Client: srv.Client()}
	t.Cleanup(func() { tgoidc.Default = saved; srv.Close() })
	internal.Configuration.API.P2P.OIDCEnabled = true
	internal.Configuration.API.P2P.OIDCClientID = "123"
	return priv
}

func testIDToken(priv ed25519.PrivateKey, userID int64, issued time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "k1"})
	c, _ := json.Marshal(map[string]interface{}{
		"iss": tgoidc.Issuer, "aud": "123", "iat": issued.Unix(), "exp": issued.Add(time.Hour).Unix(),
		"id": userID, "preferred_username": "alice",
	})
	input := enc(h) + "." + enc(c)
	return input + "." + enc(ed25519.Sign(priv, []byte(input)))
}

// p2pTokenRequest is a service-signed request carrying an ID token.
func p2pTokenRequest(path, token string, key ed25519.PrivateKey) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	r.Header.Set(IDTokenHeader, token)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := p2pNonce()
	msg := P2PSigningString(http.MethodPost, path, "", ts, nonce, token, []byte("{}"))
	r.Header.Set("X-Timestamp", ts)
	r.Header.Set("X-Nonce", nonce)
	r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(msg))))
	return r
}

func TestP2PIdentifyWithIDToken(t *testing.T) {
	withP2PConfig(t)
	priv := withTestOIDC(t)
	cfg := internal.Configuration.API.P2P
	now := time.Now()

	r := httptest.NewRequest(http.MethodGet, "/api/v1/p2p/me", nil)
	r.Header.Set(IDTokenHeader, testIDToken(priv, 987654321, now.Add(-time.Minute)))
	if id, ok := p2pIdentify(httptest.NewRecorder(), r, cfg, 10*time.Minute, now); !ok || id != 987654321 {
		t.Fatalf("valid token: id %d ok %v", id, ok)
	}

	cases := map[string]func(r *http.Request){
		"login too old for spends": func(r *http.Request) {
			r.Header.Set(IDTokenHeader, testIDToken(priv, 987654321, now.Add(-20*time.Minute)))
		},
		"both credentials": func(r *http.Request) {
			r.Header.Set(IDTokenHeader, testIDToken(priv, 987654321, now))
			r.Header.Set(InitDataHeader, tma.Sign(tma.User{ID: 42}, "123:REAL", now, nil))
		},
		"forged token": func(r *http.Request) {
			_, other, _ := ed25519.GenerateKey(nil)
			r.Header.Set(IDTokenHeader, testIDToken(other, 987654321, now))
		},
	}
	for name, set := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/p2p/me", nil)
		set(r)
		rec := httptest.NewRecorder()
		if _, ok := p2pIdentify(rec, r, cfg, 10*time.Minute, now); ok || rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: ok=%v code=%d, want 401", name, ok, rec.Code)
		}
	}

	cfg.OIDCEnabled = false
	r = httptest.NewRequest(http.MethodGet, "/api/v1/p2p/me", nil)
	r.Header.Set(IDTokenHeader, testIDToken(priv, 987654321, now))
	if _, ok := p2pIdentify(httptest.NewRecorder(), r, cfg, 10*time.Minute, now); ok {
		t.Error("ID token accepted with Telegram login turned off")
	}
}

func TestP2PLoginWithIDToken(t *testing.T) {
	withP2PConfig(t)
	priv := withTestOIDC(t)

	// A login older than init_data_max_age can't log in.
	old := testIDToken(priv, 987654321, time.Now().Add(-2*time.Hour))
	if code, errCode, reached := serveP2PAuth(p2pTokenRequest("/api/v1/init", old, testServiceKey), true); code != http.StatusUnauthorized || errCode != "unauthorized" || reached {
		t.Fatalf("old login: got %d %q reached=%v", code, errCode, reached)
	}

	// The service signature covers the ID token: swapping in another
	// user's token breaks it.
	r := p2pTokenRequest("/api/v1/init", testIDToken(priv, 1, time.Now()), testServiceKey)
	r.Header.Set(IDTokenHeader, testIDToken(priv, 2, time.Now()))
	if code, errCode, _ := serveP2PAuth(r, true); code != http.StatusUnauthorized || errCode != "bad_service_signature" {
		t.Fatalf("swapped token: got %d %q, want 401 bad_service_signature", code, errCode)
	}
}
