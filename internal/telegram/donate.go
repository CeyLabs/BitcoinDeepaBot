package telegram

import (
	"fmt"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// Donations go to bot.donate_address in the configuration.
//
// The upstream LightningTipBot code sent /donate, and any /send or /tip to the
// bot itself, to an upstream address hidden with rot13. That is gone: the
// destination is whatever the operator configures, it is named to the user,
// and with no address configured /donate is off and sends to the bot are
// ordinary transfers to the bot's wallet.

func (bot TipBot) donationHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	bot.anyTextHandler(ctx)
	endpoint := internal.Configuration.Bot.DonateEndpoint
	if endpoint == "" {
		bot.trySendMessage(m.Chat, Translate(ctx, "donationDisabledMessage"))
		return ctx, nil
	}
	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}
	// if no amount is in the command, ask for it
	amount, err := decodeAmountFromCommand(m.Text)
	if (err != nil || amount < 1) && m.Chat.Type == tb.ChatPrivate {
		_, err = bot.askForAmount(ctx, "", "CreateDonationState", 0, 0, m.Text)
		return ctx, err
	}
	if amount < 1 {
		return ctx, errors.Create(errors.InvalidAmountError)
	}
	msg := bot.trySendMessageEditable(m.Chat, Translate(ctx, "donationProgressMessage"))
	comment := fmt.Sprintf("from %s bot %s", GetUserStr(user.Telegram), GetUserStr(bot.Telegram.Me))
	invoice, err := lnbits.FetchLNURLPayInvoice(endpoint, amount*1000, comment)
	if err != nil {
		log.Errorf("[/donate] Could not get a donation invoice from %s: %v", endpoint, err)
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, err
	}
	_, err = user.Wallet.Pay(lnbits.PaymentParams{Out: true, Bolt11: invoice}, bot.Client)
	if err != nil {
		log.Errorf("[/donate] Donation failed for user %s: %v", GetUserStr(user.Telegram), err)
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, err
	}
	bot.tryDeleteMessage(msg)
	bot.trySendMessage(m.Chat, Translate(ctx, "donationSuccess"))
	return ctx, nil
}

// parseCmdDonHandler turns a /send or /tip addressed to the bot itself into a
// donation to bot.donate_address, telling the user where it goes. It returns
// an error when the command is not addressed to the bot or donations are not
// configured; the caller then carries on with a normal send or tip.
func (bot TipBot) parseCmdDonHandler(ctx intercept.Context) error {
	if internal.Configuration.Bot.DonateEndpoint == "" {
		return fmt.Errorf("donations are not configured")
	}
	m := ctx.Message()
	me := "@" + bot.Telegram.Me.Username
	arg := ""
	if strings.HasPrefix(strings.ToLower(m.Text), "/send") {
		arg, _ = getArgumentFromCommand(m.Text, 2)
	}
	if strings.HasPrefix(strings.ToLower(m.Text), "/tip") && m.ReplyTo != nil {
		arg = GetUserStr(m.ReplyTo.Sender)
	}
	// Telegram usernames are case-insensitive.
	if !strings.EqualFold(arg, me) {
		return fmt.Errorf("not addressed to the bot")
	}
	amount, err := decodeAmountFromCommand(m.Text)
	if err != nil {
		return err
	}
	bot.trySendMessage(m.Sender, str.MarkdownEscape(fmt.Sprintf(Translate(ctx, "donationRoutingMessage"), internal.Configuration.Bot.DonateAddress)))
	m.Text = fmt.Sprintf("/donate %d", amount)
	bot.donationHandler(ctx)
	// returning nil here will abort the parent ctx (/pay or /tip)
	return nil
}
