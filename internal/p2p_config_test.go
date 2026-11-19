package internal

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestSetAPIP2PDefaults(t *testing.T) {
	key := p2pTestServiceKey()

	c := APIP2PConfiguration{Enabled: true, ServicePublicKey: key}
	setAPIP2PDefaults(&c)
	if !c.Enabled || len(c.ServiceKey) != 32 {
		t.Fatalf("valid service key: enabled=%v key=%d bytes", c.Enabled, len(c.ServiceKey))
	}
	if c.InitDataMaxAge != 3600 || c.SpendInitDataMaxAge != 600 {
		t.Fatalf("defaults: init %d, spend %d", c.InitDataMaxAge, c.SpendInitDataMaxAge)
	}

	// The spend window can never be longer than the general one.
	c = APIP2PConfiguration{InitDataMaxAge: 300, SpendInitDataMaxAge: 900}
	setAPIP2PDefaults(&c)
	if c.SpendInitDataMaxAge != 300 {
		t.Fatalf("spend age not clamped: %d", c.SpendInitDataMaxAge)
	}

	for name, k := range map[string]string{"missing": "", "not base64": "!!", "wrong length": "AAAA",
		"weak (all zero)": base64.StdEncoding.EncodeToString(make([]byte, 32))} {
		c := APIP2PConfiguration{Enabled: true, ServicePublicKey: k}
		setAPIP2PDefaults(&c)
		if c.Enabled {
			t.Errorf("%s service key: P2P left enabled", name)
		}
	}

	c = APIP2PConfiguration{Enabled: true, ServicePublicKey: key, AllowedNetworks: []string{"203.0.113.7", "10.0.0.0/8"}}
	setAPIP2PDefaults(&c)
	if !c.Enabled || len(c.AllowedNets) != 2 {
		t.Fatalf("allowed networks: enabled=%v nets=%v", c.Enabled, c.AllowedNets)
	}
	for name, nets := range map[string][]string{"invalid": {"10.0.0.0/99"}, "only blank": {"", " "}} {
		c := APIP2PConfiguration{Enabled: true, ServicePublicKey: key, AllowedNetworks: nets}
		setAPIP2PDefaults(&c)
		if c.Enabled {
			t.Errorf("%s allowed_networks: P2P left enabled", name)
		}
	}
}

func TestSetAPIP2PDefaultsOIDC(t *testing.T) {
	key := p2pTestServiceKey()
	saved := Configuration.Telegram.ApiKey
	t.Cleanup(func() { Configuration.Telegram.ApiKey = saved })
	Configuration.Telegram.ApiKey = "7654321:secret"

	c := APIP2PConfiguration{Enabled: true, ServicePublicKey: key, OIDCEnabled: true}
	setAPIP2PDefaults(&c)
	if !c.OIDCEnabled || c.OIDCClientID != "7654321" {
		t.Fatalf("client id not taken from the bot token: enabled=%v id=%q", c.OIDCEnabled, c.OIDCClientID)
	}

	c = APIP2PConfiguration{Enabled: true, ServicePublicKey: key, OIDCEnabled: true, OIDCClientID: "not-a-number"}
	setAPIP2PDefaults(&c)
	if c.OIDCEnabled || !c.Enabled {
		t.Fatalf("bad client id: oidc=%v p2p=%v, want login off and P2P still on", c.OIDCEnabled, c.Enabled)
	}
}

// p2pTestServiceKey is a real Ed25519 public key in base64.
func p2pTestServiceKey() string {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 9
	return base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
}
