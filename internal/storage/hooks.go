package storage

// UpdatePendingTxStatusFn is set by the api package at startup to allow the
// telegram approval handlers to update PendingTransaction status without
// creating an import cycle (api imports telegram, telegram imports storage).
var UpdatePendingTxStatusFn func(id, status, actor string)
