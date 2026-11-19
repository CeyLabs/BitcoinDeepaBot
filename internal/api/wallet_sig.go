package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	log "github.com/sirupsen/logrus"
)

// Ed25519 request signing for the wallet (send) API and the analytics API.
//
// The bot holds only the caller's public key, so a leak of the bot's config
// cannot be used to sign requests. Each request carries:
//
//	X-Wallet-ID / X-Key-ID:  which key signed it
//	X-Timestamp:             unix seconds
//	X-Nonce:                 16-128 random chars [A-Za-z0-9_-], never reused
//	X-Signature:             base64 Ed25519 signature of the signing string
//
// Each API has its own version line, so a signature made for one API is not
// valid on the other.
const (
	ed25519SigVersion   = "BDB-SEND-ED25519-V1"
	analyticsSigVersion = "BDB-ANALYTICS-ED25519-V1"
	maxSignedBodySize   = 1 << 20 // 1 MiB
)

var nonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func signingString(version, method, path, rawQuery, timestamp, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		version, method, path, rawQuery, timestamp, nonce, hex.EncodeToString(sum[:]),
	}, "\n")
}

// Ed25519SigningString is the exact text a wallet API client signs. The body
// is hashed so the signed text stays small and unambiguous.
func Ed25519SigningString(method, path, rawQuery, timestamp, nonce string, body []byte) string {
	return signingString(ed25519SigVersion, method, path, rawQuery, timestamp, nonce, body)
}

// AnalyticsSigningString is the exact text an analytics API client signs.
func AnalyticsSigningString(method, path, rawQuery, timestamp, nonce string, body []byte) string {
	return signingString(analyticsSigVersion, method, path, rawQuery, timestamp, nonce, body)
}

// sigCheck describes one API's signature check.
type sigCheck struct {
	logPrefix string
	idHeader  string
	version   string
	tolerance int64
	key       func(id string) (ed25519.PublicKey, bool)
}

// verify checks the signature on r and returns the signer's id, or answers
// the request and returns "".
func (c sigCheck) verify(w http.ResponseWriter, r *http.Request) string {
	deny := func(code int, msg, logMsg string, args ...interface{}) string {
		log.Warnf(c.logPrefix+" "+logMsg, args...)
		http.Error(w, msg, code)
		return ""
	}

	id := r.Header.Get(c.idHeader)
	key, ok := c.key(id)
	if !ok || len(key) != ed25519.PublicKeySize {
		return deny(http.StatusUnauthorized, "Invalid signature", "unknown key: %q", id)
	}

	tsStr := r.Header.Get("X-Timestamp")
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return deny(http.StatusUnauthorized, "Invalid timestamp", "bad timestamp for %s", id)
	}
	tolerance := c.tolerance
	if tolerance <= 0 {
		tolerance = 300
	}
	if age := time.Now().Unix() - ts; age > tolerance || age < -tolerance {
		return deny(http.StatusUnauthorized, "Request expired", "timestamp outside tolerance for %s (age %ds)", id, age)
	}

	nonce := r.Header.Get("X-Nonce")
	if !nonceRe.MatchString(nonce) {
		return deny(http.StatusUnauthorized, "Invalid nonce", "bad nonce for %s", id)
	}

	sig, err := internal.DecodeBase64Any(r.Header.Get("X-Signature"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return deny(http.StatusUnauthorized, "Invalid signature", "malformed signature for %s", id)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSignedBodySize))
	if err != nil {
		return deny(http.StatusRequestEntityTooLarge, "Request too large", "body read failed for %s: %v", id, err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	msg := signingString(c.version, r.Method, r.URL.Path, r.URL.RawQuery, tsStr, nonce, body)
	if !ed25519.Verify(key, []byte(msg), sig) {
		return deny(http.StatusUnauthorized, "Invalid signature", "signature check failed for %s", id)
	}

	// Ed25519 signatures are deterministic, so the nonce (not the signature)
	// is what makes each request single use.
	if !usedSignatures.claim(c.version+":"+id+":"+nonce, time.Duration(2*tolerance)*time.Second) {
		return deny(http.StatusUnauthorized, "Request already processed", "replayed nonce for %s", id)
	}
	return id
}

// WalletAuthMiddleware authenticates wallet API requests with an Ed25519
// signature and puts the wallet id in the request context.
func WalletAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestStart := time.Now()
		check := sigCheck{
			logPrefix: "[api/ed25519]",
			idHeader:  "X-Wallet-ID",
			version:   ed25519SigVersion,
			tolerance: internal.Configuration.API.Send.TimestampTolerance,
			key: func(id string) (ed25519.PublicKey, bool) {
				wallet, ok := internal.Configuration.API.Send.WhitelistedWallets[id]
				return wallet.PublicKey, ok
			},
		}
		walletID := check.verify(w, r)
		if walletID == "" {
			return
		}
		ctx := context.WithValue(r.Context(), "authenticated_wallet", walletID)
		ctx = context.WithValue(ctx, requestStartKey, requestStart)
		log.Debugf("[api/ed25519] request authenticated for wallet %s", walletID)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// AnalyticsAuthMiddleware authenticates analytics API requests with an
// Ed25519 signature and puts the key id in the request context.
func AnalyticsAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		check := sigCheck{
			logPrefix: "[Analytics]",
			idHeader:  "X-Key-ID",
			version:   analyticsSigVersion,
			tolerance: internal.Configuration.API.Analytics.TimestampTolerance,
			key: func(id string) (ed25519.PublicKey, bool) {
				k, ok := internal.Configuration.API.Analytics.APIKeys[id]
				return k.PublicKey, ok
			},
		}
		keyID := check.verify(w, r)
		if keyID == "" {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), "analytics_api_key", keyID)))
	}
}
