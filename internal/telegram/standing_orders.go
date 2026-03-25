package telegram

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	uuid "github.com/satori/go.uuid"
)

const (
	MaxStandingOrdersPerUser = 10
	MinDayOfMonth            = 1
	MaxDayOfMonth            = 31
)

func (bot *TipBot) CreateStandingOrder(user *lnbits.User, dayOfMonth int, amount int64, potName string) (*lnbits.StandingOrder, error) {
	if dayOfMonth < MinDayOfMonth || dayOfMonth > MaxDayOfMonth {
		return nil, fmt.Errorf("day must be between %d and %d", MinDayOfMonth, MaxDayOfMonth)
	}
	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	potName = strings.TrimSpace(potName)
	if _, err := bot.GetPot(user, potName); err != nil {
		return nil, fmt.Errorf("pot '%s' not found — create it first with /createpot", potName)
	}

	var orderCount int64
	bot.DB.Users.Model(&lnbits.StandingOrder{}).Where("user_id = ? AND active = true", user.ID).Count(&orderCount)
	if orderCount >= MaxStandingOrdersPerUser {
		return nil, fmt.Errorf("maximum number of standing orders reached (%d)", MaxStandingOrdersPerUser)
	}

	order := &lnbits.StandingOrder{
		ID:         uuid.NewV4().String(),
		UserID:     user.ID,
		PotName:    potName,
		DayOfMonth: dayOfMonth,
		Amount:     amount,
		Active:     true,
	}

	if err := bot.DB.Users.Create(order).Error; err != nil {
		return nil, fmt.Errorf("failed to create standing order: %w", err)
	}

	return order, nil
}

func (bot *TipBot) ListStandingOrders(user *lnbits.User) ([]lnbits.StandingOrder, error) {
	var orders []lnbits.StandingOrder
	err := bot.DB.Users.Where("user_id = ? AND active = true", user.ID).Order("day_of_month ASC").Find(&orders).Error
	return orders, err
}

func (bot *TipBot) GetStandingOrderByID(user *lnbits.User, orderID string) (*lnbits.StandingOrder, error) {
	var order lnbits.StandingOrder
	err := bot.DB.Users.Where("id = ? AND user_id = ?", orderID, user.ID).First(&order).Error
	if err != nil {
		return nil, fmt.Errorf("standing order not found")
	}
	return &order, nil
}

func (bot *TipBot) DeleteStandingOrder(user *lnbits.User, orderID string) error {
	result := bot.DB.Users.Where("id = ? AND user_id = ?", orderID, user.ID).Delete(&lnbits.StandingOrder{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("standing order not found")
	}
	return nil
}

// ─── Telegram Handler ─────────────────────────────────────────────────────────

const soHelpText = "📅 *Standing Orders (/so)*\n\n" +
	"`/so create <day> <amount> <pot>` — create a standing order\n" +
	"`/so list` — list your standing orders\n" +
	"`/so delete <number>` — delete by list number\n\n" +
	"*Example:* `/so create 25 1000 Savings`\n" +
	"_Day 29–31 fires on the last day of shorter months._"

// soHandler is the single entry point for all /so sub-commands.
func (bot *TipBot) soHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)

	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	arguments := strings.Fields(m.Text)
	if len(arguments) < 2 {
		bot.trySendMessage(ctx.Sender(), soHelpText)
		return ctx, nil
	}

	switch strings.ToLower(arguments[1]) {
	case "create":
		return bot.soCreateHandler(ctx, user, arguments)
	case "list":
		return bot.soListHandler(ctx, user)
	case "delete":
		return bot.soDeleteHandler(ctx, user, arguments)
	default:
		bot.trySendMessage(ctx.Sender(), soHelpText)
	}
	return ctx, nil
}

func (bot *TipBot) soCreateHandler(ctx intercept.Context, user *lnbits.User, arguments []string) (intercept.Context, error) {
	// /so create <day> <amount> <pot_name>
	if len(arguments) < 5 {
		bot.trySendMessage(ctx.Sender(), "📅 *Usage:* `/so create <day> <amount> <pot_name>`\n\nExample: `/so create 25 1000 Savings`")
		return ctx, nil
	}

	dayOfMonth, err := strconv.Atoi(arguments[2])
	if err != nil {
		bot.trySendMessage(ctx.Sender(), "❌ Invalid day — must be a number between 1 and 31.")
		return ctx, nil
	}

	amount, err := getAmount(ctx, arguments[3])
	if err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ Invalid amount: %s", err.Error()))
		return ctx, err
	}

	potName := strings.Join(arguments[4:], " ")

	order, err := bot.CreateStandingOrder(user, dayOfMonth, amount, potName)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ %s", err.Error()))
		return ctx, err
	}

	bot.trySendMessage(ctx.Sender(), fmt.Sprintf(
		"✅ *Standing Order Created*\n\n📅 Day *%d* of each month\n💰 *%s* → pot *'%s'*\n\nYour balance will be checked automatically on the scheduled day.",
		order.DayOfMonth, utils.FormatSats(order.Amount), order.PotName,
	))
	return ctx, nil
}

func (bot *TipBot) soListHandler(ctx intercept.Context, user *lnbits.User) (intercept.Context, error) {
	orders, err := bot.ListStandingOrders(user)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), "❌ Failed to fetch your standing orders.")
		return ctx, err
	}

	if len(orders) == 0 {
		bot.trySendMessage(ctx.Sender(), "📅 You have no standing orders yet.\n\nUse `/so create <day> <amount> <pot>` to create one.")
		return ctx, nil
	}

	message := "📅 *Your Standing Orders:*\n\n"
	for i, order := range orders {
		lastRun := "never run"
		if order.LastExecutedAt != nil {
			lastRun = fmt.Sprintf("last run: %s", order.LastExecutedAt.Format("2006-01-02"))
		}
		message += fmt.Sprintf("%d. Day *%d* → *%s* to pot *'%s'* [%s]\n",
			i+1, order.DayOfMonth, utils.FormatSats(order.Amount), order.PotName, lastRun)
	}
	message += "\nUse `/so delete <number>` to remove one."

	bot.trySendMessage(ctx.Sender(), message)
	return ctx, nil
}

func (bot *TipBot) soDeleteHandler(ctx intercept.Context, user *lnbits.User, arguments []string) (intercept.Context, error) {
	if len(arguments) < 3 {
		bot.trySendMessage(ctx.Sender(), "📅 *Usage:* `/so delete <number>`\n\nUse `/so list` to see your list.")
		return ctx, nil
	}

	index, err := strconv.Atoi(arguments[2])
	if err != nil || index < 1 {
		bot.trySendMessage(ctx.Sender(), "❌ Invalid number. Use `/so list` to see the list numbers.")
		return ctx, nil
	}

	orders, err := bot.ListStandingOrders(user)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), "❌ Failed to fetch your standing orders.")
		return ctx, err
	}

	if index > len(orders) {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ No standing order at position %d. You have %d order(s).", index, len(orders)))
		return ctx, nil
	}

	order := orders[index-1]
	if err := bot.DeleteStandingOrder(user, order.ID); err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ Failed to delete: %s", err.Error()))
		return ctx, err
	}

	bot.trySendMessage(ctx.Sender(), fmt.Sprintf("🗑️ Deleted: Day %d → %s to pot '%s'", order.DayOfMonth, utils.FormatSats(order.Amount), order.PotName))
	return ctx, nil
}
