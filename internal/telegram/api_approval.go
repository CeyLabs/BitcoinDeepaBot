package telegram

import (
	"fmt"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/i18n"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

var (
	apiApprovalConfirmationMenu = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnCancelAPITx              = apiApprovalConfirmationMenu.Data("🚫 Cancel", "cancel_api_tx")
	btnApproveAPITx             = apiApprovalConfirmationMenu.Data("✅ Approve & Send", "approve_api_tx")
)

// APIApprovalData holds data for API transaction approval (similar to SendData)
type APIApprovalData struct {
	*storage.Base
	TransactionID   string       `json:"transaction_id"`
	FromUser        *lnbits.User `json:"from_user"`
	ToUsername      string       `json:"to_username"`
	Amount          int64        `json:"amount"`
	Memo            string       `json:"memo"`
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
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("❌ Insufficient balance: %d sat available, %d sat required", utils.CommaInt(balance), utils.CommaInt(approvalData.Amount)), &tb.ReplyMarkup{})
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

	log.Infof("[💸 api_send_approved] Send from %s to %s (%d sat).", fromUserStr, toUserStr, approvalData.Amount)

	// Notify recipient (same format as API send)
	fromUserStrMd := GetUserStrMd(from.Telegram)
	notificationMsg := fmt.Sprintf("💰 You received %d sat from %s via Automated API", utils.CommaInt(approvalData.Amount), fromUserStrMd)
	if approvalData.Memo != "" {
		notificationMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(approvalData.Memo))
	}
	bot.trySendMessage(toUser.Telegram, notificationMsg)

	// Update approval message to show success
	if ctx.Callback().Message.Private() {
		bot.tryDeleteMessage(ctx.Callback().Message)
		successMsg := fmt.Sprintf("✅ Payment approved and sent successfully!\n\n💸 Amount: %d sat\n👤 To: @%s", utils.CommaInt(approvalData.Amount), approvalData.ToUsername)
		if approvalData.Memo != "" {
			successMsg += fmt.Sprintf("\n✉️ Memo: %s", str.MarkdownEscape(approvalData.Memo))
		}
		bot.trySendMessage(ctx.Callback().Sender, successMsg)
	} else {
		toUserStrMd := GetUserStrMd(toUser.Telegram)
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("✅ API payment approved and sent!\n\n💸 %d sat → %s", utils.CommaInt(approvalData.Amount), toUserStrMd), &tb.ReplyMarkup{})
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

	log.Infof("[cancelAPITransactionHandler] API transaction %s cancelled by @%s", approvalData.TransactionID, c.Sender.Username)
	return ctx, nil
}

// CreateAPIApprovalRequest creates an approval request for API transaction (similar to send confirmation)
func CreateAPIApprovalRequest(bot *TipBot, fromUser *lnbits.User, toUsername string, amount int64, memo string, transactionID string, clientIP string) error {
	// Create confirmation text (same format as /send command)
	toUserStrMention := fmt.Sprintf("@%s", toUsername)
	confirmText := fmt.Sprintf("Do you want to pay to %s?\n\n💸 Amount: %d sat", toUserStrMention, utils.CommaInt(amount))
	if memo != "" {
		confirmText += fmt.Sprintf("\n✉️ %s", str.MarkdownEscape(memo))
	}

	// Add approval context
	confirmText += "\n\n🔔 *Admin Approval Required*\n"
	confirmText += fmt.Sprintf("This transaction requires approval because the amount (%d sat) exceeds the threshold.", utils.CommaInt(amount))

	// Create unique ID for this approval request (same pattern as send command)
	id := fmt.Sprintf("api-%d-%d-%s", fromUser.Telegram.ID, amount, RandStringRunes(5))

	// Create approval data object (similar to SendData)
	approvalData := &APIApprovalData{
		Base:          storage.New(storage.ID(id)),
		TransactionID: transactionID,
		FromUser:      fromUser,
		ToUsername:    toUsername,
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
