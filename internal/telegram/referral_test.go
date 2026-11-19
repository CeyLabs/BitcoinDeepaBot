package telegram

import (
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// openReferralTestDB spins up a fresh on-disk sqlite DB and migrates ReferralEntry,
// mirroring what AutoMigration() does for the referrals database.
func openReferralTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "referrals_test.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	closeOnCleanup(t, db)
	if err := migrateReferrals(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// closeOnCleanup closes the database when the test ends, so its file can be
// removed (Windows refuses to delete an open file).
func closeOnCleanup(t *testing.T, db *gorm.DB) {
	t.Helper()
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { sqlDB.Close() })
	}
}

// upsert goes through the same function startHandler uses.
func upsert(db *gorm.DB, telegramID int64, username, code string) error {
	return RecordReferral(db, ReferralEntry{TelegramID: telegramID, Username: username, ReferralCode: code})
}

// legacyReferralEntry is the schema before the unique index existed.
type legacyReferralEntry struct {
	ID           uint  `gorm:"primaryKey;autoIncrement"`
	TelegramID   int64 `gorm:"index"`
	Username     string
	ReferralCode string
	CreatedAt    time.Time
}

func (legacyReferralEntry) TableName() string { return "referral_entries" }

// A database written by the old code has duplicate rows. Migrating it must
// keep the most recent row per (user, code), then enforce uniqueness.
func TestReferralMigrationCollapsesExistingDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "referrals_legacy.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	closeOnCleanup(t, db)
	if err := db.AutoMigrate(&legacyReferralEntry{}); err != nil {
		t.Fatalf("legacy migrate: %v", err)
	}
	for _, e := range []legacyReferralEntry{
		{TelegramID: 1, Username: "", ReferralCode: "PROMO"},
		{TelegramID: 1, Username: "alice", ReferralCode: "PROMO"}, // latest click for (1, PROMO)
		{TelegramID: 1, Username: "alice", ReferralCode: "SALE"},
		{TelegramID: 2, Username: "bob", ReferralCode: "PROMO"},
		{TelegramID: 2, Username: "bob", ReferralCode: "PROMO"},
	} {
		if err := db.Create(&e).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := migrateReferrals(db); err != nil {
		t.Fatalf("migrate with existing duplicates: %v", err)
	}
	if got := countRows(t, db); got != 3 {
		t.Fatalf("expected 3 rows after collapsing duplicates, got %d", got)
	}
	var e ReferralEntry
	if err := db.Where("telegram_id = ? AND referral_code = ?", 1, "PROMO").First(&e).Error; err != nil {
		t.Fatal(err)
	}
	if e.Username != "alice" {
		t.Fatalf("expected the most recent row to survive, got username %q", e.Username)
	}
	// The unique index is now in place: a raw duplicate insert must fail.
	if err := db.Create(&ReferralEntry{TelegramID: 2, Username: "bob", ReferralCode: "PROMO"}).Error; err == nil {
		t.Fatal("duplicate insert succeeded; unique index missing")
	}
	// And the migration is safe to run again.
	if err := migrateReferrals(db); err != nil {
		t.Fatalf("second migration: %v", err)
	}
}

func countRows(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&ReferralEntry{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestReferralUpsertDedupesSameUserAndCode(t *testing.T) {
	db := openReferralTestDB(t)

	// Same user clicks the same link 3 times.
	for i := 0; i < 3; i++ {
		if err := upsert(db, 111, "alice", "PROMO2024"); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	if got := countRows(t, db); got != 1 {
		t.Fatalf("expected 1 row after 3 identical clicks, got %d", got)
	}
}

func TestReferralKeepsDistinctCodesPerUser(t *testing.T) {
	db := openReferralTestDB(t)

	if err := upsert(db, 222, "bob", "PROMO"); err != nil {
		t.Fatal(err)
	}
	if err := upsert(db, 222, "bob", "SALE"); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, db); got != 2 {
		t.Fatalf("expected 2 rows for same user with 2 distinct codes, got %d", got)
	}
}

func TestReferralUsernameRefreshedOnReclick(t *testing.T) {
	db := openReferralTestDB(t)

	if err := upsert(db, 333, "", "PROMO"); err != nil { // first click, no username yet
		t.Fatal(err)
	}
	if err := upsert(db, 333, "carol", "PROMO"); err != nil { // re-click after setting username
		t.Fatal(err)
	}

	if got := countRows(t, db); got != 1 {
		t.Fatalf("expected 1 row, got %d", got)
	}

	var e ReferralEntry
	if err := db.Where("telegram_id = ? AND referral_code = ?", 333, "PROMO").First(&e).Error; err != nil {
		t.Fatal(err)
	}
	if e.Username != "carol" {
		t.Fatalf("expected username refreshed to 'carol', got %q", e.Username)
	}
}

// TestReferralLookupQuery exercises the exact query the API endpoint runs.
func TestReferralLookupQuery(t *testing.T) {
	db := openReferralTestDB(t)

	// two users on PROMO, one on OTHER
	if err := upsert(db, 1, "alice", "PROMO"); err != nil {
		t.Fatal(err)
	}
	if err := upsert(db, 2, "bob", "PROMO"); err != nil {
		t.Fatal(err)
	}
	if err := upsert(db, 3, "dave", "OTHER"); err != nil {
		t.Fatal(err)
	}

	var entries []ReferralEntry
	if err := db.Where("referral_code = ?", "PROMO").Order("created_at asc").Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 PROMO users, got %d", len(entries))
	}
	if entries[0].TelegramID != 1 || entries[1].TelegramID != 2 {
		t.Fatalf("unexpected ordering/contents: %+v", entries)
	}
	// CreatedAt must be auto-populated (drives joined_at in the API response).
	if entries[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt was not auto-populated by gorm")
	}

	// Empty lookup returns no rows and no error (API returns count:0, users:[]).
	var none []ReferralEntry
	if err := db.Where("referral_code = ?", "NOPE").Find(&none).Error; err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("expected 0 rows for unknown code, got %d", len(none))
	}
}
