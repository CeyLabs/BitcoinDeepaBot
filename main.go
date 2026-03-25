package main

import (
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/api"
	"github.com/LightningTipBot/LightningTipBot/internal/api/admin"
	"github.com/LightningTipBot/LightningTipBot/internal/api/userpage"
	"github.com/LightningTipBot/LightningTipBot/internal/lndhub"
	"github.com/LightningTipBot/LightningTipBot/internal/lnurl"
	"github.com/LightningTipBot/LightningTipBot/internal/nostr"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"

	_ "net/http/pprof"

	tb "gopkg.in/lightningtipbot/telebot.v3"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits/webhook"
	"github.com/LightningTipBot/LightningTipBot/internal/price"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
)

// setLogger will initialize the log format
func setLogger() {
	log.SetLevel(log.DebugLevel)
	customFormatter := new(log.TextFormatter)
	customFormatter.TimestampFormat = "2006-01-02 15:04:05"
	customFormatter.FullTimestamp = true
	log.SetFormatter(customFormatter)
}

func main() {
	// set logger
	setLogger()

	// Create bot first
	bot := telegram.NewBot()

	defer withRecovery(bot.ErrorLogger)
	price.NewPriceWatcher().Start()
	startApiServer(&bot)
	bot.Start()
}
func startApiServer(bot *telegram.TipBot) {
	// log errors from interceptors
	bot.Telegram.OnError = func(err error, ctx tb.Context) {
		if err == nil {
			return
		}

		errMsg := err.Error()

		// Filter out empty/ghost errors from telebot (code:0, empty message)
		if errMsg == "" || errMsg == `{"message":"","Err":{},"code":0}` {
			return
		}

		// Filter out annoying interceptor errors
		if strings.Contains(errMsg, "[requirePrivateChatInterceptor]") {
			return
		}

		// Log errors to Telegram group
		if bot.ErrorLogger != nil {
			userInfo := []interface{}{}
			if ctx != nil && ctx.Sender() != nil {
				userInfo = append(userInfo, ctx.Sender())
			}
			if ctx != nil && ctx.Chat() != nil {
				userInfo = append(userInfo, ctx.Chat())
			}
			bot.ErrorLogger.LogError(err, "Telegram Bot Error", userInfo...)
		}
	}
	// start internal webhook server
	webhook.NewServer(bot)
	// start external api server
	s := api.NewServer(internal.Configuration.Bot.LNURLServerUrl.Host)

	s.AppendRoute("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}, http.MethodGet)

	// append lnurl ctx functions
	lnUrl := lnurl.New(bot)
	s.AppendRoute("/.well-known/lnurlp/{username}", lnUrl.Handle, http.MethodGet)
	// userpage server
	userpage := userpage.New(bot)
	s.AppendRoute("/@{username}", userpage.UserPageHandler, http.MethodGet)
	s.AppendRoute("/app/@{username}", userpage.UserWebAppHandler, http.MethodGet)

	// nostr nip05 identifier
	nostr := nostr.New(bot)
	s.AppendRoute("/.well-known/nostr.json", nostr.Handle, http.MethodGet)

	// append lndhub ctx functions
	hub := lndhub.New(bot)
	s.AppendAuthorizedRoute(`/lndhub/ext/auth`, api.AuthTypeNone, api.AccessKeyTypeNone, bot.DB.Users, hub.Handle)
	s.AppendAuthorizedRoute(`/lndhub/ext/{.*}`, api.AuthTypeBearerBase64, api.AccessKeyTypeAdmin, bot.DB.Users, hub.Handle)
	s.AppendAuthorizedRoute(`/lndhub/ext`, api.AuthTypeBearerBase64, api.AccessKeyTypeAdmin, bot.DB.Users, hub.Handle)

	// starting api service
	apiService := api.Service{
		Bot:       bot,
		MemoCache: utils.NewCache(time.Minute * 5),
	}
	s.AppendAuthorizedRoute(`/api/v1/paymentstatus/{payment_hash}`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.PaymentStatus, http.MethodPost)
	s.AppendAuthorizedRoute(`/api/v1/invoicestatus/{payment_hash}`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.InvoiceStatus, http.MethodPost)
	s.AppendAuthorizedRoute(`/api/v1/payinvoice`, api.AuthTypeBasic, api.AccessKeyTypeAdmin, bot.DB.Users, apiService.PayInvoice, http.MethodPost)
	s.AppendAuthorizedRoute(`/api/v1/invoicestream`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.InvoiceStream, http.MethodGet)
	s.AppendAuthorizedRoute(`/api/v1/createinvoice`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.CreateInvoice, http.MethodPost)
	s.AppendAuthorizedRoute(`/api/v1/balance`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.Balance, http.MethodGet)

	// Bot pay HTTP API module with wallet-based HMAC security (only if enabled)
	if internal.IsAPISendEnabled() {
		s.AppendRoute(`/api/v1/send`, api.WalletHMACMiddleware(apiService.Send), http.MethodPost)
		log.Infof("API Send endpoint registered at /api/v1/send with wallet-based HMAC security")

		// User balance endpoint with wallet-based HMAC security
		s.AppendRoute(`/api/v1/userbalance`, api.WalletHMACMiddleware(apiService.UserBalance), http.MethodPost)
		log.Infof("API UserBalance endpoint registered at /api/v1/userbalance with wallet-based HMAC security")
	} else {
		log.Infof("API Send endpoint disabled in configuration")
	}

	// start internal admin server
	adminService := admin.New(bot)
	internalAdminServer := api.NewServer(internal.Configuration.Bot.AdminAPIHost)
	internalAdminServer.AppendRoute("/mutex", mutex.ServeHTTP)
	internalAdminServer.AppendRoute("/mutex/unlock/{id}", mutex.UnlockHTTP)
	internalAdminServer.AppendRoute("/admin/ban/{id}", adminService.BanUser)
	internalAdminServer.AppendRoute("/admin/unban/{id}", adminService.UnbanUser)
	internalAdminServer.AppendRoute("/admin/dalle/enable", adminService.EnableDalle)
	internalAdminServer.AppendRoute("/admin/dalle/disable", adminService.DisableDalle)
	internalAdminServer.PathPrefix("/debug/pprof/", http.DefaultServeMux)

}

func withRecovery(errorLogger *telegram.ErrorLogger) {
	if r := recover(); r != nil {
		log.Errorln("Recovered panic: ", r)
		debug.PrintStack()

		// Log to Telegram if error logger is available
		if errorLogger != nil {
			errorLogger.LogPanic(r, "Main Application")
		}
	}
}
