package netutil

import (
	"net"
	"net/http/httptest"
	"testing"
)

func mustNets(t *testing.T, entries ...string) []*net.IPNet {
	t.Helper()
	n, err := ParseNetworks(entries)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParseNetworks(t *testing.T) {
	n := mustNets(t, "10.0.0.0/8", "203.0.113.7", "::1", " ", "2001:db8::/32")
	if len(n) != 4 {
		t.Fatalf("got %d networks, want 4", len(n))
	}
	for ip, want := range map[string]bool{
		"10.1.2.3": true, "203.0.113.7": true, "203.0.113.8": false,
		"::1": true, "2001:db8::5": true, "192.168.1.1": false,
	} {
		if got := Contains(n, net.ParseIP(ip)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
	for _, bad := range []string{"10.0.0.0/33", "not-an-ip", "1.2.3"} {
		if _, err := ParseNetworks([]string{bad}); err == nil {
			t.Errorf("ParseNetworks(%q) accepted bad input", bad)
		}
	}
}

func TestClientIP(t *testing.T) {
	loopback := mustNets(t, "127.0.0.1", "::1")
	tests := []struct {
		name    string
		remote  string
		xff     string
		xri     string
		trusted []*net.IPNet
		want    string
	}{
		{name: "direct client", remote: "198.51.100.4:5555", want: "198.51.100.4"},
		{name: "headers ignored without trusted proxies", remote: "198.51.100.4:5555", xff: "10.0.0.1", xri: "10.0.0.2", want: "198.51.100.4"},
		{name: "headers ignored from untrusted peer", remote: "198.51.100.4:5555", xff: "10.0.0.1", trusted: loopback, want: "198.51.100.4"},
		{name: "proxy adds real client", remote: "127.0.0.1:40000", xff: "198.51.100.4", trusted: loopback, want: "198.51.100.4"},
		// The attacker sends "X-Forwarded-For: 10.0.0.1"; nginx appends the
		// address it saw. The spoofed first entry must lose.
		{name: "spoofed first entry loses", remote: "127.0.0.1:40000", xff: "10.0.0.1, 198.51.100.4", trusted: loopback, want: "198.51.100.4"},
		{name: "skips trusted hops", remote: "127.0.0.1:40000", xff: "198.51.100.4, 127.0.0.1", trusted: loopback, want: "198.51.100.4"},
		{name: "garbage hop stops at peer", remote: "127.0.0.1:40000", xff: "198.51.100.4, garbage", trusted: loopback, want: "127.0.0.1"},
		{name: "x-real-ip from trusted proxy", remote: "127.0.0.1:40000", xri: "198.51.100.9", trusted: loopback, want: "198.51.100.9"},
		{name: "ipv6 peer", remote: "[2001:db8::1]:443", want: "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				r.Header.Set("X-Real-IP", tt.xri)
			}
			if got := ClientIPString(r, tt.trusted); got != tt.want {
				t.Fatalf("ClientIP = %s, want %s", got, tt.want)
			}
		})
	}
}
