package telegram

import (
	"fmt"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// Buttons exist at package level only so their handlers can be registered by
// unique key; each confirmation message gets its own markup (p2pMarkup).
var (
	p2pConfirmMenu   = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnP2PConfirm    = p2pConfirmMenu.Data("✅ Confirm", "p2p_confirm")
	btnP2PCancel     = p2pConfirmMenu.Data("🚫 Cancel", "p2p_cancel")
	p2pConfirmFormat = "🔐 *Confirm payment from the Mini App*\n\n💸 Amount: %s\n%s%s\n\n⏳ Expires at %s UTC. If you didn't start this, press Cancel."
)

func p2pMarkup(id string) *tb.ReplyMarkup {
	menu := &tb.ReplyMarkup{ResizeKeyboard: true}
	menu.Inline(menu.Row(menu.Data("✅ Confirm", "p2p_confirm", id), menu.Data("🚫 Cancel", "p2p_cancel", id)))
	return menu
}

func p2pTarget(t *P2PTransfer) string {
	if t.Kind == P2PKindInvoice {
		s := "⚡ Lightning invoice"
		if t.Description != "" {
			s += ": " + str.MarkdownEscape(t.Description)
		}
		return s
	}
	s := "👤 To: " + str.MarkdownEscape(t.ToName)
	if t.ToUsername != "" {
		s += " (@" + str.MarkdownEscape(t.ToUsername) + ")"
	}
	return s
}

func (bot *TipBot) sendP2PConfirmation(from *lnbits.User, t *P2PTransfer) error {
	memo := ""
	if t.Memo != "" {
		memo = "\n✉️ " + str.MarkdownEscape(t.Memo)
	}
	text := fmt.Sprintf(p2pConfirmFormat, thirdparty.FormatSatsWithLKR(t.Amount), p2pTarget(t), memo, t.ConfirmBy.UTC().Format("15:04"))
	_, err := bot.Telegram.Send(from.Telegram, text, p2pMarkup(t.ID), tb.ModeMarkdown)
	return err
}

// p2pLoadForCallback loads the transfer a button belongs to, and only for the
// user it was sent to.
func (bot *TipBot) p2pLoadForCallback(ctx intercept.Context) (*P2PTransfer, bool) {
	t, err := bot.P2PGet(ctx.Data())
	if err != nil {
		bot.tryEditMessage(ctx.Callback().Message, "❌ This request no longer exists.", &tb.ReplyMarkup{})
		return nil, false
	}
	if t.FromTelegramID != ctx.Callback().Sender.ID {
		return nil, false
	}
	return t, true
}

func (bot *TipBot) p2pConfirmHandler(ctx intercept.Context) (intercept.Context, error) {
	mutex.LockWithContext(ctx, ctx.Data())
	defer mutex.UnlockWithContext(ctx, ctx.Data())
	t, ok := bot.p2pLoadForCallback(ctx)
	if !ok {
		return ctx, nil
	}
	msg := ctx.Callback().Message
	if t.Status != P2PAwaitingConfirmation {
		bot.tryEditMessage(msg, "ℹ️ This request was already handled.", &tb.ReplyMarkup{})
		return ctx, nil
	}
	if time.Now().After(t.ConfirmBy) {
		if bot.p2pExpire(t) {
			bot.tryEditMessage(msg, "⌛ This request expired. Nothing was sent.", &tb.ReplyMarkup{})
		} else {
			bot.tryEditMessage(msg, "ℹ️ This request was already handled.", &tb.ReplyMarkup{})
		}
		return ctx, nil
	}
	if !bot.p2pClaim(t) {
		bot.tryEditMessage(msg, "ℹ️ This request was already handled.", &tb.ReplyMarkup{})
		return ctx, nil
	}
	t.Status = P2PProcessing
	if err := bot.p2pSave(t); err != nil {
		bot.tryEditMessage(msg, "❌ Could not start the payment. Nothing was sent.", &tb.ReplyMarkup{})
		return ctx, err
	}
	bot.tryEditMessage(msg, "⏳ Sending…", &tb.ReplyMarkup{})
	bot.p2pRun(t)

	switch t.Status {
	case P2PCompleted:
		bot.tryEditMessage(msg, fmt.Sprintf("✅ Confirmed and sent: %s", thirdparty.FormatSatsWithLKR(t.Amount)), &tb.ReplyMarkup{})
	case P2PInFlight:
		bot.tryEditMessage(msg, "⏳ Confirmed. The payment is in flight.", &tb.ReplyMarkup{})
	default:
		bot.tryEditMessage(msg, "❌ "+t.Error, &tb.ReplyMarkup{})
	}
	return ctx, nil
}

func (bot *TipBot) p2pCancelHandler(ctx intercept.Context) (intercept.Context, error) {
	mutex.LockWithContext(ctx, ctx.Data())
	defer mutex.UnlockWithContext(ctx, ctx.Data())
	t, ok := bot.p2pLoadForCallback(ctx)
	if !ok {
		return ctx, nil
	}
	if t.Status != P2PAwaitingConfirmation {
		bot.tryEditMessage(ctx.Callback().Message, "ℹ️ This request was already handled.", &tb.ReplyMarkup{})
		return ctx, nil
	}
	if !bot.p2pClaim(t) {
		bot.tryEditMessage(ctx.Callback().Message, "ℹ️ This request was already handled.", &tb.ReplyMarkup{})
		return ctx, nil
	}
	if t.Kind == P2PKindInvoice {
		bot.Bunt.DeleteKey(p2pInvoiceLock(t.PaymentHash))
	}
	bot.p2pFinish(t, P2PCancelled, "Cancelled in the bot chat. Nothing was sent.")
	bot.tryEditMessage(ctx.Callback().Message, "🚫 Cancelled. Nothing was sent.", &tb.ReplyMarkup{})
	log.Infof("[p2p] Transfer %s cancelled by %d", t.ID, t.FromTelegramID)
	return ctx, nil
}
