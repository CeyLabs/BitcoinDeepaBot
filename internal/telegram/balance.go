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
	// Use database balance (in msat) to reflect pot transfers
	balance := user.Wallet.Balance / 1000

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
	totalUSDValue := USDPerSat * float64(totalBalance)
	totalLKRValue := LKRPerSat * float64(totalBalance)
	potUSDValue := USDPerSat * float64(potBalance)
	potLKRValue := LKRPerSat * float64(potBalance)

	message := fmt.Sprintf(Translate(ctx, "balanceMessage"),
		utils.FormatSats(balance),
		utils.FormatFloatWithCommas(mainUSDValue),
		utils.FormatFloatWithCommas(mainLKRValue))

	if potBalance > 0 {
		potInfo := fmt.Sprintf(Translate(ctx, "potBalanceInfo"),
			utils.FormatSats(potBalance),
			utils.FormatFloatWithCommas(potUSDValue),
			utils.FormatFloatWithCommas(potLKRValue))
		totalInfo := fmt.Sprintf(Translate(ctx, "totalBalanceInfo"),
			utils.FormatSats(totalBalance),
			utils.FormatFloatWithCommas(totalUSDValue),
			utils.FormatFloatWithCommas(totalLKRValue))
		message += "\n" + potInfo + "\n" + totalInfo
	}

	bot.trySendMessage(ctx.Sender(), message)
	return ctx, nil
}
