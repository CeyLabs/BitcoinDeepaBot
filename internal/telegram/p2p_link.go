package telegram

import (
	"strconv"
	"time"

	"github.com/tidwall/buntdb"
)

// A user is "linked" once the DCA backend has made a call for them with a
// Telegram credential the bot verified (POST /api/v1/init or any other V1
// call). Only linked users can be named with X-BDB-User (V2), so users who
// only use the bot chat can't be acted on by the DCA backend alone.

// p2pLinkTTL is how long a link lasts without a new Telegram-verified call.
const p2pLinkTTL = 365 * 24 * time.Hour

func p2pLinkKey(telegramID int64) string { return "p2p-linked:" + strconv.FormatInt(telegramID, 10) }

// P2PLink records a Telegram-verified call for the user, or renews the link.
func (bot *TipBot) P2PLink(telegramID int64) {
	bot.Bunt.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(p2pLinkKey(telegramID), strconv.FormatInt(time.Now().Unix(), 10),
			&buntdb.SetOptions{Expires: true, TTL: p2pLinkTTL})
		return err
	})
}

// P2PLinked reports whether the user has made a Telegram-verified call.
func (bot *TipBot) P2PLinked(telegramID int64) bool {
	linked := false
	bot.Bunt.View(func(tx *buntdb.Tx) error {
		_, err := tx.Get(p2pLinkKey(telegramID))
		linked = err == nil
		return nil
	})
	return linked
}
