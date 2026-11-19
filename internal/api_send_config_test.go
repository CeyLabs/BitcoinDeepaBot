package internal

import (
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
	pubKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
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
