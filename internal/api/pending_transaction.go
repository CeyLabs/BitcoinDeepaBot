package api

import (
	"fmt"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// PendingTransaction represents a transaction awaiting admin approval
type PendingTransaction struct {
	*storage.Base
	ID               string        `json:"id"`
	FromUser         *lnbits.User  `json:"from_user" gorm:"-"`
	ToUser           *lnbits.User  `json:"to_user" gorm:"-"`
	FromUsername     string        `json:"from_username"`
	ToUsername       string        `json:"to_username"`
	Amount           int64         `json:"amount"`
	Memo             string        `json:"memo"`
	RequestTimestamp time.Time     `json:"request_timestamp"`
	Status           string        `json:"status"` // "pending", "approved", "rejected", "expired"
	ApprovedBy       string        `json:"approved_by,omitempty"`
	ApprovalTime     *time.Time    `json:"approval_time,omitempty"`
	ExpiryTime       time.Time     `json:"expiry_time"`
	ClientIP         string        `json:"client_ip"`
	OriginalRequest  *SendRequest  `json:"original_request" gorm:"-"`
}

const (
	PendingTransactionExpiry = 24 * time.Hour // Pending transactions expire after 24 hours
	StatusPending            = "pending"
	StatusApproved           = "approved"
	StatusRejected           = "rejected"
	StatusExpired            = "expired"
	StatusExecuted           = "executed"
)

// NewPendingTransaction creates a new pending transaction
func NewPendingTransaction(req *SendRequest, fromUser, toUser *lnbits.User, clientIP string) *PendingTransaction {
	id := fmt.Sprintf("pending-%s-%s-%d-%d", req.From, req.To, req.Amount, time.Now().Unix())
	
	return &PendingTransaction{
		Base:             storage.New(storage.ID(id)),
		ID:               id,
		FromUser:         fromUser,
		ToUser:           toUser,
		FromUsername:     req.From,
		ToUsername:       req.To,
		Amount:           req.Amount,
		Memo:             req.Memo,
		RequestTimestamp: time.Now(),
		Status:           StatusPending,
		ExpiryTime:       time.Now().Add(PendingTransactionExpiry),
		ClientIP:         clientIP,
		OriginalRequest:  req,
	}
}

// IsExpired checks if the pending transaction has expired
func (pt *PendingTransaction) IsExpired() bool {
	return time.Now().After(pt.ExpiryTime)
}

// CanBeApproved checks if the transaction can still be approved
func (pt *PendingTransaction) CanBeApproved() bool {
	return pt.Status == StatusPending && !pt.IsExpired()
}

// Approve marks the transaction as approved
func (pt *PendingTransaction) Approve(approvedBy string) error {
	if !pt.CanBeApproved() {
		return fmt.Errorf("transaction cannot be approved: status=%s, expired=%v", pt.Status, pt.IsExpired())
	}
	
	now := time.Now()
	pt.Status = StatusApproved
	pt.ApprovedBy = approvedBy
	pt.ApprovalTime = &now
	
	return nil
}

// Reject marks the transaction as rejected
func (pt *PendingTransaction) Reject(rejectedBy string) error {
	if pt.Status != StatusPending {
		return fmt.Errorf("transaction cannot be rejected: status=%s", pt.Status)
	}
	
	now := time.Now()
	pt.Status = StatusRejected
	pt.ApprovedBy = rejectedBy // Store who rejected it
	pt.ApprovalTime = &now
	
	return nil
}

// Execute marks the transaction as executed (actually processed)
func (pt *PendingTransaction) Execute() error {
	if pt.Status != StatusApproved {
		return fmt.Errorf("transaction cannot be executed: status=%s", pt.Status)
	}
	
	pt.Status = StatusExecuted
	return nil
}

// GetFormattedMessage returns a formatted message for admin notification
func (pt *PendingTransaction) GetFormattedMessage() string {
	return fmt.Sprintf(
		"🔔 *Admin Approval Required*\n\n"+
			"💸 **Large Transaction Request**\n"+
			"📊 Amount: `%d sat` (%.3f₿)\n"+
			"👤 From: @%s\n"+
			"👤 To: @%s\n"+
			"💬 Memo: %s\n"+
			"🌐 Client IP: %s\n"+
			"⏰ Requested: %s\n"+
			"⏳ Expires: %s\n\n"+
			"Transaction ID: `%s`\n\n"+
			"Use `/admin approve %s` or `/admin reject %s`",
		pt.Amount,
		float64(pt.Amount)/100_000_000,
		pt.FromUsername,
		pt.ToUsername,
		str.MarkdownEscape(pt.Memo),
		pt.ClientIP,
		pt.RequestTimestamp.Format("2006-01-02 15:04:05"),
		pt.ExpiryTime.Format("2006-01-02 15:04:05"),
		pt.ID,
		pt.ID,
		pt.ID,
	)
}

// SaveToDB saves the pending transaction to the database
func (pt *PendingTransaction) SaveToDB(bot *telegram.TipBot) error {
	return pt.Set(pt, bot.Bunt)
}

// LoadFromDB loads a pending transaction from the database
func LoadPendingTransaction(id string, bot *telegram.TipBot) (*PendingTransaction, error) {
	pt := &PendingTransaction{Base: storage.New(storage.ID(id))}
	sn, err := pt.Get(pt, bot.Bunt)
	if err != nil {
		return nil, err
	}
	return sn.(*PendingTransaction), nil
}

// CreateApprovalKeyboard creates inline keyboard for transaction approval
func CreateApprovalKeyboard(transactionID string) *tb.ReplyMarkup {
	approvalMenu := &tb.ReplyMarkup{}
	btnApprove := approvalMenu.Data("✅ Approve", "approve_api_tx", transactionID)
	btnReject := approvalMenu.Data("❌ Reject", "reject_api_tx", transactionID)
	
	approvalMenu.Inline(
		approvalMenu.Row(btnApprove, btnReject),
	)
	return approvalMenu
}

// NotifyAdmins sends a notification to the admin group about pending approval
func (pt *PendingTransaction) NotifyAdmins(bot *telegram.TipBot) error {
	message := pt.GetFormattedMessage()
	
	log.Infof("[ADMIN APPROVAL] Large transaction pending approval: %s", pt.ID)
	
	// Create approval keyboard
	approvalKeyboard := CreateApprovalKeyboard(pt.ID)
	
	// Send to the configured error/log group
	if bot.ErrorLogger != nil {
		// Send approval message to the log group
		err := pt.sendApprovalMessage(bot, message, approvalKeyboard)
		if err != nil {
			log.Errorf("[ADMIN APPROVAL] Failed to send approval message: %v", err)
		} else {
			log.Infof("[ADMIN APPROVAL] Sent approval message for transaction %s", pt.ID)
		}
	} else {
		log.Warnf("[ADMIN APPROVAL] No error logger configured, cannot send approval notification")
	}
	
	return nil
}

// sendApprovalMessage sends an approval message to the configured admin group
func (pt *PendingTransaction) sendApprovalMessage(bot *telegram.TipBot, message string, keyboard *tb.ReplyMarkup) error {
	// Access the private fields via internal package access
	logGroupId := telegram.GetLogGroupId(bot.ErrorLogger)
	if logGroupId == 0 {
		return fmt.Errorf("no log group configured")
	}
	
	// Create recipient
	recipient := &tb.Chat{ID: logGroupId}
	
	// Format the message for approval
	approvalMessage := fmt.Sprintf("🚨 *API Transaction Approval Required*\n\n%s", message)
	
	// Send message with approval keyboard
	_, err := bot.Telegram.Send(recipient, approvalMessage, keyboard, tb.ModeMarkdown)
	return err
}

// CleanupExpiredTransactions removes expired pending transactions
func CleanupExpiredTransactions(bot *telegram.TipBot) error {
	// This would need to be implemented to iterate through all pending transactions
	// and mark expired ones as expired. For now, we'll just log the intent.
	log.Debug("[ADMIN APPROVAL] Cleanup expired transactions called")
	
	// Implementation would involve:
	// 1. Query all pending transactions from database
	// 2. Check which ones are expired
	// 3. Update their status to "expired"
	// 4. Optionally notify the original requester
	
	return nil
}
