package internal

import (
	"crypto/ed25519"
	"errors"

	"filippo.io/edwards25519"
)

// ParseEd25519PublicKey decodes a base64 Ed25519 public key and rejects keys
// that can't be safe. A small-order key (for example 32 zero bytes, a common
// placeholder) lets anyone forge signatures without a private key, because
// ed25519.Verify does not reject such keys. Keys that are not a valid curve
// point are rejected too.
func ParseEd25519PublicKey(s string) (ed25519.PublicKey, error) {
	if s == "" {
		return nil, errors.New("empty")
	}
	b, err := DecodeBase64Any(s)
	if err != nil {
		return nil, errors.New("not base64")
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not 32 bytes")
	}
	p, err := new(edwards25519.Point).SetBytes(b)
	if err != nil {
		return nil, errors.New("not a valid Ed25519 point")
	}
	if new(edwards25519.Point).MultByCofactor(p).Equal(edwards25519.NewIdentityPoint()) == 1 {
		return nil, errors.New("weak key (small order); generate a real key pair")
	}
	return ed25519.PublicKey(b), nil
}
