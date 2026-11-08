package lnbits

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/imroc/req"
	log "github.com/sirupsen/logrus"
)

const (
	// requestTimeout bounds every LNbits call except payments, so users get a
	// fast answer instead of a frozen bot when LNbits stops responding.
	requestTimeout = 15 * time.Second
	// paymentTimeout is long on purpose: a payment can take a while to settle
	// and aborting it early would leave its outcome unknown.
	paymentTimeout = 24 * time.Hour
	// probeInterval is how often the background check pings LNbits.
	probeInterval = 15 * time.Second
	probeTimeout  = 10 * time.Second
)

// Health tracks whether LNbits is reachable. It is updated by every request
// the client makes and by a background probe, so the bot can show a
// maintenance message instead of wrong data (like a 0 balance) when LNbits
// is down.
type Health struct {
	mu        sync.RWMutex
	down      bool
	downSince time.Time
	lastErr   string
	// onChange is called on every switch between up and down, see
	// Client.OnHealthChange.
	onChange func(down bool, reason string, since time.Time)
}

// Available reports whether LNbits is currently reachable. A nil Health is
// always available.
func (h *Health) Available() bool {
	if h == nil {
		return true
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return !h.down
}

func (h *Health) markDown(reason string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastErr = reason
	if h.down {
		return
	}
	h.down = true
	h.downSince = time.Now()
	log.Errorf("[lnbits] LNbits is unreachable, entering maintenance mode: %s", reason)
	if h.onChange != nil {
		h.onChange(true, reason, h.downSince)
	}
}

func (h *Health) markUp() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.down {
		return
	}
	log.Warnf("[lnbits] LNbits is reachable again after %s, leaving maintenance mode", time.Since(h.downSince).Round(time.Second))
	h.down = false
	h.lastErr = ""
	if h.onChange != nil {
		h.onChange(false, "", h.downSince)
	}
}

// isOutageStatus reports whether an HTTP status means LNbits itself is down,
// as opposed to rejecting one request. 502/503/504 come from the reverse proxy
// in front of LNbits when LNbits does not answer.
func isOutageStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// isOutage reports whether a response means LNbits is down. Railway, which
// hosts LNbits, answers with its own 404 and an X-Railway-Fallback: true
// header when the app is stopped, so that 404 does not come from LNbits.
func isOutage(resp *http.Response) bool {
	return isOutageStatus(resp.StatusCode) || resp.Header.Get("X-Railway-Fallback") == "true"
}

// healthTransport records the outcome of every LNbits request in Health.
// Connection errors, timeouts, gateway errors and Railway fallback pages
// mark LNbits as down; any
// other response means LNbits answered, so it is up.
type healthTransport struct {
	next   http.RoundTripper
	health *Health
}

func (t *healthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	switch {
	case err != nil:
		t.health.markDown(err.Error())
	case isOutage(resp):
		t.health.markDown(resp.Status)
	default:
		t.health.markUp()
	}
	return resp, err
}

// newHTTP returns a req instance whose requests are tracked by health.
func newHTTP(health *Health, timeout time.Duration) *req.Req {
	r := req.New()
	r.SetClient(&http.Client{
		Transport: &healthTransport{next: http.DefaultTransport.(*http.Transport).Clone(), health: health},
		Timeout:   timeout,
	})
	return r
}

// Available reports whether LNbits is currently reachable.
func (c *Client) Available() bool {
	return c.health.Available()
}

// OnHealthChange registers fn to be called each time LNbits goes down
// (down=true, with the error) or comes back up (down=false). since is when
// the outage started. fn runs under the health lock, so calls arrive in
// order, but it must return quickly and must not call back into the client.
func (c *Client) OnHealthChange(fn func(down bool, reason string, since time.Time)) {
	if c.health == nil {
		return
	}
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	c.health.onChange = fn
}

// StartHealthCheck pings LNbits in the background until ctx is done, so the
// bot notices an outage, and the recovery, even while no user is active.
func (c *Client) StartHealthCheck(ctx context.Context) {
	probe := newHTTP(c.health, probeTimeout)
	go func() {
		t := time.NewTicker(probeInterval)
		defer t.Stop()
		for {
			// The transport records the result; the response itself is unused.
			// Any answer from LNbits itself, even a 404, proves it is up.
			// Gateway errors and Railway's fallback 404 do not, see isOutage.
			if resp, err := probe.Get(c.url + "/api/v1/health"); err == nil {
				resp.Response().Body.Close()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// httpClient returns the tracked client for normal calls. A Client built
// without NewClient (e.g. in tests) falls back to an untracked one.
func (c Client) httpClient() *req.Req {
	if c.http == nil {
		return newHTTP(nil, requestTimeout)
	}
	return c.http
}

// payClient returns the tracked client for payments, see httpClient.
func (c Client) payClient() *req.Req {
	if c.payHTTP == nil {
		return newHTTP(nil, paymentTimeout)
	}
	return c.payHTTP
}
