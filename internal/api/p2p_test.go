package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
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
	msg := P2PSigningString(method, r.URL.Path, r.URL.RawQuery, tsStr, nonce, initData, []byte(body))
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
	internal.Configuration.API.P2P.SpendInitDataMaxAge = 600
	internal.Configuration.API.P2P.ServiceKey = testServiceKey.Public().(ed25519.PublicKey)
}

// serveP2PAuth runs r through Auth or AuthSpend and reports the status, error
// code and whether the handler was reached.
func serveP2PAuth(r *http.Request, spend bool) (int, string, bool) {
	reached := false
	p := &P2PService{}
	h := p.Auth
	if spend {
		h = p.AuthSpend
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
		spend    bool
	}{
		"no init data":       {"", false},
		"garbage":            {"hello", false},
		"other bot's token":  {tma.Sign(user, "999:OTHER", time.Now(), nil), false},
		"expired":            {tma.Sign(user, "123:REAL", time.Now().Add(-2*time.Hour), nil), false},
		"too old for spends": {tma.Sign(user, "123:REAL", time.Now().Add(-20*time.Minute), nil), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", tc.initData, time.Now(), p2pNonce(), testServiceKey)
			code, errCode, reached := serveP2PAuth(r, tc.spend)
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
		"no signature":      func() *http.Request { r := base(); r.Header.Del("X-Signature"); return r },
		"wrong key":         func() *http.Request { return p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, now, p2pNonce(), otherKey) },
		"expired":           func() *http.Request { return p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, now.Add(-301*time.Second), p2pNonce(), testServiceKey) },
		"bad nonce":         func() *http.Request { return p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, now, "short", testServiceKey) },
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
			code, errCode, reached := serveP2PAuth(req(), true)
			if code != http.StatusUnauthorized || reached || errCode != "bad_service_signature" {
				t.Fatalf("got %d %q, reached=%v; want 401 bad_service_signature", code, errCode, reached)
			}
		})
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
		Base: storage.New(storage.ID("p2p-1")), Kind: telegram.P2PKindInternal, Status: telegram.P2PAwaitingConfirmation,
		ToTelegramID: 7, ToUsername: "bob", ToName: "Bob", Amount: 60000, Preimage: "secret",
		ConfirmBy: time.Now().Add(time.Minute), IdempotencyKey: "never-shown",
	}
	resp := toTransferResponse(tr)
	if resp.Preimage != "" {
		t.Fatal("preimage shown before completion")
	}
	if resp.ConfirmBy == nil || resp.To == nil || resp.To.TelegramID != 7 {
		t.Fatalf("bad response: %+v", resp)
	}
	if statusCode(tr) != http.StatusAccepted {
		t.Fatal("awaiting confirmation should be 202")
	}
	b, _ := json.Marshal(resp)
	if json.Valid(b) && contains(string(b), "never-shown") {
		t.Fatal("idempotency key leaked in the response")
	}
	tr.Status = telegram.P2PCompleted
	if r := toTransferResponse(tr); r.Preimage != "secret" || r.ConfirmBy != nil || statusCode(tr) != http.StatusOK {
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
