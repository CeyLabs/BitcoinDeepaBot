package internal

import (
	"encoding/base64"
	"testing"
)

func TestSetAPIP2PDefaults(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))

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

	for name, k := range map[string]string{"missing": "", "not base64": "!!", "wrong length": "AAAA"} {
		c := APIP2PConfiguration{Enabled: true, ServicePublicKey: k}
		setAPIP2PDefaults(&c)
		if c.Enabled {
			t.Errorf("%s service key: P2P left enabled", name)
		}
	}
}
