// Package tgoidc verifies ID tokens from "Log In with Telegram" (OpenID
// Connect). Telegram signs them with keys published at JWKSURL, so a valid
// token proves which Telegram user logged in, whoever forwards it.
//
// See https://core.telegram.org/bots/telegram-login
package tgoidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	Issuer  = "https://oauth.telegram.org"
	JWKSURL = "https://oauth.telegram.org/.well-known/jwks.json"

	// maxTokenLen bounds what an unauthenticated caller can make us parse.
	maxTokenLen = 8 << 10
	// clockSkew tolerates small clock differences with Telegram.
	clockSkew = 60 * time.Second
)

// User is the Telegram user an ID token was issued for.
type User struct {
	ID        int64  // numeric Telegram user ID ("id" claim, needs the profile scope)
	Username  string // preferred_username
	Name      string
	FirstName string
	LastName  string
	IssuedAt  time.Time
}

type claims struct {
	Iss               string          `json:"iss"`
	Aud               json.RawMessage `json:"aud"`
	Exp               int64           `json:"exp"`
	Iat               int64           `json:"iat"`
	ID                json.Number     `json:"id"`
	Name              string          `json:"name"`
	GivenName         string          `json:"given_name"`
	FamilyName        string          `json:"family_name"`
	PreferredUsername string          `json:"preferred_username"`
}

// audience reports whether aud (a string or an array of strings) names clientID.
func (c claims) audience(clientID string) bool {
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return one == clientID
	}
	var many []string
	if json.Unmarshal(c.Aud, &many) == nil {
		for _, a := range many {
			if a == clientID {
				return true
			}
		}
	}
	return false
}

// Verify checks token's signature against keys and its claims: issuer,
// audience (the bot ID, clientID), expiry, and that the login happened no more
// than maxAge before now. It returns the Telegram user.
func Verify(token string, keys *KeySet, clientID string, maxAge time.Duration, now time.Time) (User, error) {
	if token == "" || len(token) > maxTokenLen {
		return User{}, errors.New("missing or oversized token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return User{}, errors.New("not a JWT")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return User{}, fmt.Errorf("header: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return User{}, errors.New("bad signature encoding")
	}
	key, err := keys.Key(header.Kid)
	if err != nil {
		return User{}, err
	}
	if err := verifySignature(header.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return User{}, err
	}

	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return User{}, fmt.Errorf("claims: %w", err)
	}
	if c.Iss != Issuer {
		return User{}, fmt.Errorf("wrong issuer %q", c.Iss)
	}
	if clientID == "" || !c.audience(clientID) {
		return User{}, errors.New("token is for another client")
	}
	if c.Exp == 0 || !now.Before(time.Unix(c.Exp, 0).Add(clockSkew)) {
		return User{}, errors.New("token expired")
	}
	issued := time.Unix(c.Iat, 0)
	if c.Iat == 0 || issued.After(now.Add(clockSkew)) {
		return User{}, errors.New("bad issue time")
	}
	if now.Sub(issued) > maxAge {
		return User{}, errors.New("login too old")
	}
	id, err := c.ID.Int64()
	if err != nil || id <= 0 {
		return User{}, errors.New("no Telegram user id (request the profile scope)")
	}
	return User{
		ID: id, Username: c.PreferredUsername, Name: c.Name,
		FirstName: c.GivenName, LastName: c.FamilyName, IssuedAt: issued,
	}, nil
}

func decodeSegment(seg string, v interface{}) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	return dec.Decode(v)
}

// verifySignature checks sig over input for the algorithms Telegram offers.
// The key type must match alg, so a token can't pick a weaker check.
func verifySignature(alg string, key crypto.PublicKey, input, sig []byte) error {
	bad := errors.New("signature check failed")
	switch alg {
	case "RS256":
		k, ok := key.(*rsa.PublicKey)
		if !ok {
			return bad
		}
		h := sha256.Sum256(input)
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, h[:], sig) != nil {
			return bad
		}
	case "ES256":
		k, ok := key.(*ecdsa.PublicKey)
		if !ok || len(sig) != 64 {
			return bad
		}
		h := sha256.Sum256(input)
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k, h[:], r, s) {
			return bad
		}
	case "EdDSA":
		k, ok := key.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(k, input, sig) {
			return bad
		}
	default:
		return fmt.Errorf("unsupported algorithm %q", alg)
	}
	return nil
}
