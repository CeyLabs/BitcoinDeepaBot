package telegram

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/buntdb"
)

// A collect request asks a user (the payer) to pay sats to a whitelisted API
// wallet (the collector). The API only creates the request; nothing moves
// until the payer approves it from the Mini App or web app. Approving creates a P2P transfer from the payer
// to the collector, so payment, settlement checks and refunds all reuse the
// P2P code.
//
// Every request ends with one decision: approve, decline, cancel (by the
// collector) or expire. The decision is taken with GetOrSet on one key, so
// only the first one counts, and the stored value is the truth if the record
// was not updated (for example after a crash).

// Collect request states.
const (
	CollectPending   = "pending"
	CollectApproved  = "approved" // a transfer exists; see its status
	CollectDeclined  = "declined"
	CollectCancelled = "cancelled"
	CollectExpired   = "expired"
)

const (
	CollectExpiry = 15 * time.Minute
	// collectPerHour caps new requests per collector and payer, so a
	// collector cannot flood a user with prompts.
	collectPerHour = 10
)

// CollectRequest is one request to collect sats from a payer.
type CollectRequest struct {
	*storage.Base
	Status          string    `json:"status"`
	WalletID        string    `json:"wallet_id"`
	ToTelegramID    int64     `json:"to_telegram_id"` // the collector's bot user
	ToUsername      string    `json:"to_username,omitempty"`
	ToName          string    `json:"to_name,omitempty"`
	PayerTelegramID int64     `json:"payer_telegram_id"`
	PayerUsername   string    `json:"payer_username,omitempty"`
	Amount          int64     `json:"amount"`
	Memo            string    `json:"memo,omitempty"`
	ExternalID      string    `json:"external_id"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// TransferID is the ID of the transfer an approval creates. It is fixed, so
// a request can never pay twice.
func (c *CollectRequest) TransferID() string { return "p2p-" + c.ID }

func newCollectID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("col-%d", time.Now().UnixNano())
	}
	return "col-" + hex.EncodeToString(b)
}

func collectDecideKey(id string) string { return "collect-decide:" + id }

// CollectCreate creates a request from walletID's user to collect amount
// sats from payer, who approves it in the Mini App or web app. A repeat of
// the same externalID returns the original request; the same externalID with
// different parameters is refused.
func (bot *TipBot) CollectCreate(walletID string, collector, payer *lnbits.User, amount, maxAmount int64, memo, externalID string, maxMemo int) (*CollectRequest, bool, error) {
	switch {
	case amount < 1:
		return nil, false, p2pErr("invalid_amount", "Amount must be at least 1 sat.")
	case amount > maxAmount:
		return nil, false, p2pErr("amount_too_large", "The most you can collect at once is %s.", utils.FormatSats(maxAmount))
	case utf8.RuneCountInString(memo) > maxMemo:
		return nil, false, p2pErr("memo_too_long", "Memo can be at most %d characters.", maxMemo)
	case len(externalID) < 8 || len(externalID) > 128:
		return nil, false, p2pErr("invalid_external_id", "external_id must be 8-128 characters (use a UUID).")
	}
	if why := p2pEligible(collector); why != "" {
		return nil, false, p2pErr("collector_not_allowed", "The collecting wallet %s.", why)
	}
	if payer.Telegram.ID == collector.Telegram.ID {
		return nil, false, p2pErr("self_collect", "A wallet can't collect from itself.")
	}
	c := &CollectRequest{
		Base: storage.New(storage.ID(newCollectID())), Status: CollectPending, WalletID: walletID,
		ToTelegramID: collector.Telegram.ID, ToUsername: collector.Telegram.Username, ToName: DisplayName(collector.Telegram),
		PayerTelegramID: payer.Telegram.ID, PayerUsername: payer.Telegram.Username,
		Amount: amount, Memo: memo, ExternalID: externalID, ExpiresAt: time.Now().Add(CollectExpiry),
	}
	id, created, err := bot.Bunt.GetOrSet(fmt.Sprintf("collect-ext:%s:%s", walletID, externalID), c.ID, p2pIdempotencyTTL)
	if err != nil {
		return nil, false, err
	}
	if !created {
		prev, err := bot.CollectGet(id)
		if err != nil {
			return nil, false, p2pErr("in_progress", "This request is still being created. Retry in a moment.")
		}
		if prev.PayerTelegramID != c.PayerTelegramID || prev.Amount != c.Amount || prev.Memo != c.Memo {
			return nil, false, p2pErr("idempotency_conflict", "This external_id was already used for a different request.")
		}
		return prev, false, nil
	}
	extKey := fmt.Sprintf("collect-ext:%s:%s", walletID, externalID)
	hour := fmt.Sprintf("collect-rate:%s:%d:%s", walletID, payer.Telegram.ID, time.Now().UTC().Format("2006010215"))
	if ok, _, err := bot.Bunt.AddWithinLimit(hour, 1, collectPerHour, 2*time.Hour); err != nil || !ok {
		bot.Bunt.DeleteIfValue(extKey, c.ID)
		return nil, false, p2pErr("rate_limited", "Too many requests to this user. Try again later.")
	}
	if err := bot.collectSave(c); err != nil {
		bot.Bunt.DeleteIfValue(extKey, c.ID)
		return nil, false, err
	}
	log.Infof("[collect] %s asks %d for %d sat (%s)", walletID, payer.Telegram.ID, amount, c.ID)
	return c, true, nil
}

func (bot *TipBot) collectSave(c *CollectRequest) error {
	c.UpdatedAt = time.Now()
	return bot.Bunt.SetWithTTL(c, p2pRecordTTL)
}

// CollectGet loads a request by ID and brings its status up to date.
func (bot *TipBot) CollectGet(id string) (*CollectRequest, error) {
	if !strings.HasPrefix(id, "col-") || len(id) > 60 {
		return nil, fmt.Errorf("not found")
	}
	c := &CollectRequest{Base: storage.New(storage.ID(id))}
	if err := bot.Bunt.Get(c); err != nil {
		return nil, err
	}
	// The decision key is the truth: the record can lag behind it, or be
	// overwritten by a save that started before the decision.
	if d := bot.collectDecision(c.ID); d != "" {
		c.Status = d
	} else if time.Now().After(c.ExpiresAt) {
		bot.collectDecide(c, CollectExpired)
	}
	return c, nil
}

// collectDecision returns the decision taken for a request, or "".
func (bot *TipBot) collectDecision(id string) string {
	var d string
	bot.Bunt.View(func(tx *buntdb.Tx) error {
		d, _ = tx.Get(collectDecideKey(id))
		return nil
	})
	return d
}

// collectDecide records decision for c unless another decision came first.
// It returns true if decision won. Either way c ends up with the real status.
func (bot *TipBot) collectDecide(c *CollectRequest, decision string) bool {
	stored, created, err := bot.Bunt.GetOrSet(collectDecideKey(c.ID), decision, p2pRecordTTL)
	if err != nil {
		log.Errorf("[collect] Could not decide %s: %v", c.ID, err)
		return false
	}
	if c.Status != stored {
		c.Status = stored
		bot.collectSave(c)
	}
	return created
}

// CollectTransfer returns the transfer an approved request created.
func (bot *TipBot) CollectTransfer(c *CollectRequest) (*P2PTransfer, error) {
	return bot.P2PGet(c.TransferID())
}

// collectApprove takes the approve decision and creates the transfer.
// created is true only for the caller that won the decision; only that
// caller runs the transfer. A repeat gets the existing transfer.
func (bot *TipBot) collectApprove(c *CollectRequest, payer *lnbits.User) (t *P2PTransfer, created bool, err error) {
	if payer.Telegram.ID != c.PayerTelegramID {
		return nil, false, p2pErr("not_found", "Request not found.")
	}
	if c.Status == CollectPending && time.Now().After(c.ExpiresAt) {
		bot.collectDecide(c, CollectExpired) // a late approve can still lose to an earlier decision
	}
	if c.Status != CollectPending && c.Status != CollectApproved {
		return nil, false, p2pErr("request_closed", "This request is %s.", c.Status)
	}
	if !bot.collectDecide(c, CollectApproved) {
		if c.Status != CollectApproved {
			return nil, false, p2pErr("request_closed", "This request is %s.", c.Status)
		}
		t, err := bot.CollectTransfer(c)
		if err != nil {
			return nil, false, p2pErr("in_progress", "This request is being paid. Check again in a moment.")
		}
		return t, false, nil
	}
	t = &P2PTransfer{
		Base: storage.New(storage.ID(c.TransferID())), Kind: P2PKindInternal, Status: P2PProcessing,
		FromTelegramID: payer.Telegram.ID, ToTelegramID: c.ToTelegramID, ToUsername: c.ToUsername, ToName: c.ToName,
		Amount: c.Amount, Memo: c.Memo, IdempotencyKey: "collect:" + c.ID, CollectID: c.ID,
	}
	if err := bot.p2pSave(t); err != nil {
		bot.p2pEnd(t, P2PFailed, "Could not start the payment. Nothing was sent.", nil)
	}
	return t, true, nil
}

// CollectApprove approves and pays a request from the Mini App or web app.
// A repeat returns the same transfer.
func (bot *TipBot) CollectApprove(id string, payer *lnbits.User) (*P2PTransfer, error) {
	c, err := bot.CollectGet(id)
	if err != nil || c.PayerTelegramID != payer.Telegram.ID {
		return nil, p2pErr("not_found", "Request not found.")
	}
	t, created, err := bot.collectApprove(c, payer)
	if err != nil || !created || p2pFinal(t.Status) {
		return t, err
	}
	return t, bot.p2pBegin(payer, t)
}

// CollectDecline declines a request for its payer.
func (bot *TipBot) CollectDecline(id string, payerID int64) (*CollectRequest, error) {
	c, err := bot.CollectGet(id)
	if err != nil || c.PayerTelegramID != payerID {
		return nil, p2pErr("not_found", "Request not found.")
	}
	if !bot.collectDecide(c, CollectDeclined) {
		return c, p2pErr("request_closed", "This request is %s.", c.Status)
	}
	log.Infof("[collect] %s declined by %d", c.ID, payerID)
	return c, nil
}

// CollectCancel withdraws a request for the wallet that made it.
func (bot *TipBot) CollectCancel(id, walletID string) (*CollectRequest, error) {
	c, err := bot.CollectGet(id)
	if err != nil || c.WalletID != walletID {
		return nil, p2pErr("not_found", "Request not found.")
	}
	if !bot.collectDecide(c, CollectCancelled) {
		return c, p2pErr("request_closed", "This request is %s.", c.Status)
	}
	return c, nil
}
