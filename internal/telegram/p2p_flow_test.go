package telegram

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/storage"
)

// buntBot is a TipBot with only an in-memory Bunt: enough for the parts of
// the P2P flow that never touch lnbits, Telegram or the user table.
func buntBot(t *testing.T) *TipBot {
	t.Helper()
	db := storage.NewBunt(":memory:")
	t.Cleanup(func() { db.Close() })
	return &TipBot{Bunt: db}
}

func newTestTransfer(kind string, amount int64) *P2PTransfer {
	t := &P2PTransfer{Base: storage.New(storage.ID(newP2PID())), Kind: kind, Status: P2PProcessing,
		FromTelegramID: 111, Amount: amount, IdempotencyKey: "test-key-" + newP2PID()}
	if kind == P2PKindInvoice {
		t.PaymentHash = strings.Repeat("cd", 32)
	}
	return t
}

// p2pEnd runs once even when several copies of a transfer race to end it,
// and gives back the counters and the invoice lock exactly once.
func TestP2PEndRunsOnce(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	day, unconf := p2pDailyKey(111, time.Now()), p2pUnconfirmedKey(111, time.Now())
	bot.Bunt.AddWithinLimit(day, 300000, 500000, time.Hour)
	bot.Bunt.AddWithinLimit(unconf, 40000, 50000, time.Hour)

	tr := newTestTransfer(P2PKindInvoice, 30000)
	tr.DailyCounted, tr.DailyKey, tr.UnconfirmedKey = true, day, unconf
	if err := bot.p2pSave(tr); err != nil {
		t.Fatal(err)
	}
	if !bot.p2pLockInvoice(tr) {
		t.Fatal("could not lock a free invoice")
	}

	var ended atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		c, err := bot.P2PGet(tr.ID)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(c *P2PTransfer) {
			defer wg.Done()
			if bot.p2pEnd(c, P2PFailed, "x", nil) {
				ended.Add(1)
			}
		}(c)
	}
	wg.Wait()

	if n := ended.Load(); n != 1 {
		t.Fatalf("p2pEnd succeeded %d times, want 1", n)
	}
	if used, _ := bot.Bunt.CounterValue(day); used != 270000 {
		t.Errorf("daily used = %d, want 270000", used)
	}
	if used, _ := bot.Bunt.CounterValue(unconf); used != 10000 {
		t.Errorf("unconfirmed used = %d, want 10000", used)
	}
	other := newTestTransfer(P2PKindInvoice, 30000)
	if !bot.p2pLockInvoice(other) {
		t.Error("invoice lock still held after the transfer failed")
	}
	stored, _ := bot.P2PGet(tr.ID)
	if stored.Status != P2PFailed || stored.DailyCounted || stored.UnconfirmedKey != "" {
		t.Errorf("stored transfer: %+v", stored)
	}
}

// A completed invoice keeps its lock and its counters.
func TestP2PEndCompletedKeepsLock(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	day := p2pDailyKey(111, time.Now())
	bot.Bunt.AddWithinLimit(day, 30000, 500000, time.Hour)
	tr := newTestTransfer(P2PKindInvoice, 30000)
	tr.DailyCounted, tr.DailyKey = true, day
	bot.p2pSave(tr)
	bot.p2pLockInvoice(tr)

	if !bot.p2pEnd(tr, P2PCompleted, "", func(t *P2PTransfer) { t.Preimage = "pre" }) {
		t.Fatal("p2pEnd refused")
	}
	if used, _ := bot.Bunt.CounterValue(day); used != 30000 {
		t.Errorf("daily used = %d, want 30000", used)
	}
	if bot.p2pLockInvoice(newTestTransfer(P2PKindInvoice, 30000)) {
		t.Error("a paid invoice could be locked again")
	}
	if stored, _ := bot.P2PGet(tr.ID); stored.Preimage != "pre" {
		t.Errorf("preimage not stored: %+v", stored)
	}
}

// Ending a transfer never releases a lock another transfer holds.
func TestP2PEndKeepsOtherTransfersLock(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	holder := newTestTransfer(P2PKindInvoice, 1000)
	bot.p2pSave(holder)
	if !bot.p2pLockInvoice(holder) {
		t.Fatal("could not lock")
	}
	loser := newTestTransfer(P2PKindInvoice, 1000)
	bot.p2pSave(loser)
	if bot.p2pLockInvoice(loser) {
		t.Fatal("second transfer took a held lock")
	}
	bot.p2pEnd(loser, P2PFailed, "This invoice is already being paid.", nil)
	if bot.p2pLockInvoice(newTestTransfer(P2PKindInvoice, 1000)) {
		t.Error("the loser released the holder's lock")
	}
}

// After a transfer ended, a save from a stale copy does not undo it.
func TestP2PSaveAfterEndIsRefused(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	tr := newTestTransfer(P2PKindInternal, 1000)
	bot.p2pSave(tr)
	stale, _ := bot.P2PGet(tr.ID)
	bot.p2pEnd(tr, P2PCompleted, "", nil)

	stale.Status = P2PInFlight
	if err := bot.p2pSave(stale); err != nil {
		t.Fatal(err)
	}
	if stored, _ := bot.P2PGet(tr.ID); stored.Status != P2PCompleted {
		t.Fatalf("stored status %s, want completed", stored.Status)
	}
	if stale.Status != P2PCompleted {
		t.Errorf("stale copy not reloaded: %s", stale.Status)
	}
}

// Small transfers share one daily allowance without confirmation, so a
// payment split into parts still needs confirmation.
func TestP2PNeedsConfirmationAcrossSplits(t *testing.T) {
	withP2PConfig(t) // confirm_threshold 50000
	bot := buntBot(t)
	steps := []struct {
		amount int64
		want   bool
	}{
		{20000, false}, {20000, false}, {20000, true}, {10000, false}, {1, true}, {60000, true},
	}
	for i, s := range steps {
		tr := newTestTransfer(P2PKindInternal, s.amount)
		if got := bot.p2pNeedsConfirmation(tr); got != s.want {
			t.Fatalf("step %d (%d sat): needs confirmation = %v, want %v", i, s.amount, got, s.want)
		}
		if !s.want && tr.UnconfirmedKey == "" {
			t.Fatalf("step %d: not counted", i)
		}
	}
	if left := bot.P2PUnconfirmedRemaining(111); left != 0 {
		t.Errorf("remaining = %d, want 0", left)
	}
}

// A lock held by a transfer that can no longer pay doesn't block the invoice.
func TestP2PLockInvoiceFreesStaleHolders(t *testing.T) {
	withP2PConfig(t)
	cases := map[string]func(bot *TipBot, holder *P2PTransfer){
		"unconfirmed past its deadline": func(bot *TipBot, h *P2PTransfer) {
			h.Status, h.ConfirmBy = P2PAwaitingConfirmation, time.Now().Add(-time.Minute)
			bot.p2pSave(h)
		},
		"record missing": func(bot *TipBot, h *P2PTransfer) {
			bot.Bunt.DeleteKey(h.ID)
		},
		"ended without paying (older lock left behind)": func(bot *TipBot, h *P2PTransfer) {
			h.Status = P2PFailed
			bot.Bunt.SetWithTTL(h, time.Hour)
		},
	}
	for name, makeStale := range cases {
		t.Run(name, func(t *testing.T) {
			bot := buntBot(t)
			holder := newTestTransfer(P2PKindInvoice, 1000)
			bot.p2pSave(holder)
			if !bot.p2pLockInvoice(holder) {
				t.Fatal("could not lock")
			}
			makeStale(bot, holder)
			if !bot.p2pLockInvoice(newTestTransfer(P2PKindInvoice, 1000)) {
				t.Fatal("stale holder still blocks the invoice")
			}
		})
	}

	t.Run("live holder blocks", func(t *testing.T) {
		bot := buntBot(t)
		holder := newTestTransfer(P2PKindInvoice, 1000)
		holder.Status, holder.ConfirmBy = P2PAwaitingConfirmation, time.Now().Add(time.Minute)
		bot.p2pSave(holder)
		bot.p2pLockInvoice(holder)
		if bot.p2pLockInvoice(newTestTransfer(P2PKindInvoice, 1000)) {
			t.Fatal("took the lock from a transfer still awaiting confirmation")
		}
	})
}

func TestP2PExpiresIn(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute: "10 minutes", 9*time.Minute + time.Second: "10 minutes",
		61 * time.Second: "2 minutes", 30 * time.Second: "1 minute", -time.Second: "1 minute",
	} {
		if got := p2pExpiresIn(d); got != want {
			t.Errorf("p2pExpiresIn(%v) = %q, want %q", d, got, want)
		}
	}
}
