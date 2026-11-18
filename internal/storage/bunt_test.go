package storage

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/buntdb"
)

func newMemDB(t *testing.T) *DB {
	t.Helper()
	db, err := buntdb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &DB{db}
}

func TestClaimOnce(t *testing.T) {
	db := newMemDB(t)

	first, err := db.ClaimOnce("k1", time.Hour)
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v; want true, nil", first, err)
	}
	again, err := db.ClaimOnce("k1", time.Hour)
	if err != nil || again {
		t.Fatalf("repeat claim = %v, %v; want false, nil", again, err)
	}
	other, err := db.ClaimOnce("k2", time.Hour)
	if err != nil || !other {
		t.Fatalf("claim of another key = %v, %v; want true, nil", other, err)
	}
}

func TestClaimOnceExpires(t *testing.T) {
	db := newMemDB(t)
	if ok, _ := db.ClaimOnce("k", 50*time.Millisecond); !ok {
		t.Fatal("first claim failed")
	}
	time.Sleep(150 * time.Millisecond)
	if ok, err := db.ClaimOnce("k", time.Hour); err != nil || !ok {
		t.Fatalf("claim after expiry = %v, %v; want true, nil", ok, err)
	}
}

func TestClaimOnceConcurrent(t *testing.T) {
	db := newMemDB(t)
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := db.ClaimOnce("same", time.Hour); err == nil && ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d concurrent claims succeeded, want exactly 1", wins)
	}
}

func TestGetOrSet(t *testing.T) {
	db := newMemDB(t)
	v, created, err := db.GetOrSet("idem:1", "transfer-a", time.Hour)
	if err != nil || !created || v != "transfer-a" {
		t.Fatalf("first = %q, %v, %v", v, created, err)
	}
	v, created, err = db.GetOrSet("idem:1", "transfer-b", time.Hour)
	if err != nil || created || v != "transfer-a" {
		t.Fatalf("repeat = %q, %v, %v; want the first value back", v, created, err)
	}
}

func TestAddWithinLimit(t *testing.T) {
	db := newMemDB(t)
	steps := []struct {
		add       int64
		wantOK    bool
		wantTotal int64
	}{
		{300, true, 300},
		{200, true, 500},  // exactly at the limit
		{1, false, 500},   // over the limit: refused, unchanged
		{-200, true, 300}, // give back
		{-999, true, 0},   // never below zero
	}
	for i, s := range steps {
		ok, total, err := db.AddWithinLimit("daily:1", s.add, 500, time.Hour)
		if err != nil || ok != s.wantOK || total != s.wantTotal {
			t.Fatalf("step %d add %d: got ok=%v total=%d err=%v, want ok=%v total=%d", i, s.add, ok, total, err, s.wantOK, s.wantTotal)
		}
	}
	if n, _ := db.CounterValue("daily:1"); n != 0 {
		t.Fatalf("CounterValue = %d, want 0", n)
	}
}

func TestAddWithinLimitConcurrent(t *testing.T) {
	db := newMemDB(t)
	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if granted, _, err := db.AddWithinLimit("daily:2", 100, 1000, time.Hour); err == nil && granted {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 10 {
		t.Fatalf("%d of 50 concurrent 100-sat sends fit under a 1000 limit, want exactly 10", ok)
	}
}

func TestDeleteKey(t *testing.T) {
	db := newMemDB(t)
	db.ClaimOnce("lock", time.Hour)
	if err := db.DeleteKey("lock"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteKey("lock"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
	if ok, _ := db.ClaimOnce("lock", time.Hour); !ok {
		t.Fatal("key still present after delete")
	}
}
