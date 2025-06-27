package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/pkg/lightning"
	log "github.com/sirupsen/logrus"
)

// SendRequest represents the JSON request for the send API
type SendRequest struct {
	From   string `json:"from"`   // Telegram username (without @) - must be whitelisted
	To     string `json:"to"`     // Telegram username (without @) or wallet ID
	Amount int64  `json:"amount"` // Amount in satoshis
	Memo   string `json:"memo"`   // Optional memo
}

// SendResponse represents the JSON response for the send API
type SendResponse struct {
	Success         bool   `json:"success"`
	TransactionHash string `json:"transaction_hash,omitempty"`
	Message         string `json:"message"`
	FromUser        string `json:"from_user"`
	ToUser          string `json:"to_user"`
	Amount          int64  `json:"amount"`
	Memo            string `json:"memo,omitempty"`
}

// InternalNetworkMiddleware restricts access to internal network IPs (configurable)
func InternalNetworkMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientIP := getClientIP(r)

		// Parse the client IP
		ip := net.ParseIP(clientIP)
		if ip == nil {
			log.Warnf("[api/send] Invalid client IP: %s", clientIP)
			http.Error(w, "Invalid client IP", http.StatusForbidden)
			return
		}

		// Check if IP is in the internal network range defined in config
		_, internalNet, err := net.ParseCIDR(GetInternalNetworkCIDR())
		if err != nil {
			log.Errorf("[api/send] Invalid internal network CIDR configuration: %s, error: %v", GetInternalNetworkCIDR(), err)
			http.Error(w, "Internal server configuration error", http.StatusInternalServerError)
			return
		}

		if !internalNet.Contains(ip) {
			log.Warnf("[api/send] Access denied for IP: %s (not in internal network %s)", clientIP, GetInternalNetworkCIDR())
			http.Error(w, "Access denied: Internal network only", http.StatusForbidden)
			return
		}

		log.Debugf("[api/send] Access granted for internal IP: %s", clientIP)
		next.ServeHTTP(w, r)
	}
}

// getClientIP extracts the real client IP from the request
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can contain multiple IPs, take the first one
		if ips := strings.Split(xff, ","); len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// isWhitelistedAccount checks if the from account is in the whitelist
func isWhitelistedAccount(username string) bool {
	username = strings.TrimPrefix(username, "@")
	for _, allowed := range GetWhitelistedFromAccounts() {
		if strings.EqualFold(username, allowed) {
			return true
		}
	}
	return false
}

// Send handles the /api/send endpoint for programmatic Bitcoin Lightning payments
func (s Service) Send(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		log.Errorf("[api/send] Invalid JSON request: %v", err)
		RespondError(w, "Invalid JSON request")
		return
	}

	// Validate request
	if req.From == "" {
		RespondError(w, "Missing 'from' field")
		return
	}
	if req.To == "" {
		RespondError(w, "Missing 'to' field")
		return
	}
	if req.Amount <= GetMinAPITransactionAmount() {
		RespondError(w, fmt.Sprintf("Amount must be greater than %d satoshis", GetMinAPITransactionAmount()))
		return
	}
	if req.Amount > GetMaxAPITransactionAmount() {
		RespondError(w, fmt.Sprintf("Amount cannot exceed %d satoshis", GetMaxAPITransactionAmount()))
		return
	}

	// Check if amount requires admin approval
	requiresApproval := req.Amount > GetAdminApprovalThreshold()
	if len(req.Memo) > GetMaxMemoLength() {
		RespondError(w, fmt.Sprintf("Memo cannot exceed %d characters", GetMaxMemoLength()))
		return
	}

	// Clean usernames (remove @ if present)
	fromUsername := strings.TrimPrefix(req.From, "@")
	toIdentifier := strings.TrimPrefix(req.To, "@")

	// Check if from account is whitelisted
	if !isWhitelistedAccount(fromUsername) {
		log.Warnf("[api/send] Unauthorized sender: %s", fromUsername)
		RespondError(w, fmt.Sprintf("Sender account '@%s' is not authorized", fromUsername))
		return
	}

	// Get the sender user
	fromUser, err := telegram.GetUserByTelegramUsername(fromUsername, *s.Bot)
	if err != nil {
		log.Errorf("[api/send] Could not find sender user %s: %v", fromUsername, err)
		RespondError(w, fmt.Sprintf("Sender '@%s' not found or has no wallet", fromUsername))
		return
	}

	// Check sender's balance
	balance, err := s.Bot.GetUserBalance(fromUser)
	if err != nil {
		log.Errorf("[api/send] Could not get balance for %s: %v", fromUsername, err)
		RespondError(w, "Could not check sender balance")
		return
	}

	if balance < req.Amount {
		log.Warnf("[api/send] Insufficient balance for %s: %d < %d", fromUsername, balance, req.Amount)
		RespondError(w, fmt.Sprintf("Insufficient balance: %d sat available, %d sat required", balance, req.Amount))
		return
	}

	// Check if 'to' is a Lightning address
	if lightning.IsLightningAddress(toIdentifier) {
		log.Infof("[api/send] Sending to Lightning address: %s", toIdentifier)
		err = s.sendToLightningAddress(fromUser, toIdentifier, req.Amount, req.Memo)
		if err != nil {
			log.Errorf("[api/send] Lightning address payment failed: %v", err)
			RespondError(w, fmt.Sprintf("Lightning address payment failed: %v", err))
			return
		}

		response := SendResponse{
			Success:  true,
			Message:  "Payment sent successfully to Lightning address",
			FromUser: fromUsername,
			ToUser:   toIdentifier,
			Amount:   req.Amount,
			Memo:     req.Memo,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
		return
	}

	// Try to find recipient by Telegram username
	toUser, err := telegram.GetUserByTelegramUsername(toIdentifier, *s.Bot)
	if err != nil {
		log.Errorf("[api/send] Could not find recipient user %s: %v", toIdentifier, err)
		RespondError(w, fmt.Sprintf("Recipient '@%s' not found or has no wallet", toIdentifier))
		return
	}

	// Check if trying to send to self
	if fromUser.ID == toUser.ID {
		RespondError(w, "Cannot send to yourself")
		return
	}

	// Check if amount requires admin approval
	if requiresApproval {
		log.Infof("[api/send] Large transaction requires admin approval: %s -> %s (%d sat)", fromUsername, toIdentifier, req.Amount)

		// Create pending transaction
		clientIP := getClientIP(r)
		pendingTx := NewPendingTransaction(&req, fromUser, toUser, clientIP)

		// Save to database
		err = pendingTx.SaveToDB(s.Bot)
		if err != nil {
			log.Errorf("[api/send] Failed to save pending transaction: %v", err)
			RespondError(w, "Failed to create pending transaction")
			return
		}

		// Send approval request using Telegram callback buttons (same as /send command)
		err = telegram.CreateAPIApprovalRequest(s.Bot, fromUser, toIdentifier, req.Amount, req.Memo, pendingTx.ID, clientIP)
		if err != nil {
			log.Warnf("[api/send] Failed to send approval request: %v", err)
		}

		response := SendResponse{
			Success: false,
			Message: fmt.Sprintf("Transaction requires admin approval (amount: %d sat > threshold: %d sat). Approval request sent to you via Telegram. Transaction ID: %s",
				req.Amount, GetAdminApprovalThreshold(), pendingTx.ID),
			FromUser: fromUsername,
			ToUser:   toIdentifier,
			Amount:   req.Amount,
			Memo:     req.Memo,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted) // 202 Accepted - request received but needs approval
		json.NewEncoder(w).Encode(response)
		return
	}

	// Create transaction memo
	fromUserStr := telegram.GetUserStr(fromUser.Telegram)
	toUserStr := telegram.GetUserStr(toUser.Telegram)
	transactionMemo := fmt.Sprintf("💸 API Send from %s to %s.", fromUserStr, toUserStr)
	if req.Memo != "" {
		transactionMemo += fmt.Sprintf(" Memo: %s", req.Memo)
	}

	// Create and execute transaction
	t := telegram.NewTransaction(s.Bot, fromUser, toUser, req.Amount, telegram.TransactionType("api_send"))
	t.Memo = transactionMemo

	success, err := t.Send()
	if !success || err != nil {
		log.Errorf("[api/send] Transaction failed from %s to %s: %v", fromUserStr, toUserStr, err)
		if s.Bot.ErrorLogger != nil {
			s.Bot.ErrorLogger.LogTransactionError(err, "api_send", req.Amount, fromUser.Telegram, toUser.Telegram)
		}
		RespondError(w, fmt.Sprintf("Transaction failed: %v", err))
		return
	}

	log.Infof("[api/send] ✅ API Send successful: %s -> %s (%d sat)", fromUserStr, toUserStr, req.Amount)

	// Send notification to recipient with memo included in same message
	fromUserStrMd := telegram.GetUserStrMd(fromUser.Telegram)
	notificationMsg := fmt.Sprintf("💰 You received %d sat from %s via Automated API", req.Amount, fromUserStrMd)
	if req.Memo != "" {
		notificationMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(req.Memo))
	}

	_, err = s.Bot.Telegram.Send(toUser.Telegram, notificationMsg)
	if err != nil {
		log.Warnf("[api/send] Could not send notification to recipient: %v", err)
	}

	// Send confirmation to sender (from user) - same format as /send command
	toUserStrMd := telegram.GetUserStrMd(toUser.Telegram)
	senderConfirmationMsg := fmt.Sprintf("✅ Payment sent successfully!\n\n💸 Amount: %d sat\n👤 To: %s", req.Amount, toUserStrMd)
	if req.Memo != "" {
		senderConfirmationMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(req.Memo))
	}

	_, err = s.Bot.Telegram.Send(fromUser.Telegram, senderConfirmationMsg)
	if err != nil {
		log.Warnf("[api/send] Could not send confirmation to sender: %v", err)
	}

	response := SendResponse{
		Success:  true,
		Message:  "Payment sent successfully",
		FromUser: fromUsername,
		ToUser:   toIdentifier,
		Amount:   req.Amount,
		Memo:     req.Memo,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// sendToLightningAddress handles sending to Lightning addresses
func (s Service) sendToLightningAddress(fromUser *lnbits.User, lightningAddress string, amount int64, memo string) error {
	// This is a simplified implementation - you may need to implement the full Lightning address protocol
	// For now, we'll return an error as this requires additional Lightning address handling logic
	return fmt.Errorf("Lightning address payments not yet implemented in API")
}
