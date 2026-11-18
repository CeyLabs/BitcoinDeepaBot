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
