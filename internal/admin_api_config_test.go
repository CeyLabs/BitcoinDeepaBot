package internal

import (
	"strings"
	"testing"
)

func TestIsLoopbackAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:6060": true,
		"localhost:6060": true,
		"[::1]:6060":     true,
		"127.0.0.2:6060": true,
		":6060":          false, // every interface
		"0.0.0.0:6060":   false,
		"[::]:6060":      false,
		"10.0.0.5:6060":  false,
		"":               false, // net/http would use :80 on every interface
	} {
		if got := IsLoopbackAddress(addr); got != want {
			t.Errorf("IsLoopbackAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestAdminAPIAllowed(t *testing.T) {
	strong := strings.Repeat("t", 64)
	tests := []struct {
		name, host, token string
		ok                bool
	}{
		{"loopback without token", "127.0.0.1:6060", "", true},
		{"loopback with token", "127.0.0.1:6060", strong, true},
		{"public address with token", "0.0.0.0:6060", strong, true},
		{"public address without token", "0.0.0.0:6060", "", false},
		{"all interfaces without token", ":6060", "", false},
		{"weak token", "127.0.0.1:6060", "short", false},
		{"placeholder token", "127.0.0.1:6060", "change-me-" + strings.Repeat("x", 40), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := AdminAPIAllowed(tt.host, tt.token)
			if (err == nil) != tt.ok {
				t.Fatalf("AdminAPIAllowed(%q, token len %d) = %v, want ok=%v", tt.host, len(tt.token), err, tt.ok)
			}
		})
	}
}
