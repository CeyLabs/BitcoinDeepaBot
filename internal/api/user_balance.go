package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	log "github.com/sirupsen/logrus"
)

// UserBalanceRequest represents the JSON request for the user balance API
type UserBalanceRequest struct {
	TelegramID int64 `json:"telegram_id"` // Telegram user ID
}

// UserBalanceResponse represents the JSON response for the user balance API
type UserBalanceResponse struct {
	Success    bool   `json:"success"`
	TelegramID int64  `json:"telegram_id"`
	Balance    int64  `json:"balance"`               // Balance in satoshis
	BalanceLKR string `json:"balance_lkr,omitempty"` // LKR conversion
	FirstName  string `json:"first_name,omitempty"`
	LastName   string `json:"last_name,omitempty"`
	Username   string `json:"username,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	Message    string `json:"message,omitempty"`
}

// UserBalance handles the /api/v1/userbalance endpoint for getting user balance by Telegram ID
func (s Service) UserBalance(w http.ResponseWriter, r *http.Request) {
	var req UserBalanceRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		log.Errorf("[api/userbalance] Invalid JSON request: %v", err)
		RespondError(w, "Invalid JSON request")
		return
	}

	// Validate request
	if req.TelegramID <= 0 {
		RespondError(w, "Invalid Telegram ID")
		return
	}

	// Get user by Telegram ID
	user, err := telegram.GetUserByTelegramID(req.TelegramID, *s.Bot)
	if err != nil {
		log.Errorf("[api/userbalance] Failed to get user by Telegram ID %d: %v", req.TelegramID, err)
		response := UserBalanceResponse{
			Success:    false,
			TelegramID: req.TelegramID,
			Balance:    0,
			Message:    "User not found or has no wallet",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
		return
	}

	// Get user balance
	balance, err := s.Bot.GetUserBalance(user)
	if err != nil {
		log.Errorf("[api/userbalance] Failed to get balance for user %d: %v", req.TelegramID, err)
		response := UserBalanceResponse{
			Success:    false,
			TelegramID: req.TelegramID,
			Balance:    0,
			Message:    "Failed to retrieve balance",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
		return
	}

	// Convert to LKR if possible
	balanceLKR := getLKRValue(balance)

	// Extract user details
	var firstName, lastName, username string
	if user.Telegram != nil {
		firstName = user.Telegram.FirstName
		lastName = user.Telegram.LastName
		username = user.Telegram.Username
	}

	response := UserBalanceResponse{
		Success:    true,
		TelegramID: req.TelegramID,
		Balance:    balance,
		BalanceLKR: balanceLKR,
		FirstName:  firstName,
		LastName:   lastName,
		Username:   username,
		CreatedAt:  user.CreatedAt.Format(time.RFC3339),
		Message:    "Balance retrieved successfully",
	}

	log.Infof("[api/userbalance] Balance retrieved for user %d: %d sats", req.TelegramID, balance)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}
