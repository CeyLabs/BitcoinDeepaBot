package telegram

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

func newTestCollect(t *testing.T, bot *TipBot, expiresIn time.Duration) *CollectRequest {
	t.Helper()
	c := &CollectRequest{Base: storage.New(storage.ID(newCollectID())), Status: CollectPending, WalletID: "dca",
		ToTelegramID: 999, PayerTelegramID: 111, Amount: 20000, ExternalID: "ext-12345678", ExpiresAt: time.Now().Add(expiresIn)}
	if err := bot.collectSave(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func testPayer(id int64) *lnbits.User { return &lnbits.User{Telegram: &tb.User{ID: id}} }

// Only one approval wins, and it creates exactly one transfer with a fixed ID.
func TestCollectApproveOnce(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	c := newTestCollect(t, bot, time.Minute)

	var created atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cc, err := bot.CollectGet(c.ID)
			if err != nil {
				t.Error(err)
				return
			}
			tr, ok, err := bot.collectApprove(cc, testPayer(111))
			if ok {
				created.Add(1)
			}
			if err == nil && tr.ID != c.TransferID() {
				t.Errorf("transfer ID %s, want %s", tr.ID, c.TransferID())
			}
		}()
	}
	wg.Wait()
	if n := created.Load(); n != 1 {
		t.Fatalf("approved %d times, want 1", n)
	}
	tr, err := bot.P2PGet(c.TransferID())
	if err != nil || tr.CollectID != c.ID || tr.FromTelegramID != 111 || tr.ToTelegramID != 999 || tr.Amount != 20000 {
		t.Fatalf("bad transfer %+v, %v", tr, err)
	}
	if got, _ := bot.CollectGet(c.ID); got.Status != CollectApproved {
		t.Errorf("status %s, want approved", got.Status)
	}
}

// Only the payer named on the request is charged.
func TestCollectOwnership(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	c := newTestCollect(t, bot, time.Minute)
	if _, _, err := bot.collectApprove(c, testPayer(222)); err == nil {
		t.Error("charged another user")
	}
	if _, err := bot.CollectCharge(c.ID, testPayer(222)); err == nil {
		t.Error("charged another user")
	}
	if _, err := bot.P2PGet(c.TransferID()); err == nil {
		t.Error("request has a transfer after a wrong payer")
	}
}

// An expired request can't be approved, even if its record still says pending.
func TestCollectExpired(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	c := newTestCollect(t, bot, -time.Second)
	if _, _, err := bot.collectApprove(c, testPayer(111)); err == nil {
		t.Fatal("approved an expired request")
	}
	got, err := bot.CollectGet(c.ID)
	if err != nil || got.Status != CollectExpired {
		t.Fatalf("status %v, %v; want expired", got, err)
	}
}

// The decision key is the truth when the record was not updated.
func TestCollectDecisionSurvivesStaleRecord(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	c := newTestCollect(t, bot, time.Minute)
	bot.Bunt.GetOrSet(collectDecideKey(c.ID), CollectExpired, time.Hour) // record still says pending
	stale, _ := bot.CollectGet(c.ID)
	if _, _, err := bot.collectApprove(stale, testPayer(111)); err == nil {
		t.Fatal("charged an expired request")
	}
	if got, _ := bot.CollectGet(c.ID); got.Status != CollectExpired {
		t.Errorf("status %s, want expired", got.Status)
	}
}

// A save that started before the decision can't hide it.
func TestCollectStaleSaveDoesNotHideDecision(t *testing.T) {
	withP2PConfig(t)
	bot := buntBot(t)
	c := newTestCollect(t, bot, time.Minute)
	stale, _ := bot.CollectGet(c.ID)
	if _, ok, err := bot.collectApprove(c, testPayer(111)); err != nil || !ok {
		t.Fatal("approve failed", err)
	}
	stale.Memo = "edited"
	bot.collectSave(stale) // writes status "pending" back
	if got, _ := bot.CollectGet(c.ID); got.Status != CollectApproved {
		t.Errorf("status %s, want approved", got.Status)
	}
}
