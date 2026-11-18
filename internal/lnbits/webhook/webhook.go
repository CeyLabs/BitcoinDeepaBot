package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/dca"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"net/http"

	"github.com/LightningTipBot/LightningTipBot/internal/storage"

	"github.com/gorilla/mux"
	tb "gopkg.in/lightningtipbot/telebot.v3"

	"github.com/LightningTipBot/LightningTipBot/internal/i18n"
)

type Server struct {
	httpServer *http.Server
	bot        *tb.Bot
	c          *lnbits.Client
	database   *gorm.DB
	buntdb     *storage.DB
}

type Webhook struct {
	CheckingID    string          `json:"checking_id"`
	Pending       bool            `json:"pending"`
	Amount        int64           `json:"amount"`
	Fee           int64           `json:"fee"`
	Memo          string          `json:"memo"`
	Time          lnbits.UnixTime `json:"time"`
	Bolt11        string          `json:"bolt11"`
	Preimage      string          `json:"preimage"`
	PaymentHash   string          `json:"payment_hash"`
	Extra         struct{}        `json:"extra"`
	WalletID      string          `json:"wallet_id"`
	Webhook       string          `json:"webhook"`
	WebhookStatus interface{}     `json:"webhook_status"`
}

func NewServer(bot *telegram.TipBot) *Server {
	srv := &http.Server{
		Addr:         internal.Configuration.Lnbits.WebhookServerUrl.Host,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}
	apiServer := &Server{
		c:          bot.Client,
		database:   bot.DB.Users,
		bot:        bot.Telegram,
		httpServer: srv,
		buntdb:     bot.Bunt,
	}
	apiServer.httpServer.Handler = apiServer.newRouter()
	go apiServer.httpServer.ListenAndServe()
	log.Infof("[Webhook] Server started at %s (public URL: %s)", internal.Configuration.Lnbits.WebhookServerUrl, internal.GetWebhookURLRedacted())
	return apiServer
}

func (w *Server) GetUserByWalletId(walletId string) (*lnbits.User, error) {
	user := &lnbits.User{}
	tx := w.database.Where("wallet_id = ?", walletId).First(user)
	if tx.Error != nil {
		return user, tx.Error
	}
	return user, nil
}

func (w *Server) newRouter() *mux.Router {
	router := mux.NewRouter()
	router.HandleFunc("/", w.receive).Methods(http.MethodPost)
	return router
}

const (
	// maxWebhookBody bounds what an unauthenticated caller can make us read.
	maxWebhookBody = 64 << 10
	// processedTTL is how long a handled payment hash is remembered, so a
	// replayed webhook cannot trigger its callback a second time.
	processedTTL = 90 * 24 * time.Hour
	// verifyBudget bounds how long payment verification may keep retrying.
	verifyBudget = 8 * time.Second
)

// receive handles a payment webhook from lnbits.
//
// Nothing in the request is trusted. Anyone who can reach this port can post a
// body that names a wallet and a payment hash, so the payment is confirmed with
// lnbits before anything happens, the amount lnbits reports is used instead of
// the one in the body, and each payment hash is acted on once.
func (w *Server) receive(writer http.ResponseWriter, request *http.Request) {
	if !internal.WebhookSecretMatches(request.URL.Query().Get(internal.WebhookSecretParam)) {
		log.Warnf("[Webhook] Rejected request without a valid secret from %s", request.RemoteAddr)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	log.Debugln("[Webhook] Received request")
	request.Body = http.MaxBytesReader(writer, request.Body, maxWebhookBody)
	webhookEvent := Webhook{}
	// need to delete the header otherwise the Decode will fail
	request.Header.Del("content-length")
	err := json.NewDecoder(request.Body).Decode(&webhookEvent)
	if err != nil {
		log.Errorf("[Webhook] Error decoding request: %s", err.Error())
		writer.WriteHeader(400)
		return
	}
	if webhookEvent.WalletID == "" || !lnbits.ValidPaymentHash(webhookEvent.PaymentHash) {
		log.Warnf("[Webhook] Rejected request with missing wallet id or malformed payment hash from %s", request.RemoteAddr)
		writer.WriteHeader(400)
		return
	}
	user, err := w.GetUserByWalletId(webhookEvent.WalletID)
	if err != nil || user.Wallet == nil {
		log.Errorf("[Webhook] Error getting user for wallet: %v", err)
		writer.WriteHeader(400)
		return
	}

	// Confirm with lnbits, using the receiving wallet's own key.
	// Retried briefly on transport errors: lnbits never resends a failed
	// webhook. The budget stays under the server's 15 s write timeout.
	amountMsat, err := w.c.VerifyIncomingPaymentWithRetry(*user.Wallet, webhookEvent.PaymentHash, verifyBudget)
	if err != nil {
		if errors.Is(err, lnbits.ErrPaymentNotVerified) {
			log.Warnf("[Webhook] Rejected unverified payment %s for user %d from %s: %v", webhookEvent.PaymentHash, user.Telegram.ID, request.RemoteAddr, err)
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		log.Errorf("[Webhook] Could not verify payment %s with lnbits: %v", webhookEvent.PaymentHash, err)
		writer.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	claimed, err := w.buntdb.ClaimOnce("webhook-processed:"+webhookEvent.PaymentHash, processedTTL)
	if err != nil {
		log.Errorf("[Webhook] Could not record payment %s: %v", webhookEvent.PaymentHash, err)
		writer.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !claimed {
		log.Infof("[Webhook] Ignoring repeated webhook for payment %s", webhookEvent.PaymentHash)
		writer.WriteHeader(200)
		return
	}
	log.Infoln(fmt.Sprintf("[⚡️ WebHook] User %s (%d) received invoice of %d sat.", telegram.GetUserStr(user.Telegram), user.Telegram.ID, amountMsat/1000))

	go dca.NotifyDeposit(telegram.GetUserStr(user.Telegram))

	writer.WriteHeader(200)

	// trigger invoice events
	txInvoiceEvent := &telegram.InvoiceEvent{Invoice: &telegram.Invoice{PaymentHash: webhookEvent.PaymentHash}}
	err = w.buntdb.Get(txInvoiceEvent)
	if err != nil {
		log.Errorln(err)
	} else {
		// do something with the event
		if c := telegram.InvoiceCallback[txInvoiceEvent.Callback]; c.Function != nil {
			if err := telegram.AssertEventType(txInvoiceEvent, c.Type); err != nil {
				log.Errorln(err)
				return
			}
			go c.Function(txInvoiceEvent)
			return
		}
	}

	// fallback: send a message to the user if there is no callback for this invoice
	_, err = w.bot.Send(user.Telegram, fmt.Sprintf(i18n.Translate(user.Telegram.LanguageCode, "invoiceReceivedMessage"), utils.FormatSats(amountMsat/1000)))
	if err != nil {
		log.Errorln(err)
	}
}
