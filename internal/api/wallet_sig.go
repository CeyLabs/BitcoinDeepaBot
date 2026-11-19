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

// Ed25519 request signing for the wallet API.
//
// The bot holds only the wallet's public key, so a leak of the bot's config
// cannot be used to sign requests. Each request carries:
//
//	X-Wallet-ID:  the whitelisted wallet id
//	X-Timestamp:  unix seconds
//	X-Nonce:      16-128 random chars [A-Za-z0-9_-], never reused
//	X-Signature:  base64 Ed25519 signature of Ed25519SigningString(...)
const (
	ed25519SigVersion = "BDB-SEND-ED25519-V1"
	maxSignedBodySize = 1 << 20 // 1 MiB
)

var nonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// Ed25519SigningString is the exact text a client signs. The body is hashed so
// the signed text stays small and unambiguous.
func Ed25519SigningString(method, path, rawQuery, timestamp, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		ed25519SigVersion, method, path, rawQuery, timestamp, nonce, hex.EncodeToString(sum[:]),
	}, "\n")
}

// WalletAuthMiddleware authenticates wallet API requests with an Ed25519
// signature and puts the wallet id in the request context.
func WalletAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestStart := time.Now()
		deny := func(code int, msg, logMsg string, args ...interface{}) {
			log.Warnf("[api/ed25519] "+logMsg, args...)
			http.Error(w, msg, code)
		}

		walletID := r.Header.Get("X-Wallet-ID")
		wallet, ok := internal.Configuration.API.Send.WhitelistedWallets[walletID]
		if !ok || len(wallet.PublicKey) != ed25519.PublicKeySize {
			deny(http.StatusUnauthorized, "Invalid signature", "unknown wallet or no public key: %q", walletID)
			return
		}

		tsStr := r.Header.Get("X-Timestamp")
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			deny(http.StatusUnauthorized, "Invalid timestamp", "bad timestamp for wallet %s", walletID)
			return
		}
		tolerance := internal.Configuration.API.Send.TimestampTolerance
		if tolerance == 0 {
			tolerance = 300
		}
		if age := time.Now().Unix() - ts; age > tolerance || age < -tolerance {
			deny(http.StatusUnauthorized, "Request expired", "timestamp outside tolerance for wallet %s (age %ds)", walletID, age)
			return
		}

		nonce := r.Header.Get("X-Nonce")
		if !nonceRe.MatchString(nonce) {
			deny(http.StatusUnauthorized, "Invalid nonce", "bad nonce for wallet %s", walletID)
			return
		}

		sig, err := internal.DecodeBase64Any(r.Header.Get("X-Signature"))
		if err != nil || len(sig) != ed25519.SignatureSize {
			deny(http.StatusUnauthorized, "Invalid signature", "malformed signature for wallet %s", walletID)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSignedBodySize))
		if err != nil {
			deny(http.StatusRequestEntityTooLarge, "Request too large", "body read failed for wallet %s: %v", walletID, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		msg := Ed25519SigningString(r.Method, r.URL.Path, r.URL.RawQuery, tsStr, nonce, body)
		if !ed25519.Verify(wallet.PublicKey, []byte(msg), sig) {
			deny(http.StatusUnauthorized, "Invalid signature", "signature check failed for wallet %s", walletID)
			return
		}

		// Ed25519 signatures are deterministic, so the nonce (not the
		// signature) is what makes each request single use.
		if !usedSignatures.claim("ed25519:"+walletID+":"+nonce, time.Duration(2*tolerance)*time.Second) {
			deny(http.StatusUnauthorized, "Request already processed", "replayed nonce for wallet %s", walletID)
			return
		}

		ctx := context.WithValue(r.Context(), "authenticated_wallet", walletID)
		ctx = context.WithValue(ctx, requestStartKey, requestStart)
		log.Debugf("[api/ed25519] request authenticated for wallet %s", walletID)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}
