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
	// Get available balance (wallet balance - pot balance)
	availableBalance, err := bot.GetUserAvailableBalance(user)
	if err != nil {
		log.Errorf("[/balance] Error fetching %s's available balance: %s", usrStr, err)
		availableBalance = 0
	}

	log.Infof("[/balance] %s's available balance: %s\n", usrStr, utils.FormatSats(availableBalance))

	LKRPerSat, USDPerSat, err := thirdparty.GetSatPrice()
	if err != nil {
		log.Infof("[/balance] error fetching price from coingecko\n")
	}

	potBalance, err := bot.GetUserTotalPotBalance(user)
	if err != nil {
		log.Errorf("[/balance] Error fetching %s's pot balance: %s", usrStr, err)
		potBalance = 0
	}

	availableUSDValue := USDPerSat * float64(availableBalance)
	availableLKRValue := LKRPerSat * float64(availableBalance)

	potUSDValue := USDPerSat * float64(potBalance)
	potLKRValue := LKRPerSat * float64(potBalance)

	totalBalance := availableBalance + potBalance
	totalUSDValue := USDPerSat * float64(totalBalance)
	totalLKRValue := LKRPerSat * float64(totalBalance)

	message := fmt.Sprintf(Translate(ctx, "balanceMessage"),
		utils.FormatSats(availableBalance),
		utils.FormatFloatWithCommas(availableUSDValue),
		utils.FormatFloatWithCommas(availableLKRValue))

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
