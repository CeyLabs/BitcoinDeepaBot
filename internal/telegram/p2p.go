package telegram

import (
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	decodepay "github.com/fiatjaf/ln-decodepay"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/buntdb"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// P2P transfers are started from the Telegram Mini App. The sender is always
// the Telegram user in the Mini App's signed init data (checked by the API
// layer before anything here runs); nothing in a request can name a different
// sender. Recipients are resolved to a numeric Telegram ID first and paid by
// that ID, never by a username, which can change hands.
//
// Every final outcome (completed, failed, cancelled, expired) goes through
// p2pEnd, which runs once per transfer. That is where the daily counters are
// given back and the invoice lock is released, so neither can happen twice or
// be skipped.

// Transfer kinds.
const (
	P2PKindInternal = "internal" // to another bot user
	P2PKindInvoice  = "invoice"  // a bolt11 invoice
)

// Transfer states.
const (
	P2PAwaitingConfirmation = "awaiting_confirmation"
	P2PProcessing           = "processing"
	P2PInFlight             = "in_flight" // payment sent, not settled yet
	P2PCompleted            = "completed"
	P2PFailed               = "failed"
	P2PCancelled            = "cancelled"
	P2PExpired              = "expired"
)

const (
	p2pConfirmWindow  = 10 * time.Minute
	p2pIdempotencyTTL = 24 * time.Hour
	// p2pRecordTTL is how long transfer records, locks and claim keys live.
	p2pRecordTTL  = 30 * 24 * time.Hour
	p2pSettleWait = 60 * time.Second
	// p2pStaleAfter: a transfer still "processing" this long after its last
	// update, with nothing running it, was cut off (e.g. by a restart).
	p2pStaleAfter = 2 * time.Minute
	// p2pCheckInterval limits lnbits lookups to one per transfer per interval,
	// however often the Mini App polls.
	p2pCheckInterval = 20 * time.Second
)

// P2PError is a failure the caller should see. Code is stable for clients.
type P2PError struct {
	Code    string
	Message string
}

func (e *P2PError) Error() string { return e.Message }

func p2pErr(code, format string, args ...interface{}) *P2PError {
	return &P2PError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// P2PTransfer is one Mini App send or invoice payment.
type P2PTransfer struct {
	*storage.Base
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	FromTelegramID int64     `json:"from_telegram_id"`
	ToTelegramID   int64     `json:"to_telegram_id,omitempty"`
	ToUsername     string    `json:"to_username,omitempty"`
	ToName         string    `json:"to_name,omitempty"`
	Invoice        string    `json:"invoice,omitempty"`
	PaymentHash    string    `json:"payment_hash,omitempty"`
	Description    string    `json:"description,omitempty"`
	Amount         int64     `json:"amount"`
	Fee            int64     `json:"fee,omitempty"`
	Preimage       string    `json:"preimage,omitempty"`
	Memo           string    `json:"memo,omitempty"`
	Error          string    `json:"error,omitempty"`
	IdempotencyKey string    `json:"idempotency_key"`
	ConfirmBy      time.Time `json:"confirm_by,omitempty"`
	DailyCounted   bool      `json:"daily_counted,omitempty"`
	DailyKey       string    `json:"daily_key,omitempty"` // the day the amount was counted against
	// UnconfirmedKey is set when the amount was counted against today's
	// allowance for sends without confirmation.
	UnconfirmedKey string `json:"unconfirmed_key,omitempty"`
	// InternalPaymentHash is the hash of the recipient's invoice for an
	// internal send. It is saved before paying, so the outcome can be looked
	// up if the send is cut off.
	InternalPaymentHash string `json:"internal_payment_hash,omitempty"`
	// CollectID is set when the transfer pays a collect request. The payer
	// approved that exact payment, so the P2P daily limit does not apply.
	CollectID string `json:"collect_id,omitempty"`
}

// settlementHash is the payment to look up for this transfer, or "".
func (t *P2PTransfer) settlementHash() string {
	if t.Kind == P2PKindInvoice {
		return t.PaymentHash
	}
	return t.InternalPaymentHash
}

func p2pFinal(status string) bool {
	switch status {
	case P2PCompleted, P2PFailed, P2PCancelled, P2PExpired:
		return true
	}
	return false
}

func p2pConfig() internal.APIP2PConfiguration { return internal.Configuration.API.P2P }

func newP2PID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("p2p-%d", time.Now().UnixNano())
	}
	return "p2p-" + hex.EncodeToString(b)
}

// p2pEligible reports why a user cannot send or receive, or "" if they can.
func p2pEligible(u *lnbits.User) string {
	switch {
	case u == nil || u.Wallet == nil || u.Telegram == nil:
		return "has no wallet"
	case !u.Initialized:
		return "has not started the bot yet"
	case u.Banned || strings.HasPrefix(u.Wallet.Adminkey, "banned_"):
		return "cannot use the wallet"
	}
	return ""
}

// P2PSender loads the Mini App user as a sender.
func (bot *TipBot) P2PSender(telegramID int64) (*lnbits.User, error) {
	u, err := GetUserByTelegramID(telegramID, *bot)
	if err != nil {
		return nil, p2pErr("sender_not_found", "Open @%s and press Start to create your wallet first.", bot.Telegram.Me.Username)
	}
	if why := p2pEligible(u); why != "" {
		return nil, p2pErr("sender_not_allowed", "Your account %s.", why)
	}
	return u, nil
}

// P2PResolve finds a recipient by "@username", "username" or a numeric
// Telegram ID. Only users who started the bot can receive. Users who can't
// receive get the same answer as unknown users, so the lookup does not reveal
// who is banned.
func (bot *TipBot) P2PResolve(identifier string) (*lnbits.User, error) {
	id := strings.TrimPrefix(strings.TrimSpace(identifier), "@")
	if id == "" || len(id) > 64 {
		return nil, p2pErr("invalid_recipient", "Enter a Telegram @username or user ID.")
	}
	var (
		u   *lnbits.User
		err error
	)
	if n, perr := strconv.ParseInt(id, 10, 64); perr == nil && n > 0 {
		u, err = GetUserByTelegramID(n, *bot)
	} else {
		u, err = GetUserByTelegramUsername(id, *bot)
	}
	if err != nil || p2pEligible(u) != "" {
		return nil, p2pErr("recipient_not_found", "This user hasn't joined @%s yet. Invite them to start the bot first.", bot.Telegram.Me.Username)
	}
	return u, nil
}

// DisplayName is a recipient's name for the confirm screen.
func DisplayName(u *tb.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	return name
}

func validateP2PCommon(amount int64, memo, idemKey string) *P2PError {
	cfg := p2pConfig()
	if amount < 1 {
		return p2pErr("invalid_amount", "Amount must be at least 1 sat.")
	}
	if amount > cfg.MaxAmount {
		return p2pErr("amount_too_large", "The most you can send at once is %s.", utils.FormatSats(cfg.MaxAmount))
	}
	if utf8.RuneCountInString(memo) > cfg.MaxMemoLength {
		return p2pErr("memo_too_long", "Memo can be at most %d characters.", cfg.MaxMemoLength)
	}
	if l := len(idemKey); l < 8 || l > 128 {
		return p2pErr("invalid_idempotency_key", "idempotency_key must be 8-128 characters (use a UUID).")
	}
	return nil
}

// claimIdempotency maps (sender, key) to a transfer ID. A repeat of the same
// request returns the original transfer, whatever its status; the same key
// with different parameters is refused.
func (bot *TipBot) claimIdempotency(t *P2PTransfer) (*P2PTransfer, bool, error) {
	key := fmt.Sprintf("p2p-idem:%d:%s", t.FromTelegramID, t.IdempotencyKey)
	id, created, err := bot.Bunt.GetOrSet(key, t.ID, p2pIdempotencyTTL)
	if err != nil {
		return nil, false, err
	}
	if created {
		return t, true, nil
	}
	prev, err := bot.P2PGet(id)
	if err != nil {
		return nil, false, p2pErr("in_progress", "This request is still being processed. Retry in a moment.")
	}
	if prev.Kind != t.Kind || prev.Amount != t.Amount || prev.ToTelegramID != t.ToTelegramID || prev.PaymentHash != t.PaymentHash || prev.Memo != t.Memo {
		return nil, false, p2pErr("idempotency_conflict", "This idempotency_key was already used for a different request.")
	}
	return prev, false, nil
}

// P2PStartSend starts a transfer to another bot user. It returns the
// transfer, and whether this call created it (false means a retry).
func (bot *TipBot) P2PStartSend(from *lnbits.User, toTelegramID, amount int64, memo, idemKey string) (*P2PTransfer, bool, error) {
	if e := validateP2PCommon(amount, memo, idemKey); e != nil {
		return nil, false, e
	}
	to, err := bot.P2PResolve(strconv.FormatInt(toTelegramID, 10))
	if err != nil {
		return nil, false, err
	}
	if to.Telegram.ID == from.Telegram.ID {
		return nil, false, p2pErr("self_send", "You can't send to yourself.")
	}
	t := &P2PTransfer{
		Base: storage.New(storage.ID(newP2PID())), Kind: P2PKindInternal, Status: P2PProcessing,
		FromTelegramID: from.Telegram.ID, ToTelegramID: to.Telegram.ID,
		ToUsername: to.Telegram.Username, ToName: DisplayName(to.Telegram),
		Amount: amount, Memo: memo, IdempotencyKey: idemKey,
	}
	t, created, err := bot.claimIdempotency(t)
	if err != nil || !created {
		return t, false, err
	}
	if err := bot.p2pSave(t); err != nil {
		return nil, false, err
	}
	return t, true, bot.p2pBegin(from, t)
}

// P2PDecodedInvoice is what the Mini App shows before paying an invoice.
type P2PDecodedInvoice struct {
	Amount      int64
	Description string
	PaymentHash string
	ExpiresAt   time.Time
}

// P2PDecodeInvoice checks that an invoice can be paid from the Mini App.
func P2PDecodeInvoice(invoice string) (*P2PDecodedInvoice, error) {
	pr := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(invoice)), "lightning:")
	inv, err := decodepay.Decodepay(pr)
	if err != nil {
		return nil, p2pErr("invalid_invoice", "This is not a valid Lightning invoice.")
	}
	amount := inv.MSatoshi / 1000
	if amount <= 0 {
		return nil, p2pErr("amountless_invoice", "Invoices without an amount are not supported.")
	}
	if lnbits.InvoiceExpired(inv) {
		return nil, p2pErr("invoice_expired", "This invoice has expired.")
	}
	if !lnbits.ValidPaymentHash(inv.PaymentHash) {
		return nil, p2pErr("invalid_invoice", "This invoice has no valid payment hash.")
	}
	return &P2PDecodedInvoice{Amount: amount, Description: inv.Description, PaymentHash: inv.PaymentHash, ExpiresAt: lnbits.InvoiceExpiresAt(inv)}, nil
}

// P2PStartInvoice starts paying a bolt11 invoice.
func (bot *TipBot) P2PStartInvoice(from *lnbits.User, invoice, memo, idemKey string) (*P2PTransfer, bool, error) {
	dec, err := P2PDecodeInvoice(invoice)
	if err != nil {
		return nil, false, err
	}
	if e := validateP2PCommon(dec.Amount, memo, idemKey); e != nil {
		return nil, false, e
	}
	t := &P2PTransfer{
		Base: storage.New(storage.ID(newP2PID())), Kind: P2PKindInvoice, Status: P2PProcessing,
		FromTelegramID: from.Telegram.ID,
		Invoice:        strings.TrimPrefix(strings.ToLower(strings.TrimSpace(invoice)), "lightning:"),
		PaymentHash:    dec.PaymentHash, Description: dec.Description,
		Amount: dec.Amount, Memo: memo, IdempotencyKey: idemKey,
	}
	t, created, err := bot.claimIdempotency(t)
	if err != nil || !created {
		return t, false, err
	}
	// Saved before taking the invoice lock, so a competing request that finds
	// the lock can always load its holder.
	if err := bot.p2pSave(t); err != nil {
		return nil, false, err
	}
	// Never pay an invoice twice: refuse one that is already paid or in flight.
	prior, found, err := bot.Client.PriorPayment(*from.Wallet, dec.PaymentHash)
	if err != nil {
		bot.p2pEnd(t, P2PFailed, "Could not check this invoice. Nothing was paid; try again.", nil)
		return t, true, nil
	}
	if found && prior.State != lnbits.PaymentStateFailed {
		bot.p2pEnd(t, P2PFailed, "This invoice was already paid or is being paid.", nil)
		return t, true, nil
	}
	if !bot.p2pLockInvoice(t) {
		bot.p2pEnd(t, P2PFailed, "This invoice is already being paid.", nil)
		return t, true, nil
	}
	if !dec.ExpiresAt.IsZero() {
		t.ConfirmBy = dec.ExpiresAt
	}
	return t, true, bot.p2pBegin(from, t)
}

func p2pInvoiceLock(hash string) string { return "p2p-inv:" + hash }

// p2pLockInvoice takes the per-invoice lock for t. The lock holds the ID of
// the transfer that owns it, and only that transfer releases it (in p2pEnd).
// A holder that can no longer pay (an unconfirmed transfer past its deadline,
// or one that ended without paying) does not block a new attempt.
func (bot *TipBot) p2pLockInvoice(t *P2PTransfer) bool {
	key := p2pInvoiceLock(t.PaymentHash)
	for attempt := 0; attempt < 2; attempt++ {
		holder, created, err := bot.Bunt.GetOrSet(key, t.ID, p2pRecordTTL)
		if err != nil {
			log.Errorf("[p2p] Could not lock invoice for %s: %v", t.ID, err)
			return false
		}
		if created || holder == t.ID {
			return true
		}
		h, err := bot.P2PGet(holder)
		switch {
		case stderrors.Is(err, buntdb.ErrNotFound):
			bot.Bunt.DeleteIfValue(key, holder)
		case err != nil:
			return false
		case h.Status == P2PAwaitingConfirmation && time.Now().After(h.ConfirmBy):
			bot.p2pExpire(h) // releases the lock
		case p2pFinal(h.Status) && h.Status != P2PCompleted:
			bot.Bunt.DeleteIfValue(key, holder)
		default:
			return false // paid, or still being paid
		}
	}
	return false
}

func p2pUnconfirmedKey(senderID int64, now time.Time) string {
	return fmt.Sprintf("p2p-unconfirmed:%d:%s", senderID, now.UTC().Format("2006-01-02"))
}

// p2pNeedsConfirmation decides whether t must be confirmed in the bot chat.
// Sends without confirmation share one daily allowance of confirm_threshold
// sats, so splitting a payment into small parts does not skip the step. A
// transfer that fits is counted against the allowance here.
func (bot *TipBot) p2pNeedsConfirmation(t *P2PTransfer) bool {
	limit := p2pConfig().ConfirmThreshold
	if t.Amount > limit {
		return true
	}
	key := p2pUnconfirmedKey(t.FromTelegramID, time.Now())
	ok, _, err := bot.Bunt.AddWithinLimit(key, t.Amount, limit, 48*time.Hour)
	if err != nil || !ok {
		return true
	}
	t.UnconfirmedKey = key
	return false
}

// P2PUnconfirmedRemaining is how many sats the sender can still send today
// without confirming in the bot chat.
func (bot *TipBot) P2PUnconfirmedRemaining(senderID int64) int64 {
	used, _ := bot.Bunt.CounterValue(p2pUnconfirmedKey(senderID, time.Now()))
	if left := p2pConfig().ConfirmThreshold - used; left > 0 {
		return left
	}
	return 0
}

// p2pBegin either asks the sender to confirm in the bot chat, or executes.
func (bot *TipBot) p2pBegin(from *lnbits.User, t *P2PTransfer) error {
	if bot.p2pNeedsConfirmation(t) {
		deadline := time.Now().Add(p2pConfirmWindow)
		if t.ConfirmBy.IsZero() || deadline.Before(t.ConfirmBy) {
			t.ConfirmBy = deadline
		}
		t.Status = P2PAwaitingConfirmation
		if err := bot.p2pSave(t); err != nil {
			bot.p2pEnd(t, P2PFailed, "Could not start this payment. Nothing was sent; try again.", nil)
			return nil
		}
		if err := bot.sendP2PConfirmation(from, t); err != nil {
			log.Errorf("[p2p] Could not send confirmation for %s: %v", t.ID, err)
			if bot.p2pClaim(t) {
				bot.p2pEnd(t, P2PFailed, "Could not send the confirmation to your Telegram chat. Nothing was sent; try again.", nil)
			}
		}
		return nil
	}
	if err := bot.p2pSave(t); err != nil { // records UnconfirmedKey
		bot.p2pEnd(t, P2PFailed, "Could not start this payment. Nothing was sent; try again.", nil)
		return nil
	}
	if t.Kind == P2PKindInvoice {
		// Settlement can take up to a minute; run it in the background and
		// let the caller wait briefly with P2PWait, then poll.
		bot.p2pInBackground(t, func() { bot.p2pExecute(t) })
		return nil
	}
	defer p2pTrack(t.ID)()
	bot.p2pExecute(t)
	return nil
}

// p2pDone holds a channel per transfer that is being run right now.
var p2pDone sync.Map

// p2pTrack marks a transfer as running until the returned func is called, so
// P2PWait can wait for it and P2PRefresh does not treat it as stuck.
func p2pTrack(id string) func() {
	done := make(chan struct{})
	p2pDone.Store(id, done)
	return func() {
		p2pDone.Delete(id)
		close(done)
	}
}

// p2pInBackground runs fn for t in a tracked goroutine. A panic is logged,
// not allowed to take the bot down; the transfer is then picked up as stuck.
func (bot *TipBot) p2pInBackground(t *P2PTransfer, fn func()) {
	untrack := p2pTrack(t.ID)
	go func() {
		defer untrack()
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("[p2p] Panic in transfer %s: %v\n%s", t.ID, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// P2PWait waits up to d for a background transfer to finish. It returns
// immediately if the transfer is not running.
func P2PWait(id string, d time.Duration) {
	ch, ok := p2pDone.Load(id)
	if !ok {
		return
	}
	select {
	case <-ch.(chan struct{}):
	case <-time.After(d):
	}
}

// p2pClaim takes the one-time decision for a transfer. Execute, cancel and
// expire all claim it first, so exactly one of them happens even when a
// button press races the confirmation deadline.
func (bot *TipBot) p2pClaim(t *P2PTransfer) bool {
	ok, err := bot.Bunt.ClaimOnce("p2p-exec:"+t.ID, p2pRecordTTL)
	if err != nil {
		log.Errorf("[p2p] Could not claim transfer %s: %v", t.ID, err)
	}
	return err == nil && ok
}

// p2pExecute claims and runs a transfer.
func (bot *TipBot) p2pExecute(t *P2PTransfer) {
	if !bot.p2pClaim(t) {
		log.Warnf("[p2p] Transfer %s was already handled", t.ID)
		return
	}
	bot.p2pRun(t)
}

// p2pRun moves the sats. Callers must hold the claim.
func (bot *TipBot) p2pRun(t *P2PTransfer) {
	from, err := bot.P2PSender(t.FromTelegramID)
	if err != nil {
		bot.p2pEnd(t, P2PFailed, err.Error(), nil)
		return
	}
	if t.CollectID != "" {
		bot.p2pSendInternal(from, t)
		return
	}
	cfg := p2pConfig()
	day := p2pDailyKey(t.FromTelegramID, time.Now())
	ok, _, err := bot.Bunt.AddWithinLimit(day, t.Amount, cfg.DailyLimit, 48*time.Hour)
	if err != nil {
		bot.p2pEnd(t, P2PFailed, "Could not check your daily limit. Nothing was sent.", nil)
		return
	}
	if !ok {
		bot.p2pEnd(t, P2PFailed, fmt.Sprintf("Daily limit of %s reached. Nothing was sent.", utils.FormatSats(cfg.DailyLimit)), nil)
		return
	}
	t.DailyCounted, t.DailyKey = true, day
	if err := bot.p2pSave(t); err != nil {
		bot.p2pEnd(t, P2PFailed, "Could not start this payment. Nothing was sent; try again.", nil)
		return
	}
	if p2pFinal(t.Status) {
		return // ended elsewhere while starting; nothing was paid
	}
	if t.Kind == P2PKindInvoice {
		bot.p2pPayInvoice(from, t)
		return
	}
	bot.p2pSendInternal(from, t)
}

func p2pDailyKey(senderID int64, now time.Time) string {
	return fmt.Sprintf("p2p-daily:%d:%s", senderID, now.UTC().Format("2006-01-02"))
}

// P2PDailyUsed returns how many sats the sender has sent today (UTC).
func (bot *TipBot) P2PDailyUsed(senderID int64) (int64, error) {
	return bot.Bunt.CounterValue(p2pDailyKey(senderID, time.Now()))
}

func (bot *TipBot) p2pSendInternal(from *lnbits.User, t *P2PTransfer) {
	to, err := bot.P2PResolve(strconv.FormatInt(t.ToTelegramID, 10))
	if err != nil {
		bot.p2pEnd(t, P2PFailed, err.Error(), nil)
		return
	}
	reservation, balance, err := bot.ReserveBalance(from, t.Amount)
	if err != nil {
		bot.p2pEnd(t, P2PFailed, "Could not check your balance. Nothing was sent.", nil)
		return
	}
	if reservation == nil {
		bot.p2pEnd(t, P2PFailed, fmt.Sprintf("Insufficient balance: %s available.", utils.FormatSats(balance)), nil)
		return
	}
	defer reservation.Release()

	memo := fmt.Sprintf("💸 P2P Send from %s to %s.", GetUserStr(from.Telegram), GetUserStr(to.Telegram))
	txType := "p2p_send"
	if t.CollectID != "" {
		memo = fmt.Sprintf("💰 Collect by %s from %s.", GetUserStr(to.Telegram), GetUserStr(from.Telegram))
		txType = "collect"
	}
	if t.Memo != "" {
		memo += " Memo: " + t.Memo
	}
	tx := NewTransaction(bot, from, to, t.Amount, TransactionType(txType))
	tx.Memo = memo
	tx.OnInvoice = func(inv lnbits.Invoice) {
		t.InternalPaymentHash = inv.PaymentHash
		bot.p2pSave(t)
	}
	ok, err := tx.Send()
	if ok && err == nil {
		if bot.p2pEnd(t, P2PCompleted, "", nil) {
			bot.p2pNotifyInternal(from, t)
		}
		return
	}
	log.Errorf("[p2p] Transfer %s failed: %v", t.ID, err)
	if bot.ErrorLogger != nil {
		bot.ErrorLogger.LogTransactionError(err, txType, t.Amount, from.Telegram, to.Telegram)
	}
	if tx.Invoice.PaymentHash == "" {
		// Failed before paying: the recipient's invoice was never created.
		bot.p2pEnd(t, P2PFailed, "The transfer failed. Nothing was sent.", nil)
		return
	}
	// Pay was attempted. It can report an error after the transfer went
	// through (a timeout, a 5xx, or a failed balance read afterwards), so ask
	// lnbits what happened before telling the user anything.
	t.InternalPaymentHash = tx.Invoice.PaymentHash
	bot.p2pCheckPayment(from, t, true)
}

// p2pNotifyInternal tells both sides about a completed internal send.
func (bot *TipBot) p2pNotifyInternal(from *lnbits.User, t *P2PTransfer) {
	to, err := GetUserByTelegramID(t.ToTelegramID, *bot)
	log.Infof("[💸 p2p] %s -> %d (%d sat), transfer %s", GetUserStr(from.Telegram), t.ToTelegramID, t.Amount, t.ID)
	toName := str.MarkdownEscape(t.ToName)
	if err == nil && to.Telegram != nil {
		toName = GetUserStrMd(to.Telegram)
		received := fmt.Sprintf("💰 You received %s from %s", thirdparty.FormatSatsWithLKR(t.Amount), GetUserStrMd(from.Telegram))
		if t.Memo != "" {
			received += "\n✉️ " + str.MarkdownEscape(t.Memo)
		}
		bot.trySendMessage(to.Telegram, received)
	}
	sent := fmt.Sprintf("✅ Sent %s to %s", thirdparty.FormatSatsWithLKR(t.Amount), toName)
	if t.Memo != "" {
		sent += "\n✉️ " + str.MarkdownEscape(t.Memo)
	}
	bot.trySendMessage(from.Telegram, sent)
}

func (bot *TipBot) p2pPayInvoice(from *lnbits.User, t *P2PTransfer) {
	fail := func(msg string) { bot.p2pEnd(t, P2PFailed, msg, nil) }
	inv, err := decodepay.Decodepay(t.Invoice)
	if err != nil || lnbits.InvoiceExpired(inv) {
		fail("This invoice has expired. Nothing was paid.")
		return
	}
	reservation, balance, err := bot.ReserveBalance(from, utils.AmountWithFeeReserve(t.Amount))
	if err != nil {
		fail("Could not check your balance. Nothing was paid.")
		return
	}
	if reservation == nil {
		if balance < t.Amount {
			fail(fmt.Sprintf("Insufficient balance: %s available.", utils.FormatSats(balance)))
		} else {
			fail(fmt.Sprintf("Insufficient balance to cover routing fees: %s available.", utils.FormatSats(balance)))
		}
		return
	}
	_, err = from.Wallet.Pay(lnbits.PaymentParams{Out: true, Bolt11: t.Invoice}, bot.Client)
	reservation.Release()
	if err != nil && lnbits.IsPaymentRejected(err) {
		log.Errorf("[p2p] Invoice payment %s rejected: %v", t.ID, err)
		fail("The invoice payment was rejected. Nothing was paid.")
		return
	}
	settlement := bot.Client.WaitForOutgoingPayment(*from.Wallet, t.PaymentHash, p2pSettleWait)
	bot.p2pApplySettlement(from, t, settlement)
}

// p2pApplySettlement records a payment's outcome and tells the sender. A
// pending or unknown outcome moves the transfer to in_flight.
func (bot *TipBot) p2pApplySettlement(from *lnbits.User, t *P2PTransfer, s lnbits.PaymentSettlement) {
	switch s.State {
	case lnbits.PaymentStateSettled:
		ended := bot.p2pEnd(t, P2PCompleted, "", func(t *P2PTransfer) { t.Fee, t.Preimage = s.Fee, s.Preimage })
		if !ended {
			return
		}
		if t.Kind == P2PKindInternal {
			bot.p2pNotifyInternal(from, t)
			return
		}
		msg := fmt.Sprintf("✅ Invoice paid: %s", thirdparty.FormatSatsWithLKR(t.Amount))
		if t.Description != "" {
			msg += "\n📄 " + str.MarkdownEscape(t.Description)
		}
		bot.trySendMessage(from.Telegram, msg)
	case lnbits.PaymentStateFailed:
		if bot.p2pEnd(t, P2PFailed, "The payment did not go through. Nothing was sent.", nil) {
			bot.trySendMessage(from.Telegram, fmt.Sprintf("❌ Payment of %s failed. Nothing was sent.", thirdparty.FormatSatsWithLKR(t.Amount)))
		}
	default:
		bot.p2pMarkInFlight(from, t)
	}
}

// p2pCheckPayment looks the transfer's payment up in lnbits once and applies
// the result. notFoundFails says a missing payment means nothing was paid:
// true when Pay is known to have finished (it errored, or the bot restarted
// long after it began), since lnbits keeps a row for every payment it starts.
func (bot *TipBot) p2pCheckPayment(from *lnbits.User, t *P2PTransfer, notFoundFails bool) {
	s, found, err := bot.Client.PriorPayment(*from.Wallet, t.settlementHash())
	switch {
	case err != nil:
		bot.p2pMarkInFlight(from, t)
	case !found && notFoundFails:
		bot.p2pEnd(t, P2PFailed, "The transfer failed. Nothing was sent.", nil)
	case !found:
		bot.p2pMarkInFlight(from, t)
	default:
		bot.p2pApplySettlement(from, t, s)
	}
}

// p2pMarkInFlight moves t to in_flight unless it already ended, and tells
// the sender once.
func (bot *TipBot) p2pMarkInFlight(from *lnbits.User, t *P2PTransfer) {
	if t.Status == P2PInFlight || p2pFinal(t.Status) {
		return
	}
	t.Status = P2PInFlight
	if bot.p2pSave(t) != nil || t.Status != P2PInFlight {
		return
	}
	if ok, _ := bot.Bunt.ClaimOnce("p2p-inflight:"+t.ID, p2pRecordTTL); ok {
		bot.trySendMessage(from.Telegram, fmt.Sprintf("⏳ Payment of %s is still in flight. You won't be charged twice.", thirdparty.FormatSatsWithLKR(t.Amount)))
	}
}

// P2PRefresh brings a transfer up to date: confirmations past their window
// expire, and payments that are in flight, or were cut off while processing,
// are checked with lnbits. Lookups are throttled per transfer.
func (bot *TipBot) P2PRefresh(t *P2PTransfer) {
	if _, running := p2pDone.Load(t.ID); running {
		return // the background payment will record the outcome
	}
	switch t.Status {
	case P2PAwaitingConfirmation:
		if time.Now().After(t.ConfirmBy) {
			bot.p2pExpire(t)
		}
		return
	case P2PProcessing:
		if time.Since(t.UpdatedAt) < p2pStaleAfter {
			return
		}
	case P2PInFlight:
	default:
		return
	}
	if ok, err := bot.Bunt.ClaimOnce("p2p-check:"+t.ID, p2pCheckInterval); err != nil || !ok {
		return
	}
	from, err := GetUserByTelegramID(t.FromTelegramID, *bot)
	if err != nil || from.Wallet == nil {
		return
	}
	stuck := t.Status == P2PProcessing
	if t.settlementHash() == "" {
		if stuck {
			// An internal send cut off before the recipient's invoice existed:
			// nothing was paid.
			bot.p2pEnd(t, P2PFailed, "The transfer was interrupted. Nothing was sent.", nil)
		}
		return
	}
	bot.p2pCheckPayment(from, t, stuck || t.Kind == P2PKindInternal)
}

// p2pExpire expires an unconfirmed transfer unless it was already confirmed
// or cancelled, and reports whether it did.
func (bot *TipBot) p2pExpire(t *P2PTransfer) bool {
	if !bot.p2pClaim(t) {
		return false
	}
	return bot.p2pEnd(t, P2PExpired, "Not confirmed in time. Nothing was sent.", nil)
}

// p2pEnd moves t to a final status, once. The first caller wins; any other
// caller (an overlapping poll, a retry) gets false and t reloaded from the
// store. Unless the transfer completed, the daily counters are given back and
// its invoice lock is released. set, if given, records extra fields.
func (bot *TipBot) p2pEnd(t *P2PTransfer, status, msg string, set func(*P2PTransfer)) bool {
	ok, err := bot.Bunt.ClaimOnce(p2pEndKey(t.ID), p2pRecordTTL)
	if err != nil || !ok {
		if fresh, err := bot.P2PGet(t.ID); err == nil {
			*t = *fresh
		}
		return false
	}
	if fresh, err := bot.P2PGet(t.ID); err == nil {
		// Work on the stored copy, keeping anything this copy recorded that
		// may not have been saved.
		if t.DailyCounted && !fresh.DailyCounted {
			fresh.DailyCounted, fresh.DailyKey = true, t.DailyKey
		}
		if fresh.UnconfirmedKey == "" {
			fresh.UnconfirmedKey = t.UnconfirmedKey
		}
		if fresh.InternalPaymentHash == "" {
			fresh.InternalPaymentHash = t.InternalPaymentHash
		}
		*t = *fresh
	}
	if set != nil {
		set(t)
	}
	if status != P2PCompleted {
		if t.DailyCounted {
			if _, _, err := bot.Bunt.AddWithinLimit(t.DailyKey, -t.Amount, 0, 48*time.Hour); err != nil {
				log.Errorf("[p2p] Could not refund daily limit for %s: %v", t.ID, err)
			}
			t.DailyCounted = false
		}
		if t.UnconfirmedKey != "" {
			if _, _, err := bot.Bunt.AddWithinLimit(t.UnconfirmedKey, -t.Amount, 0, 48*time.Hour); err != nil {
				log.Errorf("[p2p] Could not refund unconfirmed allowance for %s: %v", t.ID, err)
			}
			t.UnconfirmedKey = ""
		}
		if t.Kind == P2PKindInvoice {
			bot.Bunt.DeleteIfValue(p2pInvoiceLock(t.PaymentHash), t.ID)
		}
	}
	t.Status, t.Error, t.Active = status, msg, false
	if err := bot.Bunt.SetWithTTL(t, p2pRecordTTL); err != nil {
		log.Errorf("[p2p] Could not save transfer %s: %v", t.ID, err)
	}
	return true
}

func p2pEndKey(id string) string { return "p2p-end:" + id }

// p2pSave stores a transfer that has not ended. Once p2pEnd has claimed the
// transfer, a late save from another copy is refused and t is reloaded, so
// a final status is never overwritten.
func (bot *TipBot) p2pSave(t *P2PTransfer) error {
	t.UpdatedAt = time.Now()
	wrote, err := bot.Bunt.SetWithTTLUnless(t, p2pEndKey(t.ID), p2pRecordTTL)
	if err != nil {
		log.Errorf("[p2p] Could not save transfer %s: %v", t.ID, err)
		return err
	}
	if !wrote {
		if fresh, err := bot.P2PGet(t.ID); err == nil {
			*t = *fresh
		}
	}
	return nil
}

// P2PGet loads a transfer by ID.
func (bot *TipBot) P2PGet(id string) (*P2PTransfer, error) {
	if !strings.HasPrefix(id, "p2p-") || len(id) > 64 {
		return nil, stderrors.New("not found")
	}
	t := &P2PTransfer{Base: storage.New(storage.ID(id))}
	if err := bot.Bunt.Get(t); err != nil {
		return nil, err
	}
	return t, nil
}
