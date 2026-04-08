package boltz

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Compiled address regexes — mirrors the patterns used in telegram/swap.go.
var (
	reEVMAddress  = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	reTronAddress = regexp.MustCompile(`^T[A-Za-z1-9]{33}$`)
)

// parseUSDTAddress mirrors the validation logic in telegram/swap.go so the
// address-format tests live alongside the rest of the Boltz-package tests.
func parseUSDTAddressForTest(addr string) (network string, valid bool) {
	switch {
	case reEVMAddress.MatchString(addr):
		return NetworkEVM, true
	case reTronAddress.MatchString(addr):
		return NetworkTRON, true
	default:
		return "", false
	}
}

// TestSignKnownVector validates that the HMAC signing logic matches an independently
// computed output for a fixed input. This acts as a regression guard for the auth header.
func TestSignKnownVector(t *testing.T) {
	c := NewClient("https://api.boltz.exchange", "key", "secret")
	ts, sig := c.sign("POST", "/v2/swap/reverse", `{"foo":"bar"}`)

	// Re-derive the HMAC independently
	msg := ts + "POST" + "/v2/swap/reverse" + `{"foo":"bar"}`
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))

	if sig != expected {
		t.Errorf("HMAC mismatch: got %s, want %s", sig, expected)
	}
}

// TestSignTimestamp checks that the TS value returned by sign() is recent.
func TestSignTimestamp(t *testing.T) {
	c := NewClient("https://api.boltz.exchange", "key", "secret")
	before := time.Now().Unix()
	ts, _ := c.sign("GET", "/path", "")
	after := time.Now().Unix()

	parsed, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		t.Fatalf("TS is not a valid int64: %s (%v)", ts, err)
	}
	if parsed < before || parsed > after {
		t.Errorf("TS %d is outside range [%d, %d]", parsed, before, after)
	}
}

// TestParseUSDTAddress covers the full regex validation table.
func TestParseUSDTAddress(t *testing.T) {
	cases := []struct {
		addr    string
		network string
		valid   bool
	}{
		// --- valid EVM: 0x + 40 hex chars ---
		{"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NetworkEVM, true},  // lowercase hex
		{"0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", NetworkEVM, true},  // uppercase hex
		{"0xAbCdEf1234567890abcdef1234567890AbCdEf12", NetworkEVM, true},  // mixed case
		{"0x0000000000000000000000000000000000000000", NetworkEVM, true},  // all zeros
		// --- invalid EVM: wrong length ---
		{"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", false},  // 41 chars total (0x + 39)
		{"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", false}, // 43 chars total (0x + 41)
		// --- invalid EVM: non-hex character ---
		{"0xGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG", "", false},
		// --- invalid EVM: missing 0x prefix ---
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", false},
		// --- valid TRON: T + exactly 33 chars from [A-Za-z1-9] ---
		{"TAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", NetworkTRON, true},  // T + 33 A's = 34 total
		{"Tzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", NetworkTRON, true},  // T + 33 z's = 34 total
		{"T111111111111111111111111111111111", NetworkTRON, true},  // T + 33 ones  = 34 total
		// --- invalid TRON: too short (T + 32 = 33 chars) ---
		{"TAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "", false},
		// --- invalid TRON: too long (T + 34 = 35 chars) ---
		{"TAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "", false},
		// --- invalid TRON: '0' (zero) is not in [A-Za-z1-9] ---
		{"T0AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "", false}, // 34 chars but contains '0'
		// --- invalid TRON: wrong first character ---
		{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "", false}, // starts with A, not T
		// --- empty ---
		{"", "", false},
		// --- gibberish ---
		{"notanaddress", "", false},
	}

	for _, tc := range cases {
		network, valid := parseUSDTAddressForTest(tc.addr)
		if valid != tc.valid {
			t.Errorf("addr=%q: valid=%v, want %v", tc.addr, valid, tc.valid)
		}
		if network != tc.network {
			t.Errorf("addr=%q: network=%q, want %q", tc.addr, network, tc.network)
		}
	}
}

// TestBoltzChainMapping verifies that network identifiers map to the correct Boltz chain values.
func TestBoltzChainMapping(t *testing.T) {
	if got := BoltzChain(NetworkEVM); got != "ETH" {
		t.Errorf("BoltzChain(EVM) = %q, want ETH", got)
	}
	if got := BoltzChain(NetworkTRON); got != "TRX" {
		t.Errorf("BoltzChain(TRON) = %q, want TRX", got)
	}
}

// TestBoltzPairKeyMapping verifies that network identifiers map to the correct pair keys.
func TestBoltzPairKeyMapping(t *testing.T) {
	if got := BoltzPairKey(NetworkEVM); got != "BTC/ETH" {
		t.Errorf("BoltzPairKey(EVM) = %q, want BTC/ETH", got)
	}
	if got := BoltzPairKey(NetworkTRON); got != "BTC/TRX" {
		t.Errorf("BoltzPairKey(TRON) = %q, want BTC/TRX", got)
	}
}

// TestCreateReverseSwapEVM verifies that an EVM swap sends the correct To field.
func TestCreateReverseSwapEVM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/swap/reverse" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		for _, hdr := range []string{"API-KEY", "TS", "API-HMAC"} {
			if r.Header.Get(hdr) == "" {
				t.Errorf("missing header: %s", hdr)
			}
		}

		var body ReverseSwapRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.From != "BTC" || body.To != BoltzChain(NetworkEVM) {
			t.Errorf("unexpected pair: %s/%s (want BTC/%s)", body.From, body.To, BoltzChain(NetworkEVM))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ReverseSwapResponse{
			ID:           "evmswap1",
			Invoice:      "lnbc10u1...",
			PreimageHash: "aabbcc",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "testkey", "testsecret")
	resp, err := c.CreateReverseSwap(ReverseSwapRequest{
		From:          "BTC",
		To:            BoltzChain(NetworkEVM),
		Address:       "0x0000000000000000000000000000000000000001",
		InvoiceAmount: 10000,
		PreimageHash:  "aabbcc",
	})
	if err != nil {
		t.Fatalf("CreateReverseSwap (EVM): %v", err)
	}
	if resp.ID != "evmswap1" {
		t.Errorf("unexpected swap ID: %s", resp.ID)
	}
}

// TestCreateReverseSwapTRON verifies that a TRON swap sends the correct To field.
func TestCreateReverseSwapTRON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body ReverseSwapRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.To != BoltzChain(NetworkTRON) {
			t.Errorf("unexpected To: %s (want %s)", body.To, BoltzChain(NetworkTRON))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ReverseSwapResponse{ID: "tronswap1"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "s")
	resp, err := c.CreateReverseSwap(ReverseSwapRequest{
		From:          "BTC",
		To:            BoltzChain(NetworkTRON),
		Address:       "TAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // T + 33 A's = 34 chars (valid Tron)
		InvoiceAmount: 10000,
		PreimageHash:  "aabbcc",
	})
	if err != nil {
		t.Fatalf("CreateReverseSwap (TRON): %v", err)
	}
	if resp.ID != "tronswap1" {
		t.Errorf("unexpected swap ID: %s", resp.ID)
	}
}

// TestGetSwapStatusSuccess verifies the swap status endpoint.
func TestGetSwapStatusSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/swap/abc123" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(SwapStatus{
			ID:    "abc123",
			State: StateTransactionConfirmed,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "testkey", "testsecret")
	status, err := c.GetSwapStatus("abc123")
	if err != nil {
		t.Fatalf("GetSwapStatus: %v", err)
	}
	if status.State != StateTransactionConfirmed {
		t.Errorf("unexpected state: %s", status.State)
	}
}

// TestAPIErrorPropagation verifies that a Boltz error field is surfaced as an error.
func TestAPIErrorPropagation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ReverseSwapResponse{
			Error: "invalid address",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "s")
	_, err := c.CreateReverseSwap(ReverseSwapRequest{
		From:          "BTC",
		To:            BoltzChain(NetworkEVM),
		Address:       "bad",
		InvoiceAmount: 100,
		PreimageHash:  "ff",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

