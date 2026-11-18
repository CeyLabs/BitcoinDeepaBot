package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBearerTokenMiddleware(t *testing.T) {
	token := strings.Repeat("k", 64)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	serve := func(mwToken, header string) int {
		r := httptest.NewRequest(http.MethodGet, "/admin/ban/1", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		BearerTokenMiddleware(mwToken)(ok).ServeHTTP(rec, r)
		return rec.Code
	}

	if code := serve("", ""); code != http.StatusOK {
		t.Fatalf("no token configured: got %d, want 200", code)
	}
	for header, want := range map[string]int{
		"Bearer " + token:       http.StatusOK,
		"":                      http.StatusUnauthorized,
		"Bearer wrong":          http.StatusUnauthorized,
		"Bearer " + token[:63]:  http.StatusUnauthorized,
		token:                   http.StatusUnauthorized,
		"Basic " + token:        http.StatusUnauthorized,
		"Bearer " + token + "x": http.StatusUnauthorized,
	} {
		if code := serve(token, header); code != want {
			t.Errorf("Authorization %q: got %d, want %d", header, code, want)
		}
	}
}
