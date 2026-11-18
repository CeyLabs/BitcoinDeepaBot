package storage

import (
	"encoding/json"
	"fmt"
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
