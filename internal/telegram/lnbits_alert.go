package telegram

import (
	"sync"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/utils"
)

// lnbitsAlertDelay is how long LNbits must stay down before admins are
// alerted, so a single failed request does not page anyone.
const lnbitsAlertDelay = 30 * time.Second

// lnbitsAlerter tells admins in the log group when LNbits goes down and when
// it comes back. Users already get the maintenance message right away; this
// is only for admins.
type lnbitsAlerter struct {
	mu        sync.Mutex
	downSince time.Time // zero while LNbits is up
	alerted   bool      // a down alert was sent for the current outage
	delay     time.Duration
	sendDown  func(reason string, since time.Time)
	sendUp    func(since time.Time)
}

// onChange is registered with lnbits.Client.OnHealthChange. It must not
// block, so alerts are sent from timers and goroutines.
func (a *lnbitsAlerter) onChange(down bool, reason string, since time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if down {
		a.downSince = since
		a.alerted = false
		time.AfterFunc(a.delay, func() { a.confirmDown(reason, since) })
		return
	}
	if a.alerted {
		go a.sendUp(since)
	}
	a.downSince = time.Time{}
	a.alerted = false
}

// confirmDown sends the down alert if the outage that started at since is
// still going on.
func (a *lnbitsAlerter) confirmDown(reason string, since time.Time) {
	a.mu.Lock()
	if !a.downSince.Equal(since) {
		// recovered already, or this is an older outage
		a.mu.Unlock()
		return
	}
	a.alerted = true
	a.mu.Unlock()
	a.sendDown(reason, since)
}

// startLNbitsAlerts hooks the admin alerts into the LNbits health tracking.
func (bot *TipBot) startLNbitsAlerts() {
	if bot.ErrorLogger == nil || !bot.ErrorLogger.enabled {
		return
	}
	a := &lnbitsAlerter{
		delay:    lnbitsAlertDelay,
		sendDown: bot.ErrorLogger.LogLNbitsDown,
		sendUp:   bot.ErrorLogger.LogLNbitsUp,
	}
	bot.Client.OnHealthChange(a.onChange)
}

// LogLNbitsDown alerts the log group that LNbits is unreachable.
func (el *ErrorLogger) LogLNbitsDown(reason string, since time.Time) {
	msg := "🔴 <b>LNbits is down</b>\n\n" +
		"Users now get the maintenance message.\n" +
		"🕒 Since: " + since.UTC().Format("2006-01-02 15:04:05 UTC") + "\n" +
		"<blockquote expandable>❗ " + utils.EscapeHTML(reason) + "</blockquote>"
	el.sendToTelegramHTML(msg)
}

// LogLNbitsUp alerts the log group that LNbits is reachable again.
func (el *ErrorLogger) LogLNbitsUp(since time.Time) {
	msg := "🟢 <b>LNbits is back up</b>\n\n" +
		"The bot works normally again.\n" +
		"⏱ Downtime: " + time.Since(since).Round(time.Second).String()
	el.sendToTelegramHTML(msg)
}
