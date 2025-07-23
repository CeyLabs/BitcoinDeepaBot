package telegram

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	uuid "github.com/satori/go.uuid"
	"gorm.io/gorm"
)

const (
	MinPotNameLength = 3
	MaxPotNameLength = 50
	MaxPotsPerUser   = 20
)

var potNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-\s]+$`)

func (bot *TipBot) CreatePot(user *lnbits.User, name string) (*lnbits.SavingsPot, error) {
	name = strings.TrimSpace(name)

	if len(name) < MinPotNameLength {
		return nil, fmt.Errorf("pot name too short (min %d characters)", MinPotNameLength)
	}

	if len(name) > MaxPotNameLength {
		return nil, fmt.Errorf("pot name too long (max %d characters)", MaxPotNameLength)
	}

	if !potNameRegex.MatchString(name) {
		return nil, fmt.Errorf("pot name can only contain letters, numbers, spaces, hyphens, and underscores")
	}

	var pot *lnbits.SavingsPot
	err := bot.DB.Users.Transaction(func(tx *gorm.DB) error {
		var existingPot lnbits.SavingsPot
		if err := tx.Where("user_id = ? AND name = ?", user.ID, name).First(&existingPot).Error; err == nil {
			return fmt.Errorf("pot with name '%s' already exists", name)
		}

		var potCount int64
		tx.Model(&lnbits.SavingsPot{}).Where("user_id = ?", user.ID).Count(&potCount)
		if potCount >= MaxPotsPerUser {
			return fmt.Errorf("maximum number of pots reached (%d)", MaxPotsPerUser)
		}

		pot = &lnbits.SavingsPot{
			ID:      uuid.NewV4().String(),
			UserID:  user.ID,
			Name:    name,
			Balance: 0,
		}

		if err := tx.Create(pot).Error; err != nil {
			return fmt.Errorf("failed to create pot: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return pot, nil
}

func (bot *TipBot) ListUserPots(user *lnbits.User) ([]lnbits.SavingsPot, error) {
	var pots []lnbits.SavingsPot
	err := bot.DB.Users.Where("user_id = ?", user.ID).Order("created_at ASC").Find(&pots).Error
	return pots, err
}

func (bot *TipBot) GetPot(user *lnbits.User, name string) (*lnbits.SavingsPot, error) {
	name = strings.TrimSpace(name)
	var pot lnbits.SavingsPot
	err := bot.DB.Users.Where("user_id = ? AND name = ?", user.ID, name).First(&pot).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("pot '%s' not found", name)
		}
		return nil, err
	}
	return &pot, nil
}

func (bot *TipBot) TransferToPot(user *lnbits.User, potName string, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}

	return bot.DB.Users.Transaction(func(tx *gorm.DB) error {
		// Get current user balance (within transaction)
		balance, err := bot.GetUserBalance(user)
		if err != nil {
			return fmt.Errorf("failed to get user balance: %w", err)
		}

		// Check if sufficient funds
		if balance < amount {
			return fmt.Errorf("insufficient balance. Available: %d sats, Requested: %d sats", balance, amount)
		}

		// Get the pot (within transaction)
		var pot lnbits.SavingsPot
		if err := tx.Where("user_id = ? AND name = ?", user.ID, strings.TrimSpace(potName)).First(&pot).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return fmt.Errorf("pot '%s' not found", potName)
			}
			return err
		}

		// Deduct from user balance
		user.Wallet.Balance -= amount
		if err := tx.Save(user).Error; err != nil {
			return fmt.Errorf("failed to update user balance: %w", err)
		}

		// Add to pot balance
		pot.Balance += amount
		if err := tx.Save(&pot).Error; err != nil {
			return fmt.Errorf("failed to update pot balance: %w", err)
		}

		return nil
	})
}

func (bot *TipBot) WithdrawFromPot(user *lnbits.User, potName string, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}

	return bot.DB.Users.Transaction(func(tx *gorm.DB) error {
		pot, err := bot.GetPot(user, potName)
		if err != nil {
			return err
		}

		if pot.Balance < amount {
			return fmt.Errorf("insufficient pot balance. Available: %d sats, Requested: %d sats", pot.Balance, amount)
		}

		// Deduct from pot balance
		pot.Balance -= amount

		if err := tx.Save(pot).Error; err != nil {
			return fmt.Errorf("failed to update pot balance: %w", err)
		}

		// Add back to user wallet balance
		user.Wallet.Balance += amount
		if err := tx.Save(user).Error; err != nil {
			return fmt.Errorf("failed to update user balance: %w", err)
		}

		return nil
	})
}

func (bot *TipBot) DeletePot(user *lnbits.User, name string) error {
	pot, err := bot.GetPot(user, name)
	if err != nil {
		return err
	}

	if pot.Balance > 0 {
		return fmt.Errorf("cannot delete pot with balance. Current balance: %d sats", pot.Balance)
	}

	return bot.DB.Users.Delete(pot).Error
}

func (bot *TipBot) GetUserTotalPotBalance(user *lnbits.User) (int64, error) {
	var totalBalance int64
	err := bot.DB.Users.Model(&lnbits.SavingsPot{}).
		Where("user_id = ?", user.ID).
		Select("COALESCE(SUM(balance), 0)").
		Scan(&totalBalance).Error
	return totalBalance, err
}

func (bot *TipBot) createPotHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)

	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	arguments := strings.Split(m.Text, " ")
	if len(arguments) < 2 {
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "createPotHelpText"))
		return ctx, nil
	}

	potName := strings.Join(arguments[1:], " ")

	pot, err := bot.CreatePot(user, potName)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ Failed to create pot: %s", err.Error()))
		return ctx, err
	}

	bot.trySendMessage(ctx.Sender(), fmt.Sprintf("✅ Created savings pot '%s'", pot.Name))
	return ctx, nil
}

func (bot *TipBot) listPotsHandler(ctx intercept.Context) (intercept.Context, error) {
	user := LoadUser(ctx)

	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	pots, err := bot.ListUserPots(user)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), "❌ Failed to fetch your pots")
		return ctx, err
	}

	if len(pots) == 0 {
		bot.trySendMessage(ctx.Sender(), "📝 You have no savings pots yet. Use /createpot <name> to create one!")
		return ctx, nil
	}

	message := "🏺 Your Savings Pots:\n\n"
	totalBalance := int64(0)

	for i, pot := range pots {
		totalBalance += pot.Balance
		message += fmt.Sprintf("%d. **%s**: %s sats\n", i+1, pot.Name, utils.FormatSats(pot.Balance))
	}

	message += fmt.Sprintf("\n💰 **Total in pots**: %s sats", utils.FormatSats(totalBalance))

	bot.trySendMessage(ctx.Sender(), message)
	return ctx, nil
}

func (bot *TipBot) addToPotHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)

	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	arguments := strings.Fields(m.Text)
	if len(arguments) < 3 {
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "addToPotHelpText"))
		return ctx, nil
	}

	// Last argument is amount, everything in between is pot name
	amountStr := arguments[len(arguments)-1]
	potName := strings.Join(arguments[1:len(arguments)-1], " ")

	amount, err := getAmount(ctx, amountStr)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ Invalid amount: %s", err.Error()))
		return ctx, err
	}

	if amount <= 0 {
		bot.trySendMessage(ctx.Sender(), "❌ Amount must be positive")
		return ctx, nil
	}

	err = bot.TransferToPot(user, potName, amount)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ Transfer failed: %s", err.Error()))
		return ctx, err
	}

	bot.trySendMessage(ctx.Sender(), fmt.Sprintf("✅ Transferred %s sats to pot '%s'", utils.FormatSats(amount), potName))
	return ctx, nil
}

func (bot *TipBot) withdrawFromPotHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)

	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	arguments := strings.Fields(m.Text)
	if len(arguments) < 3 {
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "withdrawFromPotHelpText"))
		return ctx, nil
	}

	// Last argument is amount, everything in between is pot name
	amountStr := arguments[len(arguments)-1]
	potName := strings.Join(arguments[1:len(arguments)-1], " ")

	amount, err := getAmount(ctx, amountStr)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ Invalid amount: %s", err.Error()))
		return ctx, err
	}

	if amount <= 0 {
		bot.trySendMessage(ctx.Sender(), "❌ Amount must be positive")
		return ctx, nil
	}

	err = bot.WithdrawFromPot(user, potName, amount)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ Withdrawal failed: %s", err.Error()))
		return ctx, err
	}

	bot.trySendMessage(ctx.Sender(), fmt.Sprintf("✅ Withdrew %s sats from pot '%s'", utils.FormatSats(amount), potName))
	return ctx, nil
}

func (bot *TipBot) deletePotHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)

	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	arguments := strings.Fields(m.Text)
	if len(arguments) < 2 {
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "deletePotHelpText"))
		return ctx, nil
	}

	// Everything after the command is the pot name
	potName := strings.TrimSpace(strings.Join(arguments[1:], " "))

	err := bot.DeletePot(user, potName)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), fmt.Sprintf("❌ Failed to delete pot: %s", err.Error()))
		return ctx, err
	}

	bot.trySendMessage(ctx.Sender(), fmt.Sprintf("✅ Deleted pot '%s'", potName))
	return ctx, nil
}

func getAmount(ctx context.Context, amountStr string) (int64, error) {
	return GetAmount(amountStr)
}
