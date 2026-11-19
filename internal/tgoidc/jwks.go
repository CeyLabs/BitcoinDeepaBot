package tgoidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	// keysMaxAge: cached keys are refetched after this long.
	keysMaxAge = time.Hour
	// refetchGap: an unknown key ID triggers a refetch at most this often,
	// so junk tokens can't make us hammer Telegram.
	refetchGap = 30 * time.Second
	// maxJWKSSize bounds the key set response.
	maxJWKSSize = 1 << 20
)

// KeySet holds Telegram's signing keys, fetched from URL and cached.
type KeySet struct {
	URL    string
	Client *http.Client

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time // last successful fetch
	tried   time.Time // last attempt
}

// Default is the key set for Telegram's production endpoint.
var Default = &KeySet{URL: JWKSURL, Client: &http.Client{Timeout: 5 * time.Second}}

// Key returns the public key with this key ID. It refetches the key set when
// the cache is old, or when kid is unknown (key rotation).
func (k *KeySet) Key(kid string) (crypto.PublicKey, error) {
	if kid == "" {
		return nil, errors.New("token has no key id")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	key, ok := k.keys[kid]
	stale := now.Sub(k.fetched) > keysMaxAge
	if (!ok || stale) && now.Sub(k.tried) >= refetchGap {
		k.tried = now
		if keys, err := k.fetch(); err == nil {
			k.keys, k.fetched = keys, now
			key, ok = keys[kid]
		} else if !ok {
			return nil, fmt.Errorf("could not load Telegram keys: %w", err)
		}
	}
	if !ok {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	return key, nil
}

func (k *KeySet) fetch() (map[string]crypto.PublicKey, error) {
	client := k.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Get(k.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSSize))
	if err != nil {
		return nil, err
	}
	return ParseJWKS(body)
}

// jwk is one JSON Web Key. Only the fields for RSA, EC P-256 and Ed25519.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// ParseJWKS reads a JSON Web Key Set. Keys of unsupported types are skipped.
func ParseJWKS(data []byte) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	keys := make(map[string]crypto.PublicKey)
	for _, j := range set.Keys {
		if j.Kid == "" || (j.Use != "" && j.Use != "sig") {
			continue
		}
		if key, err := j.publicKey(); err == nil {
			keys[j.Kid] = key
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks: no usable keys")
	}
	return keys, nil
}

func (j jwk) publicKey() (crypto.PublicKey, error) {
	b64 := base64.RawURLEncoding.DecodeString
	switch j.Kty {
	case "RSA":
		n, err1 := b64(j.N)
		e, err2 := b64(j.E)
		if err1 != nil || err2 != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad RSA key") // also rejects keys under 2048 bits
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		if j.Crv != "P-256" {
			return nil, errors.New("unsupported curve")
		}
		x, err1 := b64(j.X)
		y, err2 := b64(j.Y)
		if err1 != nil || err2 != nil {
			return nil, errors.New("bad EC key")
		}
		key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !key.Curve.IsOnCurve(key.X, key.Y) {
			return nil, errors.New("EC point not on curve")
		}
		return key, nil
	case "OKP":
		x, err := b64(j.X)
		if j.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("bad OKP key")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, errors.New("unsupported key type")
}
