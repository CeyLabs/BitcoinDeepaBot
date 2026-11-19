package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
)

// The Mini App server signs every P2P call with its Ed25519 key, so only it
// can call /api/v1/p2p/* even if the IP allowlist is wrong. The bot stores only
// the public key (api.p2p.service_public_key). Headers:
//
//	X-Timestamp:  unix seconds
//	X-Nonce:      16-128 random chars [A-Za-z0-9_-], never reused
//	X-Signature:  base64 Ed25519 signature of P2PSigningString(...)
const (
	p2pSigVersion  = "BDB-P2P-ED25519-V1"
	p2pMaxBodySize = 16 << 10
	p2pSigMaxSkew  = 300 // seconds
)

var p2pNonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// P2PSigningString is the exact text the Mini App server signs. It covers the
// user credential (init data or ID token) too, so a captured request cannot be
// reused for another user.
func P2PSigningString(method, path, rawQuery, timestamp, nonce, credential string, body []byte) string {
	bodySum := sha256.Sum256(body)
	credSum := sha256.Sum256([]byte(credential))
	return strings.Join([]string{
		p2pSigVersion, method, path, rawQuery, timestamp, nonce,
		hex.EncodeToString(credSum[:]), hex.EncodeToString(bodySum[:]),
	}, "\n")
}

// Body errors that are not signature failures, so the caller can answer
// 413 or 400 instead of 401.
var (
	errP2PBodyTooLarge = errors.New("body too large")
	errP2PBodyRead     = errors.New("could not read body")
)

// verifyP2PServiceSig checks the Mini App server's signature. It reads the body
// (at most p2pMaxBodySize) and puts it back for the handler. On failure it
// returns a reason for the log; the caller answers 401, or 413 / 400 for
// errP2PBodyTooLarge / errP2PBodyRead.
func verifyP2PServiceSig(w http.ResponseWriter, r *http.Request, key ed25519.PublicKey, now time.Time) error {
	if len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("no service key configured")
	}
	tsStr := r.Header.Get("X-Timestamp")
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return fmt.Errorf("bad timestamp")
	}
	if age := now.Unix() - ts; age > p2pSigMaxSkew || age < -p2pSigMaxSkew {
		return fmt.Errorf("timestamp outside tolerance (age %ds)", age)
	}
	nonce := r.Header.Get("X-Nonce")
	if !p2pNonceRe.MatchString(nonce) {
		return fmt.Errorf("bad nonce")
	}
	sig, err := internal.DecodeKeyBase64(r.Header.Get("X-Signature"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("malformed signature")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p2pMaxBodySize))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return errP2PBodyTooLarge
		}
		return fmt.Errorf("%w: %v", errP2PBodyRead, err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	// The path is signed exactly as sent (still percent-encoded), the same
	// rule as the query string.
	msg := P2PSigningString(r.Method, r.URL.EscapedPath(), r.URL.RawQuery, tsStr, nonce, p2pCredential(r), body)
	if !ed25519.Verify(key, []byte(msg), sig) {
		return fmt.Errorf("signature check failed")
	}
	// Keep the nonce until the signed timestamp can no longer pass the check
	// above, plus a second for the whole-second rounding.
	if !usedSignatures.claim("p2p:"+nonce, time.Unix(ts+p2pSigMaxSkew+1, 0).Sub(now)) {
		return fmt.Errorf("replayed nonce")
	}
	return nil
}

// p2pCredential is the user credential the request carries: the Mini App init
// data, or else the web app's ID token.
func p2pCredential(r *http.Request) string {
	if v := r.Header.Get(InitDataHeader); v != "" {
		return v
	}
	return r.Header.Get(IDTokenHeader)
}