package thirdparty

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	internal "github.com/LightningTipBot/LightningTipBot/internal"
	utils "github.com/LightningTipBot/LightningTipBot/internal/utils"
)

type PriceResponse struct {
	Bitcoin struct {
		USD float64 `json:"usd"`
	} `json:"bitcoin"`
}

type BinancePriceResponse struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

type CMCPriceResponse struct {
	Data map[string]struct {
		Quote map[string]struct {
			Price float64 `json:"price"`
		} `json:"quote"`
	} `json:"data"`
}

const SATS_PER_BITCOIN = 100_000_000

// per-source HTTP client with a tight timeout
var priceClient = &http.Client{Timeout: 5 * time.Second}

// Caching price for 10 mins
var cache = utils.NewCache(10 * time.Minute)

// GetSatPrice fetches the current Bitcoin price in USD and LKR exchange rate, then calculates the price per satoshi
func GetSatPrice() (float64, float64, error) {
	key := "sat-price"
	valueFromCache, hasCache := cache.Get(key)
	if hasCache {
		parts := strings.Split(valueFromCache, "-")
		LKRPerSat, _ := strconv.ParseFloat(parts[0], 64)
		USDPerSat, _ := strconv.ParseFloat(parts[1], 64)
		return LKRPerSat, USDPerSat, nil
	}

	bitcoinUSD, err := fetchBitcoinPriceParallel()
	if err != nil {
		return 0, 0, err
	}

	usdToLKR, err := GetUSDToLKRRate()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to fetch exchange rate: %v", err)
	}

	bitcoinLKR := bitcoinUSD * usdToLKR
	LKRPerSat := bitcoinLKR / SATS_PER_BITCOIN
	USDPerSat := bitcoinUSD / SATS_PER_BITCOIN

	cache.Set(key, fmt.Sprintf("%f-%f", LKRPerSat, USDPerSat))
	return LKRPerSat, USDPerSat, nil
}

type priceResult struct {
	source string
	price  float64
	err    error
}

// fetchBitcoinPriceParallel fires all three sources simultaneously and returns
// the first successful price. Worst-case delay = one HTTP timeout (5 s).
func fetchBitcoinPriceParallel() (float64, error) {
	ch := make(chan priceResult, 3)

	go func() {
		p, err := fetchCoinGecko()
		ch <- priceResult{"CoinGecko", p, err}
	}()
	go func() {
		p, err := fetchCMC()
		ch <- priceResult{"CoinMarketCap", p, err}
	}()
	go func() {
		p, err := fetchBinance()
		ch <- priceResult{"Binance", p, err}
	}()

	var errs []string
	for i := 0; i < 3; i++ {
		r := <-ch
		if r.err == nil {
			log.Infof("[price] fetched %.2f USD from %s", r.price, r.source)
			return r.price, nil
		}
		log.Warnf("[price] %s failed: %v", r.source, r.err)
		errs = append(errs, fmt.Sprintf("%s: %v", r.source, r.err))
	}

	return 0, fmt.Errorf("all price sources failed — %s", strings.Join(errs, "; "))
}

func fetchCoinGecko() (float64, error) {
	const apiURL = "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=usd"

	resp, err := priceClient.Get(apiURL)
	if err != nil {
		return 0, fmt.Errorf("request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, fmt.Errorf("rate limited (429): %s", string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var pr PriceResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, fmt.Errorf("decode error: %v", err)
	}
	if pr.Bitcoin.USD == 0 {
		return 0, fmt.Errorf("zero price returned")
	}
	return pr.Bitcoin.USD, nil
}

func fetchCMC() (float64, error) {
	apiKey := internal.Configuration.ThirdParty.CoinMarketCapAPIKey
	if apiKey == "" {
		return 0, fmt.Errorf("API key not configured")
	}

	const apiURL = "https://pro-api.coinmarketcap.com/v1/cryptocurrency/quotes/latest?symbol=BTC&convert=USD"
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return 0, fmt.Errorf("request build error: %v", err)
	}
	req.Header.Set("X-CMC_PRO_API_KEY", apiKey)

	resp, err := priceClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var cmcResp CMCPriceResponse
	if err := json.Unmarshal(body, &cmcResp); err != nil {
		return 0, fmt.Errorf("decode error: %v", err)
	}
	btc, ok := cmcResp.Data["BTC"]
	if !ok {
		return 0, fmt.Errorf("BTC key missing in response")
	}
	usd, ok := btc.Quote["USD"]
	if !ok || usd.Price == 0 {
		return 0, fmt.Errorf("USD price missing or zero")
	}
	return usd.Price, nil
}

func fetchBinance() (float64, error) {
	const apiURL = "https://api.binance.com/api/v3/ticker/price?symbol=BTCUSDT"

	resp, err := priceClient.Get(apiURL)
	if err != nil {
		return 0, fmt.Errorf("request error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var br BinancePriceResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return 0, fmt.Errorf("decode error: %v", err)
	}
	price, err := strconv.ParseFloat(br.Price, 64)
	if err != nil || price == 0 {
		return 0, fmt.Errorf("invalid price: %s", br.Price)
	}
	return price, nil
}

// LKRToSat converts a LKR amount to satoshis using the current price.
func LKRToSat(amount float64) (int64, error) {
	lkrPerSat, _, err := GetSatPrice()
	if err != nil || lkrPerSat == 0 {
		return 0, fmt.Errorf("price unavailable")
	}
	sats := int64(amount / lkrPerSat)
	return sats, nil
}

// FormatSatsWithLKR formats sats amount with LKR conversion in the format: {amount} sats (රු. {lkr_amount})
func FormatSatsWithLKR(amount int64) string {
	lkrPerSat, _, err := GetSatPrice()
	if err != nil {
		return utils.FormatSats(amount)
	}
	lkrValue := lkrPerSat * float64(amount)
	return fmt.Sprintf("%s (රු. %s)", utils.FormatSats(amount), utils.FormatFloatWithCommas(lkrValue))
}
