package main

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

// PaymentRequest represents the structure of a payment request
type PaymentRequest struct {
	Amount      int64  `json:"amount"`
	Destination string `json:"destination"`
	Memo        string `json:"memo,omitempty"`
}

// PaymentResponse represents the response from the payment API
type PaymentResponse struct {
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	PaymentHash string `json:"payment_hash,omitempty"`
}

// HMACClient handles HMAC-authenticated requests to the payment API
type HMACClient struct {
	baseURL    string
	hmacSecret string
	httpClient *http.Client
}

// NewHMACClient creates a new HMAC client for the payment API
func NewHMACClient(baseURL, hmacSecret string) *HMACClient {
	return &HMACClient{
		baseURL:    baseURL,
		hmacSecret: hmacSecret,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SendPayment sends a payment request with HMAC authentication
func (c *HMACClient) SendPayment(payment PaymentRequest) (*PaymentResponse, error) {
	// Marshal the request payload
	payload, err := json.Marshal(payment)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payment request: %w", err)
	}

	// Create the request URL
	url := c.baseURL + "/api/v1/send"
	
	// Generate timestamp
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	
	// Create HMAC signature
	message := fmt.Sprintf("POST/api/v1/send%s%s", timestamp, string(payload))
	signature := c.generateHMAC(message)

	// Create HTTP request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-HMAC-Signature", signature)
	req.Header.Set("X-Timestamp", timestamp)

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned error %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var paymentResp PaymentResponse
	if err := json.Unmarshal(body, &paymentResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &paymentResp, nil
}

// generateHMAC creates an HMAC-SHA256 signature
func (c *HMACClient) generateHMAC(message string) string {
	h := hmac.New(sha256.New, []byte(c.hmacSecret))
	h.Write([]byte(message))
	return hex.EncodeToString(h.Sum(nil))
}

// Example usage
func main() {
	// Initialize the HMAC client
	client := NewHMACClient("https://your-bot-api.com", "your-hmac-secret-key")

	// Create a payment request
	payment := PaymentRequest{
		Amount:      1000, // 1000 satoshis
		Destination: "lnbc10u1p3xnhl2pp5e6v94jmw....", // Lightning invoice
		Memo:        "Test payment via API",
	}

	// Send the payment
	response, err := client.SendPayment(payment)
	if err != nil {
		fmt.Printf("Payment failed: %v\n", err)
		return
	}

	fmt.Printf("Payment sent successfully!\n")
	fmt.Printf("Status: %s\n", response.Status)
	fmt.Printf("Payment Hash: %s\n", response.PaymentHash)
}

// CLI tool example for testing
func ExampleCLIUsage() {
	// Example command line usage:
	// go run hmac_client.go --url "https://your-bot.com" --secret "your-secret" --amount 1000 --destination "lnbc..."
	
	// You can extend this to accept command line arguments using the flag package
	// or a more sophisticated CLI library like cobra
}
