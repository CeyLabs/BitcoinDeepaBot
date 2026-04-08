package boltz

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

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

// TestCreateReverseSwapSuccess verifies that a well-formed swap request is sent and
// the response is decoded correctly.
func TestCreateReverseSwapSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/swap/reverse" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("API-KEY") == "" {
			t.Error("missing API-KEY header")
		}
		if r.Header.Get("TS") == "" {
			t.Error("missing TS header")
		}
		if r.Header.Get("API-HMAC") == "" {
			t.Error("missing API-HMAC header")
		}

		var body ReverseSwapRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.From != "BTC" || body.To != "USDT" {
			t.Errorf("unexpected pair: %s/%s", body.From, body.To)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ReverseSwapResponse{
			ID:           "testswap123",
			Invoice:      "lnbc10u1...",
			PreimageHash: "aabbcc",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "testkey", "testsecret")
	resp, err := c.CreateReverseSwap(ReverseSwapRequest{
		From:          "BTC",
		To:            "USDT",
		Address:       "TFakeAddress",
		InvoiceAmount: 10000,
		PreimageHash:  "aabbcc",
	})
	if err != nil {
		t.Fatalf("CreateReverseSwap: %v", err)
	}
	if resp.ID != "testswap123" {
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
		To:            "USDT",
		Address:       "bad",
		InvoiceAmount: 100,
		PreimageHash:  "ff",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

