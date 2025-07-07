package thirdparty

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	utils "github.com/LightningTipBot/LightningTipBot/internal/utils"
)

type PriceResponse struct {
	Bitcoin struct {
		USD float64 `json:"usd"`
		LKR float64 `json:"lkr"`
	} `json:"bitcoin"`
}

const SATS_PER_BITCOIN = 100_000_000

// Caching price for 10 mins
var cache = utils.NewCache(10 * time.Minute)

// GetSatPrice fetches the current Bitcoin price in USD and returns the price per satoshi
func GetSatPrice() (float64, float64, error) {
	key := "sat-price"
	valueFromCache, hasCache := cache.Get(key)
	if hasCache {
		parts := strings.Split(valueFromCache, "-")

		LKRPerSat, _ := strconv.ParseFloat(parts[0], 64)
		USDPerSat, _ := strconv.ParseFloat(parts[1], 64)

		return LKRPerSat, USDPerSat, nil
	}

	url := "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=usd,lkr"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to fetch bitcoin price: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var priceResponse PriceResponse
	if err := json.NewDecoder(resp.Body).Decode(&priceResponse); err != nil {
		return 0, 0, fmt.Errorf("failed to decode response: %v", err)
	}

	// Calculate price per sat
	LKRPerSat := priceResponse.Bitcoin.LKR / SATS_PER_BITCOIN
	USDPerSat := priceResponse.Bitcoin.USD / SATS_PER_BITCOIN

	cache.Set(key, fmt.Sprintf("%f-%f", LKRPerSat, USDPerSat))

	return LKRPerSat, USDPerSat, nil
}


// LKRToSat converts a LKR amount to satoshis using the current price.
func LKRToSat(amount float64) (int64, error) {
	lkrPerSat, _, err := GetSatPrice()
	if err != nil || lkrPerSat == 0 {
		return 0, fmt.Errorf("price unavailable")
	}
	sats := int64(amount / lkrPerSat)
	return sats, nil

// FormatSatsWithLKR formats sats amount with LKR conversion in the format: {amount} sats (රු. {lkr_amount})
func FormatSatsWithLKR(amount int64) string {
    lkrPerSat, _, err := GetSatPrice()
    if err != nil {
        // Fallback to sats only if LKR price is unavailable
        return fmt.Sprintf("%d sats", amount)
    }
    
    lkrValue := lkrPerSat * float64(amount)
    return fmt.Sprintf("%d sats (රු. %s)", amount, utils.FormatFloatWithCommas(lkrValue))

}
