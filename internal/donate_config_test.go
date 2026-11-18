package internal

import "testing"

func TestDonationEndpoint(t *testing.T) {
	ok := map[string]string{
		"":                              "",
		"donate@bitcoindeepa.com":       "https://bitcoindeepa.com/.well-known/lnurlp/donate",
		"Donate@BitcoinDeepa.COM":       "https://bitcoindeepa.com/.well-known/lnurlp/Donate",
		"https://pay.example.com/lnurl": "https://pay.example.com/lnurl",
	}
	for in, want := range ok {
		got, err := DonationEndpoint(in)
		if err != nil || got != want {
			t.Errorf("DonationEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://insecure.example.com/lnurl", "nodomain@", "@nouser", "noat", "a@b/c", "a b@c.com", "ftp://x.com"} {
		if got, err := DonationEndpoint(bad); err == nil {
			t.Errorf("DonationEndpoint(%q) accepted: %q", bad, got)
		}
	}
}

func TestConfigPath(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
		want string
	}{
		{"default", nil, "", "config.yaml"},
		{"env", nil, "/etc/btcdeepa/config.yaml", "/etc/btcdeepa/config.yaml"},
		{"flag with space", []string{"--config", "/etc/a.yaml"}, "/env.yaml", "/etc/a.yaml"},
		{"flag with equals", []string{"--config=/etc/b.yaml"}, "", "/etc/b.yaml"},
		{"single dash", []string{"-config", "/etc/c.yaml"}, "", "/etc/c.yaml"},
		{"test flags ignored", []string{"-test.v", "-test.run=X"}, "", "config.yaml"},
		{"dangling flag falls back", []string{"--config"}, "", "config.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configPath(tt.args, tt.env); got != tt.want {
				t.Fatalf("configPath = %q, want %q", got, tt.want)
			}
		})
	}
}
