package dca

import (
	"net/http"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	log "github.com/sirupsen/logrus"
)

// NotifyDeposit checks whether the given recipient username matches the configured
// DCA wallet username and, if so, asks the DCA backend to reset its retry counts.
func NotifyDeposit(toUsername string) {
	if internal.Configuration.DCA.WalletUsername == "" || toUsername != internal.Configuration.DCA.WalletUsername {
		return
	}

	url := internal.Configuration.DCA.ApiUrl + "/transaction/reset-retry-counts"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		log.Errorf("[DCA] Error creating reset-retry-counts request: %s", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Errorf("[DCA] Error calling reset-retry-counts: %s", err.Error())
		return
	}
	defer resp.Body.Close()
	log.Infof("[DCA] reset-retry-counts called for %s, status: %s", toUsername, resp.Status)
}
