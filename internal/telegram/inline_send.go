package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"

	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"

	"github.com/eko/gocache/store"

	"github.com/LightningTipBot/LightningTipBot/internal/i18n"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"

	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

var (
	inlineSendMenu      = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnCancelInlineSend = inlineSendMenu.Data("🚫 Cancel", "cancel_send_inline")
	btnAcceptInlineSend = inlineSendMenu.Data("✅ Receive", "confirm_send_inline")
)

type InlineSend struct {
	*storage.Base
	Message         string       `json:"inline_send_message"`
	Amount          int64        `json:"inline_send_amount"`
	From            *lnbits.User `json:"inline_send_from"`
	To              *lnbits.User `json:"inline_send_to"`
	To_SpecificUser bool         `json:"to_specific_user"`
	Memo            string       `json:"inline_send_memo"`
	LanguageCode    string       `json:"languagecode"`
}

func (bot TipBot) makeSendKeyboard(ctx context.Context, id string) *tb.ReplyMarkup {
	inlineSendMenu := &tb.ReplyMarkup{ResizeKeyboard: true}
	acceptInlineSendButton := inlineSendMenu.Data(Translate(ctx, "receiveButtonMessage"), "confirm_send_inline")
	cancelInlineSendButton := inlineSendMenu.Data(Translate(ctx, "cancelButtonMessage"), "cancel_send_inline")
	acceptInlineSendButton.Data = id
	cancelInlineSendButton.Data = id

	inlineSendMenu.Inline(
		inlineSendMenu.Row(
			acceptInlineSendButton,
			cancelInlineSendButton),
	)
	return inlineSendMenu
}

func (bot TipBot) handleInlineSendQuery(ctx intercept.Context) (intercept.Context, error) {
	q := ctx.Query()
	// inlineSend := NewInlineSend()
	// var err error
	amount, err := decodeAmountFromCommand(q.Text)
	if err != nil {
		bot.inlineQueryReplyWithError(ctx, TranslateUser(ctx, "inlineQuerySendTitle"), fmt.Sprintf(TranslateUser(ctx, "inlineQuerySendDescription"), bot.Telegram.Me.Username))
		return ctx, err
	}
	if amount < 1 {
		bot.inlineQueryReplyWithError(ctx, TranslateUser(ctx, "inlineSendInvalidAmountMessage"), fmt.Sprintf(Translate(ctx, "inlineQuerySendDescription"), bot.Telegram.Me.Username))
		return ctx, errors.Create(errors.InvalidAmountError)
	}
	fromUser := LoadUser(ctx)
	fromUserStr := GetUserStr(q.Sender)
	balance, err := bot.GetUserBalanceCached(fromUser)
	if err != nil {
		errmsg := fmt.Sprintf("could not get balance of user %s", fromUserStr)
		log.Errorln(errmsg)
		return ctx, err
	}
	// check if fromUser has balance
	if balance < amount {
		log.Errorf("Balance of user %s too low", fromUserStr)
		bot.inlineQueryReplyWithError(ctx, TranslateUser(ctx, "inlineSendBalanceLowMessage"), fmt.Sprintf(TranslateUser(ctx, "inlineQuerySendDescription"), bot.Telegram.Me.Username))
		return ctx, errors.Create(errors.InvalidAmountError)
	}

	// check whether the 3rd argument is a username
	// command is "@LightningTipBot send 123 @to_user This is the memo"
	memo_argn := 2 // argument index at which the memo starts, will be 3 if there is a to_username in command
	toUserDb := &lnbits.User{}
	to_SpecificUser := false
	if len(strings.Split(q.Text, " ")) > 2 {
		to_username := strings.Split(q.Text, " ")[2]
		if strings.HasPrefix(to_username, "@") {
			toUserDb, err = GetUserByTelegramUsername(to_username[1:], bot) // must be without the @
			if err != nil {
				//bot.tryDeleteMessage(m)
				//bot.trySendMessage(m.Sender, fmt.Sprintf(Translate(ctx, "sendUserHasNoWalletMessage"), toUserStrMention))
				bot.inlineQueryReplyWithError(ctx,
					fmt.Sprintf(TranslateUser(ctx, "sendUserHasNoWalletMessage"), to_username),
					fmt.Sprintf(TranslateUser(ctx, "inlineQuerySendDescription"),
						bot.Telegram.Me.Username))
				return ctx, err
			}
			memo_argn = 3 // assume that memo starts after the to_username
			to_SpecificUser = true
		}
	}

	// check for memo in command
	memo := GetMemoFromCommand(q.Text, memo_argn)
	results := make(tb.Results, 0, 2) // []tb.Result

	// helper function to create a result and store inline send data
	createResult := func(amountSat int64, title string, description string) {
		inlineMessage := fmt.Sprintf(Translate(ctx, "inlineSendMessage"), fromUserStr, thirdparty.FormatSatsWithLKR(amountSat))

		// modify message if payment is to specific user
		if to_SpecificUser {
			inlineMessage = fmt.Sprintf("@%s: %s", toUserDb.Telegram.Username, inlineMessage)
		}

		if len(memo) > 0 {
			inlineMessage = inlineMessage + fmt.Sprintf(Translate(ctx, "inlineSendAppendMemo"), memo)
		}
		result := &tb.ArticleResult{
			Text:        inlineMessage,
			Title:       title,
			Description: description,
			ThumbURL:    queryImage,
		}
		id := fmt.Sprintf("inl-send-%d-%d-%s", q.Sender.ID, amountSat, RandStringRunes(5))
		result.ReplyMarkup = &tb.ReplyMarkup{InlineKeyboard: bot.makeSendKeyboard(ctx, id).InlineKeyboard}
		result.SetResultID(id)
		results = append(results, result)

		inlineSend := InlineSend{
			Base:            storage.New(storage.ID(id)),
			Message:         inlineMessage,
			From:            fromUser,
			To:              toUserDb,
			To_SpecificUser: to_SpecificUser,
			Memo:            memo,
			Amount:          amountSat,
			LanguageCode:    ctx.Value("publicLanguageCode").(string),
		}
		bot.Cache.Set(inlineSend.ID, inlineSend, &store.Options{Expiration: 5 * time.Minute})
	}

	// result treating the amount as sats
	if balance >= amount {
		title := fmt.Sprintf(TranslateUser(ctx, "inlineResultSendTitle"), amount)
		description := fmt.Sprintf(TranslateUser(ctx, "inlineResultSendDescription"), amount)
		createResult(amount, title, description)
	}

	// result treating the amount as LKR
	amountStr, errArg := getArgumentFromCommand(q.Text, 1)
	if errArg == nil {
		if f, err := strconv.ParseFloat(strings.ReplaceAll(amountStr, ",", ""), 64); err == nil {
			if satFromLKR, err := thirdparty.LKRToSat(f); err == nil && balance >= satFromLKR {
				title := fmt.Sprintf("💸 Send %.2f LKR (~%d sat)", f, satFromLKR)
				description := fmt.Sprintf("👉 Click to send %.2f LKR (~%d sat) to this chat.", f, satFromLKR)
				createResult(satFromLKR, title, description)
			}
		}
	}

	err = bot.Telegram.Answer(q, &tb.QueryResponse{
		Results:   results,
		CacheTime: 1, // 60 == 1 minute, todo: make higher than 1 s in production

	})
	if err != nil {
		log.Errorln(err)
		return ctx, err
	}
	return ctx, nil
}

func (bot *TipBot) acceptInlineSendHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	to := LoadUser(ctx)
	tx := &InlineSend{Base: storage.New(storage.ID(c.Data))}
	mutex.LockWithContext(ctx, tx.ID)
	defer mutex.UnlockWithContext(ctx, tx.ID)
	sn, err := tx.Get(tx, bot.Bunt)
	// immediatelly set intransaction to block duplicate calls
	if err != nil {
		// log.Errorf("[acceptInlineSendHandler] %s", err.Error())
		return ctx, err
	}
	inlineSend := sn.(*InlineSend)

	fromUser := inlineSend.From
	if !inlineSend.Active {
		log.Errorf("[acceptInlineSendHandler] inline send not active anymore")
		return ctx, errors.Create(errors.NotActiveError)
	}

	defer inlineSend.Set(inlineSend, bot.Bunt)

	amount := inlineSend.Amount

	// check if this payment goes to a specific user
	if inlineSend.To_SpecificUser {
		if inlineSend.To.Telegram.ID != to.Telegram.ID {
			// log.Infof("User %d is not User %d", inlineSend.To.Telegram.ID, to.Telegram.ID)
			return ctx, errors.Create(errors.UnknownError)
		}
	} else {
		// otherwise, we just set it to the user who has clicked
		inlineSend.To = to
	}

	if fromUser.Telegram.ID == to.Telegram.ID {
		bot.trySendMessage(fromUser.Telegram, Translate(ctx, "sendYourselfMessage"))
		return ctx, errors.Create(errors.UnknownError)
	}

	toUserStrMd := GetUserStrMd(to.Telegram)
	fromUserStrMd := GetUserStrMd(fromUser.Telegram)
	toUserStr := GetUserStr(to.Telegram)
	fromUserStr := GetUserStr(fromUser.Telegram)

	// check if user exists and create a wallet if not
	_, exists := bot.UserExists(to.Telegram)
	if !exists {
		log.Infof("[sendInline] User %s has no wallet.", toUserStr)
		to, err = bot.CreateWalletForTelegramUser(to.Telegram)
		if err != nil {
			errmsg := fmt.Errorf("[sendInline] Error: Could not create wallet for %s", toUserStr)
			log.Errorln(errmsg)
			return ctx, err
		}
	}
	// set inactive to avoid double-sends
	inlineSend.Inactivate(inlineSend, bot.Bunt)

	// todo: user new get username function to get userStrings
	transactionMemo := fmt.Sprintf("💸 Send from %s to %s.", fromUserStr, toUserStr)
	t := NewTransaction(bot, fromUser, to, amount, TransactionType("inline send"))
	t.Memo = transactionMemo
	success, err := t.Send()
	if !success {
		errMsg := fmt.Sprintf("[sendInline] Transaction failed: %s", err.Error())
		log.Errorln(errMsg)
		bot.tryEditMessage(c, i18n.Translate(inlineSend.LanguageCode, "inlineSendFailedMessage"), &tb.ReplyMarkup{})
		return ctx, errors.Create(errors.UnknownError)
	}

	log.Infof("[💸 sendInline] Send from %s to %s (%d sat).", fromUserStr, toUserStr, amount)

	inlineSend.Message = fmt.Sprintf("%s", fmt.Sprintf(i18n.Translate(inlineSend.LanguageCode, "inlineSendUpdateMessageAccept"), thirdparty.FormatSatsWithLKR(amount), fromUserStrMd, toUserStrMd))
	memo := inlineSend.Memo
	if len(memo) > 0 {
		inlineSend.Message = inlineSend.Message + fmt.Sprintf(i18n.Translate(inlineSend.LanguageCode, "inlineSendAppendMemo"), memo)
	}
	if !to.Initialized {
		inlineSend.Message += "\n\n" + fmt.Sprintf(i18n.Translate(inlineSend.LanguageCode, "inlineSendCreateWalletMessage"), GetUserStrMd(bot.Telegram.Me))
	}
	bot.tryEditMessage(c, inlineSend.Message, &tb.ReplyMarkup{})
	// notify users
	bot.trySendMessage(to.Telegram, fmt.Sprintf(i18n.Translate(to.Telegram.LanguageCode, "sendReceivedMessage"), fromUserStrMd, thirdparty.FormatSatsWithLKR(amount)))
	bot.trySendMessage(fromUser.Telegram, fmt.Sprintf(i18n.Translate(fromUser.Telegram.LanguageCode, "sendSentMessage"), thirdparty.FormatSatsWithLKR(amount), toUserStrMd))
	if err != nil {
		errmsg := fmt.Errorf("[sendInline] Error: Send message to %s: %s", toUserStr, err)
		log.Warnln(errmsg)
	}
	return ctx, nil
}

func (bot *TipBot) cancelInlineSendHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	tx := &InlineSend{Base: storage.New(storage.ID(c.Data))}
	mutex.LockWithContext(ctx, tx.ID)
	defer mutex.UnlockWithContext(ctx, tx.ID)
	// immediatelly set intransaction to block duplicate calls
	sn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		log.Errorf("[cancelInlineSendHandler] %s", err.Error())
		return ctx, err
	}
	inlineSend := sn.(*InlineSend)
	if c.Sender.ID != inlineSend.From.Telegram.ID {
		return ctx, errors.Create(errors.UnknownError)
	}
	bot.tryEditMessage(c, i18n.Translate(inlineSend.LanguageCode, "sendCancelledMessage"), &tb.ReplyMarkup{})
	// set the inlineSend inactive
	inlineSend.Active = false
	return ctx, inlineSend.Set(inlineSend, bot.Bunt)
}
