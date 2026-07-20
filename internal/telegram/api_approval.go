package telegram

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/i18n"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

var (
	apiApprovalConfirmationMenu = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnCancelAPITx              = apiApprovalConfirmationMenu.Data("🚫 Cancel", "cancel_api_tx")
	btnApproveAPITx             = apiApprovalConfirmationMenu.Data("✅ Approve & Send", "approve_api_tx")
)

// isTelegramID checks if the identifier is a Telegram ID (numeric string)
func isTelegramID(identifier string) bool {
	// Check if it's all digits and has reasonable length for Telegram ID
	match, _ := regexp.MatchString(`^[0-9]{5,15}$`, identifier)
	return match
}

// APIApprovalData holds data for API transaction approval (similar to SendData)
type APIApprovalData struct {
	*storage.Base
	TransactionID   string       `json:"transaction_id"`
	FromUser        *lnbits.User `json:"from_user"`
	ToUsername      string       `json:"to_username"`
	Amount          int64        `json:"amount"`
	Memo            string       `json:"memo"`
	PaymentType     string       `json:"payment_type,omitempty"` // "invoice" for bolt11 payments; empty for internal transfers
	Invoice         string       `json:"invoice,omitempty"`      // bolt11 payment request when PaymentType == "invoice"
	Message         string       `json:"message"`
	LanguageCode    string       `json:"language_code"`
	ClientIP        string       `json:"client_ip"`
	OriginalRequest interface{}  `json:"original_request" gorm:"-"`
}

// approveAPITransactionHandler handles the approval of API transactions
func (bot *TipBot) approveAPITransactionHandler(ctx intercept.Context) (intercept.Context, error) {
	tx := &APIApprovalData{Base: storage.New(storage.ID(ctx.Data()))}
	mutex.LockWithContext(ctx, tx.ID)
	defer mutex.UnlockWithContext(ctx, tx.ID)

	sn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		log.Errorf("[approveAPITransactionHandler] %s", err.Error())
		return ctx, err
	}
	approvalData := sn.(*APIApprovalData)

	// Only the correct user can press
	if approvalData.FromUser.Telegram.ID != ctx.Callback().Sender.ID {
		return ctx, errors.Create(errors.UnknownError)
	}
	if !approvalData.Active {
		log.Errorf("[approveAPITransactionHandler] approval not active anymore")
		return ctx, errors.Create(errors.NotActiveError)
	}
	defer approvalData.Set(approvalData, bot.Bunt)

	from := LoadUser(ctx)
	ResetUserState(from, bot)

	// Invoice payment path: pay the bolt11 externally instead of an internal transfer
	if approvalData.PaymentType == "invoice" {
		bot.executeApprovedInvoicePayment(ctx, approvalData, from)
		return ctx, nil
	}

	// Get recipient user
	toUser, err := GetUserByTelegramUsername(approvalData.ToUsername, *bot)
	if err != nil {
		log.Errorf("[approveAPITransactionHandler] Could not find recipient user %s: %v", approvalData.ToUsername, err)
		bot.tryEditMessage(ctx.Callback().Message, "❌ Approval failed: recipient not found", &tb.ReplyMarkup{})
		return ctx, err
	}

	// Check sender's balance again
	balance, err := bot.GetUserBalance(from)
	if err != nil {
		log.Errorf("[approveAPITransactionHandler] Could not check sender balance: %v", err)
		bot.tryEditMessage(ctx.Callback().Message, "❌ Approval failed: could not check balance", &tb.ReplyMarkup{})
		return ctx, err
	}

	if balance < approvalData.Amount {
		log.Warnf("[approveAPITransactionHandler] Insufficient balance: %d < %d", balance, approvalData.Amount)
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("❌ Insufficient balance: %s available, %s required", utils.FormatSats(balance), utils.FormatSats(approvalData.Amount)), &tb.ReplyMarkup{})
		return ctx, errors.Create(errors.UnknownError)
	}

	// Create transaction memo
	fromUserStr := GetUserStr(from.Telegram)
	toUserStr := GetUserStr(toUser.Telegram)
	transactionMemo := fmt.Sprintf("💸 API Send from %s to %s (Approved).", fromUserStr, toUserStr)
	if approvalData.Memo != "" {
		transactionMemo += fmt.Sprintf(" Memo: %s", approvalData.Memo)
	}

	// Create and execute transaction
	t := NewTransaction(bot, from, toUser, approvalData.Amount, TransactionType("api_send_approved"))
	t.Memo = transactionMemo

	success, err := t.Send()
	if !success || err != nil {
		log.Errorf("[approveAPITransactionHandler] Transaction failed from %s to %s: %v", fromUserStr, toUserStr, err)
		if bot.ErrorLogger != nil {
			bot.ErrorLogger.LogTransactionError(err, "api_send_approved", approvalData.Amount, from.Telegram, toUser.Telegram)
		}
		bot.tryEditMessage(ctx.Callback().Message, "❌ Payment execution failed", &tb.ReplyMarkup{})
		return ctx, errors.Create(errors.UnknownError)
	}

	approvalData.Inactivate(approvalData, bot.Bunt)

	// Update the PendingTransaction status so /api/v1/send/status reflects the real outcome.
	if storage.UpdatePendingTxStatusFn != nil {
		storage.UpdatePendingTxStatusFn(approvalData.TransactionID, "executed", ctx.Callback().Sender.Username)
	}

	log.Infof("[💸 api_send_approved] Send from %s to %s (%s).", fromUserStr, toUserStr, thirdparty.FormatSatsWithLKR(approvalData.Amount))

	// Notify recipient (same format as API send)
	fromUserStrMd := GetUserStrMd(from.Telegram)
	notificationMsg := fmt.Sprintf("💰 You received %s from %s via Automated API", thirdparty.FormatSatsWithLKR(approvalData.Amount), fromUserStrMd)
	if approvalData.Memo != "" {
		notificationMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(approvalData.Memo))
	}
	bot.trySendMessage(toUser.Telegram, notificationMsg)

	// Update approval message to show success
	if ctx.Callback().Message.Private() {
		bot.tryDeleteMessage(ctx.Callback().Message)
		successMsg := fmt.Sprintf("✅ Payment approved and sent successfully!\n\n💸 Amount: %s\n👤 To: @%s", thirdparty.FormatSatsWithLKR(approvalData.Amount), approvalData.ToUsername)
		if approvalData.Memo != "" {
			successMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(approvalData.Memo))
		}
		bot.trySendMessage(ctx.Callback().Sender, successMsg)
	} else {
		toUserStrMd := GetUserStrMd(toUser.Telegram)
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("✅ API payment approved and sent!\n\n💸 %s → %s", thirdparty.FormatSatsWithLKR(approvalData.Amount), toUserStrMd), &tb.ReplyMarkup{})
	}

	return ctx, nil
}

// cancelAPITransactionHandler handles the cancellation of API transactions
func (bot *TipBot) cancelAPITransactionHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	user := LoadUser(ctx)
	ResetUserState(user, bot)

	tx := &APIApprovalData{Base: storage.New(storage.ID(c.Data))}
	mutex.LockWithContext(ctx, tx.ID)
	defer mutex.UnlockWithContext(ctx, tx.ID)

	sn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		log.Errorf("[cancelAPITransactionHandler] %s", err.Error())
		return ctx, err
	}

	approvalData := sn.(*APIApprovalData)
	// Only the correct user can press
	if approvalData.FromUser.Telegram.ID != c.Sender.ID {
		return ctx, errors.Create(errors.UnknownError)
	}

	// Delete and send cancellation message (same as cancel send)
	bot.tryDeleteMessage(c)
	bot.trySendMessage(c.Message.Chat, i18n.Translate(approvalData.LanguageCode, "sendCancelledMessage"))
	approvalData.Inactivate(approvalData, bot.Bunt)

	// Update the PendingTransaction status so /api/v1/send/status reflects the real outcome.
	if storage.UpdatePendingTxStatusFn != nil {
		storage.UpdatePendingTxStatusFn(approvalData.TransactionID, "rejected", c.Sender.Username)
	}

	log.Infof("[cancelAPITransactionHandler] API transaction %s cancelled by @%s", approvalData.TransactionID, c.Sender.Username)
	return ctx, nil
}

// executeApprovedInvoicePayment pays an approved bolt11 invoice externally via lnbits.
func (bot *TipBot) executeApprovedInvoicePayment(ctx intercept.Context, approvalData *APIApprovalData, from *lnbits.User) {
	fromUserStr := GetUserStr(from.Telegram)

	// Re-check balance (with fee reserve) before paying
	balance, err := bot.GetUserBalance(from)
	if err != nil {
		log.Errorf("[approveAPITransactionHandler] Could not check sender balance: %v", err)
		bot.tryEditMessage(ctx.Callback().Message, "❌ Approval failed: could not check balance", &tb.ReplyMarkup{})
		return
	}
	if balance < approvalData.Amount || float64(approvalData.Amount) > float64(balance)*0.98 {
		log.Warnf("[approveAPITransactionHandler] Insufficient balance for invoice: %d < %d (+fees)", balance, approvalData.Amount)
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("❌ Insufficient balance: %s available, %s required (plus fee reserve)", utils.FormatSats(balance), utils.FormatSats(approvalData.Amount)), &tb.ReplyMarkup{})
		return
	}

	inv, err := from.Wallet.Pay(lnbits.PaymentParams{Out: true, Bolt11: approvalData.Invoice}, bot.Client)
	if err != nil {
		log.Errorf("[approveAPITransactionHandler] Invoice payment failed for %s: %v", fromUserStr, err)
		if bot.ErrorLogger != nil {
			bot.ErrorLogger.LogPaymentError(err, approvalData.Amount, approvalData.Memo, approvalData.Invoice, from.Telegram)
		}
		bot.tryEditMessage(ctx.Callback().Message, "❌ Invoice payment failed", &tb.ReplyMarkup{})
		return
	}

	approvalData.Inactivate(approvalData, bot.Bunt)

	// Update the PendingTransaction status so /api/v1/send/status reflects the real outcome.
	if storage.UpdatePendingTxStatusFn != nil {
		storage.UpdatePendingTxStatusFn(approvalData.TransactionID, "executed", ctx.Callback().Sender.Username)
	}

	log.Infof("[💸 api_send_approved invoice] %s paid invoice %s (%s).", fromUserStr, inv.PaymentHash, thirdparty.FormatSatsWithLKR(approvalData.Amount))

	successMsg := fmt.Sprintf("✅ Invoice approved and paid successfully!\n\n💸 Amount: %s", thirdparty.FormatSatsWithLKR(approvalData.Amount))
	if approvalData.Memo != "" {
		successMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(approvalData.Memo))
	}
	if ctx.Callback().Message.Private() {
		bot.tryDeleteMessage(ctx.Callback().Message)
		bot.trySendMessage(ctx.Callback().Sender, successMsg)
	} else {
		bot.tryEditMessage(ctx.Callback().Message, successMsg, &tb.ReplyMarkup{})
	}
}

// CreateAPIInvoiceApprovalRequest creates an approval request for an external bolt11 invoice payment.
// It reuses the same approve/cancel callback handlers as internal API transfers.
func CreateAPIInvoiceApprovalRequest(bot *TipBot, fromUser *lnbits.User, invoice string, amount int64, memo string, transactionID string, clientIP string) error {
	confirmText := fmt.Sprintf("Do you want to pay this Lightning invoice?\n\n💸 Amount: %s", thirdparty.FormatSatsWithLKR(amount))
	if memo != "" {
		confirmText += fmt.Sprintf("\n✉️ %s", str.MarkdownEscape(memo))
	}
	confirmText += "\n\n🔔 *Admin Approval Required*\n"
	confirmText += fmt.Sprintf("This transaction requires approval because the amount (%s) exceeds the threshold.", thirdparty.FormatSatsWithLKR(amount))

	id := fmt.Sprintf("api-%d-%d-%s", fromUser.Telegram.ID, amount, RandStringRunes(5))

	approvalData := &APIApprovalData{
		Base:          storage.New(storage.ID(id)),
		TransactionID: transactionID,
		FromUser:      fromUser,
		ToUsername:    "invoice",
		Amount:        amount,
		Memo:          memo,
		PaymentType:   "invoice",
		Invoice:       invoice,
		Message:       confirmText,
		LanguageCode:  fromUser.Telegram.LanguageCode,
		ClientIP:      clientIP,
	}

	if err := approvalData.Set(approvalData, bot.Bunt); err != nil {
		log.Errorf("[CreateAPIInvoiceApprovalRequest] Failed to save approval data: %v", err)
		return err
	}

	approveButton := apiApprovalConfirmationMenu.Data("✅ Approve & Pay", "approve_api_tx")
	cancelButton := apiApprovalConfirmationMenu.Data("🚫 Cancel", "cancel_api_tx")
	approveButton.Data = id
	cancelButton.Data = id

	apiApprovalConfirmationMenu.Inline(
		apiApprovalConfirmationMenu.Row(
			approveButton,
			cancelButton),
	)

	if _, err := bot.Telegram.Send(fromUser.Telegram, confirmText, apiApprovalConfirmationMenu, tb.ModeMarkdown); err != nil {
		log.Errorf("[CreateAPIInvoiceApprovalRequest] Failed to send approval request: %v", err)
		return err
	}

	log.Infof("[CreateAPIInvoiceApprovalRequest] Sent invoice approval request to @%s for transaction %s", fromUser.Telegram.Username, transactionID)
	return nil
}

// CreateAPIApprovalRequest creates an approval request for API transaction (similar to send confirmation)
func CreateAPIApprovalRequest(bot *TipBot, fromUser *lnbits.User, toUsername string, amount int64, memo string, transactionID string, clientIP string) error {
	// Check if toUsername is actually a user ID and get the actual username
	actualUsername := toUsername
	if isTelegramID(toUsername) {
		// It's a Telegram ID, get the user and use their username
		telegramID, err := strconv.ParseInt(toUsername, 10, 64)
		if err == nil {
			toUser, err := GetUserByTelegramID(telegramID, *bot)
			if err == nil && toUser.Telegram.Username != "" {
				actualUsername = toUser.Telegram.Username
			}
		}
	}
	
	// Create confirmation text (same format as /send command)
	toUserStrMention := fmt.Sprintf("@%s", actualUsername)
	confirmText := fmt.Sprintf("Do you want to pay to %s?\n\n💸 Amount: %s", toUserStrMention, thirdparty.FormatSatsWithLKR(amount))
	if memo != "" {
		confirmText += fmt.Sprintf("\n✉️ %s", str.MarkdownEscape(memo))
	}

	// Add approval context
	confirmText += "\n\n🔔 *Admin Approval Required*\n"
	confirmText += fmt.Sprintf("This transaction requires approval because the amount (%s) exceeds the threshold.", thirdparty.FormatSatsWithLKR(amount))

	// Create unique ID for this approval request (same pattern as send command)
	id := fmt.Sprintf("api-%d-%d-%s", fromUser.Telegram.ID, amount, RandStringRunes(5))

	// Create approval data object (similar to SendData)
	approvalData := &APIApprovalData{
		Base:          storage.New(storage.ID(id)),
		TransactionID: transactionID,
		FromUser:      fromUser,
		ToUsername:    actualUsername, // Use the actual username instead of the original toUsername
		Amount:        amount,
		Memo:          memo,
		Message:       confirmText,
		LanguageCode:  fromUser.Telegram.LanguageCode,
		ClientIP:      clientIP,
	}

	// Save approval data to database
	err := approvalData.Set(approvalData, bot.Bunt)
	if err != nil {
		log.Errorf("[CreateAPIApprovalRequest] Failed to save approval data: %v", err)
		return err
	}

	// Create buttons (same pattern as send confirmation)
	approveButton := apiApprovalConfirmationMenu.Data("✅ Approve & Send", "approve_api_tx")
	cancelButton := apiApprovalConfirmationMenu.Data("🚫 Cancel", "cancel_api_tx")
	approveButton.Data = id
	cancelButton.Data = id

	apiApprovalConfirmationMenu.Inline(
		apiApprovalConfirmationMenu.Row(
			approveButton,
			cancelButton),
	)

	// Send approval request to the sender (from user)
	_, err = bot.Telegram.Send(fromUser.Telegram, confirmText, apiApprovalConfirmationMenu, tb.ModeMarkdown)
	if err != nil {
		log.Errorf("[CreateAPIApprovalRequest] Failed to send approval request: %v", err)
		return err
	}

	log.Infof("[CreateAPIApprovalRequest] Sent approval request to @%s for transaction %s", fromUser.Telegram.Username, transactionID)
	return nil
}
