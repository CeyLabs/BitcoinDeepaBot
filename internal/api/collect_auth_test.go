package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// collectReq builds POST /api/v1/collect signed with the collect signed text
// for payer user. sign overrides the signed text when set.
func collectReq(user, body string, sign func(ts, nonce string) string) *http.Request {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := newNonce()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/collect", strings.NewReader(body))
	msg := CollectSigningString(http.MethodPost, "/api/v1/collect", "", ts, nonce, user, []byte(body))
	if sign != nil {
		msg = sign(ts, nonce)
	}
	r.Header.Set("X-Wallet-ID", "testwallet")
	r.Header.Set("X-Timestamp", ts)
	r.Header.Set("X-Nonce", nonce)
	r.Header.Set(UserHeader, user)
	r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(testWalletKey, []byte(msg))))
	return r
}

func serveCollectAuth(r *http.Request) (int, string, int64) {
	var wallet string
	var payer int64
	next := func(w http.ResponseWriter, r *http.Request) {
		wallet, _ = r.Context().Value("authenticated_wallet").(string)
		payer, _ = r.Context().Value(collectPayerKey{}).(int64)
		w.WriteHeader(http.StatusOK)
	}
	rec := httptest.NewRecorder()
	CollectAuthMiddleware(next)(rec, r)
	return rec.Code, wallet, payer
}

func TestCollectAuthMiddleware(t *testing.T) {
	withTestWallets(t)
	body := `{"amount":5000,"external_id":"order-812"}`

	if code, wallet, payer := serveCollectAuth(collectReq("111", body, nil)); code != http.StatusOK || wallet != "testwallet" || payer != 111 {
		t.Fatalf("valid: %d %q %d", code, wallet, payer)
	}

	// The payer is signed: changing X-BDB-User breaks the signature.
	r := collectReq("111", body, nil)
	r.Header.Set(UserHeader, "222")
	if code, _, _ := serveCollectAuth(r); code != http.StatusUnauthorized {
		t.Fatalf("swapped payer: %d", code)
	}

	// Missing, repeated or non-numeric X-BDB-User.
	r = collectReq("111", body, nil)
	r.Header.Del(UserHeader)
	if code, _, _ := serveCollectAuth(r); code != http.StatusUnauthorized {
		t.Fatalf("missing payer: %d", code)
	}
	r = collectReq("111", body, nil)
	r.Header.Add(UserHeader, "222")
	if code, _, _ := serveCollectAuth(r); code != http.StatusUnauthorized {
		t.Fatalf("repeated payer: %d", code)
	}
	for _, bad := range []string{"abc", "0", "-5", "0111", "+111"} {
		if code, _, _ := serveCollectAuth(collectReq(bad, body, nil)); code != http.StatusUnauthorized {
			t.Fatalf("payer %q: %d", bad, code)
		}
	}

	// A send signature (BDB-SEND-ED25519-V1) is not valid on collect.
	sendSig := func(ts, nonce string) string {
		return Ed25519SigningString(http.MethodPost, "/api/v1/collect", "", ts, nonce, []byte(body))
	}
	if code, _, _ := serveCollectAuth(collectReq("111", body, sendSig)); code != http.StatusUnauthorized {
		t.Fatalf("send signature accepted: %d", code)
	}
}
