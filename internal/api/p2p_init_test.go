package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/tma"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// AuthNewUser passes on the verified Telegram profile without needing a
// wallet, and still enforces the service signature and the credential.
func TestAuthNewUser(t *testing.T) {
	withP2PConfig(t)
	p := &P2PService{}
	initData := tma.Sign(tma.User{ID: 42, FirstName: "Eve", Username: "eve"}, "123:REAL", time.Now(), nil)

	var got *tb.User
	h := p.AuthNewUser(func(w http.ResponseWriter, r *http.Request) {
		got, _ = r.Context().Value(p2pTelegramCtxKey{}).(*tb.User)
	})
	rec := httptest.NewRecorder()
	h(rec, p2pRequest(http.MethodPost, "/api/v1/init", "{}", initData, time.Now(), p2pNonce(), testServiceKey))
	if got == nil || got.ID != 42 || got.Username != "eve" || got.FirstName != "Eve" {
		t.Fatalf("got %+v (status %d), want user 42 from init data", got, rec.Code)
	}

	got = nil
	rec = httptest.NewRecorder()
	h(rec, p2pRequest(http.MethodPost, "/api/v1/init", "{}", initData, time.Now(), p2pNonce(), testWalletKey))
	if got != nil || rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong service key: status %d, reached %v", rec.Code, got != nil)
	}

	rec = httptest.NewRecorder()
	bad := tma.Sign(tma.User{ID: 42}, "999:OTHER", time.Now(), nil)
	h(rec, p2pRequest(http.MethodPost, "/api/v1/init", "{}", bad, time.Now(), p2pNonce(), testServiceKey))
	if got != nil || rec.Code != http.StatusUnauthorized {
		t.Errorf("forged init data: status %d, reached %v", rec.Code, got != nil)
	}
}
