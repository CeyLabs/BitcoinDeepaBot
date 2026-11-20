package api

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
)

func entryKeys(t *testing.T, e UserTransactionData) ([]string, string) {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, string(b)
}

// A Lightning payment with every LNbits field set, including the ones that
// must never leave the bot.
func testLNbitsPayment(amountMsat int64) lnbits.Payment {
	return lnbits.Payment{
		CheckingID: "chk-1", Amount: amountMsat, Fee: -1000, Time: lnbits.UnixTime(time.Now().Unix()),
		Bolt11: "lnbc1…", PaymentHash: "ab12", WalletID: "wallet-xyz",
		Webhook: "https://bot.example/?s=SECRET", WebhookStatus: 200,
	}
}

func TestUserTransactionEntriesAreUniform(t *testing.T) {
	internalE := internalEntry(111, telegram.Transaction{ID: 7, FromId: 222, ToId: 111, Type: "p2p_send", Amount: 1000, Success: false})
	lightningE := lightningEntry(111, testLNbitsPayment(-5000), "")

	ik, _ := entryKeys(t, internalE)
	lk, _ := entryKeys(t, lightningE)
	if !reflect.DeepEqual(ik, lk) {
		t.Fatalf("key sets differ:\ninternal  %v\nlightning %v", ik, lk)
	}
	want := []string{"amount", "amount_lkr", "direction", "fee", "from_id", "from_user", "id", "memo", "payment_hash", "status", "success", "time", "to_id", "to_user", "type"}
	if !reflect.DeepEqual(ik, want) {
		t.Fatalf("keys %v, want %v", ik, want)
	}

	if internalE.Status != "failed" || internalE.Fee != 0 || internalE.Direction != "incoming" {
		t.Errorf("internal entry: %+v", internalE)
	}
	if lightningE.Status != "success" || lightningE.Fee != 1 || lightningE.Amount != 5 || lightningE.Direction != "outgoing" || lightningE.PaymentHash != "ab12" {
		t.Errorf("lightning entry: %+v", lightningE)
	}
}

func TestLightningEntryStatus(t *testing.T) {
	p := testLNbitsPayment(1000)
	p.Pending = true
	if e := lightningEntry(1, p, ""); e.Status != "pending" || e.Success {
		t.Errorf("pending: %+v", e)
	}
	p.Pending, p.Status = false, "failed"
	if e := lightningEntry(1, p, ""); e.Status != "failed" || e.Success {
		t.Errorf("failed: %+v", e)
	}
}

func TestLightningEntryNoSecrets(t *testing.T) {
	for _, e := range []UserTransactionData{lightningEntry(1, testLNbitsPayment(1000), ""), lightningEntry(1, testLNbitsPayment(-1000), "")} {
		_, out := entryKeys(t, e)
		lower := strings.ToLower(out)
		for _, bad := range []string{"webhook", "adminkey", "inkey", "?s=", "secret", "wallet-xyz", "chk-1"} {
			if strings.Contains(lower, strings.ToLower(bad)) {
				t.Errorf("output contains %q: %s", bad, out)
			}
		}
	}
}

// A Lightning-address receipt has no LNbits memo: the comment the bot stored
// for it becomes the memo.
func TestLightningEntryUsesStoredComment(t *testing.T) {
	db := storage.NewBunt(":memory:")
	t.Cleanup(func() { db.Close() })
	s := Service{Bot: &telegram.TipBot{Bunt: db}}
	rec := &telegram.LNURLInvoice{Invoice: &telegram.Invoice{PaymentHash: "ab12"}, Comment: "thanks for the coffee"}
	if err := db.Set(rec); err != nil {
		t.Fatal(err)
	}

	p := testLNbitsPayment(21000)
	p.Memo = ""
	if e := lightningEntry(1, p, s.lnurlComment(p)); e.Memo != "thanks for the coffee" {
		t.Fatalf("memo %q, want the stored comment", e.Memo)
	}

	// An LNbits memo wins over the comment.
	p.Memo = "Pay to alice"
	if e := lightningEntry(1, p, s.lnurlComment(p)); e.Memo != "Pay to alice" {
		t.Fatalf("memo %q, want the LNbits memo", e.Memo)
	}

	// No record: empty memo.
	p.Memo, p.PaymentHash = "", "cd34"
	if e := lightningEntry(1, p, s.lnurlComment(p)); e.Memo != "" {
		t.Fatalf("memo %q, want empty", e.Memo)
	}
}
