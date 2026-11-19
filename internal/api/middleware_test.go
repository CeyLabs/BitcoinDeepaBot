package api

import (
	"testing"
	"time"
)

func TestSignatureSetClaim(t *testing.T) {
	s := &signatureSet{seen: make(map[string]time.Time)}

	if !s.claim("a", time.Minute) {
		t.Fatal("first claim of a = false, want true")
	}
	if s.claim("a", time.Minute) {
		t.Fatal("second claim of a = true, want false")
	}
	if !s.claim("b", time.Minute) {
		t.Fatal("claim of b = false, want true")
	}

	// An expired entry is forgotten, so the set cannot grow without bound.
	if !s.claim("old", -time.Second) {
		t.Fatal("claim of old = false, want true")
	}
	if !s.claim("old", time.Minute) {
		t.Error("claim of expired old = false, want true")
	}
}
