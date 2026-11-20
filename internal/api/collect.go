package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// CollectResponse describes a collect request. Transfer is the payment; its
// status says whether the sats arrived.
type CollectResponse struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Paid       bool              `json:"paid"`
	Amount     int64             `json:"amount"`
	AmountLKR  string            `json:"amount_lkr,omitempty"`
	Memo       string            `json:"memo,omitempty"`
	ExternalID string            `json:"external_id,omitempty"`
	Payer      RecipientResponse `json:"payer"`
	Collector  RecipientResponse `json:"collector"`
	ExpiresAt  time.Time         `json:"expires_at"`
	CreatedAt  time.Time         `json:"created_at"`
	Transfer   *TransferResponse `json:"transfer,omitempty"`
}

func (p *P2PService) collectResponse(c *telegram.CollectRequest, withExternalID bool) CollectResponse {
	resp := CollectResponse{
		ID: c.ID, Status: c.Status, Amount: c.Amount, AmountLKR: thirdparty.FormatSatsWithLKR(c.Amount), Memo: c.Memo,
		Payer:     RecipientResponse{TelegramID: c.PayerTelegramID, Username: c.PayerUsername},
		Collector: RecipientResponse{TelegramID: c.ToTelegramID, Username: c.ToUsername, Name: c.ToName},
		ExpiresAt: c.ExpiresAt, CreatedAt: c.CreatedAt,
	}
	if withExternalID {
		resp.ExternalID = c.ExternalID
	}
	if c.Status == telegram.CollectApproved {
		if t, err := p.Bot.CollectTransfer(c); err == nil {
			p.Bot.P2PRefresh(t)
			tr := toTransferResponse(t)
			resp.Transfer = &tr
			resp.Paid = t.Status == telegram.P2PCompleted
		}
	}
	return resp
}

func authenticatedWallet(r *http.Request) string {
	id, _ := r.Context().Value("authenticated_wallet").(string)
	return id
}

// collectWallet returns the calling wallet if it may collect, or answers 403.
func collectWallet(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := authenticatedWallet(r)
	if wallet, ok := GetWhitelistedWallets()[id]; !ok || !wallet.CanCollect {
		p2pFail(w, http.StatusForbidden, "collect_not_allowed", "This wallet is not allowed to collect.")
		return "", false
	}
	return id, true
}

// Collect: POST /api/v1/collect {"amount", "memo", "external_id"}, with the
// payer in X-BDB-User (signed). Charges the payer and pays the calling
// wallet at once, with no approval. Any user with a bot wallet can be
// charged. A repeat of the same external_id returns the same request and never pays
// twice.
func (p *P2PService) Collect(w http.ResponseWriter, r *http.Request) {
	walletID, ok := collectWallet(w, r)
	if !ok {
		return
	}
	var req struct {
		Amount     int64  `json:"amount"`
		Memo       string `json:"memo"`
		ExternalID string `json:"external_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	collector, err := telegram.GetUserByTelegramUsername(GetWhitelistedWallets()[walletID].Username, *p.Bot)
	if err != nil {
		log.Errorf("[api/collect] Wallet %s user not found: %v", walletID, err)
		p2pFail(w, http.StatusForbidden, "collector_not_allowed", "The collecting wallet has no bot user.")
		return
	}
	payerID, _ := r.Context().Value(collectPayerKey{}).(int64)
	payer, err := p.Bot.P2PResolve(strconv.FormatInt(payerID, 10))
	if err != nil {
		p2pWriteErr(w, err)
		return
	}
	c, created, err := p.Bot.CollectCreate(walletID, collector, payer, req.Amount, GetWalletMaxAmount(walletID), req.Memo, req.ExternalID, GetMaxMemoLength())
	if err != nil {
		p2pWriteErr(w, err)
		return
	}
	if _, err := p.Bot.CollectCharge(c.ID, payer); err != nil {
		var pe *telegram.P2PError
		if !errors.As(err, &pe) || pe.Code != "request_closed" {
			log.Warnf("[api/collect] Charge %s: %v", c.ID, err)
		}
	}
	if c, err = p.Bot.CollectGet(c.ID); err != nil {
		p2pWriteErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	p2pWriteJSON(w, status, p.collectResponse(c, true))
}

// CollectStatus: GET /api/v1/collect/{id}. Only the wallet that made it.
func (p *P2PService) CollectStatus(w http.ResponseWriter, r *http.Request) {
	walletID, ok := collectWallet(w, r)
	if !ok {
		return
	}
	c, err := p.Bot.CollectGet(mux.Vars(r)["id"])
	if err != nil || c.WalletID != walletID {
		p2pFail(w, http.StatusNotFound, "not_found", "Request not found.")
		return
	}
	p2pWriteJSON(w, http.StatusOK, p.collectResponse(c, true))
}

// MiniAppCollect: GET /api/v1/p2p/collect/{id}. Only the payer can read it.
func (p *P2PService) MiniAppCollect(w http.ResponseWriter, r *http.Request) {
	payer := p2pSender(r)
	if p.limited(w, p.pollRate, payer) {
		return
	}
	c, err := p.Bot.CollectGet(mux.Vars(r)["id"])
	if err != nil || c.PayerTelegramID != payer.Telegram.ID {
		p2pFail(w, http.StatusNotFound, "not_found", "Request not found.")
		return
	}
	p2pWriteJSON(w, http.StatusOK, p.collectResponse(c, false))
}
