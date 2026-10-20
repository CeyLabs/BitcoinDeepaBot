package telegram

import (
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/storage"
)

func TestAPIApprovalExpiresAt(t *testing.T) {
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	explicit := created.Add(time.Hour)

	withExpiry := &APIApprovalData{Base: &storage.Base{CreatedAt: created}, ExpiresAt: explicit}
	if got := withExpiry.expiresAt(); !got.Equal(explicit) {
		t.Errorf("expiresAt() = %s, want the stored ExpiresAt %s", got, explicit)
	}

	// Approvals saved before ExpiresAt existed fall back to created + window.
	legacy := &APIApprovalData{Base: &storage.Base{CreatedAt: created}}
	if got, want := legacy.expiresAt(), created.Add(storage.PendingTxExpiry); !got.Equal(want) {
		t.Errorf("legacy expiresAt() = %s, want %s", got, want)
	}

	// A record with no timestamps at all must count as expired, not valid forever.
	empty := &APIApprovalData{Base: &storage.Base{}}
	if !time.Now().After(empty.expiresAt()) {
		t.Error("an approval with no timestamps must be treated as expired")
	}
}
