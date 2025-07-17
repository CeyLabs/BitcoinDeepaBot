package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
)

// SendRequest represents the JSON request for the send API
type SendRequest struct {
	To     string `json:"to"`     // Telegram user ID (numeric)
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

// isTelegramID checks if the given string is a valid Telegram ID (numeric)
func isTelegramID(identifier string) bool {
	// Remove @ prefix if present
	identifier = strings.TrimPrefix(identifier, "@")
	// Check if it's all digits and has reasonable length for Telegram ID
	match, _ := regexp.MatchString(`^[0-9]{5,15}$`, identifier)
	return match
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
	if req.To == "" {
		RespondError(w, "Missing 'to' field")
		return
	}
	if req.Amount <= GetMinAPITransactionAmount() {
		RespondError(w, fmt.Sprintf("Amount must be greater than %s", utils.FormatSats(GetMinAPITransactionAmount())))
		return
	}
	if req.Amount > GetMaxAPITransactionAmount() {
		RespondError(w, fmt.Sprintf("Amount cannot exceed %s", utils.FormatSats(GetMaxAPITransactionAmount())))
		return
	}

	// Check if amount requires admin approval
	requiresApproval := req.Amount > GetAdminApprovalThreshold()
	if len(req.Memo) > GetMaxMemoLength() {
		RespondError(w, fmt.Sprintf("Memo cannot exceed %d characters", GetMaxMemoLength()))
		return
	}

	// Parse sender Telegram ID
	fromUserIdStr := GetAPIFromUserId()
	fromUserId, err := strconv.ParseInt(fromUserIdStr, 10, 64)
	if err != nil {
		log.Errorf("[api/send] Invalid sender Telegram ID %d: %v", fromUserId, err)
		RespondError(w, "Invalid sender configuration")
		return
	}

	// Validate recipient Telegram ID
	if !isTelegramID(req.To) {
		RespondError(w, "Recipient 'to' field must be a valid Telegram user ID (numeric)")
		return
	}

	// Get the sender user
	fromUser, err := telegram.GetUserByTelegramID(fromUserId, *s.Bot)
	if err != nil {
		log.Errorf("[api/send] Could not find sender user %d: %v", fromUserId, err)
		RespondError(w, fmt.Sprintf("Sender '%d' not found or has no wallet", fromUserId))
		return
	}

	// Check sender's balance
	balance, err := s.Bot.GetUserBalance(fromUser)
	if err != nil {
		log.Errorf("[api/send] Could not get balance for %d: %v", fromUserId, err)
		RespondError(w, "Could not check sender balance")
		return
	}

	if balance < req.Amount {
		log.Warnf("[api/send] Insufficient balance for %d: %d < %d", fromUserId, balance, req.Amount)
		RespondError(w, fmt.Sprintf("Insufficient balance: %s available, %s required", utils.FormatSats(balance), utils.FormatSats(req.Amount)))
		return
	}

	// Parse recipient Telegram ID
	toUserId, err := strconv.ParseInt(req.To, 10, 64)
	if err != nil {
		log.Errorf("[api/send] Invalid Telegram ID %s: %v", req.To, err)
		RespondError(w, fmt.Sprintf("Invalid Telegram ID '%s'", req.To))
		return
	}

	// Find recipient by Telegram ID
	toUser, err := telegram.GetUserByTelegramID(toUserId, *s.Bot)
	if err != nil {
		log.Errorf("[api/send] Could not find recipient user with ID %d: %v", toUserId, err)
		RespondError(w, fmt.Sprintf("Recipient '%d' not found or has no wallet", toUserId))
		return
	}
	log.Infof("[api/send] Found recipient by Telegram ID: %d", toUserId)

	// Check if trying to send to self
	if fromUser.ID == toUser.ID {
		RespondError(w, "Cannot send to yourself")
		return
	}

	// Check if amount requires admin approval
	if requiresApproval {
		log.Infof("[api/send] Large transaction requires admin approval: %d -> %d (%d sat(s))", fromUserId, toUserId, req.Amount)

		// Create pending transaction
		clientIP := getClientIP(r)
		pendingTx := NewPendingTransaction(&req, fromUser, toUser, clientIP, fromUserId)

		// Save to database
		err = pendingTx.SaveToDB(s.Bot)
		if err != nil {
			log.Errorf("[api/send] Failed to save pending transaction: %v", err)
			RespondError(w, "Failed to create pending transaction")
			return
		}

		// Send approval request using Telegram callback buttons (same as /send command)
		err = telegram.CreateAPIApprovalRequest(s.Bot, fromUser, toUserId, req.Amount, req.Memo, pendingTx.ID, clientIP)
		if err != nil {
			log.Warnf("[api/send] Failed to send approval request: %v", err)
		}

		response := SendResponse{
			Success: false,
			Message: fmt.Sprintf("Transaction requires admin approval (amount: %s > threshold: %s). Approval request sent to you via Telegram. Transaction ID: %s",
				utils.FormatSats(req.Amount), utils.FormatSats(GetAdminApprovalThreshold()), pendingTx.ID),
			FromUser: fromUserIdStr,
			ToUser:   req.To,
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

	log.Infof("[api/send] ✅ API Send successful: %s -> %s (%d sat(s))", fromUserStr, toUserStr, req.Amount)

	// Send notification to recipient with memo included in same message
	fromUserStrMd := telegram.GetUserStrMd(fromUser.Telegram)
	notificationMsg := fmt.Sprintf("💰 You received %s from %s via Automated API", utils.FormatSats(req.Amount), fromUserStrMd)
	if req.Memo != "" {
		notificationMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(req.Memo))
	}

	_, err = s.Bot.Telegram.Send(toUser.Telegram, notificationMsg)
	if err != nil {
		log.Warnf("[api/send] Could not send notification to recipient: %v", err)
	}

	// Send confirmation to sender (from user) - same format as /send command
	toUserStrMd := telegram.GetUserStrMd(toUser.Telegram)
	senderConfirmationMsg := fmt.Sprintf("✅ Payment sent successfully!\n\n💸 Amount: %s\n👤 To: %s", utils.FormatSats(req.Amount), toUserStrMd)
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
		FromUser: fromUserIdStr,
		ToUser:   req.To,
		Amount:   req.Amount,
		Memo:     req.Memo,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}
