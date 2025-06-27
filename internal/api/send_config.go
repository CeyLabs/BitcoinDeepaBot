package api

import (
	"github.com/LightningTipBot/LightningTipBot/internal"
)

// GetWhitelistedFromAccounts returns the list of whitelisted sender accounts
func GetWhitelistedFromAccounts() []string {
	return internal.Configuration.API.Send.WhitelistedSenders
}

// GetInternalNetworkCIDR returns the allowed internal network range
func GetInternalNetworkCIDR() string {
	return internal.Configuration.API.Send.InternalNetwork
}

// GetMaxAPITransactionAmount returns the maximum transaction amount
func GetMaxAPITransactionAmount() int64 {
	return internal.Configuration.API.Send.MaxAmount
}

// GetMinAPITransactionAmount returns the minimum transaction amount
func GetMinAPITransactionAmount() int64 {
	return internal.Configuration.API.Send.MinAmount
}

// GetAdminApprovalThreshold returns the admin approval threshold
func GetAdminApprovalThreshold() int64 {
	return internal.Configuration.API.Send.AdminApprovalThreshold
}

// GetMaxMemoLength returns the maximum memo length
func GetMaxMemoLength() int {
	return internal.Configuration.API.Send.MaxMemoLength
}

// GetAPIRateLimit returns the API rate limit
func GetAPIRateLimit() int {
	return internal.Configuration.API.Send.RateLimit
}
