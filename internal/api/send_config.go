package api

import (
	"github.com/BitcoinDeepaBot/BitcoinDeepaBot/internal"
)

// GetWhitelistedWallets returns the map of whitelisted wallets with their HMAC secrets
func GetWhitelistedWallets() map[string]internal.WhitelistedWallet {
	return internal.Configuration.API.Send.WhitelistedWallets
}

// GetWalletHMACSecret returns the HMAC secret for a specific wallet
func GetWalletHMACSecret(walletID string) (string, bool) {
	wallet, exists := internal.Configuration.API.Send.WhitelistedWallets[walletID]
	if !exists {
		return "", false
	}
	return wallet.HMACSecret, true
}

// IsWhitelistedWallet checks if a wallet ID is whitelisted
func IsWhitelistedWallet(walletID string) bool {
	_, exists := internal.Configuration.API.Send.WhitelistedWallets[walletID]
	return exists
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
