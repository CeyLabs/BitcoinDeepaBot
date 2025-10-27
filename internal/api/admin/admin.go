package admin

import (
	"github.com/BitcoinDeepaBot/BitcoinDeepaBot/internal/telegram"
)

type Service struct {
	bot *telegram.TipBot
}

func New(b *telegram.TipBot) Service {
	return Service{
		bot: b,
	}
}
