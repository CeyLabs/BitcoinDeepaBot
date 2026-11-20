package telegram

import (
	"regexp"
	"strconv"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// referralCodeRe matches what Telegram allows in a /start payload.
var referralCodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// P2PInit creates a wallet for a Mini App or web app user who never pressed
// Start, the same way /start does. tg comes from a verified Telegram
// credential, so it is the real user. created is false if the user already
// had a wallet. canMessage says whether the bot could message the user; it
// can't until they start the bot or allow messages in the Mini App.
func (bot *TipBot) P2PInit(tg *tb.User, referralCode string) (u *lnbits.User, created, canMessage bool, err error) {
	if tg == nil || tg.ID <= 0 || tg.IsBot {
		return nil, false, false, p2pErr("sender_not_allowed", "This account can't have a wallet.")
	}
	if referralCode != "" && !referralCodeRe.MatchString(referralCode) {
		return nil, false, false, p2pErr("invalid_referral_code", "Invalid referral code.")
	}
	// Same lock as the bot's own handlers, so /start and /api/v1/init can't both
	// create a wallet for this user.
	key := strconv.FormatInt(tg.ID, 10)
	mutex.Lock(key)
	defer mutex.Unlock(key)

	if existing, err := GetUserByTelegramID(tg.ID, *bot); err == nil && existing.Initialized {
		u, err := bot.P2PSender(tg.ID)
		return u, false, false, err
	}
	if referralCode != "" {
		if err := RecordReferral(bot.DB.Referrals, ReferralEntry{TelegramID: tg.ID, Username: tg.Username, ReferralCode: referralCode}); err != nil {
			log.Warnf("[init] Could not save referral for %d: %v", tg.ID, err)
		}
	}
	if _, err := bot.initWallet(tg); err != nil {
		log.Errorf("[init] Could not create wallet for %d: %v", tg.ID, err)
		return nil, false, false, p2pErr("wallet_unavailable", "Could not create your wallet. Try again.")
	}
	u, err = bot.P2PSender(tg.ID)
	if err != nil {
		return nil, false, false, err
	}
	log.Infof("[⭐️ init] New user from the Mini App / web app: %s (%d)", GetUserStr(tg), tg.ID)
	_, sendErr := bot.Telegram.Send(tg, "✅ Your Bitcoin Deepa wallet is ready. You'll get payment receipts here.")
	return u, true, sendErr == nil, nil
}
