package telegram

import (
	"fmt"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

var errLNbitsUnavailable = fmt.Errorf("lnbits unavailable")

// worksWithoutLNbits reports whether a handler can run while LNbits is down.
// Help and price commands never touch LNbits, and cancel buttons only edit
// messages. /balance is listed because it handles the outage itself, so it
// can also show the last known balance.
func worksWithoutLNbits(endpoints []interface{}) bool {
	allowed := []interface{}{
		"/help", &btnHelpMainMenu, "/basics", "/advanced",
		"/lkrsats", &btnConvertMainMenu, "/convert",
		"/balance", &btnBalanceMainMenu,
		&btnCancelPay, &btnCancelSend, &btnCancelSendBatch, &btnCancelAPITx,
		&btnCancelInlineSend, &btnCancelInlineReceive, &btnCancelInlineFaucet,
		&btnCancelInlineTipjar, &btnCancelWithdraw, &btnCancelAuth,
		tb.OnDocument, tb.OnVideo, tb.OnAnimation, tb.OnVoice, tb.OnAudio, tb.OnSticker, tb.OnVideoNote,
	}
	for _, e := range endpoints {
		for _, a := range allowed {
			if e == a {
				return true
			}
		}
	}
	return false
}

// lnbitsAvailableInterceptor stops a request with a maintenance message while
// LNbits is down, instead of letting it hang and then fail with a confusing
// error or wrong data. It runs before the lock interceptor, so nothing needs
// to be unlocked when it stops the request.
func (bot TipBot) lnbitsAvailableInterceptor(ctx intercept.Context) (intercept.Context, error) {
	if bot.Client.Available() {
		return ctx, nil
	}
	// The balance button's text includes the balance, so it usually arrives
	// as plain text. Let it through: balanceHandler shows the maintenance
	// message together with the last known balance.
	if m := ctx.Message(); m != nil && strings.HasPrefix(m.Text, MainMenuCommandBalance) {
		return ctx, nil
	}
	ctx, _ = bot.localizerInterceptor(ctx)
	bot.sendMaintenanceNotice(ctx)
	return ctx, errLNbitsUnavailable
}

// sendMaintenanceNotice tells the user that BitcoinDeepa is under maintenance,
// in the way that fits the update type.
func (bot TipBot) sendMaintenanceNotice(ctx intercept.Context) {
	switch {
	case ctx.Message() != nil:
		m := ctx.Message()
		// In groups, only answer commands. Plain chat messages also pass
		// through the bot and must not trigger a reply each.
		if !m.Private() && !strings.HasPrefix(m.Text, "/") {
			return
		}
		bot.trySendMessage(m.Chat, Translate(ctx, "maintenanceMessage"))
	case ctx.Callback() != nil:
		err := bot.Telegram.Respond(ctx.Callback(), &tb.CallbackResponse{
			CallbackID: ctx.Callback().ID,
			Text:       TranslateUser(ctx, "maintenanceMessage"),
			ShowAlert:  true,
		})
		if err != nil {
			log.Errorf("[maintenance] could not answer callback: %v", err)
		}
	case ctx.Query() != nil:
		bot.inlineQueryReplyWithError(ctx, TranslateUser(ctx, "maintenanceTitle"), TranslateUser(ctx, "maintenanceMessage"))
	}
}

// maintenanceBalanceMessage is the /balance reply while LNbits is down. It adds
// the last balance the bot saw, so users can see their funds are still there.
func maintenanceBalanceMessage(ctx intercept.Context, lastKnownMsat int64) string {
	msg := Translate(ctx, "maintenanceMessage")
	if lastKnownMsat > 0 {
		msg += "\n\n" + fmt.Sprintf(Translate(ctx, "maintenanceLastBalanceMessage"), utils.FormatSats(lastKnownMsat/1000))
	}
	return msg
}
