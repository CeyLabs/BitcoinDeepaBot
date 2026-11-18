package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/tma"
)

func TestP2PAuthRejectsBadInitData(t *testing.T) {
	savedTG, savedP2P := internal.Configuration.Telegram, internal.Configuration.API.P2P
	t.Cleanup(func() { internal.Configuration.Telegram, internal.Configuration.API.P2P = savedTG, savedP2P })
	internal.Configuration.Telegram.ApiKey = "123:REAL"
	internal.Configuration.API.P2P.InitDataMaxAge = 3600

	reached := false
	p := &P2PService{}
	h := p.Auth(func(w http.ResponseWriter, r *http.Request) { reached = true })

	user := tma.User{ID: 42, FirstName: "Eve"}
	cases := map[string]string{
		"no header":         "",
		"garbage":           "hello",
		"other bot's token": tma.Sign(user, "999:OTHER", time.Now(), nil),
		"expired":           tma.Sign(user, "123:REAL", time.Now().Add(-2*time.Hour), nil),
	}
	for name, initData := range cases {
		t.Run(name, func(t *testing.T) {
			reached = false
			r := httptest.NewRequest(http.MethodPost, "/api/v1/p2p/send", nil)
			if initData != "" {
				r.Header.Set(InitDataHeader, initData)
			}
			rec := httptest.NewRecorder()
			h(rec, r)
			if rec.Code != http.StatusUnauthorized || reached {
				t.Fatalf("got %d, handler reached=%v; want 401 and not reached", rec.Code, reached)
			}
			var body P2PErrorBody
			json.NewDecoder(rec.Body).Decode(&body)
			if body.Code != "unauthorized" {
				t.Fatalf("error code %q", body.Code)
			}
		})
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
