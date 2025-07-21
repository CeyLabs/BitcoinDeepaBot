package telegram

import (
	"fmt"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"

	log "github.com/sirupsen/logrus"

	tb "gopkg.in/lightningtipbot/telebot.v3"
)

func (bot *TipBot) balanceHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	// check and print all commands
	if len(m.Text) > 0 {
		bot.anyTextHandler(ctx)
	}

	// reply only in private message
	if m.Chat.Type != tb.ChatPrivate {
		// delete message
		bot.tryDeleteMessage(m)
	}
	// first check whether the user is initialized
	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	if !user.Initialized {
		return bot.startHandler(ctx)
	}

	usrStr := GetUserStr(ctx.Sender())
	balance, err := bot.GetUserBalance(user)
	if err != nil {
		log.Errorf("[/balance] Error fetching %s's balance: %s", usrStr, err)
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "balanceErrorMessage"))
		return ctx, err
	}

	log.Infof("[/balance] %s's balance: %s\n", usrStr, utils.FormatSats(balance))

	LKRPerSat, USDPerSat, err := thirdparty.GetSatPrice()
	if err != nil {
		log.Infof("[/balance] error fetching price from coingecko\n")
	}

	potBalance, err := bot.GetUserTotalPotBalance(user)
	if err != nil {
		log.Errorf("[/balance] Error fetching %s's pot balance: %s", usrStr, err)
		potBalance = 0
	}

	totalBalance := balance + potBalance
	mainUSDValue := USDPerSat * float64(balance)
	mainLKRValue := LKRPerSat * float64(balance)
	USDValue := USDPerSat * float64(totalBalance)
	LKRValue := LKRPerSat * float64(totalBalance)

	message := fmt.Sprintf(Translate(ctx, "balanceMessage"), utils.FormatSats(balance), utils.FormatFloatWithCommas(mainUSDValue), utils.FormatFloatWithCommas(mainLKRValue))
	
	if potBalance > 0 {
		message += fmt.Sprintf("\n💰 **In savings pots**: %s sats\n🏦 **Total balance**: %s sats (%s USD / රු. %s)", utils.FormatSats(potBalance), utils.FormatSats(totalBalance), utils.FormatFloatWithCommas(USDValue), utils.FormatFloatWithCommas(LKRValue))
	}

	bot.trySendMessage(ctx.Sender(), message)
	return ctx, nil
}
