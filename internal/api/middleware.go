package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/BitcoinDeepaBot/BitcoinDeepaBot/internal"
	"github.com/BitcoinDeepaBot/BitcoinDeepaBot/internal/lnbits"
	"github.com/BitcoinDeepaBot/BitcoinDeepaBot/internal/telegram"
	"gorm.io/gorm"

	log "github.com/sirupsen/logrus"
)

func LoggingMiddleware(prefix string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Tracef("[%s] %s %s", prefix, r.Method, r.URL.Path)
		log.Tracef("[%s]\n%s", prefix, dump(r))
		r.BasicAuth()
		next.ServeHTTP(w, r)
	}
}

type AuthType struct {
	Type    string
	Decoder func(s string) ([]byte, error)
}

var AuthTypeBasic = AuthType{Type: "Basic"}
var AuthTypeBearerBase64 = AuthType{Type: "Bearer", Decoder: base64.StdEncoding.DecodeString}
var AuthTypeNone = AuthType{}

// invoice key or admin key requirement
type AccessKeyType struct {
	Type string
}

var AccessKeyTypeInvoice = AccessKeyType{Type: "invoice"}
var AccessKeyTypeAdmin = AccessKeyType{Type: "admin"}
var AccessKeyTypeNone = AccessKeyType{Type: "none"} // no authorization required

func AuthorizationMiddleware(database *gorm.DB, authType AuthType, accessType AccessKeyType, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if accessType.Type == "none" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		// check if the user is banned
		if auth == "" {
			w.WriteHeader(401)
			log.Warn("[api] no auth")
			return
		}
		_, password, ok := parseAuth(authType, auth)
		if !ok {
			w.WriteHeader(401)
			return
		}
		// first we make sure that the password is not already "banned_"
		if strings.Contains(password, "_") || strings.HasPrefix(password, "banned_") {
			w.WriteHeader(401)
			log.Warnf("[api] Banned user %s. Not forwarding request", password)
			return
		}
		// then we check whether the "normal" password provided is in the database (it should be not if the user is banned)

		user := &lnbits.User{}
		var tx *gorm.DB
		if accessType.Type == "admin" {
			tx = database.Where("wallet_adminkey = ? COLLATE NOCASE", password).First(user)
		} else if accessType.Type == "invoice" {
			tx = database.Where("wallet_inkey = ? OR wallet_adminkey = ? COLLATE NOCASE", password, password).First(user)
		} else {
			log.Errorf("[api] route without access type")
			w.WriteHeader(401)
			return
		}
		if tx.Error != nil {
			log.Warnf("[api] could not load access key: %v", tx.Error)
			w.WriteHeader(401)
			return
		}

		log.Debugf("[api] User: %s Endpoint: %s %s %s", telegram.GetUserStr(user.Telegram), r.Method, r.URL.Path, r.URL.RawQuery)
		r = r.WithContext(context.WithValue(r.Context(), "user", user))
		next.ServeHTTP(w, r)
	}
}

// parseAuth parses an HTTP Basic Authentication string.
// "Bearer QWxhZGRpbjpvcGVuIHNlc2FtZQ==" returns ("Aladdin", "open sesame", true).
func parseAuth(authType AuthType, auth string) (username, password string, ok bool) {
	parse := func(prefix string) (username, password string, ok bool) {
		// Case insensitive prefix match. See Issue 22736.
		if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
			return
		}
		if authType.Decoder != nil {
			c, err := authType.Decoder(auth[len(prefix):])
			if err != nil {
				return
			}
			cs := string(c)
			s := strings.IndexByte(cs, ':')
			if s < 0 {
				return
			}
			return cs[:s], cs[s+1:], true
		}
		return auth[len(prefix):], auth[len(prefix):], true

	}
	return parse(fmt.Sprintf("%s ", authType.Type))

}

func dump(r *http.Request) string {
	x, err := httputil.DumpRequest(r, true)
	if err != nil {
		return ""
	}
	return string(x)
}

// WalletHMACMiddleware validates HMAC signatures for wallet-based API endpoints
// It identifies the sending wallet by validating the signature against each whitelisted wallet's secret
func WalletHMACMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get timestamp from header for replay attack prevention
		timestampStr := r.Header.Get("X-Timestamp")
		if timestampStr == "" {
			log.Warn("Missing timestamp in wallet API request")
			http.Error(w, "Missing timestamp", http.StatusUnauthorized)
			return
		}

		// Parse timestamp
		timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
		if err != nil {
			log.Warn("Invalid timestamp format in wallet API request")
			http.Error(w, "Invalid timestamp", http.StatusBadRequest)
			return
		}

		// Check if request is not too old (prevent replay attacks)
		now := time.Now().Unix()
		tolerance := internal.Configuration.API.Send.TimestampTolerance
		if tolerance == 0 {
			tolerance = 300 // Default 5 minutes
		}

		if now-timestamp > tolerance {
			log.Warnf("Request timestamp too old (age: %d seconds, tolerance: %d)", now-timestamp, tolerance)
			http.Error(w, "Request expired", http.StatusUnauthorized)
			return
		}

		// Get signature from header
		signature := r.Header.Get("X-HMAC-Signature")
		if signature == "" {
			log.Warn("Missing HMAC signature in wallet API request")
			http.Error(w, "Missing signature", http.StatusUnauthorized)
			return
		}

		// Read request body
		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Error("Failed to read request body for HMAC verification: ", err)
			http.Error(w, "Failed to read request", http.StatusBadRequest)
			return
		}

		// Restore body for next handler
		r.Body = io.NopCloser(strings.NewReader(string(body)))

		// Create message to sign: METHOD + PATH + TIMESTAMP + BODY
		message := fmt.Sprintf("%s%s%s%s", r.Method, r.URL.Path, timestampStr, string(body))

		// Try to validate signature against each whitelisted wallet
		var authenticatedWallet string
		for walletID, wallet := range internal.Configuration.API.Send.WhitelistedWallets {
			expectedSignature := calculateHMAC(message, wallet.HMACSecret)
			if hmac.Equal([]byte(signature), []byte(expectedSignature)) {
				authenticatedWallet = walletID
				log.Debugf("HMAC signature verified for wallet: %s", walletID)
				break
			}
		}

		if authenticatedWallet == "" {
			log.Warn("HMAC signature verification failed - no matching wallet found")
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}

		// Add authenticated wallet info to request context
		ctx := context.WithValue(r.Context(), "authenticated_wallet", authenticatedWallet)
		r = r.WithContext(ctx)

		log.Debugf("Wallet API request authenticated for wallet: %s", authenticatedWallet)
		next.ServeHTTP(w, r)
	}
}

// calculateHMAC calculates HMAC-SHA256 signature
func calculateHMAC(message, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(message))
	return hex.EncodeToString(h.Sum(nil))
}

// GenerateHMACSignature helper function for clients
func GenerateHMACSignature(method, path, timestamp, body, secret string) string {
	message := fmt.Sprintf("%s%s%s%s", method, path, timestamp, body)
	return calculateHMAC(message, secret)
}
