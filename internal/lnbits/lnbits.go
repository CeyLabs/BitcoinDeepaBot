package lnbits

import (
	"fmt"
	"time"

	"github.com/imroc/req"
)

// parseLNbitsError extracts a meaningful error from an LNbits HTTP response.
func parseLNbitsError(resp *req.Resp) Error {
	statusCode := resp.Response().StatusCode
	rawBody := resp.String()

	var reqErr Error
	resp.ToJSON(&reqErr)
	reqErr.StatusCode = statusCode
	if reqErr.Detail == "" && reqErr.Message == "" {
		reqErr.RawBody = rawBody
	}
	return reqErr
}

// NewClient returns a new lnbits api client. Pass your API key, the ACL token
// used for account management, and the url here.
func NewClient(key, adminToken, url string) *Client {
	return &Client{
		url: url,
		// info: this header holds the ADMIN key for the entire API
		// it can be used to create wallets for example
		// if you want to check the balance of a user, use w.Inkey
		// if you want to make a payment, use w.Adminkey
		header: req.Header{
			"Content-Type": "application/json",
			"Accept":       "application/json",
			"X-Api-Key":    key,
		},
		adminHeader: req.Header{
			"Content-Type":  "application/json",
			"Accept":        "application/json",
			"Authorization": "Bearer " + adminToken,
		},
	}
}

// GetUser returns user information
func (c *Client) GetUser(userId string) (user User, err error) {
	resp, err := req.Get(c.url+"/users/api/v1/user/"+userId, c.adminHeader, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}

	err = resp.ToJSON(&user)
	return
}

// CreateUserWithInitialWallet creates new user with initial wallet.
//
// lnbits 1.x creates the account's first wallet itself, named after
// lnbits_default_wallet_name, and its create-user response omits the wallets.
// So the wallet is fetched separately and renamed to walletName, keeping the
// "<telegram id> (<handle>)" convention the existing wallets use.
//
// adminId is unused: lnbits 1.x has no admin_id concept. It is kept so callers
// do not have to change.
func (c *Client) CreateUserWithInitialWallet(userName, walletName, adminId string, email string) (wal User, err error) {
	// email is deliberately not sent: the bot passes a telegram handle here,
	// which lnbits 1.x rejects via is_valid_email_address.
	resp, err := req.Post(c.url+"/users/api/v1/user", c.adminHeader, req.BodyJSON(struct {
		UserName string `json:"username"`
	}{userName}))
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}
	if err = resp.ToJSON(&wal); err != nil {
		return
	}

	wallets, err := c.Wallets(wal)
	if err != nil {
		return
	}
	if len(wallets) == 0 {
		err = fmt.Errorf("lnbits created user %s without a wallet", wal.ID)
		return
	}
	if err = c.renameWallet(wallets[0], walletName); err != nil {
		return
	}

	// the create-user response echoes back only what it was given, so the name
	// the bot stores is the username it asked for.
	wal.Name = userName
	return
}

// renameWallet sets a wallet's name. It authenticates with the wallet's own
// admin key rather than the ACL token, so it stays outside that token's scope.
func (c *Client) renameWallet(w Wallet, name string) error {
	resp, err := req.Patch(c.url+"/api/v1/wallet", req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Adminkey,
	}, req.BodyJSON(struct {
		Name string `json:"name"`
	}{name}))
	if err != nil {
		return err
	}
	if resp.Response().StatusCode >= 300 {
		return parseLNbitsError(resp)
	}
	return nil
}

// CreateWallet creates a new wallet.
//
// adminId is unused, see CreateUserWithInitialWallet.
func (c *Client) CreateWallet(userId, walletName, adminId string) (wal Wallet, err error) {
	resp, err := req.Post(c.url+"/users/api/v1/user/"+userId+"/wallet", c.adminHeader,
		req.BodyJSON(struct {
			Name string `json:"name"`
		}{walletName}))
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}
	err = resp.ToJSON(&wal)
	return
}

// Invoice creates an invoice associated with this wallet.
func (w Wallet) Invoice(params InvoiceParams, c *Client) (lntx Invoice, err error) {
	// custom header with invoice key
	invoiceHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Inkey,
	}
	resp, err := req.Post(c.url+"/api/v1/payments", invoiceHeader, req.BodyJSON(&params))
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}

	err = resp.ToJSON(&lntx)
	return
}

// Info returns wallet information
func (c Client) Info(w Wallet) (wtx Wallet, err error) {
	// custom header with invoice key
	invoiceHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Inkey,
	}
	resp, err := req.Get(c.url+"/api/v1/wallet", invoiceHeader, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}

	err = resp.ToJSON(&wtx)
	return
}

// Payments returns the 60 most recent wallet payments (default behavior).
func (c Client) Payments(w Wallet) (wtx Payments, err error) {
	return c.PaymentsWithOptions(w, 60, 0)
}

// PaymentsWithOptions returns wallet payments with configurable limit and offset.
func (c Client) PaymentsWithOptions(w Wallet, limit, offset int) (wtx Payments, err error) {
	// custom header with invoice key
	invoiceHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Inkey,
	}
	url := fmt.Sprintf("%s/api/v1/payments?limit=%d&offset=%d", c.url, limit, offset)
	resp, err := req.Get(url, invoiceHeader, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}

	err = resp.ToJSON(&wtx)
	return
}

// Payment state of a payment
func (c Client) Payment(w Wallet, payment_hash string) (payment LNbitsPayment, err error) {
	// custom header with invoice key
	invoiceHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Inkey,
	}
	resp, err := req.Get(c.url+fmt.Sprintf("/api/v1/payments/%s", payment_hash), invoiceHeader, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}

	err = resp.ToJSON(&payment)
	return
}

// Wallets returns all wallets belonging to an user
func (c Client) Wallets(w User) (wtx []Wallet, err error) {
	resp, err := req.Get(c.url+"/users/api/v1/user/"+w.ID+"/wallet", c.adminHeader, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}

	err = resp.ToJSON(&wtx)
	return
}

// Pay pays a given invoice with funds from the wallet.
func (w Wallet) Pay(params PaymentParams, c *Client) (wtx Invoice, err error) {
	// custom header with admin key
	adminHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Adminkey,
	}
	r := req.New()
	r.SetTimeout(time.Hour * 24)
	resp, err := r.Post(c.url+"/api/v1/payments", adminHeader, req.BodyJSON(&params))
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		err = parseLNbitsError(resp)
		return
	}

	err = resp.ToJSON(&wtx)
	return
}
