package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	"github.com/LightningTipBot/LightningTipBot/pkg/lightning"
	log "github.com/sirupsen/logrus"
)

// SendRequest represents the JSON request for the send API
type SendRequest struct {
	To     string `json:"to"`     // Telegram username (without @), Telegram ID, or wallet ID
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
	AmountLKR       string `json:"amount_lkr,omitempty"` // LKR conversion
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

	// Get authenticated wallet from context (set by WalletHMACMiddleware)
	authenticatedWallet := r.Context().Value("authenticated_wallet")
	if authenticatedWallet == nil {
		log.Error("[api/send] No authenticated wallet found in request context")
		RespondError(w, "Authentication failed")
		return
	}

	walletID := authenticatedWallet.(string)
	wallet, exists := GetWhitelistedWallets()[walletID]
	if !exists {
		log.Errorf("[api/send] Authenticated wallet %s not found in configuration", walletID)
		RespondError(w, "Invalid wallet configuration")
		return
	}

	fromUsername := wallet.Username

	// Validate request
	if req.To == "" {
		RespondError(w, "Missing 'to' field")
		return
	}
	if req.Amount <= GetMinAPITransactionAmount() {
		RespondError(w, fmt.Sprintf("Amount must be greater than %s", thirdparty.FormatSatsWithLKR(GetMinAPITransactionAmount())))
		return
	}
	if req.Amount > GetMaxAPITransactionAmount() {
		RespondError(w, fmt.Sprintf("Amount cannot exceed %s", thirdparty.FormatSatsWithLKR(GetMaxAPITransactionAmount())))
		return
	}

	// Check if amount requires admin approval
	requiresApproval := req.Amount > GetAdminApprovalThreshold()
	if len(req.Memo) > GetMaxMemoLength() {
		RespondError(w, fmt.Sprintf("Memo cannot exceed %d characters", GetMaxMemoLength()))
		return
	}

	if req.Memo != "" {
		memoLockKey := fmt.Sprintf("api_send_memo_%s", req.Memo)

		// Try to acquire lock first to prevent concurrent processing
		if success := s.MemoCache.SetNX(memoLockKey, "locked"); !success {
			log.Warnf("[api/send] Transaction with memo '%s' is already processing", req.Memo)
			RespondError(w, fmt.Sprintf("Transaction with memo '%s' is already processing", req.Memo))
			return
		}
		// Unlock when done
		defer s.MemoCache.Delete(memoLockKey)

		// Check if transaction with this memo already exists in database
		// We search for the memo in the transaction memo field
		// The stored memo format is: "💸 API Send from @User to @User. Memo: <req.Memo>"
		// So we search for the suffix "Memo: <req.Memo>"
		var count int64
		memoSearch := fmt.Sprintf("%%Memo: %s", req.Memo)
		err := s.Bot.DB.Transactions.Model(&telegram.Transaction{}).Where("memo LIKE ? AND success = ?", memoSearch, true).Count(&count).Error
		if err != nil {
			log.Errorf("[api/send] Database error checking for duplicate memo: %v", err)
			// Continue but log error - fail open or closed? Let's fail closed for safety
			RespondError(w, "Internal server error checking transaction history")
			return
		}
		if count > 0 {
			log.Warnf("[api/send] Transaction with memo '%s' already completed", req.Memo)
			RespondError(w, fmt.Sprintf("Transaction with memo '%s' already completed", req.Memo))
			return
		}
	}

	// Clean usernames (remove @ if present)
	toIdentifier := strings.TrimPrefix(req.To, "@")

	// Get the sender user
	fromUser, err := telegram.GetUserByTelegramUsername(fromUsername, *s.Bot)
	if err != nil {
		log.Errorf("[api/send] Could not find sender user %s: %v", fromUsername, err)
		RespondError(w, fmt.Sprintf("Sender '@%s' not found or has no wallet", fromUsername))
		return
	}

	// Check sender's available balance (wallet balance - pot balance)
	balance, err := s.Bot.GetUserAvailableBalance(fromUser)
	if err != nil {
		log.Errorf("[api/send] Could not get available balance for %s: %v", fromUsername, err)
		RespondError(w, "Could not check sender balance")
		return
	}

	if balance < req.Amount {
		log.Warnf("[api/send] Insufficient available balance for %s: %d < %d", fromUsername, balance, req.Amount)
		RespondError(w, fmt.Sprintf("Insufficient balance: %s available, %s required", thirdparty.FormatSatsWithLKR(balance), thirdparty.FormatSatsWithLKR(req.Amount)))
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
			Success:   true,
			Message:   "Payment sent successfully to Lightning address",
			FromUser:  fromUsername,
			ToUser:    toIdentifier,
			Amount:    req.Amount,
			AmountLKR: getLKRValue(req.Amount),
			Memo:      req.Memo,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
		return
	}

	// Try to find recipient by Telegram username or ID
	var toUser *lnbits.User
	if isTelegramID(toIdentifier) {
		// It's a Telegram ID
		telegramID, err := strconv.ParseInt(toIdentifier, 10, 64)
		if err != nil {
			log.Errorf("[api/send] Invalid Telegram ID %s: %v", toIdentifier, err)
			RespondError(w, fmt.Sprintf("Invalid Telegram ID '%s'", toIdentifier))
			return
		}
		toUser, err = telegram.GetUserByTelegramID(telegramID, *s.Bot)
		if err != nil {
			log.Errorf("[api/send] Could not find recipient user with ID %d: %v", telegramID, err)
			RespondError(w, fmt.Sprintf("Recipient '%s' not found or has no wallet", toIdentifier))
			return
		}
		log.Infof("[api/send] Found recipient by Telegram ID: %d", telegramID)
	} else {
		// It's a Telegram username
		toUser, err = telegram.GetUserByTelegramUsername(toIdentifier, *s.Bot)
		if err != nil {
			log.Errorf("[api/send] Could not find recipient user %s: %v", toIdentifier, err)
			RespondError(w, fmt.Sprintf("Recipient '@%s' not found or has no wallet", toIdentifier))
			return
		}
		log.Infof("[api/send] Found recipient by username: %s", toIdentifier)
	}

	// Check if trying to send to self
	if fromUser.ID == toUser.ID {
		RespondError(w, "Cannot send to yourself")
		return
	}

	// Check if amount requires admin approval
	if requiresApproval {
		log.Infof("[api/send] Large transaction requires admin approval: %s -> %s (%d sat(s))", fromUsername, toIdentifier, req.Amount)

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
			Message: fmt.Sprintf("Transaction requires admin approval (amount: %s > threshold: %s). Approval request sent to you via Telegram. Transaction ID: %s",
				thirdparty.FormatSatsWithLKR(req.Amount), thirdparty.FormatSatsWithLKR(GetAdminApprovalThreshold()), pendingTx.ID),
			FromUser:  fromUsername,
			ToUser:    toIdentifier,
			Amount:    req.Amount,
			AmountLKR: getLKRValue(req.Amount),
			Memo:      req.Memo,
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
	notificationMsg := fmt.Sprintf("💰 You received %s from %s via Automated API", thirdparty.FormatSatsWithLKR(req.Amount), fromUserStrMd)
	if req.Memo != "" {
		notificationMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(req.Memo))
	}

	_, err = s.Bot.Telegram.Send(toUser.Telegram, notificationMsg)
	if err != nil {
		log.Warnf("[api/send] Could not send notification to recipient: %v", err)
	}

	// Send confirmation to sender (from user) - same format as /send command
	toUserStrMd := telegram.GetUserStrMd(toUser.Telegram)
	senderConfirmationMsg := fmt.Sprintf("✅ Payment sent successfully!\n\n💸 Amount: %s\n👤 To: %s", thirdparty.FormatSatsWithLKR(req.Amount), toUserStrMd)
	if req.Memo != "" {
		senderConfirmationMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(req.Memo))
	}

	_, err = s.Bot.Telegram.Send(fromUser.Telegram, senderConfirmationMsg)
	if err != nil {
		log.Warnf("[api/send] Could not send confirmation to sender: %v", err)
	}

	response := SendResponse{
		Success:   true,
		Message:   "Payment sent successfully",
		FromUser:  fromUsername,
		ToUser:    toIdentifier,
		Amount:    req.Amount,
		AmountLKR: getLKRValue(req.Amount),
		Memo:      req.Memo,
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

// getLKRValue converts satoshi amount to LKR string or returns empty string if price unavailable
func getLKRValue(amount int64) string {
	lkrPerSat, _, err := thirdparty.GetSatPrice()
	if err != nil {
		return "" // Return empty string if LKR price is unavailable
	}
	lkrValue := lkrPerSat * float64(amount)
	return utils.FormatFloatWithCommas(lkrValue)
}
