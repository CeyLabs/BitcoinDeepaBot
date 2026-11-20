package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	log "github.com/sirupsen/logrus"
)

// maxHistoryWindow caps offset+limit for the merged history: both the
// internal table and LNbits are read from the newest entry up to it.
const maxHistoryWindow = 1000

// UserTransactionsRequest represents the JSON request for the user transactions API
type UserTransactionsRequest struct {
	TelegramID int64 `json:"telegram_id"` // Telegram user ID
	Limit      int   `json:"limit"`       // Optional: max transactions to return (default 100, max 250)
	Offset     int   `json:"offset"`      // Optional: number of transactions to skip for pagination
}

// UserTransactionData is one entry in a user's history: an internal transfer
// (tips, sends, P2P, ...) or a Lightning payment from LNbits (type
// "lightning": /pay, /invoice, Lightning-address receipts). Every entry has
// every field (no omitempty), with "" or 0 when a value is unknown.
type UserTransactionData struct {
	ID          uint   `json:"id"` // internal row ID; 0 for Lightning payments
	Time        string `json:"time"`
	Direction   string `json:"direction"` // "incoming" or "outgoing" relative to the queried user
	FromID      int64  `json:"from_id"`
	ToID        int64  `json:"to_id"`
	FromUser    string `json:"from_user"`
	ToUser      string `json:"to_user"`
	Type        string `json:"type"`
	Amount      int64  `json:"amount"` // sats
	AmountLKR   string `json:"amount_lkr"`
	Memo        string `json:"memo"`
	Success     bool   `json:"success"`
	Fee         int64  `json:"fee"`    // sats; 0 for internal transfers
	Status      string `json:"status"` // "success", "pending" or "failed"
	PaymentHash string `json:"payment_hash"`
}

// UserTransactionsResponse represents the JSON response for the user transactions API
type UserTransactionsResponse struct {
	Success      bool                  `json:"success"`
	TelegramID   int64                 `json:"telegram_id"`
	Count        int                   `json:"count"`
	Limit        int                   `json:"limit"`
	Offset       int                   `json:"offset"`
	Transactions []UserTransactionData `json:"transactions"`
	Message      string                `json:"message,omitempty"`
}

// UserTransactions handles the /api/v1/usertransactions endpoint for fetching a user's
// internal transaction history by Telegram ID. Authenticated with the same wallet Ed25519 signature
// used by the send API.
func (s Service) UserTransactions(w http.ResponseWriter, r *http.Request) {
	var req UserTransactionsRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		log.Errorf("[api/usertransactions] Invalid JSON request: %v", err)
		RespondError(w, "Invalid JSON request")
		return
	}

	// Validate request
	if req.TelegramID <= 0 {
		RespondError(w, "Invalid Telegram ID")
		return
	}

	// Apply pagination defaults and caps (shared with the analytics API)
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > maxAnalyticsLimit {
		limit = maxAnalyticsLimit
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > maxAnalyticsOffset {
		offset = maxAnalyticsOffset
	}

	if offset+limit > maxHistoryWindow {
		offset = maxHistoryWindow - limit
		if offset < 0 {
			offset = 0
		}
	}
	window := offset + limit

	// Confirm the user exists so we can distinguish "unknown user" from "no transactions"
	user, err := telegram.GetUserByTelegramID(req.TelegramID, *s.Bot)
	if err != nil {
		log.Warnf("[api/usertransactions] User not found for Telegram ID %d: %v", req.TelegramID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(UserTransactionsResponse{
			Success:      false,
			TelegramID:   req.TelegramID,
			Count:        0,
			Limit:        limit,
			Offset:       offset,
			Transactions: []UserTransactionData{},
			Message:      "User not found or has no wallet",
		})
		return
	}

	// Fetch internal transactions where the user is either sender or recipient.
	// Both sources are read from the newest entry up to offset+limit, merged
	// by time, and then the page is cut out.
	var internalTxs []telegram.Transaction
	dbTx := s.Bot.DB.Transactions.
		Where("from_id = ? OR to_id = ?", req.TelegramID, req.TelegramID).
		Order("time desc").
		Limit(window).
		Find(&internalTxs)
	if dbTx.Error != nil {
		log.Errorf("[api/usertransactions] Database error fetching transactions for user %d: %v", req.TelegramID, dbTx.Error)
		RespondError(w, "Failed to retrieve transactions")
		return
	}

	type entry struct {
		at   time.Time
		data UserTransactionData
	}
	entries := make([]entry, 0, len(internalTxs))
	for _, tx := range internalTxs {
		entries = append(entries, entry{tx.Time, internalEntry(req.TelegramID, tx)})
	}

	for _, p := range s.lightningPayments(user, window) {
		entries = append(entries, entry{time.Unix(int64(p.Time), 0).UTC(), lightningEntry(req.TelegramID, p, s.lnurlComment(p))})
	}

	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })
	transactions := make([]UserTransactionData, 0, limit)
	for i := offset; i < len(entries) && i < window; i++ {
		transactions = append(transactions, entries[i].data)
	}

	log.Infof("[api/usertransactions] Retrieved %d transaction(s) for user %d", len(transactions), req.TelegramID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(UserTransactionsResponse{
		Success:      true,
		TelegramID:   req.TelegramID,
		Count:        len(transactions),
		Limit:        limit,
		Offset:       offset,
		Transactions: transactions,
		Message:      "Transactions retrieved successfully",
	})
}

// lightningPayments returns the user's newest LNbits payments, up to n,
// leaving out those that are internal transfers (already in the internal
// table, matched by payment hash). Errors are logged and give no payments, so
// the internal history is still returned.
func (s Service) lightningPayments(user *lnbits.User, n int) lnbits.Payments {
	if user.Wallet == nil || s.Bot.Client == nil {
		return nil
	}
	payments, err := s.Bot.Client.PaymentsWithOptions(*user.Wallet, n, 0)
	if err != nil {
		log.Warnf("[api/usertransactions] LNbits payments for user %d: %v", user.Telegram.ID, err)
		return nil
	}
	hashes := make([]string, 0, len(payments))
	for _, p := range payments {
		if p.PaymentHash != "" {
			hashes = append(hashes, p.PaymentHash)
		}
	}
	internal := map[string]bool{}
	if len(hashes) > 0 {
		var found []string
		if err := s.Bot.DB.Transactions.Model(&telegram.Transaction{}).
			Where("invoice_payment_hash IN ?", hashes).
			Pluck("invoice_payment_hash", &found).Error; err != nil {
			log.Warnf("[api/usertransactions] Matching internal transfers for user %d: %v", user.Telegram.ID, err)
			return nil
		}
		for _, h := range found {
			internal[h] = true
		}
	}
	out := payments[:0]
	for _, p := range payments {
		if !internal[p.PaymentHash] {
			out = append(out, p)
		}
	}
	return out
}

// internalEntry turns an internal transfer into a history entry.
func internalEntry(telegramID int64, tx telegram.Transaction) UserTransactionData {
	direction := "incoming"
	if tx.FromId == telegramID {
		direction = "outgoing"
	}
	status := "failed"
	if tx.Success {
		status = "success"
	}
	return UserTransactionData{
		ID:          tx.ID,
		Time:        tx.Time.Format(time.RFC3339),
		Direction:   direction,
		FromID:      tx.FromId,
		ToID:        tx.ToId,
		FromUser:    tx.FromUser,
		ToUser:      tx.ToUser,
		Type:        tx.Type,
		Amount:      tx.Amount,
		AmountLKR:   getLKRValue(tx.Amount),
		Memo:        tx.Memo,
		Success:     tx.Success,
		Status:      status,
		PaymentHash: tx.Invoice.PaymentHash,
	}
}

// lnurlComment returns the payer's comment the bot stored for a
// Lightning-address receipt ("lnurl-p:<payment_hash>"), or "". It is only
// looked up when LNbits has no memo.
func (s Service) lnurlComment(p lnbits.Payment) string {
	if p.Memo != "" || p.PaymentHash == "" || s.Bot == nil || s.Bot.Bunt == nil {
		return ""
	}
	rec := &telegram.LNURLInvoice{Invoice: &telegram.Invoice{PaymentHash: p.PaymentHash}}
	if err := s.Bot.Bunt.Get(rec); err != nil {
		return ""
	}
	return rec.Comment
}

// lightningEntry turns an LNbits payment (amounts in msat, negative when
// outgoing) into a history entry. comment is used as the memo when LNbits
// has none. Only the fields below are copied: never the webhook (it holds
// the webhook secret), wallet_id, checking_id or any wallet key.
func lightningEntry(telegramID int64, p lnbits.Payment, comment string) UserTransactionData {
	amount, fee := p.Amount/1000, p.Fee/1000
	direction, from, to := "incoming", int64(0), telegramID
	if p.Amount < 0 {
		amount, direction, from, to = -amount, "outgoing", telegramID, 0
	}
	if fee < 0 {
		fee = -fee
	}
	status := p.Status
	if status == "" {
		status = "success"
		if p.Pending {
			status = "pending"
		}
	}
	memo := p.Memo
	if memo == "" {
		memo = comment
	}
	return UserTransactionData{
		Time:        time.Unix(int64(p.Time), 0).UTC().Format(time.RFC3339),
		Direction:   direction,
		FromID:      from,
		ToID:        to,
		Type:        "lightning",
		Amount:      amount,
		AmountLKR:   getLKRValue(amount),
		Memo:        memo,
		Success:     status == "success",
		Fee:         fee,
		Status:      status,
		PaymentHash: p.PaymentHash,
	}
}
