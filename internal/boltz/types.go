package boltz

// ReverseSwapRequest is the payload for POST /v2/swap/reverse (Lightning → on-chain USDT).
type ReverseSwapRequest struct {
	From         string `json:"from"`
	To           string `json:"to"`
	Address      string `json:"address"`
	InvoiceAmount int64  `json:"invoiceAmount"`
	PreimageHash string `json:"preimageHash"`
	CallbackURL  string `json:"callbackUrl,omitempty"`
}

// ReverseSwapResponse is the response from POST /v2/swap/reverse.
type ReverseSwapResponse struct {
	ID           string `json:"id"`
	Invoice      string `json:"invoice"`
	PreimageHash string `json:"preimageHash"`
	ExpiresAt    int64  `json:"timeoutBlockHeight"` // block height or unix timestamp depending on chain
	Error        string `json:"error,omitempty"`
}

// SwapStatus represents the current state of a swap returned by GET /v2/swap/{id}.
type SwapStatus struct {
	ID    string `json:"id"`
	State string `json:"status"`
	Error string `json:"error,omitempty"`
}

// BoltzWebhookPayload is the JSON body Boltz POSTs to the bot's callback URL.
type BoltzWebhookPayload struct {
	ID    string `json:"id"`
	State string `json:"status"`
	Error string `json:"error,omitempty"`
}

// PairInfo holds limit/fee information for one currency pair.
type PairInfo struct {
	Rate float64 `json:"rate"`
	Fees struct {
		Percentage float64 `json:"percentage"`
		MinerFees  struct {
			BaseAsset struct {
				Normal  int64 `json:"normal"`
				Reverse struct {
					Claim   int64 `json:"claim"`
					Lockup  int64 `json:"lockup"`
				} `json:"reverse"`
			} `json:"baseAsset"`
			QuoteAsset struct {
				Normal  int64 `json:"normal"`
				Reverse struct {
					Claim   int64 `json:"claim"`
					Lockup  int64 `json:"lockup"`
				} `json:"reverse"`
			} `json:"quoteAsset"`
		} `json:"minerFees"`
	} `json:"fees"`
	Limits struct {
		Maximal int64 `json:"maximal"`
		Minimal int64 `json:"minimal"`
	} `json:"limits"`
}

// PairsResponse is the response from GET /v2/pairs.
type PairsResponse struct {
	Reverse map[string]PairInfo `json:"reverse"`
	Normal  map[string]PairInfo `json:"submarine"`
}

// Swap states returned by Boltz.
const (
	StateCreated              = "swap.created"
	StateInvoiceSet           = "invoice.set"
	StateInvoicePaid          = "invoice.paid"
	StateTransactionMempool   = "transaction.mempool"
	StateTransactionConfirmed = "transaction.confirmed"
	StateSwapExpired          = "swap.expired"
	StateInvoiceExpired       = "invoice.expired"
	StateTransactionFailed    = "transaction.failed"
)

// Network identifiers used internally to distinguish USDT destination chains.
const (
	NetworkEVM  = "EVM"  // Ethereum and EVM-compatible chains (ERC-20 USDT)
	NetworkTRON = "TRON" // Tron network (TRC-20 USDT)
)

// BoltzChain returns the Boltz chain identifier for a given internal network name.
// These values match the identifiers used in Boltz API v2's `to` field.
func BoltzChain(network string) string {
	switch network {
	case NetworkTRON:
		return "TRX"
	default: // NetworkEVM
		return "ETH"
	}
}

// BoltzPairKey returns the Boltz pairs map key for a given internal network name,
// used for fee and limit lookups from GET /v2/pairs.
func BoltzPairKey(network string) string {
	switch network {
	case NetworkTRON:
		return "BTC/TRX"
	default: // NetworkEVM
		return "BTC/ETH"
	}
}

// NetworkLabel returns a human-readable network label for display in Telegram messages.
func NetworkLabel(network string) string {
	switch network {
	case NetworkTRON:
		return "Tron (TRC-20)"
	default: // NetworkEVM
		return "Ethereum (ERC-20)"
	}
}
