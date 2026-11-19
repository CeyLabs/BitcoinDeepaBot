package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/netutil"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/tgoidc"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/LightningTipBot/LightningTipBot/internal/tma"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// InitDataHeader carries Telegram.WebApp.initData, unchanged, on P2P calls
// from the Mini App.
const InitDataHeader = "X-Telegram-Init-Data"

// IDTokenHeader carries the "Log In with Telegram" ID token, unchanged, on
// P2P calls from the web app. A request carries this or InitDataHeader.
const IDTokenHeader = "X-Telegram-ID-Token"

// p2pInvoiceWait is how long a pay-invoice request waits for settlement
// before answering "in_flight" and leaving the client to poll.
const p2pInvoiceWait = 15 * time.Second

type p2pCtxKey struct{}

func p2pSender(r *http.Request) *lnbits.User {
	u, _ := r.Context().Value(p2pCtxKey{}).(*lnbits.User)
	return u
}

// P2PErrorBody is the JSON error body. Code is stable; Message is for the user.
type P2PErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func p2pWriteJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func p2pFail(w http.ResponseWriter, status int, code, msg string) {
	p2pWriteJSON(w, status, P2PErrorBody{Code: code, Message: msg})
}

// p2pWriteErr maps errors from the telegram layer to HTTP responses.
func p2pWriteErr(w http.ResponseWriter, err error) {
	var pe *telegram.P2PError
	if !errors.As(err, &pe) {
		log.Errorf("[api/p2p] %v", err)
		p2pFail(w, http.StatusInternalServerError, "internal_error", "Something went wrong. Nothing was sent; try again.")
		return
	}
	status := http.StatusBadRequest
	switch pe.Code {
	case "sender_not_found", "sender_not_allowed":
		status = http.StatusForbidden
	case "recipient_not_found":
		status = http.StatusNotFound
	case "idempotency_conflict", "in_progress", "request_closed":
		status = http.StatusConflict
	case "not_found":
		status = http.StatusNotFound
	case "rate_limited":
		status = http.StatusTooManyRequests
	case "collector_not_allowed":
		status = http.StatusForbidden
	}
	p2pFail(w, status, pe.Code, pe.Message)
}

// rateLimiter allows n events per window per key.
type rateLimiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	events map[string][]time.Time
}

func newRateLimiter(n int, window time.Duration) *rateLimiter {
	return &rateLimiter{n: n, window: window, events: map[string][]time.Time{}}
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	kept := l.events[key][:0]
	for _, t := range l.events[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.n {
		l.events[key] = kept
		return false
	}
	l.events[key] = append(kept, now)
	if len(l.events) > 10000 { // drop idle keys
		for k, ts := range l.events {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(l.events, k)
			}
		}
	}
	return true
}

// P2PService serves /api/v1/p2p/*.
type P2PService struct {
	Bot      *telegram.TipBot
	payRate  *rateLimiter // send + invoice pay
	readRate *rateLimiter // resolve + decode
	pollRate *rateLimiter // transfer status
}

// p2pPollsPerMinute caps GET /transfers/{id} per sender: one poll every 2 s.
const p2pPollsPerMinute = 30

func NewP2PService(bot *telegram.TipBot) *P2PService {
	cfg := internal.Configuration.API.P2P
	return &P2PService{
		Bot:      bot,
		payRate:  newRateLimiter(cfg.RateLimitPerMinute, time.Minute),
		readRate: newRateLimiter(cfg.RateLimitPerMinute*3, time.Minute),
		pollRate: newRateLimiter(p2pPollsPerMinute, time.Minute),
	}
}

// Auth checks the Mini App server's signature, then the Mini App init data,
// and loads the sender. Nothing else in the request can name or change the
// sender. Use it for read endpoints.
func (p *P2PService) Auth(next http.HandlerFunc) http.HandlerFunc {
	return p.auth(next, false)
}

// AuthSpend is Auth for endpoints that move sats. It accepts only fresh init
// data (spend_init_data_max_age), so leaked init data can't spend for long.
func (p *P2PService) AuthSpend(next http.HandlerFunc) http.HandlerFunc {
	return p.auth(next, true)
}

func (p *P2PService) auth(next http.HandlerFunc, spend bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := internal.Configuration.API.P2P
		now := time.Now()
		if len(cfg.AllowedNets) > 0 {
			ip := netutil.ClientIP(r, internal.Configuration.Bot.TrustedProxyNets)
			if !netutil.Contains(cfg.AllowedNets, ip) {
				log.Warnf("[api/p2p] Access denied for IP %v (not in api.p2p.allowed_networks)", ip)
				p2pFail(w, http.StatusForbidden, "forbidden", "Access denied.")
				return
			}
		}
		if err := verifyP2PServiceSig(w, r, cfg.ServiceKey, now); err != nil {
			log.Warnf("[api/p2p] Rejected service signature from %s: %v", getClientIP(r), err)
			switch {
			case errors.Is(err, errP2PBodyTooLarge):
				p2pFail(w, http.StatusRequestEntityTooLarge, "too_large", "Request too large.")
			case errors.Is(err, errP2PBodyRead):
				p2pFail(w, http.StatusBadRequest, "invalid_request", "Could not read the request.")
			default:
				p2pFail(w, http.StatusUnauthorized, "bad_service_signature", "Request could not be verified.")
			}
			return
		}
		maxAge := cfg.InitDataMaxAge
		if spend {
			maxAge = cfg.SpendInitDataMaxAge
		}
		telegramID, ok := p2pIdentify(w, r, cfg, time.Duration(maxAge)*time.Second, now)
		if !ok {
			return
		}
		sender, err := p.Bot.P2PSender(telegramID)
		if err != nil {
			p2pWriteErr(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), p2pCtxKey{}, sender)))
	}
}

// p2pIdentify returns the Telegram user a request is for, from exactly one
// Telegram-signed credential: Mini App init data, or a "Log In with Telegram"
// ID token from the web app. The login must be no older than maxAge. On
// failure it answers 401 and returns false.
func p2pIdentify(w http.ResponseWriter, r *http.Request, cfg internal.APIP2PConfiguration, maxAge time.Duration, now time.Time) (int64, bool) {
	initData, idToken := r.Header.Get(InitDataHeader), r.Header.Get(IDTokenHeader)
	switch {
	case initData != "" && idToken != "":
		log.Warnf("[api/p2p] Rejected request from %s with both init data and an ID token", getClientIP(r))
		p2pFail(w, http.StatusUnauthorized, "unauthorized", "Send either Mini App init data or a Telegram login, not both.")
		return 0, false
	case idToken != "":
		if !cfg.OIDCEnabled {
			p2pFail(w, http.StatusUnauthorized, "unauthorized", "Telegram login is not enabled for this API.")
			return 0, false
		}
		u, err := tgoidc.Verify(idToken, tgoidc.Default, cfg.OIDCClientID, maxAge, now)
		if err != nil {
			log.Warnf("[api/p2p] Rejected ID token from %s: %v", getClientIP(r), err)
			p2pFail(w, http.StatusUnauthorized, "unauthorized", "Your Telegram login expired. Log in with Telegram again.")
			return 0, false
		}
		return u.ID, true
	default:
		u, err := tma.Verify(initData, internal.Configuration.Telegram.ApiKey, maxAge, now)
		if err != nil {
			log.Warnf("[api/p2p] Rejected init data from %s: %v", getClientIP(r), err)
			p2pFail(w, http.StatusUnauthorized, "unauthorized", "Your session expired. Close and reopen the Mini App.")
			return 0, false
		}
		return u.ID, true
	}
}

func (p *P2PService) limited(w http.ResponseWriter, l *rateLimiter, sender *lnbits.User) bool {
	if l.allow(strconv.FormatInt(sender.Telegram.ID, 10), time.Now()) {
		return false
	}
	p2pFail(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Wait a minute and try again.")
	return true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		p2pFail(w, http.StatusBadRequest, "invalid_json", "Invalid request body.")
		return false
	}
	return true
}

// RecipientResponse is what the Mini App shows on the confirm screen.
type RecipientResponse struct {
	TelegramID int64  `json:"telegram_id"`
	Username   string `json:"username,omitempty"`
	Name       string `json:"name"`
}

// Resolve: POST /api/v1/p2p/resolve {"to": "@name" | "123456"}
func (p *P2PService) Resolve(w http.ResponseWriter, r *http.Request) {
	sender := p2pSender(r)
	if p.limited(w, p.readRate, sender) {
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	u, err := p.Bot.P2PResolve(req.To)
	if err != nil {
		p2pWriteErr(w, err)
		return
	}
	if u.Telegram.ID == sender.Telegram.ID {
		p2pFail(w, http.StatusBadRequest, "self_send", "You can't send to yourself.")
		return
	}
	p2pWriteJSON(w, http.StatusOK, RecipientResponse{TelegramID: u.Telegram.ID, Username: u.Telegram.Username, Name: telegram.DisplayName(u.Telegram)})
}

// TransferResponse describes a transfer to the Mini App.
type TransferResponse struct {
	ID          string             `json:"id"`
	Kind        string             `json:"kind"`
	Status      string             `json:"status"`
	Amount      int64              `json:"amount"`
	AmountLKR   string             `json:"amount_lkr,omitempty"`
	Fee         int64              `json:"fee,omitempty"`
	Memo        string             `json:"memo,omitempty"`
	To          *RecipientResponse `json:"to,omitempty"`
	PaymentHash string             `json:"payment_hash,omitempty"`
	Description string             `json:"description,omitempty"`
	Preimage    string             `json:"preimage,omitempty"`
	Error       string             `json:"error,omitempty"`
	ConfirmBy   *time.Time         `json:"confirm_by,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
}

func toTransferResponse(t *telegram.P2PTransfer) TransferResponse {
	resp := TransferResponse{
		ID: t.ID, Kind: t.Kind, Status: t.Status, Amount: t.Amount, AmountLKR: thirdparty.FormatSatsWithLKR(t.Amount),
		Fee: t.Fee, Memo: t.Memo, PaymentHash: t.PaymentHash, Description: t.Description, Error: t.Error, CreatedAt: t.CreatedAt,
	}
	if t.Kind == telegram.P2PKindInternal {
		resp.To = &RecipientResponse{TelegramID: t.ToTelegramID, Username: t.ToUsername, Name: t.ToName}
	}
	if t.Status == telegram.P2PCompleted {
		resp.Preimage = t.Preimage
	}
	if t.Status == telegram.P2PAwaitingConfirmation {
		c := t.ConfirmBy
		resp.ConfirmBy = &c
	}
	return resp
}

// statusCode: 200 done (either way), 202 still pending (confirm or settle).
func statusCode(t *telegram.P2PTransfer) int {
	switch t.Status {
	case telegram.P2PAwaitingConfirmation, telegram.P2PProcessing, telegram.P2PInFlight:
		return http.StatusAccepted
	}
	return http.StatusOK
}

// Send: POST /api/v1/p2p/send {"to_telegram_id", "amount", "memo", "idempotency_key"}
func (p *P2PService) Send(w http.ResponseWriter, r *http.Request) {
	sender := p2pSender(r)
	if p.limited(w, p.payRate, sender) {
		return
	}
	var req struct {
		ToTelegramID   int64  `json:"to_telegram_id"`
		Amount         int64  `json:"amount"`
		Memo           string `json:"memo"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	t, _, err := p.Bot.P2PStartSend(sender, req.ToTelegramID, req.Amount, req.Memo, req.IdempotencyKey)
	if err != nil && t == nil {
		p2pWriteErr(w, err)
		return
	}
	if t, err = p.reload(t); err != nil {
		p2pWriteErr(w, err)
		return
	}
	p2pWriteJSON(w, statusCode(t), toTransferResponse(t))
}

// DecodeInvoice: POST /api/v1/p2p/invoice/decode {"invoice"}
func (p *P2PService) DecodeInvoice(w http.ResponseWriter, r *http.Request) {
	sender := p2pSender(r)
	if p.limited(w, p.readRate, sender) {
		return
	}
	var req struct {
		Invoice string `json:"invoice"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	dec, err := telegram.P2PDecodeInvoice(req.Invoice)
	if err != nil {
		p2pWriteErr(w, err)
		return
	}
	cfg := internal.Configuration.API.P2P
	p2pWriteJSON(w, http.StatusOK, map[string]interface{}{
		"amount": dec.Amount, "amount_lkr": thirdparty.FormatSatsWithLKR(dec.Amount),
		"description": dec.Description, "payment_hash": dec.PaymentHash, "expires_at": dec.ExpiresAt,
		"needs_confirmation": dec.Amount > p.Bot.P2PUnconfirmedRemaining(sender.Telegram.ID),
		"over_limit":         dec.Amount > cfg.MaxAmount,
	})
}

// PayInvoice: POST /api/v1/p2p/invoice/pay {"invoice", "memo", "idempotency_key"}
func (p *P2PService) PayInvoice(w http.ResponseWriter, r *http.Request) {
	sender := p2pSender(r)
	if p.limited(w, p.payRate, sender) {
		return
	}
	var req struct {
		Invoice        string `json:"invoice"`
		Memo           string `json:"memo"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	t, _, err := p.Bot.P2PStartInvoice(sender, req.Invoice, req.Memo, req.IdempotencyKey)
	if err != nil && t == nil {
		p2pWriteErr(w, err)
		return
	}
	telegram.P2PWait(t.ID, p2pInvoiceWait)
	if t, err = p.reload(t); err != nil {
		p2pWriteErr(w, err)
		return
	}
	p2pWriteJSON(w, statusCode(t), toTransferResponse(t))
}

func (p *P2PService) reload(t *telegram.P2PTransfer) (*telegram.P2PTransfer, error) {
	fresh, err := p.Bot.P2PGet(t.ID)
	if err != nil {
		return nil, err
	}
	return fresh, nil
}

// Transfer: GET /api/v1/p2p/transfers/{id}. Only the sender can read it.
// lnbits is asked about a payment at most once per transfer every 20 s,
// however often this is polled.
func (p *P2PService) Transfer(w http.ResponseWriter, r *http.Request) {
	sender := p2pSender(r)
	if p.limited(w, p.pollRate, sender) {
		return
	}
	t, err := p.Bot.P2PGet(mux.Vars(r)["id"])
	if err != nil || t.FromTelegramID != sender.Telegram.ID {
		p2pFail(w, http.StatusNotFound, "not_found", "Transfer not found.")
		return
	}
	p.Bot.P2PRefresh(t)
	p2pWriteJSON(w, statusCode(t), toTransferResponse(t))
}

// Me: GET /api/v1/p2p/me. Balance and what is left of today's limit.
func (p *P2PService) Me(w http.ResponseWriter, r *http.Request) {
	sender := p2pSender(r)
	balance, err := p.Bot.GetUserAvailableBalance(sender)
	if err != nil {
		p2pFail(w, http.StatusServiceUnavailable, "balance_unavailable", "Could not load your balance. Try again.")
		return
	}
	used, _ := p.Bot.P2PDailyUsed(sender.Telegram.ID)
	cfg := internal.Configuration.API.P2P
	remaining := cfg.DailyLimit - used
	if remaining < 0 {
		remaining = 0
	}
	p2pWriteJSON(w, http.StatusOK, map[string]interface{}{
		"telegram_id": sender.Telegram.ID, "username": sender.Telegram.Username, "name": telegram.DisplayName(sender.Telegram),
		"balance": balance, "balance_lkr": thirdparty.FormatSatsWithLKR(balance),
		"limits": map[string]int64{
			"max_amount": cfg.MaxAmount, "daily_limit": cfg.DailyLimit, "daily_used": used,
			"daily_remaining": remaining, "confirm_threshold": cfg.ConfirmThreshold,
			"unconfirmed_remaining": p.Bot.P2PUnconfirmedRemaining(sender.Telegram.ID),
		},
	})
}
