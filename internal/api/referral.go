package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	log "github.com/sirupsen/logrus"
)

type ReferralUserResult struct {
	TelegramID   int64     `json:"telegram_id"`
	Username     string    `json:"username"`
	ReferralCode string    `json:"referral_code"`
	JoinedAt     time.Time `json:"joined_at"`
}

type ReferralLookupResponse struct {
	Code  string               `json:"code"`
	Count int                  `json:"count"`
	Users []ReferralUserResult `json:"users"`
}

// ReferralLookup handles GET /api/v1/referral/lookup?code=<referral_code>
// Returns all users who joined using the given referral code.
// Authenticated with the same wallet HMAC used by the send API.
func (s Service) ReferralLookup(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		RespondError(w, "missing 'code' query parameter")
		return
	}

	var entries []telegram.ReferralEntry
	tx := s.Bot.DB.Referrals.Where("referral_code = ?", code).Order("created_at asc").Find(&entries)
	if tx.Error != nil {
		log.Errorf("[api/referral] db error: %v", tx.Error)
		RespondError(w, "database error")
		return
	}

	results := make([]ReferralUserResult, 0, len(entries))
	for _, e := range entries {
		results = append(results, ReferralUserResult{
			TelegramID:   e.TelegramID,
			Username:     e.Username,
			ReferralCode: e.ReferralCode,
			JoinedAt:     e.CreatedAt,
		})
	}

	resp := ReferralLookupResponse{
		Code:  code,
		Count: len(results),
		Users: results,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}
