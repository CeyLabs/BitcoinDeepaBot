package admin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/BitcoinDeepaBot/BitcoinDeepaBot/internal/api"
	"github.com/BitcoinDeepaBot/BitcoinDeepaBot/internal/telegram"
	"github.com/BitcoinDeepaBot/BitcoinDeepaBot/internal/thirdparty"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// ApprovePendingTransaction handles admin approval of pending transactions
func (s Service) ApprovePendingTransaction(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	transactionID := vars["id"]

	if transactionID == "" {
		http.Error(w, "Transaction ID is required", http.StatusBadRequest)
		return
	}

	// Load pending transaction
	pendingTx, err := api.LoadPendingTransaction(transactionID, s.bot)
	if err != nil {
		log.Errorf("[ADMIN] Failed to load pending transaction %s: %v", transactionID, err)
		http.Error(w, "Transaction not found", http.StatusNotFound)
		return
	}

	// Check if transaction can be approved
	if !pendingTx.CanBeApproved() {
		log.Warnf("[ADMIN] Transaction %s cannot be approved: status=%s, expired=%v",
			transactionID, pendingTx.Status, pendingTx.IsExpired())
		http.Error(w, fmt.Sprintf("Transaction cannot be approved: status=%s", pendingTx.Status), http.StatusBadRequest)
		return
	}

	// Approve the transaction
	approverIP := getClientIP(r)
	err = pendingTx.Approve(fmt.Sprintf("admin:%s", approverIP))
	if err != nil {
		log.Errorf("[ADMIN] Failed to approve transaction %s: %v", transactionID, err)
		http.Error(w, "Failed to approve transaction", http.StatusInternalServerError)
		return
	}

	// Save the updated transaction
	err = pendingTx.SaveToDB(s.bot)
	if err != nil {
		log.Errorf("[ADMIN] Failed to save approved transaction %s: %v", transactionID, err)
		http.Error(w, "Failed to save approval", http.StatusInternalServerError)
		return
	}

	// Execute the transaction
	err = s.executePendingTransaction(pendingTx)
	if err != nil {
		log.Errorf("[ADMIN] Failed to execute approved transaction %s: %v", transactionID, err)
		http.Error(w, fmt.Sprintf("Transaction approved but execution failed: %v", err), http.StatusInternalServerError)
		return
	}

	log.Infof("[ADMIN] Transaction %s approved and executed successfully", transactionID)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("Transaction %s approved and executed successfully", transactionID)))
}

// RejectPendingTransaction handles admin rejection of pending transactions
func (s Service) RejectPendingTransaction(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	transactionID := vars["id"]

	if transactionID == "" {
		http.Error(w, "Transaction ID is required", http.StatusBadRequest)
		return
	}

	// Load pending transaction
	pendingTx, err := api.LoadPendingTransaction(transactionID, s.bot)
	if err != nil {
		log.Errorf("[ADMIN] Failed to load pending transaction %s: %v", transactionID, err)
		http.Error(w, "Transaction not found", http.StatusNotFound)
		return
	}

	// Check if transaction can be rejected
	if pendingTx.Status != api.StatusPending {
		log.Warnf("[ADMIN] Transaction %s cannot be rejected: status=%s", transactionID, pendingTx.Status)
		http.Error(w, fmt.Sprintf("Transaction cannot be rejected: status=%s", pendingTx.Status), http.StatusBadRequest)
		return
	}

	// Reject the transaction
	rejectorIP := getClientIP(r)
	err = pendingTx.Reject(fmt.Sprintf("admin:%s", rejectorIP))
	if err != nil {
		log.Errorf("[ADMIN] Failed to reject transaction %s: %v", transactionID, err)
		http.Error(w, "Failed to reject transaction", http.StatusInternalServerError)
		return
	}

	// Save the updated transaction
	err = pendingTx.SaveToDB(s.bot)
	if err != nil {
		log.Errorf("[ADMIN] Failed to save rejected transaction %s: %v", transactionID, err)
		http.Error(w, "Failed to save rejection", http.StatusInternalServerError)
		return
	}

	log.Infof("[ADMIN] Transaction %s rejected successfully", transactionID)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("Transaction %s rejected successfully", transactionID)))
}

// ListPendingTransactions lists all pending transactions requiring approval
func (s Service) ListPendingTransactions(w http.ResponseWriter, r *http.Request) {
	// This would need to be implemented to query all pending transactions
	// For now, we'll return a placeholder response
	log.Info("[ADMIN] List pending transactions requested")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Pending transactions list - implementation needed"))
}

// executePendingTransaction executes an approved pending transaction
func (s Service) executePendingTransaction(pendingTx *api.PendingTransaction) error {
	// Load the users again to ensure they still exist and have wallets
	fromUser, err := telegram.GetUserByTelegramUsername(pendingTx.FromUsername, *s.bot)
	if err != nil {
		return fmt.Errorf("sender user %s no longer exists or has no wallet: %v", pendingTx.FromUsername, err)
	}

	toUser, err := telegram.GetUserByTelegramUsername(pendingTx.ToUsername, *s.bot)
	if err != nil {
		return fmt.Errorf("recipient user %s no longer exists or has no wallet: %v", pendingTx.ToUsername, err)
	}

	// Check sender's balance again
	balance, err := s.bot.GetUserBalance(fromUser)
	if err != nil {
		return fmt.Errorf("could not check sender balance: %v", err)
	}

	if balance < pendingTx.Amount {
		return fmt.Errorf("insufficient balance: %d sat available, %d sat required", balance, pendingTx.Amount)
	}

	// Create transaction memo
	fromUserStr := telegram.GetUserStr(fromUser.Telegram)
	toUserStr := telegram.GetUserStr(toUser.Telegram)
	transactionMemo := fmt.Sprintf("💸 Admin-approved API Send from %s to %s. TX ID: %s", fromUserStr, toUserStr, pendingTx.ID)
	if pendingTx.Memo != "" {
		transactionMemo += fmt.Sprintf(" Memo: %s", pendingTx.Memo)
	}

	// Create and execute transaction
	t := telegram.NewTransaction(s.bot, fromUser, toUser, pendingTx.Amount, telegram.TransactionType("api_send_admin_approved"))
	t.Memo = transactionMemo

	success, err := t.Send()
	if !success || err != nil {
		return fmt.Errorf("transaction execution failed: %v", err)
	}

	// Mark as executed
	err = pendingTx.Execute()
	if err != nil {
		log.Warnf("[ADMIN] Failed to mark transaction as executed: %v", err)
	}

	// Save the final state
	err = pendingTx.SaveToDB(s.bot)
	if err != nil {
		log.Warnf("[ADMIN] Failed to save executed transaction state: %v", err)
	}

	// Send notifications
	fromUserStrMd := telegram.GetUserStrMd(fromUser.Telegram)
	_, err = s.bot.Telegram.Send(toUser.Telegram, fmt.Sprintf("💰 You received %s from %s via admin-approved API payment", thirdparty.FormatSatsWithLKR(pendingTx.Amount), fromUserStrMd))
	if err != nil {
		log.Warnf("[ADMIN] Could not send notification to recipient: %v", err)
	}

	// Send memo if provided
	if pendingTx.Memo != "" {
		_, err = s.bot.Telegram.Send(toUser.Telegram, fmt.Sprintf("✉️ %s", pendingTx.Memo))
		if err != nil {
			log.Warnf("[ADMIN] Could not send memo to recipient: %v", err)
		}
	}

	log.Infof("[ADMIN] ✅ Admin-approved API Send executed: %s -> %s (%d sat) [TX: %s]",
		fromUserStr, toUserStr, pendingTx.Amount, pendingTx.ID)

	return nil
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
	if host := r.RemoteAddr; host != "" {
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			return host[:idx]
		}
		return host
	}

	return "unknown"
}
