package storage

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/buntdb"
)

// Storable items must provide a function to retrieve the database key
type Storable interface {
	Key() string
}

type DB struct {
	*buntdb.DB
}

func NewBunt(filePath string) *DB {
	db, err := buntdb.Open(filePath)
	if err != nil {
		log.Fatal(err)
	}

	return &DB{db}
}

// Exists checks is storable item exists
func (db *DB) Exists(storable Storable) (ok bool, err error) {
	ok = false
	err = db.View(func(tx *buntdb.Tx) error {
		_, err := tx.Get(storable.Key())
		if err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		if err == buntdb.ErrNotFound {
			return
		}
		return
	}
	ok = true
	return

}

// Get a storable item
func (db *DB) Get(object Storable) error {
	err := db.View(func(tx *buntdb.Tx) error {
		val, err := tx.Get(object.Key())
		if err != nil {
			return err
		}
		err = json.Unmarshal([]byte(val), object)
		if err != nil {
			fmt.Println(err)
			return err
		}
		return nil
	})
	return err
}

// Set a storable item.
func (db *DB) Set(object Storable) error {
	err := db.Update(func(tx *buntdb.Tx) error {
		b, err := json.Marshal(object)
		if err != nil {
			return err
		}
		_, _, err = tx.Set(object.Key(), string(b), nil)

		return err
	})
	return err
}

// GetOrSet returns the value stored under key, or stores value there if the
// key is free. created reports which happened. Check and write are one
// transaction, so concurrent callers with the same key all get the same value.
func (db *DB) GetOrSet(key, value string, ttl time.Duration) (stored string, created bool, err error) {
	err = db.Update(func(tx *buntdb.Tx) error {
		if v, err := tx.Get(key); err == nil {
			stored = v
			return nil
		} else if err != buntdb.ErrNotFound {
			return err
		}
		if _, _, err := tx.Set(key, value, &buntdb.SetOptions{Expires: true, TTL: ttl}); err != nil {
			return err
		}
		stored, created = value, true
		return nil
	})
	return stored, created, err
}

// AddWithinLimit adds amount to the counter under key if the new total stays
// at or below limit, and returns the total after the call. The counter
// expires after ttl. Check and update are one transaction, so concurrent
// callers cannot together exceed the limit. A negative amount gives sats back.
func (db *DB) AddWithinLimit(key string, amount, limit int64, ttl time.Duration) (ok bool, total int64, err error) {
	err = db.Update(func(tx *buntdb.Tx) error {
		var current int64
		if v, err := tx.Get(key); err == nil {
			current, _ = strconv.ParseInt(v, 10, 64)
		} else if err != buntdb.ErrNotFound {
			return err
		}
		next := current + amount
		if next < 0 {
			next = 0
		}
		if amount > 0 && next > limit {
			total = current
			return nil
		}
		if _, _, err := tx.Set(key, strconv.FormatInt(next, 10), &buntdb.SetOptions{Expires: true, TTL: ttl}); err != nil {
			return err
		}
		ok, total = true, next
		return nil
	})
	return ok, total, err
}

// CounterValue reads a counter written by AddWithinLimit; a missing key is 0.
func (db *DB) CounterValue(key string) (int64, error) {
	var n int64
	err := db.View(func(tx *buntdb.Tx) error {
		v, err := tx.Get(key)
		if err == buntdb.ErrNotFound {
			return nil
		} else if err != nil {
			return err
		}
		n, _ = strconv.ParseInt(v, 10, 64)
		return nil
	})
	return n, err
}

// SetWithTTL stores object under its key. It expires after ttl.
func (db *DB) SetWithTTL(object Storable, ttl time.Duration) error {
	b, err := json.Marshal(object)
	if err != nil {
		return err
	}
	return db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(object.Key(), string(b), &buntdb.SetOptions{Expires: true, TTL: ttl})
		return err
	})
}

// SetWithTTLUnless stores object like SetWithTTL, but only if guard does not
// exist. Check and write are one transaction. It reports whether it wrote.
func (db *DB) SetWithTTLUnless(object Storable, guard string, ttl time.Duration) (bool, error) {
	b, err := json.Marshal(object)
	if err != nil {
		return false, err
	}
	wrote := false
	err = db.Update(func(tx *buntdb.Tx) error {
		if _, err := tx.Get(guard); err == nil {
			return nil
		} else if err != buntdb.ErrNotFound {
			return err
		}
		if _, _, err := tx.Set(object.Key(), string(b), &buntdb.SetOptions{Expires: true, TTL: ttl}); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return wrote, err
}

// DeleteIfValue removes key only if it currently holds value, so a caller can
// release a lock it owns without releasing someone else's.
func (db *DB) DeleteIfValue(key, value string) error {
	return db.Update(func(tx *buntdb.Tx) error {
		v, err := tx.Get(key)
		if err == buntdb.ErrNotFound || (err == nil && v != value) {
			return nil
		} else if err != nil {
			return err
		}
		_, err = tx.Delete(key)
		return err
	})
}

// DeleteKey removes key if present. A missing key is not an error.
func (db *DB) DeleteKey(key string) error {
	return db.Update(func(tx *buntdb.Tx) error {
		if _, err := tx.Delete(key); err != nil && err != buntdb.ErrNotFound {
			return err
		}
		return nil
	})
}

// ClaimOnce records key and reports whether this call was the first to do so
// within ttl. The check and the write happen in one transaction, so concurrent
// callers racing on the same key get exactly one true. Use it to make sure an
// event that can be delivered more than once (a webhook, a client retry) is
// acted on once.
func (db *DB) ClaimOnce(key string, ttl time.Duration) (claimed bool, err error) {
	err = db.Update(func(tx *buntdb.Tx) error {
		if _, err := tx.Get(key); err == nil {
			return nil
		} else if err != buntdb.ErrNotFound {
			return err
		}
		if _, _, err := tx.Set(key, time.Now().UTC().Format(time.RFC3339), &buntdb.SetOptions{Expires: true, TTL: ttl}); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}

// Delete a storable item.
func (db *DB) Delete(index string, object Storable) error {
	return db.Update(func(tx *buntdb.Tx) error {
		_, err := tx.Get(object.Key())
		if err != nil {
			return err
		}
		if _, err := tx.Delete(object.Key()); err != nil {
			return err
		}
		// OLD: from gohumble:
		// todo -- not ascend users index
		// var delkeys []string
		// runtime.IgnoreError(
		// 	tx.Ascend(index, func(key, value string) bool {
		// 		if key == object.Key() {
		// 			delkeys = append(delkeys, key)
		// 		}
		// 		return true
		// 	}),
		// )
		// for _, k := range delkeys {
		// 	if _, err := tx.Delete(k); err != nil {
		// 		return err
		// 	}
		// }
		return nil
	})
}
