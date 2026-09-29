package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	log "github.com/sirupsen/logrus"
)

// UserTransactionsRequest represents the JSON request for the user transactions API
type UserTransactionsRequest struct {
	TelegramID int64 `json:"telegram_id"` // Telegram user ID
	Limit      int   `json:"limit"`       // Optional: max transactions to return (default 100, max 250)
	Offset     int   `json:"offset"`      // Optional: number of transactions to skip for pagination
}

// UserTransactionData represents a single internal transaction in the response
type UserTransactionData struct {
	ID        uint   `json:"id"`
	Time      string `json:"time"`
	Direction string `json:"direction"` // "incoming" or "outgoing" relative to the queried user
	FromID    int64  `json:"from_id"`
	ToID      int64  `json:"to_id"`
	FromUser  string `json:"from_user"`
	ToUser    string `json:"to_user"`
	Type      string `json:"type"`
	Amount    int64  `json:"amount"` // Amount in satoshis
	AmountLKR string `json:"amount_lkr,omitempty"`
	Memo      string `json:"memo,omitempty"`
	Success   bool   `json:"success"`
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
// internal transaction history by Telegram ID. Authenticated with the same wallet HMAC
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

	// Confirm the user exists so we can distinguish "unknown user" from "no transactions"
	if _, err := telegram.GetUserByTelegramID(req.TelegramID, *s.Bot); err != nil {
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

	// Fetch internal transactions where the user is either sender or recipient
	var internalTxs []telegram.Transaction
	dbTx := s.Bot.DB.Transactions.
		Where("from_id = ? OR to_id = ?", req.TelegramID, req.TelegramID).
		Order("time desc").
		Limit(limit).Offset(offset).
		Find(&internalTxs)
	if dbTx.Error != nil {
		log.Errorf("[api/usertransactions] Database error fetching transactions for user %d: %v", req.TelegramID, dbTx.Error)
		RespondError(w, "Failed to retrieve transactions")
		return
	}

	transactions := make([]UserTransactionData, 0, len(internalTxs))
	for _, tx := range internalTxs {
		direction := "incoming"
		if tx.FromId == req.TelegramID {
			direction = "outgoing"
		}
		transactions = append(transactions, UserTransactionData{
			ID:        tx.ID,
			Time:      tx.Time.Format(time.RFC3339),
			Direction: direction,
			FromID:    tx.FromId,
			ToID:      tx.ToId,
			FromUser:  tx.FromUser,
			ToUser:    tx.ToUser,
			Type:      tx.Type,
			Amount:    tx.Amount,
			AmountLKR: getLKRValue(tx.Amount),
			Memo:      tx.Memo,
			Success:   tx.Success,
		})
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
