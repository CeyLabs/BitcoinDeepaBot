package lnbits

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	decodepay "github.com/fiatjaf/ln-decodepay"
)

// lnurlHTTP talks to remote LNURL-pay servers. Tests replace it.
var lnurlHTTP = &http.Client{Timeout: 20 * time.Second}

// maxLNURLResponse bounds how much a remote LNURL server can make us read.
const maxLNURLResponse = 64 << 10

type lnurlPayParams struct {
	Tag            string `json:"tag"`
	Callback       string `json:"callback"`
	MinSendable    int64  `json:"minSendable"`
	MaxSendable    int64  `json:"maxSendable"`
	Metadata       string `json:"metadata"`
	CommentAllowed int    `json:"commentAllowed"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
}

type lnurlPayValues struct {
	PR     string `json:"pr"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// FetchLNURLPayInvoice runs the LNURL-pay flow (LUD-06) against endpoint and
// returns an invoice for exactly amountMsat.
//
// The endpoint is remote and untrusted, and whatever invoice it returns is
// about to be paid from a user's wallet. So the callback must be https, and the
// invoice must be for the requested amount, unexpired, and committed to the
// endpoint's metadata. Anything else is refused before it can be paid.
func FetchLNURLPayInvoice(endpoint string, amountMsat int64, comment string) (string, error) {
	if amountMsat <= 0 {
		return "", fmt.Errorf("amount must be positive")
	}
	var params lnurlPayParams
	if err := getLNURLJSON(endpoint, &params); err != nil {
		return "", err
	}
	if strings.EqualFold(params.Status, "ERROR") {
		return "", fmt.Errorf("lnurl-pay endpoint error: %s", params.Reason)
	}
	if params.Tag != "payRequest" {
		return "", fmt.Errorf("not an LNURL-pay endpoint (tag %q)", params.Tag)
	}
	if amountMsat < params.MinSendable || (params.MaxSendable > 0 && amountMsat > params.MaxSendable) {
		return "", fmt.Errorf("amount %d msat is outside the endpoint's range %d..%d msat", amountMsat, params.MinSendable, params.MaxSendable)
	}
	callback, err := url.Parse(params.Callback)
	if err != nil || callback.Scheme != "https" || callback.Host == "" {
		return "", fmt.Errorf("lnurl-pay callback must be an https URL, got %q", params.Callback)
	}
	q := callback.Query()
	q.Set("amount", strconv.FormatInt(amountMsat, 10))
	if params.CommentAllowed > 0 && comment != "" {
		if len(comment) > params.CommentAllowed {
			comment = comment[:params.CommentAllowed]
		}
		q.Set("comment", comment)
	}
	callback.RawQuery = q.Encode()

	var values lnurlPayValues
	if err := getLNURLJSON(callback.String(), &values); err != nil {
		return "", err
	}
	if strings.EqualFold(values.Status, "ERROR") {
		return "", fmt.Errorf("lnurl-pay callback error: %s", values.Reason)
	}
	if err := checkLNURLPayInvoice(values.PR, amountMsat, params.Metadata); err != nil {
		return "", err
	}
	return values.PR, nil
}

// checkLNURLPayInvoice verifies an invoice returned by an LNURL-pay callback.
func checkLNURLPayInvoice(pr string, amountMsat int64, metadata string) error {
	inv, err := decodepay.Decodepay(pr)
	if err != nil {
		return fmt.Errorf("lnurl-pay returned an invalid invoice: %w", err)
	}
	if inv.MSatoshi != amountMsat {
		return fmt.Errorf("lnurl-pay invoice is for %d msat, %d msat was requested", inv.MSatoshi, amountMsat)
	}
	if InvoiceExpired(inv) {
		return fmt.Errorf("lnurl-pay invoice has already expired")
	}
	sum := sha256.Sum256([]byte(metadata))
	if !strings.EqualFold(inv.DescriptionHash, hex.EncodeToString(sum[:])) {
		return fmt.Errorf("lnurl-pay invoice is not committed to the endpoint's metadata")
	}
	return nil
}

func getLNURLJSON(rawURL string, v interface{}) error {
	resp, err := lnurlHTTP.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLNURLResponse))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("lnurl server returned HTTP %d with invalid JSON: %w", resp.StatusCode, err)
	}
	return nil
}
