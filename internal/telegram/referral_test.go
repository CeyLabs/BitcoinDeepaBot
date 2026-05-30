package telegram

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	if err := db.AutoMigrate(&ReferralEntry{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// upsert replicates the capture path in startHandler.
func upsert(db *gorm.DB, telegramID int64, username, code string) error {
	entry := ReferralEntry{TelegramID: telegramID, Username: username, ReferralCode: code}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "telegram_id"}, {Name: "referral_code"}},
		DoUpdates: clause.AssignmentColumns([]string{"username", "created_at"}),
	}).Create(&entry).Error
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
