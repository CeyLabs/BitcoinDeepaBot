package telegram

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"

	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

var (
	sendBatchConfirmationMenu = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnCancelSendBatch        = sendBatchConfirmationMenu.Data("🚫 Cancel", "cancel_send_batch")
	btnConfirmSendBatch       = sendBatchConfirmationMenu.Data("✅ Send", "confirm_send_batch")
)

type BatchEntry struct {
	Amount         int64  `json:"amount"`
	ToUsername     string `json:"to_username"`
	ToTelegramId   int64  `json:"to_telegram_id"`
	Memo           string `json:"memo"`
}

type SendBatchData struct {
	*storage.Base
	From         *lnbits.User `json:"from"`
	Entries      []BatchEntry `json:"entries"`
	TotalAmount  int64        `json:"total_amount"`
	SharedMemo   string       `json:"shared_memo"`
	LanguageCode string       `json:"languagecode"`
}

// parseBatchEntries parses multi-line batch send message.
// Format 1: /sendbatch\n<amount> @user [memo]\n...
// Format 2: /sendbatch <shared memo>\n<amount> @user\n...
func parseBatchEntries(text string) (entries []parsedEntry, sharedMemo string, err error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) < 2 {
		return nil, "", fmt.Errorf("need at least one recipient line after /sendbatch")
	}

	// Parse first line for shared memo
	firstLine := strings.TrimSpace(lines[0])
	parts := strings.SplitN(firstLine, " ", 2)
	if len(parts) > 1 {
		sharedMemo = strings.TrimSpace(parts[1])
	}

	// Parse recipient lines
	for i, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		entry, err := parseRecipientLine(line, sharedMemo)
		if err != nil {
			return nil, "", fmt.Errorf("line %d: %s", i+2, err.Error())
		}
		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		return nil, "", fmt.Errorf("no valid recipient lines found")
	}

	return entries, sharedMemo, nil
}

type parsedEntry struct {
	Amount        int64
	Username      string // without @
	Memo          string
	DisplayAmount string
}

func parseRecipientLine(line string, sharedMemo string) (parsedEntry, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return parsedEntry{}, fmt.Errorf("expected: <amount> @<username> [memo]")
	}

	var amount int64
	var displayAmount string
	amountStr := strings.ReplaceAll(fields[0], ",", "")
	// Check for lkr suffix
	if strings.HasSuffix(strings.ToLower(amountStr), "lkr") {
		lkrPerSat, _, err := thirdparty.GetSatPrice()
		if err != nil {
			return parsedEntry{}, fmt.Errorf("failed to get LKR price: %v", err)
		}
		if lkrPerSat <= 0 {
			return parsedEntry{}, fmt.Errorf("invalid LKR price")
		}

		valStr := strings.TrimSuffix(strings.ToLower(amountStr), "lkr")
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil || val <= 0 {
			return parsedEntry{}, fmt.Errorf("invalid amount: %s", fields[0])
		}
		amount = int64(val / lkrPerSat)
		displayAmount = fmt.Sprintf("`%s` (%d sats)", fields[0], amount)
	} else {
		// Parse amount as sats
		parsedAmount, err := strconv.ParseInt(amountStr, 10, 64)
		if err != nil || parsedAmount < 1 {
			return parsedEntry{}, fmt.Errorf("invalid amount: %s", fields[0])
		}
		amount = parsedAmount
		displayAmount = fmt.Sprintf("`%d` sats", amount)
	}

	// Parse username
	username := strings.TrimPrefix(fields[1], "@")
	if username == "" {
		return parsedEntry{}, fmt.Errorf("invalid username: %s", fields[1])
	}

	// Parse per-line memo (everything after amount and username)
	memo := ""
	if len(fields) > 2 {
		memo = strings.Join(fields[2:], " ")
	}

	// Fall back to shared memo if no per-line memo
	if memo == "" {
		memo = sharedMemo
	}

	return parsedEntry{
		Amount:        amount,
		Username:      username,
		Memo:          memo,
		DisplayAmount: displayAmount,
	}, nil
}

// sendbatchHandler invoked on "/sendbatch" command
func (bot *TipBot) sendbatchHandler(ctx intercept.Context) (intercept.Context, error) {
	bot.anyTextHandler(ctx)
	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	ResetUserState(user, bot)

	// Parse the batch entries
	parsed, sharedMemo, err := parseBatchEntries(ctx.Message().Text)
	if err != nil {
		helpMsg := fmt.Sprintf("❌ *Batch Send Error*\n\n%s\n\n", str.MarkdownEscape(err.Error()))
		helpMsg += "*Usage:*\n```\n/sendbatch [shared memo]\n<amount> @user [memo]\n<amount> @user [memo]\n```\n\n"
		helpMsg += "*Example 1 (Shared Memo):*\n```\n/sendbatch Salary 2026\n1000 @alice\n500 @bob\n```\n\n"
		helpMsg += "*Example 2 (Individual Memos & LKR):*\n```\n/sendbatch\n1000 @alice Pizza\n500lkr @bob Coffee\n```"

		bot.trySendMessage(ctx.Message().Sender, helpMsg)
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	// Resolve all recipients and build entries
	var entries []BatchEntry
	var totalAmount int64
	var confirmLines []string

	for i, p := range parsed {
		// Look up recipient
		toUser, err := GetUserByTelegramUsername(p.Username, *bot)
		if err != nil {
			bot.trySendMessage(ctx.Message().Sender, fmt.Sprintf("❌ *Batch Send Error*\n\nLine %d: User @%s not found or has no wallet.", i+1, str.MarkdownEscape(p.Username)))
			return ctx, errors.Create(errors.InvalidSyntaxError)
		}

		// Prevent self-send
		if user.Telegram.ID == toUser.Telegram.ID {
			bot.trySendMessage(ctx.Message().Sender, fmt.Sprintf("❌ *Batch Send Error*\n\nLine %d: Cannot send to yourself.", i+1))
			return ctx, errors.Create(errors.SelfPaymentError)
		}

		entries = append(entries, BatchEntry{
			Amount:       p.Amount,
			ToUsername:   p.Username,
			ToTelegramId: toUser.Telegram.ID,
			Memo:         p.Memo,
		})
		totalAmount += p.Amount

		// Build confirmation line
		line := fmt.Sprintf("%s → @%s", p.DisplayAmount, str.MarkdownEscape(p.Username))
		if p.Memo != "" {
			line += fmt.Sprintf(" _%s_", str.MarkdownEscape(p.Memo))
		}
		confirmLines = append(confirmLines, line)
	}

	// Check sender has enough balance for total
	balance, err := bot.GetUserAvailableBalance(user)
	if err != nil {
		log.Errorln(err.Error())
		bot.trySendMessage(ctx.Message().Sender, "❌ Could not check your balance. Please try again.")
		return ctx, err
	}
	if balance < totalAmount {
		bot.trySendMessage(ctx.Message().Sender, fmt.Sprintf("❌ *Insufficient balance*\n\nRequired: %s\nAvailable: %s",
			thirdparty.FormatSatsWithLKR(totalAmount),
			thirdparty.FormatSatsWithLKR(balance)))
		return ctx, fmt.Errorf("insufficient balance for batch send")
	}

	// Build confirmation message
	confirmText := fmt.Sprintf("📦 *Batch Send Confirmation*\n\n%s\n\n━━━━━━━━━━━━━━━━━━\n*Total:* %s\n*Recipients:* %d",
		strings.Join(confirmLines, "\n"),
		thirdparty.FormatSatsWithLKR(totalAmount),
		len(entries))

	// Persist batch data
	id := fmt.Sprintf("sendbatch-%d-%d-%s", ctx.Message().Sender.ID, totalAmount, RandStringRunes(5))
	batchData := &SendBatchData{
		Base:         storage.New(storage.ID(id)),
		From:         user,
		Entries:      entries,
		TotalAmount:  totalAmount,
		SharedMemo:   sharedMemo,
		LanguageCode: ctx.Value("publicLanguageCode").(string),
	}
	runtime.IgnoreError(batchData.Set(batchData, bot.Bunt))

	// Set up confirmation buttons
	confirmBtn := sendBatchConfirmationMenu.Data("✅ Confirm Send", "confirm_send_batch")
	cancelBtn := sendBatchConfirmationMenu.Data("🚫 Cancel", "cancel_send_batch")
	confirmBtn.Data = id
	cancelBtn.Data = id

	sendBatchConfirmationMenu.Inline(
		sendBatchConfirmationMenu.Row(confirmBtn, cancelBtn),
	)

	bot.trySendMessage(ctx.Chat(), confirmText, sendBatchConfirmationMenu)
	return ctx, nil
}

// confirmSendBatchHandler executes the batch send after user confirms
func (bot *TipBot) confirmSendBatchHandler(ctx intercept.Context) (intercept.Context, error) {
	tx := &SendBatchData{Base: storage.New(storage.ID(ctx.Data()))}
	mutex.LockWithContext(ctx, tx.ID)
	defer mutex.UnlockWithContext(ctx, tx.ID)

	sn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		log.Errorf("[confirmSendBatchHandler] %s", err.Error())
		return ctx, err
	}
	batchData := sn.(*SendBatchData)

	// Only the sender can confirm
	if batchData.From.Telegram.ID != ctx.Callback().Sender.ID {
		return ctx, errors.Create(errors.UnknownError)
	}
	if !batchData.Active {
		log.Errorf("[confirmSendBatchHandler] batch not active anymore")
		return ctx, errors.Create(errors.NotActiveError)
	}
	defer batchData.Set(batchData, bot.Bunt)

	from := LoadUser(ctx)
	ResetUserState(from, bot)

	// Re-check balance before executing
	balance, err := bot.GetUserAvailableBalance(from)
	if err != nil {
		log.Errorln(err.Error())
		bot.tryEditMessage(ctx.Callback().Message, "❌ Could not verify balance. Batch cancelled.", &tb.ReplyMarkup{})
		batchData.Inactivate(batchData, bot.Bunt)
		return ctx, err
	}
	if balance < batchData.TotalAmount {
		bot.tryEditMessage(ctx.Callback().Message, fmt.Sprintf("❌ *Insufficient balance*\n\nRequired: %s\nAvailable: %s\n\nBatch cancelled.",
			thirdparty.FormatSatsWithLKR(batchData.TotalAmount),
			thirdparty.FormatSatsWithLKR(balance)), &tb.ReplyMarkup{})
		batchData.Inactivate(batchData, bot.Bunt)
		return ctx, fmt.Errorf("insufficient balance for batch send")
	}

	// Update message to show processing
	bot.tryEditMessage(ctx.Callback().Message, "⏳ *Processing batch send...*", &tb.ReplyMarkup{})

	// Execute transfers sequentially
	var succeeded []string
	var failed []string
	var totalSent int64

	for _, entry := range batchData.Entries {
		to, err := GetLnbitsUser(&tb.User{ID: entry.ToTelegramId, Username: entry.ToUsername}, *bot)
		if err != nil {
			log.Errorf("[sendbatch] failed to get user @%s: %s", entry.ToUsername, err.Error())
			failed = append(failed, fmt.Sprintf("@%s — user error", entry.ToUsername))
			// Stop on first failure to prevent partial state issues
			for _, remaining := range batchData.Entries[len(succeeded)+len(failed):] {
				failed = append(failed, fmt.Sprintf("@%s — skipped", remaining.ToUsername))
			}
			break
		}

		fromUserStr := GetUserStr(from.Telegram)
		toUserStr := GetUserStr(to.Telegram)

		t := NewTransaction(bot, from, to, entry.Amount, TransactionType("sendbatch"))
		if entry.Memo != "" {
			t.Memo = entry.Memo
		} else {
			t.Memo = fmt.Sprintf("📦 Batch send from %s to %s.", fromUserStr, toUserStr)
		}

		success, err := t.Send()
		if !success || err != nil {
			log.Errorf("[sendbatch] transfer to @%s failed: %s", entry.ToUsername, err.Error())
			if bot.ErrorLogger != nil {
				bot.ErrorLogger.LogTransactionError(err, "sendbatch", entry.Amount, from.Telegram, to.Telegram)
			}
			failed = append(failed, fmt.Sprintf("@%s — transfer failed", entry.ToUsername))
			// Stop on failure — remaining are skipped
			for _, remaining := range batchData.Entries[len(succeeded)+len(failed):] {
				failed = append(failed, fmt.Sprintf("@%s — skipped", remaining.ToUsername))
			}
			break
		}

		totalSent += entry.Amount
		succeeded = append(succeeded, fmt.Sprintf("@%s — %s", entry.ToUsername, thirdparty.FormatSatsWithLKR(entry.Amount)))

		// Notify recipient
		fromUserStrMd := GetUserStrMd(from.Telegram)
		notifyMsg := fmt.Sprintf("📦 You received %s from %s", thirdparty.FormatSatsWithLKR(entry.Amount), fromUserStrMd)
		bot.trySendMessage(to.Telegram, notifyMsg)

		// Send memo to recipient if present
		if entry.Memo != "" {
			bot.trySendMessage(to.Telegram, fmt.Sprintf("✉️ %s", str.MarkdownEscape(entry.Memo)))
		}

		log.Infof("[📦 sendbatch] Send from %s to %s (%d sat).", fromUserStr, toUserStr, entry.Amount)
	}

	batchData.Inactivate(batchData, bot.Bunt)

	// Build result message
	var resultLines []string
	resultLines = append(resultLines, "📦 *Batch Send Complete*\n")

	if len(succeeded) > 0 {
		resultLines = append(resultLines, "*✅ Succeeded:*")
		for _, s := range succeeded {
			resultLines = append(resultLines, s)
		}
	}

	if len(failed) > 0 {
		resultLines = append(resultLines, "\n*❌ Failed:*")
		for _, f := range failed {
			resultLines = append(resultLines, f)
		}
	}

	resultLines = append(resultLines, fmt.Sprintf("\n*Total sent:* %s", thirdparty.FormatSatsWithLKR(totalSent)))

	resultMsg := strings.Join(resultLines, "\n")

	bot.tryDeleteMessage(ctx.Callback().Message)
	bot.trySendMessage(ctx.Callback().Sender, resultMsg)

	return ctx, nil
}

// cancelSendBatchHandler cancels the batch send
func (bot *TipBot) cancelSendBatchHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	user := LoadUser(ctx)
	ResetUserState(user, bot)

	tx := &SendBatchData{Base: storage.New(storage.ID(c.Data))}
	mutex.LockWithContext(ctx, tx.ID)
	defer mutex.UnlockWithContext(ctx, tx.ID)

	sn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		log.Errorf("[cancelSendBatchHandler] %s", err.Error())
		return ctx, err
	}

	batchData := sn.(*SendBatchData)
	// Only the sender can cancel
	if batchData.From.Telegram.ID != c.Sender.ID {
		return ctx, errors.Create(errors.UnknownError)
	}

	bot.tryDeleteMessage(c)
	bot.trySendMessage(c.Message.Chat, "🚫 Batch send cancelled.")
	batchData.Inactivate(batchData, bot.Bunt)

	return ctx, nil
}
