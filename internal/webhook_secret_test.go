package internal

import (
	"net/url"
	"strings"
	"testing"
)

func TestWithWebhookSecret(t *testing.T) {
	secret := strings.Repeat("a", 64)
	tests := []struct {
		name, base, want string
	}{
		{"plain url", "https://bot.example/webhook", "https://bot.example/webhook?s=" + secret},
		{"keeps existing query", "https://bot.example/webhook?x=1", "https://bot.example/webhook?s=" + secret + "&x=1"},
		{"host only", "http://127.0.0.1:5588", "http://127.0.0.1:5588?s=" + secret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withWebhookSecret(tt.base, secret)
			if got != tt.want {
				t.Fatalf("withWebhookSecret = %q, want %q", got, tt.want)
			}
			u, err := url.Parse(got)
			if err != nil || u.Query().Get(WebhookSecretParam) != secret {
				t.Fatalf("secret not recoverable from %q", got)
			}
		})
	}
	if got := withWebhookSecret("https://bot.example/webhook", ""); got != "https://bot.example/webhook" {
		t.Fatalf("empty secret changed the url: %q", got)
	}
}

func TestSecretMatches(t *testing.T) {
	secret := strings.Repeat("a", 64)
	tests := []struct {
		name      string
		got, want string
		ok        bool
	}{
		{"no secret configured lets requests through to verification", "", "", true},
		{"correct secret", secret, secret, true},
		{"missing secret", "", secret, false},
		{"wrong secret", strings.Repeat("b", 64), secret, false},
		{"prefix of secret", secret[:63], secret, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secretMatches(tt.got, tt.want); got != tt.ok {
				t.Fatalf("secretMatches = %v, want %v", got, tt.ok)
			}
		})
	}
}

func TestCheckWebhookSecret(t *testing.T) {
	for _, ok := range []string{"", strings.Repeat("a", 32), strings.Repeat("f", 64)} {
		if err := checkWebhookSecret(ok); err != nil {
			t.Fatalf("secret of length %d rejected: %v", len(ok), err)
		}
	}
	for _, bad := range []string{"short", strings.Repeat("a", 31), "change-me-" + strings.Repeat("a", 40)} {
		if err := checkWebhookSecret(bad); err == nil {
			t.Fatalf("weak secret %q accepted", bad)
		}
	}
}
