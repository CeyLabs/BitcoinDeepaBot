package main

import (
	"net"
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
	internal.CheckLnbitsConfiguration()

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
	// api.allowed_networks applies to every /api/v1/ route on this server.
	s.Use(api.PathAllowlistMiddleware("/api/v1/", func() []*net.IPNet { return internal.Configuration.API.AllowedNets }))

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
	api.RegisterUpdatePendingTxHook(bot)
	p2p := api.NewP2PService(bot)
	s.AppendAuthorizedRoute(`/api/v1/paymentstatus/{payment_hash}`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.PaymentStatus, http.MethodPost)
	s.AppendAuthorizedRoute(`/api/v1/invoicestatus/{payment_hash}`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.InvoiceStatus, http.MethodPost)
	s.AppendAuthorizedRoute(`/api/v1/payinvoice`, api.AuthTypeBasic, api.AccessKeyTypeAdmin, bot.DB.Users, apiService.PayInvoice, http.MethodPost)
	s.AppendAuthorizedRoute(`/api/v1/invoicestream`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.InvoiceStream, http.MethodGet)
	s.AppendAuthorizedRoute(`/api/v1/createinvoice`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.CreateInvoice, http.MethodPost)
	s.AppendAuthorizedRoute(`/api/v1/balance`, api.AuthTypeBasic, api.AccessKeyTypeInvoice, bot.DB.Users, apiService.Balance, http.MethodGet)

	// Analytics API endpoints (Ed25519 signed)
	if internal.IsAPIAnalyticsEnabled() {
		s.AppendRoute(`/api/v1/analytics/transactions`, api.AnalyticsAuthMiddleware(apiService.GetTransactionAnalytics), http.MethodGet)
		s.AppendRoute(`/api/v1/analytics/user/{user_id}/transactions`, api.AnalyticsAuthMiddleware(apiService.GetUserTransactionHistory), http.MethodGet)
		log.Infof("Analytics API endpoints registered with Ed25519 signatures")
	} else {
		log.Infof("Analytics API endpoints disabled in configuration")
	}

	// Bot pay HTTP API module with wallet Ed25519 signatures (only if enabled)
	if internal.IsAPISendEnabled() {
		s.AppendRoute(`/api/v1/send`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(apiService.Send)), http.MethodPost)
		log.Infof("API Send endpoint registered at /api/v1/send with wallet Ed25519 signatures")

		// User balance endpoint with wallet Ed25519 signatures
		s.AppendRoute(`/api/v1/userbalance`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(apiService.UserBalance)), http.MethodPost)
		log.Infof("API UserBalance endpoint registered at /api/v1/userbalance with wallet Ed25519 signatures")

		// User transactions endpoint with wallet Ed25519 signatures
		s.AppendRoute(`/api/v1/usertransactions`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(apiService.UserTransactions)), http.MethodPost)
		log.Infof("API UserTransactions endpoint registered at /api/v1/usertransactions with wallet Ed25519 signatures")

		// Referral lookup endpoint with wallet Ed25519 signatures
		s.AppendRoute(`/api/v1/referral/lookup`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(apiService.ReferralLookup)), http.MethodGet)
		log.Infof("API ReferralLookup endpoint registered at /api/v1/referral/lookup with wallet Ed25519 signatures")

		// Send status endpoint — check approval status of a pending transaction
		s.AppendRoute(`/api/v1/send/status/{transaction_id}`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(apiService.SendStatus)), http.MethodGet)
		log.Infof("API SendStatus endpoint registered at /api/v1/send/status/{transaction_id} with wallet Ed25519 signatures")

		// Payment settlement endpoint — resolve an invoice payment left in flight
		s.AppendRoute(`/api/v1/send/payment/{payment_hash}`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(apiService.SendPaymentStatus)), http.MethodGet)
		log.Infof("API SendPaymentStatus endpoint registered at /api/v1/send/payment/{payment_hash} with wallet Ed25519 signatures")

		// Collect: ask a user to pay a wallet with can_collect. The user approves each request.
		s.AppendRoute(`/api/v1/collect`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(p2p.Collect)), http.MethodPost)
		s.AppendRoute(`/api/v1/collect/{id}`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(p2p.CollectStatus)), http.MethodGet)
		s.AppendRoute(`/api/v1/collect/{id}/cancel`, api.NetworkAllowlistMiddleware(api.WalletAuthMiddleware(p2p.CollectCancel)), http.MethodPost)
		log.Infof("API Collect endpoints registered at /api/v1/collect with wallet Ed25519 signatures")
	} else {
		log.Infof("API Send endpoint disabled in configuration")
	}
	s.ListenAndServe()

	// Telegram Mini App sending. The sender is the user in the signed init data.
	if internal.Configuration.API.P2P.Enabled {
		s.AppendRoute(`/api/v1/init`, p2p.AuthNewUser(p2p.Init), http.MethodPost)
		s.AppendRoute(`/api/v1/p2p/me`, p2p.Auth(p2p.Me), http.MethodGet)
		s.AppendRoute(`/api/v1/p2p/resolve`, p2p.Auth(p2p.Resolve), http.MethodPost)
		s.AppendRoute(`/api/v1/p2p/send`, p2p.AuthSpend(p2p.Send), http.MethodPost)
		s.AppendRoute(`/api/v1/p2p/invoice/decode`, p2p.Auth(p2p.DecodeInvoice), http.MethodPost)
		s.AppendRoute(`/api/v1/p2p/invoice/pay`, p2p.AuthSpend(p2p.PayInvoice), http.MethodPost)
		s.AppendRoute(`/api/v1/p2p/transfers/{id}`, p2p.Auth(p2p.Transfer), http.MethodGet)
		s.AppendRoute(`/api/v1/p2p/collect/{id}`, p2p.Auth(p2p.MiniAppCollect), http.MethodGet)
		s.AppendRoute(`/api/v1/p2p/collect/{id}/approve`, p2p.AuthSpend(p2p.MiniAppCollectApprove), http.MethodPost)
		s.AppendRoute(`/api/v1/p2p/collect/{id}/decline`, p2p.Auth(p2p.MiniAppCollectDecline), http.MethodPost)
		log.Infof("P2P API endpoints registered at /api/v1/p2p/*")
	}
	// Start serving only after every middleware and route is added.
	s.ListenAndServe()

	// start internal admin server
	adminHost, adminToken := internal.Configuration.Bot.AdminAPIHost, internal.Configuration.Bot.AdminAPIToken
	if err := internal.AdminAPIAllowed(adminHost, adminToken); err != nil {
		log.Errorf("[admin] Admin API not started: %v", err)
		return
	}
	adminService := admin.New(bot)
	internalAdminServer := api.NewServer(adminHost)
	internalAdminServer.Use(api.BearerTokenMiddleware(adminToken))
	internalAdminServer.AppendRoute("/mutex", mutex.ServeHTTP)
	internalAdminServer.AppendRoute("/mutex/unlock/{id}", mutex.UnlockHTTP)
	internalAdminServer.AppendRoute("/admin/ban/{id}", adminService.BanUser)
	internalAdminServer.AppendRoute("/admin/unban/{id}", adminService.UnbanUser)
	internalAdminServer.AppendRoute("/admin/dalle/enable", adminService.EnableDalle)
	internalAdminServer.AppendRoute("/admin/dalle/disable", adminService.DisableDalle)
	if internal.Configuration.Bot.AdminPprof {
		internalAdminServer.PathPrefix("/debug/pprof/", http.DefaultServeMux)
		log.Warnf("[admin] pprof enabled at %s/debug/pprof/", adminHost)
	}
	internalAdminServer.ListenAndServe()
	if adminToken == "" {
		log.Warnf("[admin] Admin API on %s has no token; set bot.admin_api_token", adminHost)
	}
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
