package storage

import "time"

// PendingTxExpiry is how long an approval request stays approvable. It lives
// here so the Telegram approve button enforces the same window that
// /api/v1/send/status reports as expired.
const PendingTxExpiry = 24 * time.Hour

// UpdatePendingTxStatusFn is set by the api package at startup to allow the
// telegram approval handlers to update PendingTransaction status without
// creating an import cycle (api imports telegram, telegram imports storage).
var UpdatePendingTxStatusFn func(id, status, actor string)

// PendingTransaction status values.
//
// These live here rather than in the api package because both api and telegram
// write them and telegram cannot import api. The api package re-exports them
// under its own names for callers that already use those.
const (
	TxStatusPending  = "pending"
	TxStatusApproved = "approved"
	TxStatusRejected = "rejected"
	TxStatusExpired  = "expired"
	TxStatusExecuted = "executed"
	TxStatusFailed   = "failed"

	// TxStatusInFlight marks an approved payment that lnbits accepted but that
	// has not reached a terminal state within the settlement timeout.
	//
	// It is deliberately distinct from TxStatusPending: a pending transaction
	// is reported as expired once its 24h approval window passes, which for an
	// in-flight (and possibly settled) payment would tell the caller the sats
	// were never sent and invite them to pay a second invoice.
	TxStatusInFlight = "in_flight"
)
