// Package netutil holds small, dependency-free helpers for working out who is
// on the other end of an HTTP request.
package netutil

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ParseNetworks parses CIDRs and bare IP addresses. A bare address becomes a
// single-host network (/32 or /128).
func ParseNetworks(entries []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP address %q", e)
			}
			bits := 128
			if ip.To4() != nil {
				ip, bits = ip.To4(), 32
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("invalid network %q: %w", e, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// Contains reports whether ip falls inside any of nets.
func Contains(nets []*net.IPNet, ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns the address of the client that made r.
//
// Forwarding headers are written by whoever sends the request, so they are only
// believed when the TCP peer is one of the trusted proxies. In that case
// X-Forwarded-For is read from the right, skipping trusted hops: a client can
// put anything at the start of the header, but each proxy appends the address
// it actually saw at the end. X-Real-IP is used only when there is no
// X-Forwarded-For and the peer is trusted.
//
// With no trusted proxies configured, the TCP peer address is the answer.
func ClientIP(r *http.Request, trusted []*net.IPNet) net.IP {
	peer := remoteIP(r.RemoteAddr)
	if peer == nil || !Contains(trusted, peer) {
		return peer
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(hops[i]))
			if ip == nil {
				// An unparseable hop means the real client cannot be known.
				// Returning nil ("unknown") rather than the proxy's own address
				// keeps a client from posing as the proxy with a garbage header.
				return nil
			}
			if !Contains(trusted, ip) {
				return ip
			}
		}
		return peer
	}
	if xri := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); xri != nil {
		return xri
	}
	return peer
}

// ClientIPString is ClientIP formatted for logs and records.
func ClientIPString(r *http.Request, trusted []*net.IPNet) string {
	if ip := ClientIP(r, trusted); ip != nil {
		return ip.String()
	}
	return "unknown"
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}
