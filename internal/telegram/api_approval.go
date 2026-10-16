package telegram

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/i18n"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	decodepay "github.com/fiatjaf/ln-decodepay"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// These package-level buttons exist only so the callback handlers can be
// registered by their unique key. The markup sent to a user is always built
// per request (see apiApprovalMarkup) because approval requests originate from
// concurrent HTTP handlers, which are not serialised by the Telegram
// lockInterceptor.
var (
	apiApprovalConfirmationMenu = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnCancelAPITx              = apiApprovalConfirmationMenu.Data("🚫 Cancel", "cancel_api_tx")
	btnApproveAPITx             = apiApprovalConfirmationMenu.Data("✅ Approve & Send", "approve_api_tx")
)

// apiApprovalMarkup builds a fresh inline keyboard carrying the approval id as
// callback data. A new ReplyMarkup per call keeps concurrent approval requests
// from overwriting each other's callback data before Send serialises it.
func apiApprovalMarkup(approveText, id string) *tb.ReplyMarkup {
	menu := &tb.ReplyMarkup{ResizeKeyboard: true}
	menu.Inline(
		menu.Row(
			menu.Data(approveText, "approve_api_tx", id),
			menu.Data("🚫 Cancel", "cancel_api_tx", id)),
	)
	return menu
}

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
	// ExpiresAt is the PendingTransaction's expiry time. It is empty on
	// approvals created before it was added; see expiresAt.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// expiresAt returns when this approval stops being approvable. Approvals
// saved without ExpiresAt fall back to their creation time plus the standard
// window, so an old approve button cannot stay valid forever.
func (a *APIApprovalData) expiresAt() time.Time {
	if !a.ExpiresAt.IsZero() {
		return a.ExpiresAt
	}
	return a.CreatedAt.Add(storage.PendingTxExpiry)
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
	// Past its window /api/v1/send/status already reports the request as
	// expired, and the caller may have resubmitted it. Paying now would move
	// sats the caller was told were never sent.
	if time.Now().After(approvalData.expiresAt()) {
		log.Warnf("[approveAPITransactionHandler] approval %s for transaction %s expired at %s", approvalData.ID, approvalData.TransactionID, approvalData.expiresAt().Format(time.RFC3339))
		approvalData.Inactivate(approvalData, bot.Bunt)
		if storage.UpdatePendingTxStatusFn != nil {
			storage.UpdatePendingTxStatusFn(approvalData.TransactionID, storage.TxStatusExpired, ctx.Callback().Sender.Username)
		}
		bot.tryEditMessage(ctx.Callback().Message, "⌛ This approval request has expired. Nothing was sent.", &tb.ReplyMarkup{})
		return ctx, nil
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

	// Check sender's balance again and reserve it until the transfer is done.
	// This uses the *available* balance (wallet minus pots) less whatever the
	// sender's other in-flight payments hold, so neither pot funds nor a
	// concurrent payment's sats can be spent here.
	reservation, balance, err := bot.ReserveBalance(from, approvalData.Amount)
	if err != nil {
		log.Errorf("[approveAPITransactionHandler] Could not check sender balance: %v", err)
		bot.tryEditMessage(ctx.Callback().Message, "❌ Approval failed: could not check balance", &tb.ReplyMarkup{})
		return ctx, err
	}
	if reservation != nil {
		defer reservation.Release()
	}

	if reservation == nil {
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
		storage.UpdatePendingTxStatusFn(approvalData.TransactionID, storage.TxStatusExecuted, ctx.Callback().Sender.Username)
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
		storage.UpdatePendingTxStatusFn(approvalData.TransactionID, storage.TxStatusRejected, c.Sender.Username)
	}

	log.Infof("[cancelAPITransactionHandler] API transaction %s cancelled by @%s", approvalData.TransactionID, c.Sender.Username)
	return ctx, nil
}

// executeApprovedInvoicePayment pays an approved bolt11 invoice externally via lnbits.
func (bot *TipBot) executeApprovedInvoicePayment(ctx intercept.Context, approvalData *APIApprovalData, from *lnbits.User) {
	fromUserStr := GetUserStr(from.Telegram)

	// Re-check balance (with fee reserve) and reserve it until lnbits has the
	// payment. Uses the *available* balance less the sender's other in-flight
	// payments, so an approval cannot spend sats reserved in a pot or already
	// held by a concurrent payment.
	reservation, balance, err := bot.ReserveBalance(from, utils.AmountWithFeeReserve(approvalData.Amount))
	if err != nil {
		log.Errorf("[approveAPITransactionHandler] Could not check sender balance: %v", err)
		bot.tryEditMessage(ctx.Callback().Message, "❌ Approval failed: could not check balance", &tb.ReplyMarkup{})
		return
	}
	if reservation == nil {
		log.Warnf("[approveAPITransactionHandler] Insufficient balance for invoice: %d < %d (+fees)", balance, approvalData.Amount)
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("❌ Insufficient balance: %s available, %s required (plus fee reserve)", utils.FormatSats(balance), utils.FormatSats(approvalData.Amount)), &tb.ReplyMarkup{})
		return
	}
	defer reservation.Release()

	inv, err := from.Wallet.Pay(lnbits.PaymentParams{Out: true, Bolt11: approvalData.Invoice}, bot.Client)
	// lnbits now accounts for the payment in its own balance (or rejected
	// it), so free the reservation before the long settlement wait.
	reservation.Release()
	if err != nil {
		log.Errorf("[approveAPITransactionHandler] Invoice payment failed for %s: %v", fromUserStr, err)
		if bot.ErrorLogger != nil {
			bot.ErrorLogger.LogPaymentError(err, approvalData.Amount, approvalData.Memo, approvalData.Invoice, from.Telegram)
		}
		bot.tryEditMessage(ctx.Callback().Message, "❌ Invoice payment failed", &tb.ReplyMarkup{})
		return
	}

	// The approval is spent either way: lnbits has accepted the payment, so the
	// button must not stay pressable while we wait for it to settle.
	approvalData.Inactivate(approvalData, bot.Bunt)

	// lnbits does not always echo the payment hash back. Fall back to the one in
	// the invoice itself so the settlement check still has something to follow.
	paymentHash := inv.PaymentHash
	if paymentHash == "" {
		if decoded, derr := decodepay.Decodepay(approvalData.Invoice); derr == nil {
			paymentHash = decoded.PaymentHash
		}
	}

	// lnbits answers as soon as it has handed the payment to its backend, which
	// is not the same as the sats reaching the destination node. Follow the
	// payment to a terminal state before reporting it as paid.
	settlement := bot.Client.WaitForOutgoingPayment(*from.Wallet, paymentHash, internal.APISendSettlementTimeout())

	switch settlement.State {
	case lnbits.PaymentStateFailed:
		log.Errorf("[💸 api_send_approved invoice] %s: payment %s did not settle: %v", fromUserStr, paymentHash, settlement.Err)
		if bot.ErrorLogger != nil {
			bot.ErrorLogger.LogPaymentError(fmt.Errorf("payment did not settle: %s", settlement.State), approvalData.Amount, approvalData.Memo, approvalData.Invoice, from.Telegram)
		}
		if storage.UpdatePendingTxStatusFn != nil {
			storage.UpdatePendingTxStatusFn(approvalData.TransactionID, storage.TxStatusFailed, ctx.Callback().Sender.Username)
		}
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("❌ Invoice payment failed — the payment could not be routed and your %s was not sent.", thirdparty.FormatSatsWithLKR(approvalData.Amount)), &tb.ReplyMarkup{})
		return

	case lnbits.PaymentStatePending, lnbits.PaymentStateUnknown:
		// The sats may still leave the node, so this is neither a success nor a
		// failure. Move the transaction to in_flight rather than leaving it
		// pending: a pending transaction is reported as expired once its 24h
		// window passes, which would tell the caller a payment that may well
		// have settled was never sent.
		log.Warnf("[💸 api_send_approved invoice] %s: payment %s unresolved (state %s): %v", fromUserStr, paymentHash, settlement.State, settlement.Err)
		if storage.UpdatePendingTxStatusFn != nil {
			storage.UpdatePendingTxStatusFn(approvalData.TransactionID, storage.TxStatusInFlight, ctx.Callback().Sender.Username)
		}
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("⏳ Invoice payment of %s is still in flight.\n\nIt has not settled yet — check your balance shortly for the final result. Do not retry this invoice.", thirdparty.FormatSatsWithLKR(approvalData.Amount)), &tb.ReplyMarkup{})
		return
	}

	// Update the PendingTransaction status so /api/v1/send/status reflects the real outcome.
	if storage.UpdatePendingTxStatusFn != nil {
		storage.UpdatePendingTxStatusFn(approvalData.TransactionID, storage.TxStatusExecuted, ctx.Callback().Sender.Username)
	}

	log.Infof("[💸 api_send_approved invoice] %s paid invoice %s (%s, fee %d sat).", fromUserStr, paymentHash, thirdparty.FormatSatsWithLKR(approvalData.Amount), settlement.Fee)

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
func CreateAPIInvoiceApprovalRequest(bot *TipBot, fromUser *lnbits.User, invoice string, amount int64, memo string, transactionID string, clientIP string, expiresAt time.Time) error {
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
		ExpiresAt:     expiresAt,
	}

	if err := approvalData.Set(approvalData, bot.Bunt); err != nil {
		log.Errorf("[CreateAPIInvoiceApprovalRequest] Failed to save approval data: %v", err)
		return err
	}

	if _, err := bot.Telegram.Send(fromUser.Telegram, confirmText, apiApprovalMarkup("✅ Approve & Pay", id), tb.ModeMarkdown); err != nil {
		log.Errorf("[CreateAPIInvoiceApprovalRequest] Failed to send approval request: %v", err)
		// The caller abandons this transaction, so a message that reached the
		// user despite the error must not be approvable.
		approvalData.Inactivate(approvalData, bot.Bunt)
		return err
	}

	log.Infof("[CreateAPIInvoiceApprovalRequest] Sent invoice approval request to @%s for transaction %s", fromUser.Telegram.Username, transactionID)
	return nil
}

// CreateAPIApprovalRequest creates an approval request for API transaction (similar to send confirmation)
func CreateAPIApprovalRequest(bot *TipBot, fromUser *lnbits.User, toUsername string, amount int64, memo string, transactionID string, clientIP string, expiresAt time.Time) error {
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
		ExpiresAt:     expiresAt,
	}

	// Save approval data to database
	err := approvalData.Set(approvalData, bot.Bunt)
	if err != nil {
		log.Errorf("[CreateAPIApprovalRequest] Failed to save approval data: %v", err)
		return err
	}

	// Send approval request to the sender (from user)
	_, err = bot.Telegram.Send(fromUser.Telegram, confirmText, apiApprovalMarkup("✅ Approve & Send", id), tb.ModeMarkdown)
	if err != nil {
		log.Errorf("[CreateAPIApprovalRequest] Failed to send approval request: %v", err)
		// The caller abandons this transaction, so a message that reached the
		// user despite the error must not be approvable.
		approvalData.Inactivate(approvalData, bot.Bunt)
		return err
	}

	log.Infof("[CreateAPIApprovalRequest] Sent approval request to @%s for transaction %s", fromUser.Telegram.Username, transactionID)
	return nil
}
