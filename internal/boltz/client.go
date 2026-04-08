package boltz

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Client is a lightweight HTTP client for the Boltz REST API v2.
type Client struct {
	apiURL     string
	apiKey     string
	apiSecret  string
	httpClient *http.Client
}

// NewClient creates a new Boltz API client.
func NewClient(apiURL, apiKey, apiSecret string) *Client {
	return &Client{
		apiURL:    apiURL,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// sign computes the TS and HMAC-SHA256 signature required by Boltz.
// The message is: TS + METHOD + PATH + BODY (body may be empty for GET requests).
func (c *Client) sign(method, path, body string) (ts, sig string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	msg := ts + method + path + body
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(msg))
	sig = hex.EncodeToString(mac.Sum(nil))
	return
}

// addAuthHeaders adds the three Boltz authentication headers to req.
func (c *Client) addAuthHeaders(req *http.Request, method, path, body string) {
	ts, sig := c.sign(method, path, body)
	req.Header.Set("API-KEY", c.apiKey)
	req.Header.Set("TS", ts)
	req.Header.Set("API-HMAC", sig)
	req.Header.Set("Content-Type", "application/json")
}

// do executes an authenticated request and returns the raw response body.
func (c *Client) do(method, path string, payload interface{}) ([]byte, int, error) {
	var bodyStr string
	var bodyReader io.Reader

	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("boltz: marshal request: %w", err)
		}
		bodyStr = string(b)
		bodyReader = bytes.NewBufferString(bodyStr)
	}

	req, err := http.NewRequest(method, c.apiURL+path, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("boltz: new request: %w", err)
	}
	c.addAuthHeaders(req, method, path, bodyStr)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("boltz: http do: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("boltz: read body: %w", err)
	}
	return data, resp.StatusCode, nil
}

// CreateReverseSwap calls POST /v2/swap/reverse to initiate a Lightning→on-chain swap.
func (c *Client) CreateReverseSwap(req ReverseSwapRequest) (ReverseSwapResponse, error) {
	data, status, err := c.do(http.MethodPost, "/v2/swap/reverse", req)
	if err != nil {
		return ReverseSwapResponse{}, err
	}

	var resp ReverseSwapResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return ReverseSwapResponse{}, fmt.Errorf("boltz: decode reverse swap response (status %d): %w", status, err)
	}
	if resp.Error != "" {
		return resp, fmt.Errorf("boltz: API error: %s", resp.Error)
	}
	if status >= 300 {
		return resp, fmt.Errorf("boltz: unexpected HTTP status %d", status)
	}
	return resp, nil
}

// GetSwapStatus calls GET /v2/swap/{id} to poll the current state of a swap.
func (c *Client) GetSwapStatus(swapID string) (SwapStatus, error) {
	path := "/v2/swap/" + swapID
	data, status, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return SwapStatus{}, err
	}

	var resp SwapStatus
	if err := json.Unmarshal(data, &resp); err != nil {
		return SwapStatus{}, fmt.Errorf("boltz: decode swap status (status %d): %w", status, err)
	}
	if status >= 300 {
		return resp, fmt.Errorf("boltz: unexpected HTTP status %d getting swap %s", status, swapID)
	}
	return resp, nil
}

// GetPairs calls GET /v2/pairs to retrieve supported pair info including fees and limits.
func (c *Client) GetPairs() (PairsResponse, error) {
	data, status, err := c.do(http.MethodGet, "/v2/pairs", nil)
	if err != nil {
		return PairsResponse{}, err
	}

	var resp PairsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return PairsResponse{}, fmt.Errorf("boltz: decode pairs response (status %d): %w", status, err)
	}
	if status >= 300 {
		return resp, fmt.Errorf("boltz: unexpected HTTP status %d getting pairs", status)
	}
	return resp, nil
}
