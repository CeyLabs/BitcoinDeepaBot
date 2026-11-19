package tgoidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var b64 = base64.RawURLEncoding.EncodeToString

type testKeys struct {
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
	ed  ed25519.PrivateKey
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, d, _ := ed25519.GenerateKey(rand.Reader)
	return testKeys{rsa: r, ec: e, ed: d}
}

func (k testKeys) jwks() []byte {
	pad := func(b *big.Int) string { out := make([]byte, 32); b.FillBytes(out); return b64(out) }
	set := map[string]interface{}{"keys": []map[string]string{
		{"kty": "RSA", "kid": "rsa1", "use": "sig", "n": b64(k.rsa.N.Bytes()), "e": b64(big.NewInt(int64(k.rsa.E)).Bytes())},
		{"kty": "EC", "kid": "ec1", "crv": "P-256", "x": pad(k.ec.X), "y": pad(k.ec.Y)},
		{"kty": "OKP", "kid": "ed1", "crv": "Ed25519", "x": b64(k.ed.Public().(ed25519.PublicKey))},
	}}
	b, _ := json.Marshal(set)
	return b
}

// sign builds a token. alg picks the key: RS256 rsa1, ES256 ec1, EdDSA ed1.
func (k testKeys) sign(t *testing.T, alg, kid string, claims map[string]interface{}) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	input := b64(h) + "." + b64(c)
	var sig []byte
	switch alg {
	case "RS256":
		d := sha256.Sum256([]byte(input))
		sig, _ = rsa.SignPKCS1v15(rand.Reader, k.rsa, crypto.SHA256, d[:])
	case "ES256":
		d := sha256.Sum256([]byte(input))
		r, s, _ := ecdsa.Sign(rand.Reader, k.ec, d[:])
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	case "EdDSA":
		sig = ed25519.Sign(k.ed, []byte(input))
	}
	return input + "." + b64(sig)
}

func serveKeys(t *testing.T, keys testKeys) (*KeySet, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(keys.jwks())
	}))
	t.Cleanup(srv.Close)
	return &KeySet{URL: srv.URL, Client: srv.Client()}, &hits
}

func goodClaims(now time.Time) map[string]interface{} {
	return map[string]interface{}{
		"iss": Issuer, "aud": "123456", "sub": "pairwise-xyz",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(59 * time.Minute).Unix(),
		"id": 987654321, "name": "Alice Doe", "given_name": "Alice", "family_name": "Doe", "preferred_username": "alice",
	}
}

func TestVerify(t *testing.T) {
	keys := newTestKeys(t)
	ks, _ := serveKeys(t, keys)
	now := time.Now()
	maxAge := 10 * time.Minute

	for _, c := range []struct{ alg, kid string }{{"RS256", "rsa1"}, {"ES256", "ec1"}, {"EdDSA", "ed1"}} {
		u, err := Verify(keys.sign(t, c.alg, c.kid, goodClaims(now)), ks, "123456", maxAge, now)
		if err != nil {
			t.Fatalf("%s: valid token rejected: %v", c.alg, err)
		}
		if u.ID != 987654321 || u.Username != "alice" || u.FirstName != "Alice" {
			t.Fatalf("%s: user %+v", c.alg, u)
		}
	}

	// aud may be an array.
	cl := goodClaims(now)
	cl["aud"] = []string{"other", "123456"}
	if _, err := Verify(keys.sign(t, "RS256", "rsa1", cl), ks, "123456", maxAge, now); err != nil {
		t.Fatalf("array aud rejected: %v", err)
	}

	mutate := func(f func(map[string]interface{})) string {
		c := goodClaims(now)
		f(c)
		return keys.sign(t, "RS256", "rsa1", c)
	}
	good := keys.sign(t, "RS256", "rsa1", goodClaims(now))
	parts := strings.Split(good, ".")
	other := newTestKeys(t)

	bad := map[string]string{
		"empty":            "",
		"not a jwt":        "abc.def",
		"wrong issuer":     mutate(func(c map[string]interface{}) { c["iss"] = "https://evil.example" }),
		"wrong audience":   mutate(func(c map[string]interface{}) { c["aud"] = "999" }),
		"expired":          mutate(func(c map[string]interface{}) { c["exp"] = now.Add(-2 * time.Minute).Unix() }),
		"login too old":    mutate(func(c map[string]interface{}) { c["iat"] = now.Add(-11 * time.Minute).Unix() }),
		"issued in future": mutate(func(c map[string]interface{}) { c["iat"] = now.Add(5 * time.Minute).Unix() }),
		"no id claim":      mutate(func(c map[string]interface{}) { delete(c, "id") }),
		"no exp":           mutate(func(c map[string]interface{}) { delete(c, "exp") }),
		"claims tampered":  parts[0] + "." + b64([]byte(`{"iss":"https://oauth.telegram.org","aud":"123456","iat":1,"exp":9999999999,"id":1}`)) + "." + parts[2],
		"signed by stranger": other.sign(t, "RS256", "rsa1", goodClaims(now)),
		"unknown kid":        keys.sign(t, "RS256", "nope", goodClaims(now)),
		"alg none":           b64([]byte(`{"alg":"none","kid":"rsa1"}`)) + "." + parts[1] + ".",
		"alg/key mismatch":   keys.sign(t, "EdDSA", "rsa1", goodClaims(now)),
		"HS256":              b64([]byte(`{"alg":"HS256","kid":"rsa1"}`)) + "." + parts[1] + "." + parts[2],
	}
	for name, tok := range bad {
		if _, err := Verify(tok, ks, "123456", maxAge, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Verify(good, ks, "", maxAge, now); err == nil {
		t.Error("empty client id accepted")
	}
}

// Keys are cached; an unknown kid refetches at most once per refetchGap.
func TestKeySetCaching(t *testing.T) {
	keys := newTestKeys(t)
	ks, hits := serveKeys(t, keys)
	for i := 0; i < 5; i++ {
		if _, err := ks.Key("rsa1"); err != nil {
			t.Fatal(err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d fetches for a cached key, want 1", n)
	}
	for i := 0; i < 5; i++ {
		if _, err := ks.Key("unknown"); err == nil {
			t.Fatal("unknown kid found")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d fetches after unknown kids within the gap, want 1", n)
	}
}

func TestParseJWKSSkipsBadKeys(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	data, _ := json.Marshal(map[string]interface{}{"keys": []map[string]string{
		{"kty": "RSA", "kid": "weak", "n": b64(small.N.Bytes()), "e": "AQAB"},
		{"kty": "EC", "kid": "offcurve", "crv": "P-256", "x": b64(make([]byte, 32)), "y": b64(make([]byte, 32))},
		{"kty": "oct", "kid": "sym", "k": "c2VjcmV0"},
	}})
	if _, err := ParseJWKS(data); err == nil {
		t.Fatal("key set with only bad keys accepted")
	}
}
