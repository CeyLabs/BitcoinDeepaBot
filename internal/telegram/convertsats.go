package telegram

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
)

// satToFiatHandler converts satoshis to USD and LKR values
func (bot *TipBot) satToFiatHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	bot.anyTextHandler(ctx)

	args := strings.Split(m.Text, " ")
	if len(args) < 2 {
		bot.trySendMessage(m.Sender, Translate(ctx, "convertEnterAmountMessage"))
		return ctx, nil
	}
	amountStr := strings.ReplaceAll(args[1], ",", "")
	sats, err := strconv.ParseInt(amountStr, 10, 64)
	if err != nil || sats <= 0 {
		bot.trySendMessage(m.Sender, Translate(ctx, "convertInvalidAmountMessage"))
		return ctx, nil
	}

	lkrPerSat, usdPerSat, err := thirdparty.GetSatPrice()
	if err != nil || lkrPerSat == 0 || usdPerSat == 0 {
		log.Errorf("[satToFiat] error fetching price: %v", err)
		bot.trySendMessage(m.Sender, Translate(ctx, "convertPriceErrorMessage"))
		return ctx, err
	}

	usd := usdPerSat * float64(sats)
	lkr := lkrPerSat * float64(sats)

	bot.trySendMessage(m.Sender, fmt.Sprintf(Translate(ctx, "convertSatsResultMessage"), sats, utils.FormatFloatWithCommas(usd), utils.FormatFloatWithCommas(lkr)))
	return ctx, nil
}
