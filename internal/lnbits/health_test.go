package lnbits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthTracksRequests(t *testing.T) {
	var status int32
	atomic.StoreInt32(&status, http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&status)))
	}))
	defer srv.Close()

	c := NewClient("key", "token", srv.URL)
	w := Wallet{Inkey: "k"}

	steps := []struct {
		name   string
		status int
		up     bool
	}{
		{"ok response", http.StatusOK, true},
		{"gateway error means down", http.StatusBadGateway, false},
		{"plain 404 means up again", http.StatusNotFound, true},
		{"503 means down", http.StatusServiceUnavailable, false},
		{"request error is not an outage", http.StatusBadRequest, true},
		{"504 means down", http.StatusGatewayTimeout, false},
		{"500 is a request error, not an outage", http.StatusInternalServerError, true},
	}
	for _, s := range steps {
		atomic.StoreInt32(&status, int32(s.status))
		c.Info(w)
		if got := c.Available(); got != s.up {
			t.Fatalf("%s: Available() = %v, want %v", s.name, got, s.up)
		}
	}
}

func TestHealthMarksDownWhenUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient("key", "token", url)
	if _, err := c.Info(Wallet{Inkey: "k"}); err == nil {
		t.Fatal("expected an error from a closed server")
	}
	if c.Available() {
		t.Fatal("Available() = true after connection error, want false")
	}
}

func TestHealthCheckRecovers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			t.Errorf("probe hit %s, want /api/v1/health", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient("key", "token", srv.URL)
	c.health.markDown("test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartHealthCheck(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for !c.Available() {
		if time.Now().After(deadline) {
			t.Fatal("health check did not mark LNbits as up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestZeroClientIsAvailable(t *testing.T) {
	if !(&Client{}).Available() {
		t.Fatal("a Client without health tracking should report available")
	}
}
