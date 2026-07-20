package api

import (
	"fmt"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	log "github.com/sirupsen/logrus"
)

// PendingTransaction represents a transaction awaiting admin approval
type PendingTransaction struct {
	*storage.Base
	ID               string       `json:"id"`
	FromUser         *lnbits.User `json:"from_user" gorm:"-"`
	ToUser           *lnbits.User `json:"to_user" gorm:"-"`
	FromUsername     string       `json:"from_username"`
	ToUsername       string       `json:"to_username"`
	Amount           int64        `json:"amount"`
	Memo             string       `json:"memo"`
	PaymentType      string       `json:"payment_type,omitempty"` // "" / "internal" for user transfers, "invoice" for bolt11 payments
	Invoice          string       `json:"invoice,omitempty"`      // bolt11 payment request when PaymentType == "invoice"
	RequestTimestamp time.Time    `json:"request_timestamp"`
	Status           string       `json:"status"` // "pending", "approved", "rejected", "expired"
	ApprovedBy       string       `json:"approved_by,omitempty"`
	ApprovalTime     *time.Time   `json:"approval_time,omitempty"`
	ExpiryTime       time.Time    `json:"expiry_time"`
	ClientIP         string       `json:"client_ip"`
	OriginalRequest  *SendRequest `json:"original_request" gorm:"-"`
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
	fromUsername := ""
	if fromUser != nil {
		fromUsername = fromUser.Telegram.Username
	}

	id := fmt.Sprintf("pending-%s-%s-%d-%d", fromUsername, req.To, req.Amount, time.Now().Unix())

	return &PendingTransaction{
		Base:             storage.New(storage.ID(id)),
		ID:               id,
		FromUser:         fromUser,
		ToUser:           toUser,
		FromUsername:     fromUsername,
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

// NewPendingInvoiceTransaction creates a new pending transaction for an external bolt11 invoice payment
func NewPendingInvoiceTransaction(fromUsername, invoice, paymentHash string, amount int64, memo string, fromUser *lnbits.User, clientIP string) *PendingTransaction {
	id := fmt.Sprintf("pending-%s-invoice-%s-%d", fromUsername, paymentHash, time.Now().Unix())

	return &PendingTransaction{
		Base:             storage.New(storage.ID(id)),
		ID:               id,
		FromUser:         fromUser,
		FromUsername:     fromUsername,
		ToUsername:       invoice,
		Amount:           amount,
		Memo:             memo,
		PaymentType:      "invoice",
		Invoice:          invoice,
		RequestTimestamp: time.Now(),
		Status:           StatusPending,
		ExpiryTime:       time.Now().Add(PendingTransactionExpiry),
		ClientIP:         clientIP,
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

	pendingTx := sn.(*PendingTransaction)

	// Load user objects from usernames
	fromUser, err := telegram.GetUserByTelegramUsername(pendingTx.FromUsername, *bot)
	if err != nil {
		log.Warnf("[ADMIN APPROVAL] Could not load from user @%s: %v", pendingTx.FromUsername, err)
		// Continue with nil user - this will be handled in the calling functions
	} else {
		pendingTx.FromUser = fromUser
	}

	// Invoice payments have no internal recipient user, so skip the lookup.
	if pendingTx.PaymentType != "invoice" {
		toUser, err := telegram.GetUserByTelegramUsername(pendingTx.ToUsername, *bot)
		if err != nil {
			log.Warnf("[ADMIN APPROVAL] Could not load to user @%s: %v", pendingTx.ToUsername, err)
			// Continue with nil user - this will be handled in the calling functions
		} else {
			pendingTx.ToUser = toUser
		}
	}

	return pendingTx, nil
}

// RegisterUpdatePendingTxHook sets the storage-level hook so the telegram
// package can update PendingTransaction status without an import cycle.
func RegisterUpdatePendingTxHook(bot *telegram.TipBot) {
	storage.UpdatePendingTxStatusFn = func(id, status, actor string) {
		pt := &PendingTransaction{Base: storage.New(storage.ID(id))}
		sn, err := pt.Get(pt, bot.Bunt)
		if err != nil {
			log.Warnf("[pending tx hook] could not load %s: %v", id, err)
			return
		}
		ptx := sn.(*PendingTransaction)
		now := time.Now()
		ptx.Status = status
		ptx.ApprovedBy = actor
		ptx.ApprovalTime = &now
		if err := ptx.Set(ptx, bot.Bunt); err != nil {
			log.Errorf("[pending tx hook] could not save status for %s: %v", id, err)
		}
		log.Infof("[pending tx hook] updated %s → status=%s actor=%s", id, status, actor)
	}
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
