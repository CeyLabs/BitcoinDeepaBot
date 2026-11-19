package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/tma"
)

// p2pRequestV2 builds a V2 request: the user is named in X-BDB-User and the
// signature covers it instead of a Telegram credential.
func p2pRequestV2(method, path, body, userID string, key ed25519.PrivateKey) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set(UserHeader, userID)
	ts, nonce := strconv.FormatInt(time.Now().Unix(), 10), p2pNonce()
	msg := P2PSigningStringV2(method, r.URL.EscapedPath(), r.URL.RawQuery, ts, nonce, userID, []byte(body))
	r.Header.Set("X-Timestamp", ts)
	r.Header.Set("X-Nonce", nonce)
	r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(msg))))
	return r
}

func v2Service(t *testing.T) *P2PService {
	t.Helper()
	db := storage.NewBunt(":memory:")
	t.Cleanup(func() { db.Close() })
	return &P2PService{Bot: &telegram.TipBot{Bunt: db}}
}

func errCode(rec *httptest.ResponseRecorder) string {
	var b P2PErrorBody
	json.NewDecoder(rec.Body).Decode(&b)
	return b.Code
}

func TestP2PServiceUserAuth(t *testing.T) {
	withP2PConfig(t)
	p := v2Service(t)
	// call runs r as a normal P2P call (login=false) or as /api/v1/init (login=true).
	call := func(r *http.Request, normal bool) (int64, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		u, ok := p.p2pVerifyCaller(rec, r, !normal)
		if !ok {
			return 0, rec
		}
		return u.ID, rec
	}

	// Not linked yet: refused.
	if id, rec := call(p2pRequestV2(http.MethodPost, "/api/v1/p2p/send", "{}", "42", testServiceKey), true); id != 0 || rec.Code != http.StatusForbidden || errCode(rec) != "login_required" {
		t.Fatalf("unlinked user: id %d, %d %s", id, rec.Code, errCode(rec))
	}

	// A Telegram-verified (V1) call links the user.
	initData := tma.Sign(tma.User{ID: 42, FirstName: "Eve"}, "123:REAL", time.Now(), nil)
	if id, rec := call(p2pRequest(http.MethodPost, "/api/v1/init", "{}", initData, time.Now(), p2pNonce(), testServiceKey), false); id != 42 {
		t.Fatalf("V1 call failed: %d %s", rec.Code, errCode(rec))
	}

	// Now V2 works, for spends too.
	if id, rec := call(p2pRequestV2(http.MethodPost, "/api/v1/p2p/send", "{}", "42", testServiceKey), true); id != 42 {
		t.Fatalf("linked user: %d %s", rec.Code, errCode(rec))
	}

	// Another user, never verified with Telegram: refused.
	if id, _ := call(p2pRequestV2(http.MethodGet, "/api/v1/p2p/me", "", "43", testServiceKey), true); id != 0 {
		t.Fatal("unlinked user 43 accepted")
	}

	// /init never accepts V2.
	if id, rec := call(p2pRequestV2(http.MethodPost, "/api/v1/init", "{}", "42", testServiceKey), false); id != 0 || errCode(rec) != "login_required" {
		t.Fatalf("init accepted V2: %d %s", rec.Code, errCode(rec))
	}

	// The user ID is signed: changing it breaks the signature.
	r := p2pRequestV2(http.MethodGet, "/api/v1/p2p/me", "", "42", testServiceKey)
	r.Header.Set(UserHeader, "43")
	if id, rec := call(r, true); id != 0 || errCode(rec) != "bad_service_signature" {
		t.Fatalf("tampered user accepted: %d %s", rec.Code, errCode(rec))
	}

	// Signed by someone else: refused.
	if id, _ := call(p2pRequestV2(http.MethodGet, "/api/v1/p2p/me", "", "42", testWalletKey), true); id != 0 {
		t.Fatal("wrong key accepted")
	}

	// X-BDB-User plus a credential: refused.
	both := p2pRequestV2(http.MethodGet, "/api/v1/p2p/me", "", "42", testServiceKey)
	both.Header.Set(InitDataHeader, initData)
	if id, _ := call(both, true); id != 0 {
		t.Fatal("both identities accepted")
	}

	// A Telegram credential on a normal call (instead of X-BDB-User): refused.
	if id, rec := call(p2pRequest(http.MethodPost, "/api/v1/p2p/send", "{}", initData, time.Now(), p2pNonce(), testServiceKey), true); id != 0 || rec.Code != http.StatusUnauthorized {
		t.Fatalf("credential on a normal call accepted: %d", rec.Code)
	}

	// No identity at all: refused.
	if id, rec := call(p2pRequest(http.MethodGet, "/api/v1/p2p/me", "", "", time.Now(), p2pNonce(), testServiceKey), true); id != 0 || rec.Code != http.StatusUnauthorized {
		t.Fatalf("no identity accepted: %d", rec.Code)
	}
}
