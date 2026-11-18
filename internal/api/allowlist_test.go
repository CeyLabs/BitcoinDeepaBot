package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/netutil"
)

func withNetworks(t *testing.T, allowed, trusted []string) {
	t.Helper()
	savedSend, savedBot := internal.Configuration.API.Send, internal.Configuration.Bot
	t.Cleanup(func() {
		internal.Configuration.API.Send = savedSend
		internal.Configuration.Bot = savedBot
	})
	var err error
	if internal.Configuration.API.Send.AllowedNets, err = netutil.ParseNetworks(allowed); err != nil {
		t.Fatal(err)
	}
	if internal.Configuration.Bot.TrustedProxyNets, err = netutil.ParseNetworks(trusted); err != nil {
		t.Fatal(err)
	}
}

func serveAllowlist(remote string, headers map[string]string) int {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/send", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	NetworkAllowlistMiddleware(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })(rec, r)
	return rec.Code
}

func TestNetworkAllowlistMiddleware(t *testing.T) {
	t.Run("no allowlist lets everything through", func(t *testing.T) {
		withNetworks(t, nil, []string{"127.0.0.1"})
		if code := serveAllowlist("198.51.100.4:1234", nil); code != http.StatusOK {
			t.Fatalf("got %d, want 200", code)
		}
	})

	withNetworks(t, []string{"203.0.113.7"}, []string{"127.0.0.1"})
	tests := []struct {
		name    string
		remote  string
		headers map[string]string
		want    int
	}{
		{"allowed caller", "203.0.113.7:1234", nil, http.StatusOK},
		{"other caller", "198.51.100.4:1234", nil, http.StatusForbidden},
		{"forged header from outside", "198.51.100.4:1234", map[string]string{"X-Forwarded-For": "203.0.113.7", "X-Real-IP": "203.0.113.7"}, http.StatusForbidden},
		{"allowed caller through local proxy", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.7"}, http.StatusOK},
		{"forged first hop through local proxy", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.7, 198.51.100.4"}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := serveAllowlist(tt.remote, tt.headers); code != tt.want {
				t.Fatalf("got %d, want %d", code, tt.want)
			}
		})
	}
}

func TestWalletHMACMiddlewareIgnoresWeakSecrets(t *testing.T) {
	saved := internal.Configuration.API.Send
	t.Cleanup(func() { internal.Configuration.API.Send = saved })
	internal.Configuration.API.Send.TimestampTolerance = 300
	internal.Configuration.API.Send.WhitelistedWallets = map[string]internal.WhitelistedWallet{
		"empty": {Username: "mallory", HMACSecret: ""},
	}

	// A signature made with an empty key is trivial to compute. It must not
	// authenticate, even if a weak secret reached the configuration somehow.
	r := signedRequest(http.MethodPost, "/api/v1/send", `{"amount":1}`, time.Now().Unix(), "")
	rec, wallet := serveHMAC(r)
	if rec.Code != http.StatusUnauthorized || wallet != "" {
		t.Fatalf("got %d with wallet %q, want 401 and no wallet", rec.Code, wallet)
	}
}
