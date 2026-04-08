package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/boltz"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
	"gorm.io/gorm"

	"github.com/LightningTipBot/LightningTipBot/internal/i18n"
)

type Server struct {
	httpServer *http.Server
	bot        *tb.Bot
	tipBot     *telegram.TipBot
	c          *lnbits.Client
	database   *gorm.DB
	buntdb     *storage.DB
}

type Webhook struct {
	CheckingID    string      `json:"checking_id"`
	Pending       bool        `json:"pending"`
	Amount        int64       `json:"amount"`
	Fee           int64       `json:"fee"`
	Memo          string      `json:"memo"`
	Time          int64       `json:"time"`
	Bolt11        string      `json:"bolt11"`
	Preimage      string      `json:"preimage"`
	PaymentHash   string      `json:"payment_hash"`
	Extra         struct{}    `json:"extra"`
	WalletID      string      `json:"wallet_id"`
	Webhook       string      `json:"webhook"`
	WebhookStatus interface{} `json:"webhook_status"`
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
		tipBot:     bot,
		httpServer: srv,
		buntdb:     bot.Bunt,
	}
	apiServer.httpServer.Handler = apiServer.newRouter()
	go apiServer.httpServer.ListenAndServe()
	log.Infof("[Webhook] Server started at %s (public URL: %s)", internal.Configuration.Lnbits.WebhookServerUrl, internal.GetWebhookURL())
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
	// Boltz swap status callbacks
	boltzPath := internal.Configuration.Boltz.WebhookPath
	if boltzPath == "" {
		boltzPath = "/boltz/webhook"
	}
	router.HandleFunc(boltzPath, w.receiveBoltz).Methods(http.MethodPost)
	return router
}

func (w *Server) receive(writer http.ResponseWriter, request *http.Request) {
	log.Debugln("[Webhook] Received request")
	webhookEvent := Webhook{}
	// need to delete the header otherwise the Decode will fail
	request.Header.Del("content-length")
	err := json.NewDecoder(request.Body).Decode(&webhookEvent)
	if err != nil {
		log.Errorf("[Webhook] Error decoding request: %s", err.Error())
		writer.WriteHeader(400)
		return
	}
	user, err := w.GetUserByWalletId(webhookEvent.WalletID)
	if err != nil {
		log.Errorf("[Webhook] Error getting user: %s", err.Error())
		writer.WriteHeader(400)
		return
	}
	log.Infoln(fmt.Sprintf("[⚡️ WebHook] User %s (%d) received invoice of %d sat.", telegram.GetUserStr(user.Telegram), user.Telegram.ID, webhookEvent.Amount/1000))

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
	_, err = w.bot.Send(user.Telegram, fmt.Sprintf(i18n.Translate(user.Telegram.LanguageCode, "invoiceReceivedMessage"), utils.FormatSats(webhookEvent.Amount/1000)))
	if err != nil {
		log.Errorln(err)
	}
}

// receiveBoltz handles Boltz swap status update callbacks (POST /boltz/webhook?id=<localID>&token=<hmac>).
func (w *Server) receiveBoltz(writer http.ResponseWriter, request *http.Request) {
	if !internal.IsBoltzEnabled() {
		writer.WriteHeader(http.StatusNotFound)
		return
	}

	localID := request.URL.Query().Get("id")
	token := request.URL.Query().Get("token")

	if localID == "" || token == "" {
		log.Warn("[Boltz webhook] missing id or token query parameters")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	if !telegram.VerifyBoltzWebhookToken(localID, token) {
		log.Warnf("[Boltz webhook] invalid token for swap local:%s", localID)
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}

	var payload boltz.BoltzWebhookPayload
	request.Header.Del("content-length")
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		log.Errorf("[Boltz webhook] decode payload: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Infof("[Boltz webhook] swap local:%s state:%s", localID, payload.State)
	writer.WriteHeader(http.StatusOK)

	// Dispatch asynchronously so the HTTP response is returned promptly
	go w.tipBot.HandleBoltzWebhook(localID, payload)
}

