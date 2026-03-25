package telegram

import (
	"context"
	"fmt"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
)

// StandingOrderScheduler runs hourly and executes standing orders on the
// configured day of month, clamping days 29–31 to the last day of short months.
type StandingOrderScheduler struct {
	bot           *TipBot
	CheckInterval time.Duration
}

// NewStandingOrderScheduler creates a new scheduler instance attached to the given bot.
func NewStandingOrderScheduler(bot *TipBot) *StandingOrderScheduler {
	return &StandingOrderScheduler{
		bot:           bot,
		CheckInterval: 1 * time.Hour,
	}
}

// Start launches the scheduler in a background goroutine.
// The provided context should be cancelled when the bot is shutting down
// so the scheduler exits cleanly without waiting for the next tick.
func (s *StandingOrderScheduler) Start(ctx context.Context) {
	go s.run(ctx)
}

// run is the main scheduler loop. It processes due orders immediately on start,
// then repeats every CheckInterval. It exits when ctx is cancelled.
func (s *StandingOrderScheduler) run(ctx context.Context) {
	ticker := time.NewTicker(s.CheckInterval)
	defer ticker.Stop()

	// Run immediately on start so orders due today are not delayed by one interval
	s.processDueOrders()

	for {
		select {
		case <-ctx.Done():
			// Bot is shutting down — exit the goroutine cleanly
			log.Infof("[StandingOrderScheduler] Shutting down.")
			return
		case <-ticker.C:
			s.processDueOrders()
		}
	}
}

// effectiveDayForMonth returns the day the order should fire in the given month.
// If the configured day exceeds the month's last day (e.g. day 31 in April),
// it clamps to the last day so salary-day users are never skipped.
func effectiveDayForMonth(configuredDay int, t time.Time) int {
	// time.Date with day=0 of next month gives last day of current month
	lastDay := time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, t.Location()).Day()
	if configuredDay > lastDay {
		return lastDay
	}
	return configuredDay
}

// shouldExecuteToday returns true if the order has not already run today.
// Protects against double-execution when the bot restarts mid-day.
func shouldExecuteToday(order lnbits.StandingOrder, now time.Time) bool {
	if order.LastExecutedAt == nil {
		return true
	}
	last := *order.LastExecutedAt
	return last.Year() != now.Year() || last.Month() != now.Month() || last.Day() != now.Day()
}

// processDueOrders fetches all active standing orders, filters to those due today
// (accounting for month-end clamping), and executes each one.
func (s *StandingOrderScheduler) processDueOrders() {
	now := time.Now()
	today := now.Day()

	var orders []lnbits.StandingOrder
	if err := s.bot.DB.Users.Where("active = true").Find(&orders).Error; err != nil {
		log.Errorf("[StandingOrderScheduler] Failed to fetch orders: %v", err)
		return
	}

	for _, order := range orders {
		if effectiveDayForMonth(order.DayOfMonth, now) != today {
			continue
		}
		if !shouldExecuteToday(order, now) {
			continue
		}

		// Load the user
		var user lnbits.User
		if err := s.bot.DB.Users.Where("id = ?", order.UserID).First(&user).Error; err != nil {
			log.Errorf("[StandingOrderScheduler] User not found for order %s: %v", order.ID, err)
			continue
		}

		// Skip banned or wallet-less users silently
		if user.Banned || user.Wallet == nil {
			continue
		}

		if err := s.executeOrder(&order, &user); err != nil {
			s.notifyFailure(&user, order, err)
		} else {
			s.notifySuccess(&user, order)
		}
	}
}

// executeOrder transfers the standing order amount to the target pot.
//
// LastExecutedAt is saved BEFORE the transfer so that if the transfer succeeds
// but the subsequent DB write fails, the order is not executed again on the next
// tick (double-execution). If the transfer itself fails, LastExecutedAt is reset
// to its previous value so the order can be retried next month.
//
// Worst case of this approach: a failed transfer + a failed reset means the order
// is skipped for this month — which is far safer than a double transfer.
func (s *StandingOrderScheduler) executeOrder(order *lnbits.StandingOrder, user *lnbits.User) error {
	now := time.Now()
	previousExecutedAt := order.LastExecutedAt

	// Mark as executed before the transfer to prevent double-execution
	order.LastExecutedAt = &now
	if err := s.bot.DB.Users.Save(order).Error; err != nil {
		return fmt.Errorf("failed to mark order as executed: %w", err)
	}

	// Execute the transfer
	if err := s.bot.TransferToPot(user, order.PotName, order.Amount); err != nil {
		// Transfer failed — reset LastExecutedAt so the order can be retried next month
		order.LastExecutedAt = previousExecutedAt
		if resetErr := s.bot.DB.Users.Save(order).Error; resetErr != nil {
			log.Errorf("[StandingOrderScheduler] Failed to reset LastExecutedAt for order %s after transfer failure: %v", order.ID, resetErr)
		}
		return err
	}

	return nil
}

// notifySuccess logs and sends a Telegram message to the user after a successful execution.
func (s *StandingOrderScheduler) notifySuccess(user *lnbits.User, order lnbits.StandingOrder) {
	log.Infof("[StandingOrderScheduler] Executed order %s for user %s: %s → pot '%s'",
		order.ID, user.Name, utils.FormatSats(order.Amount), order.PotName)
	msg := fmt.Sprintf(
		"✅ *Standing Order Executed*\n\n📅 Day %d of month\n💰 *%s* transferred to pot *'%s'*",
		order.DayOfMonth, utils.FormatSats(order.Amount), order.PotName,
	)
	s.bot.trySendMessage(user.Telegram, msg)
}

// notifyFailure logs the error and sends a Telegram message to the user explaining why the order failed.
func (s *StandingOrderScheduler) notifyFailure(user *lnbits.User, order lnbits.StandingOrder, err error) {
	log.Errorf("[StandingOrderScheduler] Failed to execute order %s for user %s: %v", order.ID, user.Name, err)
	msg := fmt.Sprintf(
		"⚠️ *Standing Order Failed*\n\n📅 Day %d of month\n💰 %s → pot *'%s'*\n\n🚫 Reason: %s",
		order.DayOfMonth, utils.FormatSats(order.Amount), order.PotName, err.Error(),
	)
	s.bot.trySendMessage(user.Telegram, msg)
}
