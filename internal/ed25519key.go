package internal

import (
	"crypto/ed25519"
	"errors"

	"filippo.io/edwards25519"
)

// ValidateEd25519PublicKey reports why b can't be used as an Ed25519 public
// key, or nil. A small-order key (for example 32 zero bytes, a common
// placeholder) lets anyone forge signatures without a private key, because
// ed25519.Verify does not reject such keys. Bytes that are not a valid curve
// point are rejected too.
func ValidateEd25519PublicKey(b []byte) error {
	if len(b) != ed25519.PublicKeySize {
		return errors.New("not 32 bytes")
	}
	p, err := new(edwards25519.Point).SetBytes(b)
	if err != nil {
		return errors.New("not a valid Ed25519 point")
	}
	if new(edwards25519.Point).MultByCofactor(p).Equal(edwards25519.NewIdentityPoint()) == 1 {
		return errors.New("weak key (small order); generate a real key pair")
	}
	return nil
}
