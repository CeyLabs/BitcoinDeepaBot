package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
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
// init data too, so a captured request cannot be replayed for another user.
func P2PSigningString(method, path, rawQuery, timestamp, nonce, initData string, body []byte) string {
	bodySum := sha256.Sum256(body)
	initSum := sha256.Sum256([]byte(initData))
	return strings.Join([]string{
		p2pSigVersion, method, path, rawQuery, timestamp, nonce,
		hex.EncodeToString(initSum[:]), hex.EncodeToString(bodySum[:]),
	}, "\n")
}

// verifyP2PServiceSig checks the Mini App server's signature. It reads the body
// (at most p2pMaxBodySize) and puts it back for the handler. On failure it
// returns a reason for the log; the caller answers 401.
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
		return fmt.Errorf("body read: %v", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	msg := P2PSigningString(r.Method, r.URL.Path, r.URL.RawQuery, tsStr, nonce, r.Header.Get(InitDataHeader), body)
	if !ed25519.Verify(key, []byte(msg), sig) {
		return fmt.Errorf("signature check failed")
	}
	if !usedSignatures.claim("p2p:"+nonce, 2*p2pSigMaxSkew*time.Second) {
		return fmt.Errorf("replayed nonce")
	}
	return nil
}
