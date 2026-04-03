package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

const (
	// maxAnalyticsLimit caps the maximum number of records per request to prevent memory exhaustion
	maxAnalyticsLimit = 250
	// maxAnalyticsOffset caps the offset to prevent abuse
	maxAnalyticsOffset = 100000
	// minValidTimestamp is 2009-01-03 (Bitcoin genesis block) - no valid data before this
	minValidTimestamp int64 = 1230940800
	// maxValidTimestamp is 2100-01-01 - reasonable upper bound
	maxValidTimestamp int64 = 4102444800
)

// lnbitsRateLimiter enforces a shared rate limit of 150 req/min (safely below LNbits' 200/min limit)
// across all concurrent analytics requests.
var lnbitsRateLimiter = rate.NewLimiter(rate.Every(400*time.Millisecond), 1)

// TransactionAnalyticsResponse represents the analytics data response
type TransactionAnalyticsResponse struct {
	Status           string                      `json:"status"`
	ExternalPayments []ExternalPaymentData       `json:"external_payments,omitempty"`
	InternalTxs      []InternalTransactionData   `json:"internal_transactions,omitempty"`
	Summary          TransactionSummary          `json:"summary"`
	Filters          map[string]string           `json:"filters_applied"`
}

// ExternalPaymentData represents external LNbits payment data
type ExternalPaymentData struct {
	UserID        int64  `json:"user_id"`
	Username      string `json:"username"`
	CheckingID    string `json:"checking_id"`
	Pending       bool   `json:"pending"`
	Amount        int64  `json:"amount_msats"`
	AmountSats    int64  `json:"amount_sats"`
	Fee           int64  `json:"fee_msats"`
	FeeSats       int64  `json:"fee_sats"`
	Memo          string `json:"memo"`
	Time          int    `json:"time"`
	Timestamp     string `json:"timestamp"`
	PaymentType   string `json:"payment_type"` // "incoming" or "outgoing"
	Bolt11        string `json:"bolt11,omitempty"`
	PaymentHash   string `json:"payment_hash"`
	WalletID      string `json:"wallet_id"`
}

// InternalTransactionData represents internal bot transactions
type InternalTransactionData struct {
	ID          uint   `json:"id"`
	Time        string `json:"time"`
	FromID      int64  `json:"from_id"`
	ToID        int64  `json:"to_id"`
	FromUser    string `json:"from_user"`
	ToUser      string `json:"to_user"`
	Type        string `json:"type"`
	Amount      int64  `json:"amount_sats"`
	ChatID      int64  `json:"chat_id,omitempty"`
	ChatName    string `json:"chat_name,omitempty"`
	Memo        string `json:"memo"`
	Success     bool   `json:"success"`
}

// TransactionSummary provides aggregate statistics
type TransactionSummary struct {
	TotalExternalCount    int   `json:"total_external_count"`
	TotalInternalCount    int   `json:"total_internal_count"`
	ExternalIncoming      int64 `json:"external_incoming_sats"`
	ExternalOutgoing      int64 `json:"external_outgoing_sats"`
	InternalVolume        int64 `json:"internal_volume_sats"`
	UniqueUsers           int   `json:"unique_users"`
}

// GetTransactionAnalytics retrieves transaction data for analytics
// Endpoint: GET /api/v1/analytics/transactions
// Query Parameters:
//   - user_id: Filter by specific user Telegram ID
//   - username: Filter by username (without @)
//   - start_date: Start date (YYYY-MM-DD or Unix timestamp)
//   - end_date: End date (YYYY-MM-DD or Unix timestamp)
//   - payment_type: Filter external payments by type (incoming/outgoing/all)
//   - include_external: Include external LNbits payments (true/false, default: true)
//   - include_internal: Include internal bot transactions (true/false, default: true)
//   - limit: Maximum number of transactions per type (default: 100, max: 250)
//   - offset: Number of transactions to skip for pagination (default: 0)
//   - format: Response format - "json" (default) or "csv"
func (s Service) GetTransactionAnalytics(w http.ResponseWriter, r *http.Request) {
	// Parse query parameters
	params := r.URL.Query()

	userIDStr := params.Get("user_id")
	username := params.Get("username")
	startDateStr := params.Get("start_date")
	endDateStr := params.Get("end_date")
	paymentTypeFilter := params.Get("payment_type")
	includeExternal := params.Get("include_external") != "false"
	includeInternal := params.Get("include_internal") != "false"
	limitStr := params.Get("limit")
	offsetStr := params.Get("offset")
	outputFormat := params.Get("format")

	// Set default limit with max cap
	limit := 100
	if limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}
	if limit > maxAnalyticsLimit {
		limit = maxAnalyticsLimit
	}

	// Set default offset with max cap
	offset := 0
	if offsetStr != "" {
		if parsedOffset, err := strconv.Atoi(offsetStr); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}
	if offset > maxAnalyticsOffset {
		offset = maxAnalyticsOffset
	}

	// Parse dates
	var startDate, endDate time.Time
	var err error

	if startDateStr != "" {
		startDate, err = parseDate(startDateStr)
		if err != nil {
			RespondError(w, "Invalid start_date format. Use YYYY-MM-DD or Unix timestamp")
			return
		}
	}

	if endDateStr != "" {
		endDate, err = parseDate(endDateStr)
		if err != nil {
			RespondError(w, "Invalid end_date format. Use YYYY-MM-DD or Unix timestamp")
			return
		}
	}

	response := TransactionAnalyticsResponse{
		Status:  StatusOk,
		Filters: make(map[string]string),
	}

	// Track applied filters
	if userIDStr != "" {
		response.Filters["user_id"] = userIDStr
	}
	if username != "" {
		response.Filters["username"] = username
	}
	if startDateStr != "" {
		response.Filters["start_date"] = startDateStr
	}
	if endDateStr != "" {
		response.Filters["end_date"] = endDateStr
	}
	if paymentTypeFilter != "" {
		response.Filters["payment_type"] = paymentTypeFilter
	}
	response.Filters["limit"] = strconv.Itoa(limit)
	response.Filters["offset"] = strconv.Itoa(offset)

	var targetUsers []*lnbits.User

	// Find target user(s)
	if userIDStr != "" {
		userID, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil {
			RespondError(w, "Invalid user_id")
			return
		}
		user := &lnbits.User{}
		tx := s.Bot.DB.Users.Where("telegram_id = ?", userID).First(user)
		if tx.Error != nil {
			RespondError(w, "User not found")
			return
		}
		targetUsers = append(targetUsers, user)
	} else if username != "" {
		user := &lnbits.User{}
		tx := s.Bot.DB.Users.Where("telegram_username = ?", username).First(user)
		if tx.Error != nil {
			RespondError(w, "User not found")
			return
		}
		targetUsers = append(targetUsers, user)
	} else {
		// Get all users if no specific user requested
		var allUsers []*lnbits.User
		tx := s.Bot.DB.Users.Find(&allUsers)
		if tx.Error != nil {
			RespondError(w, "Error fetching users")
			return
		}
		targetUsers = allUsers
	}

	uniqueUserMap := make(map[int64]bool)

	// Fetch external payments from LNbits with configurable limit+offset
	if includeExternal {
		for _, user := range targetUsers {
			if len(response.ExternalPayments) >= limit {
				break
			}

			if user.Wallet == nil {
				continue
			}

			uniqueUserMap[user.Telegram.ID] = true

			payments, err := fetchPaymentsWithRateLimit(r.Context(), s, user, limit+offset)
			if err != nil {
				log.Errorf("[Analytics] Error fetching payments for user %d: %s", user.Telegram.ID, err.Error())
				continue
			}

			for _, payment := range payments {
				// Apply date filters
				paymentTime := time.Unix(int64(payment.Time), 0)
				if !startDate.IsZero() && paymentTime.Before(startDate) {
					continue
				}
				if !endDate.IsZero() && paymentTime.After(endDate) {
					continue
				}

				// Determine payment type
				paymentType := "outgoing"
				if payment.Amount > 0 {
					paymentType = "incoming"
				}

				// Apply payment type filter
				if paymentTypeFilter != "" && paymentTypeFilter != "all" && paymentTypeFilter != paymentType {
					continue
				}

				// Check limit
				if len(response.ExternalPayments) >= limit {
					break
				}

				externalPayment := ExternalPaymentData{
					UserID:      user.Telegram.ID,
					Username:    user.Telegram.Username,
					CheckingID:  payment.CheckingID,
					Pending:     payment.Pending,
					Amount:      payment.Amount,
					AmountSats:  payment.Amount / 1000,
					Fee:         payment.Fee,
					FeeSats:     payment.Fee / 1000,
					Memo:        payment.Memo,
					Time:        payment.Time,
					Timestamp:   paymentTime.Format(time.RFC3339),
					PaymentType: paymentType,
					Bolt11:      payment.Bolt11,
					PaymentHash: payment.PaymentHash,
					WalletID:    payment.WalletID,
				}

				response.ExternalPayments = append(response.ExternalPayments, externalPayment)

				// Update summary
				response.Summary.TotalExternalCount++
				if paymentType == "incoming" {
					response.Summary.ExternalIncoming += externalPayment.AmountSats
				} else {
					response.Summary.ExternalOutgoing += abs(externalPayment.AmountSats)
				}
			}
		}
	}

	// Fetch internal transactions from bot database
	if includeInternal {
		var internalTxs []telegram.Transaction
		dbQuery := s.Bot.DB.Transactions.Model(&telegram.Transaction{})

		// Apply filters
		if userIDStr != "" {
			userID, _ := strconv.ParseInt(userIDStr, 10, 64)
			dbQuery = dbQuery.Where("from_id = ? OR to_id = ?", userID, userID)
		} else if username != "" {
			dbQuery = dbQuery.Where("from_user = ? OR to_user = ?", username, username)
		}

		if !startDate.IsZero() {
			dbQuery = dbQuery.Where("time >= ?", startDate)
		}

		if !endDate.IsZero() {
			dbQuery = dbQuery.Where("time <= ?", endDate)
		}

		dbQuery = dbQuery.Order("time desc").Limit(limit).Offset(offset).Find(&internalTxs)

		if dbQuery.Error != nil {
			log.Errorf("[Analytics] Error fetching internal transactions: %s", dbQuery.Error)
		} else {
			for _, tx := range internalTxs {
				uniqueUserMap[tx.FromId] = true
				uniqueUserMap[tx.ToId] = true

				internalTx := InternalTransactionData{
					ID:       tx.ID,
					Time:     tx.Time.Format(time.RFC3339),
					FromID:   tx.FromId,
					ToID:     tx.ToId,
					FromUser: tx.FromUser,
					ToUser:   tx.ToUser,
					Type:     tx.Type,
					Amount:   tx.Amount,
					ChatID:   tx.ChatID,
					ChatName: tx.ChatName,
					Memo:     tx.Memo,
					Success:  tx.Success,
				}

				response.InternalTxs = append(response.InternalTxs, internalTx)

				// Update summary
				response.Summary.TotalInternalCount++
				if tx.Success {
					response.Summary.InternalVolume += tx.Amount
				}
			}
		}
	}

	response.Summary.UniqueUsers = len(uniqueUserMap)

	// Respond in requested format
	if outputFormat == "csv" {
		writeCSVResponse(w, response)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// GetUserTransactionHistory retrieves all transactions for a specific user
// Endpoint: GET /api/v1/analytics/user/{user_id}/transactions
// Query Parameters:
//   - limit: Maximum number of transactions per type (default: 100, max: 250)
//   - offset: Number of transactions to skip for pagination (default: 0)
//   - format: Response format - "json" (default) or "csv"
func (s Service) GetUserTransactionHistory(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	userIDStr := vars["user_id"]
	params := r.URL.Query()
	outputFormat := params.Get("format")

	// Parse limit and offset with max caps
	limit := 100
	if limitStr := params.Get("limit"); limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}
	if limit > maxAnalyticsLimit {
		limit = maxAnalyticsLimit
	}
	offset := 0
	if offsetStr := params.Get("offset"); offsetStr != "" {
		if parsedOffset, err := strconv.Atoi(offsetStr); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}
	if offset > maxAnalyticsOffset {
		offset = maxAnalyticsOffset
	}

	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		RespondError(w, "Invalid user_id")
		return
	}

	user := &lnbits.User{}
	tx := s.Bot.DB.Users.Where("telegram_id = ?", userID).First(user)
	if tx.Error != nil {
		RespondError(w, "User not found")
		return
	}

	response := TransactionAnalyticsResponse{
		Status: StatusOk,
		Filters: map[string]string{
			"user_id": userIDStr,
			"limit":   strconv.Itoa(limit),
			"offset":  strconv.Itoa(offset),
		},
	}

	// Fetch external payments from LNbits
	if user.Wallet != nil {
		payments, err := fetchPaymentsWithRateLimit(r.Context(), s, user, limit+offset)
		if err != nil {
			log.Errorf("[Analytics] Error fetching payments for user %d: %s", userID, err.Error())
		} else {
			for _, payment := range payments {
				paymentTime := time.Unix(int64(payment.Time), 0)
				paymentType := "outgoing"
				if payment.Amount > 0 {
					paymentType = "incoming"
				}

				if len(response.ExternalPayments) >= limit {
					break
				}

				externalPayment := ExternalPaymentData{
					UserID:      user.Telegram.ID,
					Username:    user.Telegram.Username,
					CheckingID:  payment.CheckingID,
					Pending:     payment.Pending,
					Amount:      payment.Amount,
					AmountSats:  payment.Amount / 1000,
					Fee:         payment.Fee,
					FeeSats:     payment.Fee / 1000,
					Memo:        payment.Memo,
					Time:        payment.Time,
					Timestamp:   paymentTime.Format(time.RFC3339),
					PaymentType: paymentType,
					Bolt11:      payment.Bolt11,
					PaymentHash: payment.PaymentHash,
					WalletID:    payment.WalletID,
				}

				response.ExternalPayments = append(response.ExternalPayments, externalPayment)
				response.Summary.TotalExternalCount++

				if paymentType == "incoming" {
					response.Summary.ExternalIncoming += externalPayment.AmountSats
				} else {
					response.Summary.ExternalOutgoing += abs(externalPayment.AmountSats)
				}
			}
		}
	}

	// Fetch internal transactions
	var internalTxs []telegram.Transaction
	s.Bot.DB.Transactions.Where("from_id = ? OR to_id = ?", userID, userID).
		Order("time desc").
		Limit(limit).Offset(offset).
		Find(&internalTxs)

	for _, tx := range internalTxs {
		internalTx := InternalTransactionData{
			ID:       tx.ID,
			Time:     tx.Time.Format(time.RFC3339),
			FromID:   tx.FromId,
			ToID:     tx.ToId,
			FromUser: tx.FromUser,
			ToUser:   tx.ToUser,
			Type:     tx.Type,
			Amount:   tx.Amount,
			ChatID:   tx.ChatID,
			ChatName: tx.ChatName,
			Memo:     tx.Memo,
			Success:  tx.Success,
		}

		response.InternalTxs = append(response.InternalTxs, internalTx)
		response.Summary.TotalInternalCount++

		if tx.Success {
			response.Summary.InternalVolume += tx.Amount
		}
	}

	response.Summary.UniqueUsers = 1

	// Respond in requested format
	if outputFormat == "csv" {
		writeCSVResponse(w, response)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// fetchPaymentsWithRateLimit waits for the shared LNbits rate limiter token, then fetches
// payments for the given user. On HTTP 429 it retries up to 3 times with exponential backoff
// (2s, 4s, 8s) before giving up.
func fetchPaymentsWithRateLimit(ctx context.Context, s Service, user *lnbits.User, count int) ([]lnbits.Payment, error) {
	const maxRetries = 3
	backoff := 2 * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := lnbitsRateLimiter.Wait(ctx); err != nil {
			return nil, err
		}

		payments, err := s.Bot.Client.PaymentsWithOptions(*user.Wallet, count, 0)
		if err == nil {
			return payments, nil
		}

		if !strings.Contains(err.Error(), "429") || attempt == maxRetries {
			return nil, err
		}

		log.Warnf("[Analytics] LNbits rate limit hit for user %d, retrying in %s (attempt %d/%d)",
			user.Telegram.ID, backoff, attempt+1, maxRetries)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, fmt.Errorf("exceeded max retries for user %d", user.Telegram.ID)
}

// parseDate parses a date string and validates it falls within reasonable bounds.
func parseDate(dateStr string) (time.Time, error) {
	var t time.Time

	// Try parsing as Unix timestamp first
	if timestamp, err := strconv.ParseInt(dateStr, 10, 64); err == nil {
		if timestamp < minValidTimestamp || timestamp > maxValidTimestamp {
			return time.Time{}, fmt.Errorf("timestamp out of range: %d", timestamp)
		}
		return time.Unix(timestamp, 0), nil
	}

	// Try parsing as date string
	layouts := []string{
		"2006-01-02",
		"2006-01-02T15:04:05",
		time.RFC3339,
	}

	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, dateStr); err == nil {
			t = parsed
			break
		}
	}

	if t.IsZero() {
		return time.Time{}, fmt.Errorf("unsupported date format: %s", dateStr)
	}

	// Validate parsed date is within reasonable bounds
	if t.Unix() < minValidTimestamp || t.Unix() > maxValidTimestamp {
		return time.Time{}, fmt.Errorf("date out of range: %s", dateStr)
	}

	return t, nil
}

// Helper function to get absolute value
func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// sanitizeCSVField prevents CSV formula injection by prefixing dangerous characters
// with a single quote. Fields starting with =, +, -, @, tab, or carriage return
// can be interpreted as formulas by spreadsheet software.
func sanitizeCSVField(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 0 {
		switch s[0] {
		case '=', '+', '-', '@', '\t':
			return "'" + s
		}
	}
	return s
}

// writeCSVResponse writes the analytics response as a CSV file
func writeCSVResponse(w http.ResponseWriter, response TransactionAnalyticsResponse) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=transactions.csv")
	w.WriteHeader(http.StatusOK)

	writer := csv.NewWriter(w)
	defer writer.Flush()

	// Write header
	writer.Write([]string{
		"source", "id", "time", "user_id", "username", "from_id", "from_user",
		"to_id", "to_user", "type", "amount_sats", "fee_sats", "memo",
		"payment_type", "pending", "success", "payment_hash", "chat_id", "chat_name",
	})

	// Write external payments
	for _, p := range response.ExternalPayments {
		writer.Write([]string{
			"external",
			p.CheckingID,
			p.Timestamp,
			strconv.FormatInt(p.UserID, 10),
			sanitizeCSVField(p.Username),
			"", "", "", "", // from/to fields not applicable
			p.PaymentType,
			strconv.FormatInt(p.AmountSats, 10),
			strconv.FormatInt(p.FeeSats, 10),
			sanitizeCSVField(p.Memo),
			p.PaymentType,
			strconv.FormatBool(p.Pending),
			"", // success not applicable
			p.PaymentHash,
			"", "", // chat fields not applicable
		})
	}

	// Write internal transactions
	for _, t := range response.InternalTxs {
		writer.Write([]string{
			"internal",
			strconv.FormatUint(uint64(t.ID), 10),
			t.Time,
			"", "", // user_id/username not applicable
			strconv.FormatInt(t.FromID, 10),
			sanitizeCSVField(t.FromUser),
			strconv.FormatInt(t.ToID, 10),
			sanitizeCSVField(t.ToUser),
			sanitizeCSVField(t.Type),
			strconv.FormatInt(t.Amount, 10),
			"", // fee not applicable
			sanitizeCSVField(t.Memo),
			"", "", // payment_type/pending not applicable
			strconv.FormatBool(t.Success),
			"", // payment_hash not applicable
			strconv.FormatInt(t.ChatID, 10),
			sanitizeCSVField(t.ChatName),
		})
	}
}
