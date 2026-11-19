package api

import (
	"net/http"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/LightningTipBot/LightningTipBot/internal/thirdparty"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// CollectResponse describes a collect request. Transfer is set once the
// payer approved; its status says whether the sats arrived.
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
	Notified   bool              `json:"notified"`
	ExpiresAt  time.Time         `json:"expires_at"`
	CreatedAt  time.Time         `json:"created_at"`
	Transfer   *TransferResponse `json:"transfer,omitempty"`
}

func (p *P2PService) collectResponse(c *telegram.CollectRequest, withExternalID bool) CollectResponse {
	resp := CollectResponse{
		ID: c.ID, Status: c.Status, Amount: c.Amount, AmountLKR: thirdparty.FormatSatsWithLKR(c.Amount), Memo: c.Memo,
		Payer:     RecipientResponse{TelegramID: c.PayerTelegramID, Username: c.PayerUsername},
		Collector: RecipientResponse{TelegramID: c.ToTelegramID, Username: c.ToUsername, Name: c.ToName},
		Notified:  c.Notified, ExpiresAt: c.ExpiresAt, CreatedAt: c.CreatedAt,
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

// Collect: POST /api/v1/collect {"to", "amount", "memo", "external_id"}.
// Asks the user in "to" to pay the calling wallet. Nothing moves until the
// user approves.
func (p *P2PService) Collect(w http.ResponseWriter, r *http.Request) {
	walletID, ok := collectWallet(w, r)
	if !ok {
		return
	}
	var req struct {
		To         string `json:"to"`
		Amount     int64  `json:"amount"`
		Memo       string `json:"memo"`
		ExternalID string `json:"external_id"`
		// ChatPrompt sends the user Pay / Decline buttons in the bot chat.
		// Default true; false when the user approves in the Mini App.
		ChatPrompt *bool `json:"chat_prompt"`
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
	payer, err := p.Bot.P2PResolve(req.To)
	if err != nil {
		p2pWriteErr(w, err)
		return
	}
	chatPrompt := req.ChatPrompt == nil || *req.ChatPrompt
	c, created, err := p.Bot.CollectCreate(walletID, collector, payer, req.Amount, GetWalletMaxAmount(walletID), req.Memo, req.ExternalID, GetMaxMemoLength(), chatPrompt)
	if err != nil {
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

// CollectCancel: POST /api/v1/collect/{id}/cancel. Withdraws a request that
// was not answered yet.
func (p *P2PService) CollectCancel(w http.ResponseWriter, r *http.Request) {
	walletID, ok := collectWallet(w, r)
	if !ok {
		return
	}
	c, err := p.Bot.CollectCancel(mux.Vars(r)["id"], walletID)
	if err != nil {
		p2pWriteErr(w, err)
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

// MiniAppCollectApprove: POST /api/v1/p2p/collect/{id}/approve. Pays the
// request from the logged-in user. Repeats return the same transfer.
func (p *P2PService) MiniAppCollectApprove(w http.ResponseWriter, r *http.Request) {
	payer := p2pSender(r)
	if p.limited(w, p.payRate, payer) {
		return
	}
	t, err := p.Bot.CollectApprove(mux.Vars(r)["id"], payer)
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

// MiniAppCollectDecline: POST /api/v1/p2p/collect/{id}/decline.
func (p *P2PService) MiniAppCollectDecline(w http.ResponseWriter, r *http.Request) {
	payer := p2pSender(r)
	if p.limited(w, p.readRate, payer) {
		return
	}
	c, err := p.Bot.CollectDecline(mux.Vars(r)["id"], payer.Telegram.ID)
	if err != nil {
		p2pWriteErr(w, err)
		return
	}
	p2pWriteJSON(w, http.StatusOK, p.collectResponse(c, false))
}
