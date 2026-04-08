package telegram

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/boltz"
	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/i18n"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/tidwall/buntdb"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

var (
	swapConfirmationMenu = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnCancelSwap        = swapConfirmationMenu.Data("🚫 Cancel", "cancel_swap")
	btnConfirmSwap       = swapConfirmationMenu.Data("✅ Confirm Swap", "confirm_swap")
)

// boltzClient is the global Boltz API client, initialized when Boltz is enabled.
var boltzClient *boltz.Client

// initBoltzClient creates the Boltz client from configuration. Called from bot Start().
func initBoltzClient() {
	if !internal.IsBoltzEnabled() {
		return
	}
	cfg := internal.Configuration.Boltz
	boltzClient = boltz.NewClient(cfg.APIURL, cfg.APIKey, cfg.APISecret)
	log.Infof("[boltz] Client initialized (url: %s)", cfg.APIURL)
}

// SwapData is the BuntDB-persisted record for one pending swap.
// Key is always "swap:<Base.ID>" so that we can look it up from the webhook URL.
type SwapData struct {
	*storage.Base
	From            *lnbits.User `json:"from"`
	SwapID          string       `json:"swap_id"`           // Boltz-assigned swap ID
	Invoice         string       `json:"invoice"`
	PreimageHashHex string       `json:"preimage_hash"`     // SHA-256(preimage), safe to persist
	AmountSat       int64        `json:"amount_sat"`
	USDTAddress     string       `json:"usdt_address"`
	State           string       `json:"state"`
	ExpiresAt       int64        `json:"expires_at"`
	LanguageCode    string       `json:"languagecode"`
	TelegramMessage *tb.Message  `json:"telegram_message,omitempty"`
}

// Key implements storage.Storable. Always keyed on Base.ID.
func (s *SwapData) Key() string {
	return fmt.Sprintf("swap:%s", s.Base.ID)
}

// validateUSDTAddress performs a basic sanity check on the destination address.
func validateUSDTAddress(addr string) bool {
	addr = strings.TrimSpace(addr)
	if len(addr) == 0 {
		return false
	}
	// Tron (TRC20): starts with T, 34 chars, base58 characters
	if strings.HasPrefix(addr, "T") && len(addr) == 34 {
		return true
	}
	// Ethereum / Arbitrum / EVM (ERC20): 0x prefix, 42 chars
	if strings.HasPrefix(addr, "0x") && len(addr) == 42 {
		return true
	}
	return false
}

// swapHandler is invoked on "/swap <amount> <address>" command.
func (bot *TipBot) swapHandler(ctx intercept.Context) (intercept.Context, error) {
	bot.anyTextHandler(ctx)
	if !internal.IsBoltzEnabled() {
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "swapDisabledMessage"))
		return ctx, errors.Create(errors.UnknownError)
	}

	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	m := ctx.Message()
	if m.Chat.Type != tb.ChatPrivate {
		bot.tryDeleteMessage(m)
		return ctx, errors.Create(errors.NoPrivateChatError)
	}

	// Parse: /swap <amount> [<address>]
	args := strings.Fields(m.Text)
	if len(args) < 2 {
		bot.trySendMessage(ctx.Sender(), helpSwapUsage(ctx, ""))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	amount, err := GetAmount(args[1])
	if err != nil || amount < 1 {
		bot.trySendMessage(ctx.Sender(), helpSwapUsage(ctx, Translate(ctx, "swapInvalidAmountMessage")))
		return ctx, errors.Create(errors.InvalidAmountError)
	}

	cfg := internal.Configuration.Boltz
	if amount < cfg.MinSwapSat || amount > cfg.MaxSwapSat {
		bot.trySendMessage(ctx.Sender(),
			fmt.Sprintf(Translate(ctx, "swapAmountOutOfRangeMessage"), cfg.MinSwapSat, cfg.MaxSwapSat))
		return ctx, errors.Create(errors.InvalidAmountError)
	}

	// No address yet — ask for it
	if len(args) < 3 {
		localID := fmt.Sprintf("%d-%d-%s", ctx.Sender().ID, amount, RandStringRunes(5))
		swapData := &SwapData{
			Base:         storage.New(storage.ID(localID)),
			From:         user,
			AmountSat:    amount,
			LanguageCode: ctx.Value("publicLanguageCode").(string),
			State:        "pending",
		}
		runtime.IgnoreError(swapData.Set(swapData, bot.Bunt))
		SetUserState(user, bot, lnbits.UserStateSwapEnterAddress, localID)
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "swapEnterAddressMessage"), tb.ForceReply)
		return ctx, nil
	}

	address := strings.TrimSpace(args[2])
	return bot.processSwapConfirmation(ctx, user, amount, address)
}

// enterSwapAddressHandler is invoked from the state machine when the user submits
// their USDT address after being prompted.
func (bot *TipBot) enterSwapAddressHandler(ctx intercept.Context) (intercept.Context, error) {
	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}
	if user.StateKey != lnbits.UserStateSwapEnterAddress {
		ResetUserState(user, bot)
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	localID := user.StateData
	swapData := &SwapData{Base: storage.New(storage.ID(localID))}
	sn, err := swapData.Get(swapData, bot.Bunt)
	if err != nil {
		ResetUserState(user, bot)
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "errorTryLaterMessage"))
		return ctx, err
	}
	sd := sn.(*SwapData)

	address := strings.TrimSpace(ctx.Message().Text)
	return bot.processSwapConfirmation(ctx, user, sd.AmountSat, address)
}

// processSwapConfirmation validates inputs and shows the confirmation keyboard.
func (bot *TipBot) processSwapConfirmation(ctx intercept.Context, user *lnbits.User, amountSat int64, address string) (intercept.Context, error) {
	if !validateUSDTAddress(address) {
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "swapInvalidAddressMessage"))
		ResetUserState(user, bot)
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	// Balance check with 1% fee reserve
	balance, err := bot.GetUserBalance(user)
	if err != nil {
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "errorTryLaterMessage"))
		return ctx, err
	}
	required := amountSat + int64(float64(amountSat)*0.01)
	if balance < required {
		bot.trySendMessage(ctx.Sender(),
			fmt.Sprintf(Translate(ctx, "insufficientFundsMessage"), amountSat, balance))
		return ctx, errors.Create(errors.InvalidAmountError)
	}

	confirmText := buildSwapConfirmText(ctx, amountSat, address)

	localID := fmt.Sprintf("%d-%d-%s", ctx.Sender().ID, amountSat, RandStringRunes(5))

	confirmButton := swapConfirmationMenu.Data(Translate(ctx, "swapConfirmButtonMessage"), "confirm_swap", localID)
	cancelButton := swapConfirmationMenu.Data(Translate(ctx, "cancelButtonMessage"), "cancel_swap", localID)
	swapConfirmationMenu.Inline(swapConfirmationMenu.Row(confirmButton, cancelButton))

	msg := bot.trySendMessageEditable(ctx.Sender(), confirmText, swapConfirmationMenu)

	swapData := &SwapData{
		Base:            storage.New(storage.ID(localID)),
		From:            user,
		AmountSat:       amountSat,
		USDTAddress:     address,
		LanguageCode:    ctx.Value("publicLanguageCode").(string),
		State:           "pending",
		TelegramMessage: msg,
	}
	runtime.IgnoreError(swapData.Set(swapData, bot.Bunt))
	SetUserState(user, bot, lnbits.UserStateConfirmSwap, localID)
	return ctx, nil
}

// buildSwapConfirmText produces the confirmation message with best-effort fee info.
func buildSwapConfirmText(ctx intercept.Context, amountSat int64, address string) string {
	if boltzClient != nil {
		pairs, err := boltzClient.GetPairs()
		if err == nil {
			if info, ok := pairs.Reverse["BTC/USDT"]; ok {
				feeSat := int64(float64(amountSat) * info.Fees.Percentage / 100)
				if info.Rate > 0 {
					netSat := amountSat - feeSat
					usdtApprox := float64(netSat) / 100_000_000 * info.Rate
					return fmt.Sprintf(Translate(ctx, "swapConfirmMessage"),
						amountSat, usdtApprox, address, feeSat)
				}
			}
		}
	}
	return fmt.Sprintf(Translate(ctx, "swapConfirmSimpleMessage"), amountSat, address)
}

// helpSwapUsage returns the usage help message for /swap.
func helpSwapUsage(ctx intercept.Context, errormsg string) string {
	if len(errormsg) > 0 {
		return fmt.Sprintf(Translate(ctx, "swapHelpText"), errormsg)
	}
	return fmt.Sprintf(Translate(ctx, "swapHelpText"), "")
}

// confirmSwapHandler is triggered when the user clicks "✅ Confirm Swap".
func (bot *TipBot) confirmSwapHandler(ctx intercept.Context) (intercept.Context, error) {
	localID := ctx.Data()
	tx := &SwapData{Base: storage.New(storage.ID(localID))}
	mutex.LockWithContext(ctx, tx.Key())
	defer mutex.UnlockWithContext(ctx, tx.Key())

	sn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		log.Errorf("[confirmSwapHandler] load SwapData %s: %v", localID, err)
		return ctx, err
	}
	sd := sn.(*SwapData)

	if sd.From.Telegram.ID != ctx.Sender().ID {
		return ctx, errors.Create(errors.UnknownError)
	}
	if !sd.Active {
		bot.tryEditMessage(ctx.Message(), i18n.Translate(sd.LanguageCode, "errorTryLaterMessage"), &tb.ReplyMarkup{})
		return ctx, errors.Create(errors.NotActiveError)
	}

	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}
	ResetUserState(user, bot)

	bot.tryEditMessage(ctx.Message(), i18n.Translate(sd.LanguageCode, "swapPayingMessage"), &tb.ReplyMarkup{})

	// Generate cryptographically random preimage
	preimageBytes := make([]byte, 32)
	if _, err := rand.Read(preimageBytes); err != nil {
		log.Errorf("[confirmSwapHandler] rand.Read: %v", err)
		bot.trySendMessage(ctx.Sender(), i18n.Translate(sd.LanguageCode, "errorTryLaterMessage"))
		return ctx, err
	}
	hashBytes := sha256.Sum256(preimageBytes)
	preimageHashHex := hex.EncodeToString(hashBytes[:])

	// Webhook URL encodes the local ID so the webhook handler can look up the SwapData
	webhookURL := buildBoltzWebhookURL(localID)

	swapResp, err := boltzClient.CreateReverseSwap(boltz.ReverseSwapRequest{
		From:          "BTC",
		To:            "USDT",
		Address:       sd.USDTAddress,
		InvoiceAmount: sd.AmountSat,
		PreimageHash:  preimageHashHex,
		CallbackURL:   webhookURL,
	})
	if err != nil {
		log.Errorf("[confirmSwapHandler] CreateReverseSwap: %v", err)
		if bot.ErrorLogger != nil {
			bot.ErrorLogger.LogError(err, "Boltz CreateReverseSwap", user.Telegram)
		}
		bot.tryEditMessage(ctx.Message(),
			fmt.Sprintf(i18n.Translate(sd.LanguageCode, "swapFailedMessage"), err.Error()), &tb.ReplyMarkup{})
		runtime.IgnoreError(sd.Inactivate(sd, bot.Bunt))
		return ctx, err
	}

	sd.SwapID = swapResp.ID
	sd.Invoice = swapResp.Invoice
	sd.PreimageHashHex = preimageHashHex
	sd.ExpiresAt = swapResp.ExpiresAt
	sd.State = boltz.StateCreated
	runtime.IgnoreError(sd.Set(sd, bot.Bunt))

	log.Infof("[swap] paying invoice for swap %s (preimageHash: %s)", sd.SwapID, sd.PreimageHashHex)
	_, err = user.Wallet.Pay(lnbits.PaymentParams{Out: true, Bolt11: sd.Invoice}, bot.Client)
	if err != nil {
		log.Errorf("[confirmSwapHandler] Pay invoice for swap %s: %v", sd.SwapID, err)
		if bot.ErrorLogger != nil {
			bot.ErrorLogger.LogPaymentError(err, sd.AmountSat, "Boltz swap "+sd.SwapID, sd.Invoice, user.Telegram)
		}
		bot.tryEditMessage(ctx.Message(),
			fmt.Sprintf(i18n.Translate(sd.LanguageCode, "swapFailedMessage"), err.Error()), &tb.ReplyMarkup{})
		sd.State = "failed"
		runtime.IgnoreError(sd.Inactivate(sd, bot.Bunt))
		return ctx, err
	}

	sd.State = boltz.StateInvoicePaid
	runtime.IgnoreError(sd.Set(sd, bot.Bunt))
	bot.tryEditMessage(ctx.Message(), i18n.Translate(sd.LanguageCode, "swapWaitingOnChainMessage"), &tb.ReplyMarkup{})

	// Polling goroutine as fallback in case the webhook is not delivered
	go bot.pollSwapUntilDone(localID)

	return ctx, nil
}

// cancelSwapHandler is triggered when the user clicks "🚫 Cancel".
func (bot *TipBot) cancelSwapHandler(ctx intercept.Context) (intercept.Context, error) {
	user := LoadUser(ctx)
	ResetUserState(user, bot)

	localID := ctx.Data()
	tx := &SwapData{Base: storage.New(storage.ID(localID))}
	mutex.LockWithContext(ctx, tx.Key())
	defer mutex.UnlockWithContext(ctx, tx.Key())

	sn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		return ctx, err
	}
	sd := sn.(*SwapData)

	if sd.From.Telegram.ID != ctx.Sender().ID {
		return ctx, errors.Create(errors.UnknownError)
	}
	bot.tryDeleteMessage(ctx.Message())
	bot.trySendMessage(ctx.Sender(), i18n.Translate(sd.LanguageCode, "swapCancelledMessage"))
	return ctx, sd.Inactivate(sd, bot.Bunt)
}

// buildBoltzWebhookURL constructs the callback URL Boltz POSTs swap updates to.
// The local swap ID is embedded as a query parameter; a short HMAC token is also
// included to guard against unauthenticated callback deliveries.
func buildBoltzWebhookURL(localID string) string {
	webhookBase := internal.GetWebhookURL()
	path := internal.Configuration.Boltz.WebhookPath
	h := sha256.Sum256([]byte(localID + internal.Configuration.Boltz.APISecret))
	token := hex.EncodeToString(h[:8])
	return fmt.Sprintf("%s%s?id=%s&token=%s", webhookBase, path, localID, token)
}

// VerifyBoltzWebhookToken validates the token query parameter on an incoming callback.
func VerifyBoltzWebhookToken(localID, token string) bool {
	h := sha256.Sum256([]byte(localID + internal.Configuration.Boltz.APISecret))
	expected := hex.EncodeToString(h[:8])
	return token == expected
}

// HandleBoltzWebhook processes a swap status update POSTed by Boltz.
// Called by the webhook HTTP handler.
func (bot *TipBot) HandleBoltzWebhook(localID string, payload boltz.BoltzWebhookPayload) {
	if localID == "" {
		log.Warn("[boltz webhook] missing local swap ID")
		return
	}

	key := fmt.Sprintf("swap:%s", localID)
	mutex.Lock(key)
	defer mutex.Unlock(key)

	sd := &SwapData{Base: storage.New(storage.ID(localID))}
	sn, err := sd.Get(sd, bot.Bunt)
	if err != nil {
		log.Errorf("[boltz webhook] SwapData not found for local ID %s: %v", localID, err)
		return
	}
	sd = sn.(*SwapData)

	if !sd.Active {
		log.Debugf("[boltz webhook] swap %s already finalised, ignoring callback", localID)
		return
	}

	bot.applySwapState(sd, payload.State, payload.Error)
}

// applySwapState updates persisted state and notifies the user.
func (bot *TipBot) applySwapState(sd *SwapData, state, apiErr string) {
	sd.State = state
	lang := sd.LanguageCode
	if lang == "" {
		lang = "en"
	}

	switch state {
	case boltz.StateTransactionConfirmed:
		bot.trySendMessage(sd.From.Telegram,
			fmt.Sprintf(i18n.Translate(lang, "swapCompleteMessage"), sd.USDTAddress))
		runtime.IgnoreError(sd.Inactivate(sd, bot.Bunt))
		log.Infof("[boltz] swap %s complete — USDT sent to %s", sd.SwapID, sd.USDTAddress)

	case boltz.StateSwapExpired, boltz.StateInvoiceExpired:
		bot.trySendMessage(sd.From.Telegram, i18n.Translate(lang, "swapExpiredMessage"))
		runtime.IgnoreError(sd.Inactivate(sd, bot.Bunt))
		log.Infof("[boltz] swap %s expired", sd.SwapID)

	case boltz.StateTransactionFailed:
		msg := apiErr
		if msg == "" {
			msg = "on-chain transaction failed"
		}
		bot.trySendMessage(sd.From.Telegram,
			fmt.Sprintf(i18n.Translate(lang, "swapFailedMessage"), msg))
		runtime.IgnoreError(sd.Inactivate(sd, bot.Bunt))
		log.Warnf("[boltz] swap %s failed: %s", sd.SwapID, msg)

	default:
		runtime.IgnoreError(sd.Set(sd, bot.Bunt))
		log.Debugf("[boltz] swap %s state → %s", sd.SwapID, state)
	}
}

// pollSwapUntilDone polls Boltz for the swap status until a terminal state or timeout.
func (bot *TipBot) pollSwapUntilDone(localID string) {
	const interval = 30 * time.Second
	const maxDuration = 24 * time.Hour

	deadline := time.Now().Add(maxDuration)

	// Read expiry from persisted state
	sdCheck := &SwapData{Base: storage.New(storage.ID(localID))}
	if sn, err := sdCheck.Get(sdCheck, bot.Bunt); err == nil {
		if fresh := sn.(*SwapData); fresh.ExpiresAt > 0 {
			exp := time.Unix(fresh.ExpiresAt, 0).Add(5 * time.Minute)
			if exp.Before(deadline) {
				deadline = exp
			}
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	key := fmt.Sprintf("swap:%s", localID)

	for range ticker.C {
		if time.Now().After(deadline) {
			log.Infof("[boltz poller] swap local:%s deadline reached", localID)
			mutex.Lock(key)
			sdExp := &SwapData{Base: storage.New(storage.ID(localID))}
			if sn, err := sdExp.Get(sdExp, bot.Bunt); err == nil {
				bot.applySwapState(sn.(*SwapData), boltz.StateSwapExpired, "")
			}
			mutex.Unlock(key)
			return
		}

		// Re-read SwapID from BuntDB in case it was set after initial creation
		sdCurrent := &SwapData{Base: storage.New(storage.ID(localID))}
		sn, err := sdCurrent.Get(sdCurrent, bot.Bunt)
		if err != nil {
			log.Warnf("[boltz poller] cannot load swap local:%s: %v", localID, err)
			continue
		}
		sd := sn.(*SwapData)

		if !sd.Active {
			return // webhook already finalised this swap
		}
		if sd.SwapID == "" {
			continue // swap not yet created
		}

		status, err := boltzClient.GetSwapStatus(sd.SwapID)
		if err != nil {
			log.Warnf("[boltz poller] GetSwapStatus %s: %v", sd.SwapID, err)
			continue
		}

		mutex.Lock(key)
		// Re-load to guard against concurrent webhook delivery
		sdFresh := &SwapData{Base: storage.New(storage.ID(localID))}
		if freshSn, dbErr := sdFresh.Get(sdFresh, bot.Bunt); dbErr == nil {
			sdFresh = freshSn.(*SwapData)
		}
		if sdFresh.Active {
			bot.applySwapState(sdFresh, status.State, status.Error)
		}
		mutex.Unlock(key)

		switch status.State {
		case boltz.StateTransactionConfirmed,
			boltz.StateSwapExpired,
			boltz.StateInvoiceExpired,
			boltz.StateTransactionFailed:
			return
		}
	}
}

// ResumePendingSwaps restores polling goroutines for swaps in-flight at restart.
func (bot *TipBot) ResumePendingSwaps() {
	if !internal.IsBoltzEnabled() || boltzClient == nil {
		return
	}
	log.Info("[boltz] scanning BuntDB for pending swaps to resume...")
	count := 0
	_ = bot.Bunt.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys("swap:*", func(key, value string) bool {
			var sd SwapData
			if err := json.Unmarshal([]byte(value), &sd); err != nil {
				return true
			}
			if sd.Base == nil {
				return true
			}
			if sd.Active && sd.SwapID != "" &&
				sd.State != boltz.StateTransactionConfirmed {
				go bot.pollSwapUntilDone(sd.Base.ID)
				count++
			}
			return true
		})
	})
log.Infof("[boltz] resumed %d pending swap(s)", count)
}
