package telegram

import (
	"sync"
	"testing"
	"time"
)

type alertRecorder struct {
	mu    sync.Mutex
	downs int
	ups   int
}

func (r *alertRecorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.downs, r.ups
}

func newTestAlerter(r *alertRecorder) *lnbitsAlerter {
	return &lnbitsAlerter{
		delay:    20 * time.Millisecond,
		sendDown: func(string, time.Time) { r.mu.Lock(); r.downs++; r.mu.Unlock() },
		sendUp:   func(time.Time) { r.mu.Lock(); r.ups++; r.mu.Unlock() },
	}
}

func TestLNbitsAlertSkipsShortOutage(t *testing.T) {
	r := &alertRecorder{}
	a := newTestAlerter(r)
	since := time.Now()
	a.onChange(true, "blip", since)
	a.onChange(false, "", since)
	time.Sleep(60 * time.Millisecond)
	if d, u := r.counts(); d != 0 || u != 0 {
		t.Fatalf("short outage sent %d down / %d up alerts, want none", d, u)
	}
}

func TestLNbitsAlertLongOutage(t *testing.T) {
	r := &alertRecorder{}
	a := newTestAlerter(r)
	since := time.Now()
	a.onChange(true, "connection refused", since)
	time.Sleep(60 * time.Millisecond)
	if d, _ := r.counts(); d != 1 {
		t.Fatalf("down alerts = %d, want 1", d)
	}
	a.onChange(false, "", since)
	time.Sleep(20 * time.Millisecond)
	if d, u := r.counts(); d != 1 || u != 1 {
		t.Fatalf("got %d down / %d up alerts, want 1 / 1", d, u)
	}
}
