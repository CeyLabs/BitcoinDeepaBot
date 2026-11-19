package internal

import (
	"crypto/ed25519"
	"encoding/base64"
	"net"
	"strings"
	"testing"
)

func TestWeakHMACSecret(t *testing.T) {
	for _, weak := range []string{
		"", "short", strings.Repeat("a", 31),
		"change-me-" + strings.Repeat("a", 40), "CHANGE-ME-" + strings.Repeat("a", 40),
		// placeholders that shipped in config.yaml.example; anyone could read them
		"your-unique-bitcoindeepa-secret-here", "your-unique-ceycube-secret-here", "your-analytics-hmac-secret-here",
		"another-secure-secret-here-xxxxxxxxxxxxx", "placeholder-" + strings.Repeat("b", 30),
	} {
		if !WeakHMACSecret(weak) {
			t.Errorf("WeakHMACSecret(%q) = false, want true", weak)
		}
	}
	if WeakHMACSecret(strings.Repeat("a", 32)) {
		t.Error("32-character secret reported weak")
	}
}

func TestParseTrustedProxies(t *testing.T) {
	def, err := parseTrustedProxies(nil)
	if err != nil || len(def) != 2 || !def[0].Contains(net.ParseIP("127.0.0.1")) {
		t.Fatalf("empty list should default to loopback, got %v, %v", def, err)
	}
	for _, none := range [][]string{{"none"}, {"NONE"}, {" none "}} {
		if n, err := parseTrustedProxies(none); err != nil || n != nil {
			t.Fatalf("%q should trust no proxy, got %v, %v", none, n, err)
		}
	}
	if n, err := parseTrustedProxies([]string{"10.0.0.0/8"}); err != nil || len(n) != 1 {
		t.Fatalf("explicit list not parsed: %v, %v", n, err)
	}
	if _, err := parseTrustedProxies([]string{"none", "127.0.0.1"}); err == nil {
		t.Fatal(`"none" mixed with addresses must be rejected`)
	}
}

func TestValidateAPISend(t *testing.T) {
	pubKey := testPublicKey()
	good := func() APISendConfiguration {
		return APISendConfiguration{WhitelistedWallets: map[string]WhitelistedWallet{
			"dca": {Username: "BitcoinDeepaDCA", Ed25519PublicKey: pubKey},
		}}
	}

	cfg := good()
	if err := validateAPISend(&cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if len(cfg.AllowedNets) != 0 {
		t.Fatalf("no allowed_networks should mean no restriction, got %v", cfg.AllowedNets)
	}
	if len(cfg.WhitelistedWallets["dca"].PublicKey) != 32 {
		t.Fatal("public key not parsed")
	}

	cfg = good()
	cfg.AllowedNetworks = []string{"203.0.113.7", "10.0.0.0/8"}
	if err := validateAPISend(&cfg); err != nil {
		t.Fatalf("valid networks rejected: %v", err)
	}
	if len(cfg.AllowedNets) != 2 || !cfg.AllowedNets[0].Contains(net.ParseIP("203.0.113.7")) {
		t.Fatalf("allowed networks not parsed: %v", cfg.AllowedNets)
	}

	// Unpadded base64 is accepted too.
	cfg = good()
	cfg.WhitelistedWallets["dca"] = WhitelistedWallet{Username: "x", Ed25519PublicKey: strings.TrimRight(pubKey, "=")}
	if err := validateAPISend(&cfg); err != nil {
		t.Fatalf("unpadded key rejected: %v", err)
	}

	bad := map[string]func(*APISendConfiguration){
		"no wallets (the old built-in default is gone)": func(c *APISendConfiguration) { c.WhitelistedWallets = nil },
		"no public key": func(c *APISendConfiguration) {
			c.WhitelistedWallets["dca"] = WhitelistedWallet{Username: "x"}
		},
		"bad public key": func(c *APISendConfiguration) {
			c.WhitelistedWallets["dca"] = WhitelistedWallet{Username: "x", Ed25519PublicKey: "AAAA"}
		},
		"leftover hmac secret": func(c *APISendConfiguration) {
			c.WhitelistedWallets["dca"] = WhitelistedWallet{Username: "x", Ed25519PublicKey: pubKey, HMACSecret: strings.Repeat("s", 64)}
		},
		"missing username": func(c *APISendConfiguration) {
			c.WhitelistedWallets["dca"] = WhitelistedWallet{Ed25519PublicKey: pubKey}
		},
		"invalid network": func(c *APISendConfiguration) { c.AllowedNetworks = []string{"10.0.0.0/99"} },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := good()
			mutate(&cfg)
			if err := validateAPISend(&cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestValidateAPIAnalytics(t *testing.T) {
	pubKey := testPublicKey()
	cfg := APIAnalyticsConfiguration{APIKeys: map[string]AnalyticsAPIKey{"data": {Name: "Data", Ed25519PublicKey: pubKey}}}
	if err := validateAPIAnalytics(&cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if len(cfg.APIKeys["data"].PublicKey) != 32 {
		t.Fatal("public key not parsed")
	}
	for name, k := range map[string]AnalyticsAPIKey{
		"no public key":        {Name: "x"},
		"bad public key":       {Name: "x", Ed25519PublicKey: "AAAA"},
		"leftover hmac secret": {Name: "x", Ed25519PublicKey: pubKey, HMACSecret: strings.Repeat("s", 64)},
	} {
		cfg := APIAnalyticsConfiguration{APIKeys: map[string]AnalyticsAPIKey{"data": k}}
		if err := validateAPIAnalytics(&cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validateAPIAnalytics(&APIAnalyticsConfiguration{}); err == nil {
		t.Error("no keys: accepted")
	}
}

// testPublicKey is a real Ed25519 public key in base64.
func testPublicKey() string {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 7
	return base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
}

// Small-order keys verify forged signatures, so they must be refused.
func TestParseEd25519PublicKeyRejectsWeakKeys(t *testing.T) {
	if _, err := ParseEd25519PublicKey(testPublicKey()); err != nil {
		t.Fatalf("real key rejected: %v", err)
	}
	one := make([]byte, 32)
	one[0] = 1 // the identity point
	order2 := make([]byte, 32)
	for i := range order2 {
		order2[i] = 0xff
	}
	order2[0], order2[31] = 0xec, 0x7f // -1 (order 2)
	for name, b := range map[string][]byte{
		"all zero": make([]byte, 32),
		"identity": one,
		"order 2":  order2,
		"short":    make([]byte, 31),
	} {
		if _, err := ParseEd25519PublicKey(base64.StdEncoding.EncodeToString(b)); err == nil {
			t.Errorf("%s: weak key accepted", name)
		}
	}
	for _, s := range []string{"", "not base64!"} {
		if _, err := ParseEd25519PublicKey(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
