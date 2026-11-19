package telegram

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	irate "github.com/LightningTipBot/LightningTipBot/internal/rate"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/eko/gocache/store"
	gocache "github.com/patrickmn/go-cache"
	tb "gopkg.in/lightningtipbot/telebot.v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// These tests use a real sqlite user table, so they need cgo.

// p2pTestBot is a TipBot with a real sqlite user table and an in-memory Bunt,
// holding one eligible user (Telegram ID 111).
func p2pTestBot(t *testing.T) *TipBot {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "users.db")), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, db)
	if err := db.AutoMigrate(&lnbits.User{}); err != nil {
		t.Fatal(err)
	}
	u := &lnbits.User{Name: "111", ID: "lnb-1", Initialized: true,
		Telegram: &tb.User{ID: 111, FirstName: "Alice", Username: "alice"},
		Wallet:   &lnbits.Wallet{ID: "w1", Adminkey: "adm"}}
	if err := db.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	return &TipBot{DB: &Databases{Users: db}, Bunt: storage.NewBunt(":memory:")}
}

// fakeBackends serves lnbits payment lookups with paymentJSON and counts
// Telegram sendMessage calls.
func fakeBackends(t *testing.T, bot *TipBot, paymentJSON string) *atomic.Int64 {
	t.Helper()
	var tgSends atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/payments/") {
			time.Sleep(50 * time.Millisecond)
			w.Write([]byte(paymentJSON))
			return
		}
		if strings.Contains(r.URL.Path, "sendMessage") {
			tgSends.Add(1)
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":111}}}`))
	}))
	t.Cleanup(srv.Close)
	irate.Start()
	bot.Cache = Cache{GoCacheStore: store.NewGoCache(gocache.New(time.Minute, time.Minute), nil)}
	bot.Client = lnbits.NewClient("k", "", srv.URL)
	tg, err := tb.NewBot(tb.Settings{URL: srv.URL, Token: "1:x", Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	bot.Telegram = tg
	return &tgSends
}

// An invoice payment that fails the daily-limit check must release the
// per-invoice lock, or the invoice can't be paid through P2P for 30 days.
// (Review finding 1.)
func TestP2PInvoiceLockReleasedOnDailyLimit(t *testing.T) {
	withP2PConfig(t)
	bot := p2pTestBot(t)
	hash := strings.Repeat("aa", 32)

	day := p2pDailyKey(111, time.Now())
	bot.Bunt.AddWithinLimit(day, 500000, 500000, 48*time.Hour) // today's limit already used

	tr := &P2PTransfer{Base: storage.New(storage.ID(newP2PID())), Kind: P2PKindInvoice, Status: P2PProcessing,
		FromTelegramID: 111, PaymentHash: hash, Amount: 1000, IdempotencyKey: "repro-key-1"}
	if !bot.p2pLockInvoice(tr) { // as P2PStartInvoice does
		t.Fatal("could not lock")
	}
	bot.p2pExecute(tr)

	if tr.Status != P2PFailed {
		t.Fatalf("status %s, want failed", tr.Status)
	}
	if ok, _ := bot.Bunt.ClaimOnce(p2pInvoiceLock(hash), p2pRecordTTL); !ok {
		t.Errorf("invoice lock still held after %q", tr.Error)
	}
	if used, _ := bot.Bunt.CounterValue(day); used != 500000 {
		t.Errorf("daily used = %d, want 500000 unchanged", used)
	}
}

// Two overlapping GET /transfers/{id} polls on an in-flight invoice payment
// that then fails must apply the outcome once. (Review finding 3.)
func TestP2PRefreshAppliesSettlementOnce(t *testing.T) {
	withP2PConfig(t)
	bot := p2pTestBot(t)
	tgSends := fakeBackends(t, bot, `{"paid": false, "status": "failed", "details": {"status": "failed"}}`)

	day := p2pDailyKey(111, time.Now())
	bot.Bunt.AddWithinLimit(day, 300000, 500000, 48*time.Hour) // includes this 100k transfer
	tr := &P2PTransfer{Base: storage.New(storage.ID(newP2PID())), Kind: P2PKindInvoice, Status: P2PInFlight,
		FromTelegramID: 111, PaymentHash: strings.Repeat("ab", 32), Amount: 100000, IdempotencyKey: "repro-key-2",
		DailyCounted: true, DailyKey: day}
	if err := bot.p2pSave(tr); err != nil {
		t.Fatal(err)
	}

	// Each request handler loads its own copy, then refreshes it.
	a, _ := bot.P2PGet(tr.ID)
	b, _ := bot.P2PGet(tr.ID)
	var wg sync.WaitGroup
	for _, c := range []*P2PTransfer{a, b} {
		wg.Add(1)
		go func(c *P2PTransfer) { defer wg.Done(); bot.P2PRefresh(c) }(c)
	}
	wg.Wait()

	if used, _ := bot.Bunt.CounterValue(day); used != 200000 {
		t.Errorf("daily used = %d after one failed 100k payment, want 200000", used)
	}
	if n := tgSends.Load(); n != 1 {
		t.Errorf("user was sent %d failure messages, want 1", n)
	}
}

// A transfer cut off while "processing" (e.g. by a restart) is resolved on
// the next poll instead of staying stuck. (Review finding 6.)
func TestP2PRefreshResolvesStuckProcessing(t *testing.T) {
	stale := func(bot *TipBot, tr *P2PTransfer) {
		tr.Base.UpdatedAt = time.Now().Add(-5 * time.Minute)
		bot.Bunt.SetWithTTL(tr, time.Hour) // bypass p2pSave, which stamps UpdatedAt
	}

	t.Run("internal send that never created an invoice fails and refunds", func(t *testing.T) {
		withP2PConfig(t)
		bot := p2pTestBot(t)
		day := p2pDailyKey(111, time.Now())
		bot.Bunt.AddWithinLimit(day, 1000, 500000, time.Hour)
		tr := &P2PTransfer{Base: storage.New(storage.ID(newP2PID())), Kind: P2PKindInternal, Status: P2PProcessing,
			FromTelegramID: 111, ToTelegramID: 222, Amount: 1000, IdempotencyKey: "stuck-1", DailyCounted: true, DailyKey: day}
		stale(bot, tr)
		bot.P2PRefresh(tr)
		if tr.Status != P2PFailed {
			t.Fatalf("status %s, want failed", tr.Status)
		}
		if used, _ := bot.Bunt.CounterValue(day); used != 0 {
			t.Errorf("daily used = %d, want 0", used)
		}
	})

	t.Run("internal send that settled completes", func(t *testing.T) {
		withP2PConfig(t)
		bot := p2pTestBot(t)
		fakeBackends(t, bot, `{"paid": true, "preimage": "`+strings.Repeat("11", 32)+`", "details": {"status": "success"}}`)
		tr := &P2PTransfer{Base: storage.New(storage.ID(newP2PID())), Kind: P2PKindInternal, Status: P2PProcessing,
			FromTelegramID: 111, ToTelegramID: 222, Amount: 1000, IdempotencyKey: "stuck-2",
			InternalPaymentHash: strings.Repeat("ef", 32)}
		stale(bot, tr)
		bot.P2PRefresh(tr)
		if tr.Status != P2PCompleted {
			t.Fatalf("status %s, want completed", tr.Status)
		}
	})

	t.Run("recent processing transfer is left alone", func(t *testing.T) {
		withP2PConfig(t)
		bot := p2pTestBot(t)
		tr := &P2PTransfer{Base: storage.New(storage.ID(newP2PID())), Kind: P2PKindInternal, Status: P2PProcessing,
			FromTelegramID: 111, Amount: 1000, IdempotencyKey: "stuck-3"}
		bot.p2pSave(tr)
		bot.P2PRefresh(tr)
		if tr.Status != P2PProcessing {
			t.Fatalf("status %s, want processing", tr.Status)
		}
	})
}
