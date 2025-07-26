package thirdparty

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type ExchangeRateResponse struct {
	Description                     string  `json:"description"`
	BuyingRate                      float64 `json:"buying_rate"`
	SellingRate                     float64 `json:"selling_rate"`
	ChequeBuyingRate                float64 `json:"cheque_buying_rate"`
	ChequeSellingRate               float64 `json:"cheque_selling_rate"`
	TelegraphicTransfersBuyingRate  float64 `json:"telegraphic_transfers_buying_rate"`
	TelegraphicTransfersSellingRate float64 `json:"telegraphic_transfers_selling_rate"`
}

// GetUSDToLKRRate fetches USD to LKR exchange rate from Ceylon Cash
func GetUSDToLKRRate() (float64, error) {
	url := "https://fx.ceyloncash.com/currency/USD"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch exchange rate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var exchangeResponse ExchangeRateResponse
	if err := json.NewDecoder(resp.Body).Decode(&exchangeResponse); err != nil {
		return 0, fmt.Errorf("failed to decode exchange response: %v", err)
	}

	// Use selling rate as it's typically what you'd pay to get LKR for USD
	return exchangeResponse.SellingRate, nil
}
