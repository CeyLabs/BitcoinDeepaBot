package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/netutil"
	"github.com/gorilla/mux"
)

func TestPathAllowlistMiddleware(t *testing.T) {
	saved := internal.Configuration.Bot
	t.Cleanup(func() { internal.Configuration.Bot = saved })
	internal.Configuration.Bot.TrustedProxyNets, _ = netutil.ParseNetworks([]string{"127.0.0.1"})

	var allowed []*net.IPNet
	router := mux.NewRouter()
	router.Use(PathAllowlistMiddleware("/api/v1/", func() []*net.IPNet { return allowed }))
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	for _, p := range []string{"/api/v1/send", "/api/v1/balance", "/.well-known/lnurlp/{u}", "/health", "/lndhub/ext"} {
		router.HandleFunc(p, ok)
	}
	serve := func(path, remote string, headers map[string]string) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = remote
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec.Code
	}

	if code := serve("/api/v1/send", "198.51.100.4:1", nil); code != http.StatusOK {
		t.Fatalf("no allowlist: got %d, want 200", code)
	}

	allowed, _ = netutil.ParseNetworks([]string{"208.77.246.15"})
	tests := []struct {
		name, path, remote string
		headers            map[string]string
		want               int
	}{
		{"allowed caller, send", "/api/v1/send", "208.77.246.15:1", nil, http.StatusOK},
		{"allowed caller, wallet api", "/api/v1/balance", "208.77.246.15:1", nil, http.StatusOK},
		{"other caller, send", "/api/v1/send", "198.51.100.4:1", nil, http.StatusForbidden},
		{"other caller, wallet api", "/api/v1/balance", "198.51.100.4:1", nil, http.StatusForbidden},
		{"forged header from outside", "/api/v1/send", "198.51.100.4:1", map[string]string{"X-Forwarded-For": "208.77.246.15"}, http.StatusForbidden},
		{"allowed caller via local proxy", "/api/v1/send", "127.0.0.1:9", map[string]string{"X-Forwarded-For": "208.77.246.15"}, http.StatusOK},
		{"other caller via local proxy", "/api/v1/send", "127.0.0.1:9", map[string]string{"X-Forwarded-For": "198.51.100.4"}, http.StatusForbidden},
		{"lightning address stays open", "/.well-known/lnurlp/alice", "198.51.100.4:1", nil, http.StatusOK},
		{"health stays open", "/health", "198.51.100.4:1", nil, http.StatusOK},
		{"lndhub stays open", "/lndhub/ext", "198.51.100.4:1", nil, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := serve(tt.path, tt.remote, tt.headers); code != tt.want {
				t.Fatalf("got %d, want %d", code, tt.want)
			}
		})
	}
}
