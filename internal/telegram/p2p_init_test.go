package telegram

import (
	"testing"

	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// Bots, empty users and bad referral codes are refused before anything is created.
func TestP2PInitRejects(t *testing.T) {
	bot := buntBot(t)
	cases := map[string]struct {
		tg  *tb.User
		ref string
	}{
		"nil user":      {nil, ""},
		"bot account":   {&tb.User{ID: 5, IsBot: true}, ""},
		"zero id":       {&tb.User{ID: 0}, ""},
		"bad referral":  {&tb.User{ID: 5}, "has space"},
		"long referral": {&tb.User{ID: 5}, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for name, c := range cases {
		if _, created, _, err := bot.P2PInit(c.tg, c.ref); err == nil || created {
			t.Errorf("%s: created=%v err=%v, want refused", name, created, err)
		}
	}
}
