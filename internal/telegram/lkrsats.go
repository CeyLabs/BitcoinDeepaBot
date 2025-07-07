package telegram

import (
	"fmt"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	log "github.com/sirupsen/logrus"
	"strconv"
	"strings"
)

func (bot *TipBot) lkrToSatHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	bot.anyTextHandler(ctx)

	args := strings.Split(m.Text, " ")
	if len(args) < 2 {
		bot.trySendMessage(m.Sender, Translate(ctx, "convertEnterAmountMessage"))
		return ctx, nil
	}
	amountStr := strings.ReplaceAll(args[1], ",", "")
	amount, err := strconv.ParseFloat(amountStr, 64)
	if err != nil || amount <= 0 {
		bot.trySendMessage(m.Sender, Translate(ctx, "convertInvalidAmountMessage"))
		return ctx, nil
	}

	lkrPerSat, _, err := thirdparty.GetSatPrice()
	if err != nil || lkrPerSat == 0 {
		log.Errorf("[lkrToSat] error fetching price: %v", err)
		bot.trySendMessage(m.Sender, Translate(ctx, "convertPriceErrorMessage"))
		return ctx, err
	}
	sats := int64(amount / lkrPerSat)
	bot.trySendMessage(m.Sender, fmt.Sprintf(Translate(ctx, "convertResultMessage"), amount, sats))
	return ctx, nil
}
