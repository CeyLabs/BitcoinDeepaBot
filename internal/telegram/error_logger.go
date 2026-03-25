package telegram

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/utils"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// ErrorLogger handles logging errors to Telegram group
type ErrorLogger struct {
	bot        *TipBot
	logGroupId int64
	threadId   int64
	enabled    bool
}

// NewErrorLogger creates a new error logger instance
func NewErrorLogger(bot *TipBot) *ErrorLogger {
	logger := &ErrorLogger{
		bot:        bot,
		logGroupId: internal.Configuration.Telegram.LogGroupId,
		threadId:   internal.Configuration.Telegram.ErrorThreadId,
		enabled:    internal.Configuration.Telegram.LogGroupId != 0,
	}

	if logger.enabled {
		log.Infof("[ErrorLogger] Error logging enabled for group: %d", logger.logGroupId)
	} else {
		log.Warnf("[ErrorLogger] Error logging disabled - no log_group_id configured")
	}

	return logger
}

// GetLogGroupId returns the log group ID for external packages
func GetLogGroupId(el *ErrorLogger) int64 {
	if el == nil {
		return 0
	}
	return el.logGroupId
}

// LogError logs an error to the configured Telegram group
func (el *ErrorLogger) LogError(err error, context string, userInfo ...interface{}) {
	if !el.enabled || err == nil {
		return
	}

	// Filter out empty/ghost errors and irrelevant messages
	errorMsg := err.Error()
	if errorMsg == "" || errorMsg == `{"message":"","Err":{},"code":0}` {
		return // Skip empty/meaningless errors
	}
	if strings.Contains(errorMsg, "[requirePrivateChatInterceptor]") {
		return // Skip logging this specific interceptor error
	}

	// Format error message as HTML (like payment error)
	htmlEscape := utils.EscapeHTML
	timestamp := time.Now().Format("2006-01-02 15:04:05 UTC")

	// User and chat details
	userStr := "Unknown"
	userId := ""
	chatStr := ""
	chatId := ""

	for _, info := range userInfo {
		switch v := info.(type) {
		case *tb.User:
			if v.Username != "" {
				userStr = "<span class=\"tg-spoiler\">@" + htmlEscape(v.Username) + "</span>"
			} else {
				userStr = "<span class=\"tg-spoiler\">" + htmlEscape(v.FirstName+" "+v.LastName) + "</span>"
			}
			userId = fmt.Sprintf("%d", v.ID)
		case *tb.Chat:
			chatStr = htmlEscape(v.Title)
			chatId = fmt.Sprintf("%d", v.ID)
		}
	}

	msg := ""
	msg += "🚫 <b>Bot Error</b>\n\n"
	msg += " Context: " + htmlEscape(context) + "\n"
	if userStr != "" && userId != "" {
		msg += "👤 User: " + userStr + " (ID: " + userId + ")\n"
	}
	if chatStr != "" && chatId != "" {
		msg += "💬 Chat: " + chatStr + " (ID: " + chatId + ")\n"
	}
	msg += "<blockquote expandable>\n"
	msg += "❗ Error: " + htmlEscape(err.Error()) + "\n"
	// Add stack trace location if available
	if pc, file, line, ok := runtime.Caller(1); ok {
		if strings.Contains(file, "error_logger.go") {
			if pc2, file2, line2, ok2 := runtime.Caller(2); ok2 {
				pc = pc2
				file = file2
				line = line2
			}
		}
		funcName := runtime.FuncForPC(pc).Name()
		msg += fmt.Sprintf("📍 Location: %s:%d in %s\n", htmlEscape(file), line, htmlEscape(funcName))
	}
	msg += "🕒 Time: " + htmlEscape(timestamp) + "\n"
	msg += "</blockquote>"

	go el.sendToTelegramHTML(msg)
}

// LogPanic logs a panic with stack trace to the Telegram group
func (el *ErrorLogger) LogPanic(panicData interface{}, context string) {
	if !el.enabled {
		return
	}

	// Get stack trace
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	stackTrace := string(buf[:n])

	errorMsg := fmt.Sprintf("🚨 *PANIC DETECTED*\n\n"+
		"*Context:* `%s`\n"+
		"*Panic:* `%v`\n\n"+
		"*Stack Trace:*\n```\n%s\n```\n\n"+
		"*Time:* `%s`",
		el.escapeMarkdownV2(context),
		panicData,
		el.truncateStackTrace(stackTrace),
		el.escapeMarkdownV2(time.Now().Format("2006-01-02 15:04:05 UTC")))

	go el.sendToTelegram(errorMsg)
}

// LogCriticalError logs critical errors that require immediate attention
func (el *ErrorLogger) LogCriticalError(err error, context string, userInfo ...interface{}) {
	if !el.enabled || err == nil {
		return
	}

	errorMsg := "🔥 *CRITICAL ERROR* 🔥\n\n" + el.formatErrorMessage(err, context, userInfo...)

	// Send to Telegram group immediately (not in goroutine for critical errors)
	el.sendToTelegram(errorMsg)
}

// formatErrorMessage creates a formatted error message
func (el *ErrorLogger) formatErrorMessage(err error, context string, userInfo ...interface{}) string {
	timestamp := time.Now().Format("2006-01-02 15:04:05 UTC")

	msg := fmt.Sprintf(
		"*Time:* `%s`\n"+
			"*Context:* `%s`\n"+
			"*Error Details:*\n"+
			"> %s\n",
		el.escapeMarkdownV2(timestamp),
		el.escapeMarkdownV2(context),
		el.escapeMarkdownV2(err.Error()))

	// Add user information if provided
	if len(userInfo) > 0 {
		var userDetails []string
		for _, info := range userInfo {
			switch v := info.(type) {
			case *tb.User:
				userDetails = append(userDetails, fmt.Sprintf("*User:* %s \\(ID: %d\\)", el.getUserStrV2(v), v.ID))
			case *tb.Chat:
				userDetails = append(userDetails, fmt.Sprintf("*Chat:* %s \\(ID: %d\\)", el.escapeMarkdownV2(v.Title), v.ID))
			case string:
				userDetails = append(userDetails, v)
			default:
				userDetails = append(userDetails, fmt.Sprintf("%v", v))
			}
		}
		if len(userDetails) > 0 {
			msg += fmt.Sprintf("\n*Additional Details:*\n%s\n", strings.Join(userDetails, "\n"))
		}
	}

	// Add stack trace for debugging (limited to 3 most recent calls)
	if pc, file, line, ok := runtime.Caller(2); ok {
		// if the caller is within this file, step one level further up the stack
		if strings.Contains(file, "error_logger.go") {
			if pc2, file2, line2, ok2 := runtime.Caller(3); ok2 {
				pc = pc2
				file = file2
				line = line2
			}
		}
		funcName := runtime.FuncForPC(pc).Name()
		msg += fmt.Sprintf("\n*Location:* `%s:%d` in `%s`", el.escapeMarkdownV2(file), line, el.escapeMarkdownV2(funcName))
	}

	return msg
}

// sendToTelegram sends the formatted message to the Telegram group
func (el *ErrorLogger) sendToTelegram(message string) {
	if el.bot == nil || el.bot.Telegram == nil {
		log.Warnf("[ErrorLogger] Cannot send error log - Telegram bot not initialized")
		return
	}

	// Create recipient
	recipient := &tb.Chat{ID: el.logGroupId}

	// Prepare send options
	sendOptions := &tb.SendOptions{
		ParseMode:             tb.ModeMarkdownV2,
		DisableWebPagePreview: true,
	}

	// Add thread ID if specified (for Telegram topics/threads)
	if el.threadId > 0 {
		sendOptions.ReplyTo = &tb.Message{ID: int(el.threadId)}
	}

	// Send message
	_, err := el.bot.Telegram.Send(recipient, message, sendOptions)
	if err != nil {
		log.Errorf("[ErrorLogger] Failed to send error log to Telegram: %v", err)
		// Try sending without markdown if parsing fails
		if strings.Contains(err.Error(), "parse") {
			plainMessage := el.stripMarkdown(message)
			plainOptions := &tb.SendOptions{
				DisableWebPagePreview: true,
			}
			if el.threadId > 0 {
				plainOptions.ReplyTo = &tb.Message{ID: int(el.threadId)}
			}
			_, fallbackErr := el.bot.Telegram.Send(recipient, plainMessage, plainOptions)
			if fallbackErr != nil {
				log.Errorf("[ErrorLogger] Failed to send plain error log: %v", fallbackErr)
			}
		}
	}
}

// escapeMarkdown escapes special markdown characters
func (el *ErrorLogger) escapeMarkdown(text string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"`", "\\`",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(text)
}

// escapeMarkdownV2 escapes special MarkdownV2 characters
func (el *ErrorLogger) escapeMarkdownV2(text string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(text)
}

// stripMarkdown removes markdown formatting
func (el *ErrorLogger) stripMarkdown(text string) string {
	replacer := strings.NewReplacer(
		"**", "",
		"*", "",
		"`", "",
		"_", "",
		"~", "",
		"```", "",
	)
	return replacer.Replace(text)
}

// truncateStackTrace limits stack trace length for Telegram
func (el *ErrorLogger) truncateStackTrace(stackTrace string) string {
	const maxLength = 2000 // Telegram message limit consideration
	if len(stackTrace) <= maxLength {
		return stackTrace
	}
	return stackTrace[:maxLength] + "\n... (truncated)"
}

// getUserStr returns a string representation of a Telegram user
func (el *ErrorLogger) getUserStr(user *tb.User) string {
	if user == nil {
		return "Unknown"
	}
	if user.Username != "" {
		return "@" + user.Username
	}
	return fmt.Sprintf("%s %s", user.FirstName, user.LastName)
}

// getUserStrV2 returns a MarkdownV2 escaped string representation of a Telegram user
func (el *ErrorLogger) getUserStrV2(user *tb.User) string {
	if user == nil {
		return "Unknown"
	}
	if user.Username != "" {
		return "@" + el.escapeMarkdownV2(user.Username)
	}
	return fmt.Sprintf("%s %s", el.escapeMarkdownV2(user.FirstName), el.escapeMarkdownV2(user.LastName))
}

// LogPaymentError logs payment-related errors with detailed information
func (el *ErrorLogger) LogPaymentError(err error, amount int64, memo, invoice string, user *tb.User) {
	if !el.enabled || err == nil {
		return
	}

	if len(invoice) > 80 {
		invoice = invoice[:80] + "..."
	}
	if memo == "" {
		memo = "None"
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05 UTC")

	// HTML escaping utility
	htmlEscape := utils.EscapeHTML

	userStr := "Unknown"
	if user != nil {
		if user.Username != "" {
			userStr = "<span class=\"tg-spoiler\">@" + htmlEscape(user.Username) + "</span>"
		} else {
			userStr = "<span class=\"tg-spoiler\">" + htmlEscape(user.FirstName+" "+user.LastName) + "</span>"
		}
	}

	amountStr := "<span class=\"tg-spoiler\">" + htmlEscape(utils.FormatSats(amount)) + "</span>"
	memoStr := htmlEscape(memo)
	invoiceStr := htmlEscape(invoice)

	// Compose HTML message
	msg := fmt.Sprintf(
		"🚫 Payment Error for <b>%s</b> (ID: %d)\n\n"+
			"💰 Amount: <span class=\"tg-spoiler\">%s</span>\n"+
			"📄 Invoice: %s\n"+
			"📝 Memo: %s\n"+
			"<blockquote expandable>",
		userStr, user.ID, amountStr, invoiceStr, memoStr,
	)

	// Expandable details (Telegram does not support true expandable blocks, but blockquote visually separates)
	detail := fmt.Sprintf("❗ Error: %s\n", htmlEscape(err.Error()))

	if _, file, line, ok := runtime.Caller(1); ok {
		if strings.Contains(file, "error_logger.go") {
			if _, file2, line2, ok2 := runtime.Caller(2); ok2 {
				file = file2
				line = line2
			}
		}
		if idx := strings.Index(file, "/internal/"); idx > -1 {
			file = file[idx:]
		}
		detail += fmt.Sprintf("📍 Logged at:\n%s:%d\n(from github.com/LightningTipBot/LightningTipBot)\n", htmlEscape(file), line)
	}
	detail += fmt.Sprintf("🕒 Time: %s", htmlEscape(timestamp))

	msg += detail + "</blockquote>"

	go el.sendToTelegramHTML(msg)
}

// sendToTelegramHTML sends the formatted HTML message to the Telegram group
func (el *ErrorLogger) sendToTelegramHTML(message string) {
	if el.bot == nil || el.bot.Telegram == nil {
		log.Warnf("[ErrorLogger] Cannot send error log - Telegram bot not initialized")
		return
	}

	recipient := &tb.Chat{ID: el.logGroupId}
	sendOptions := &tb.SendOptions{
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
	}
	if el.threadId > 0 {
		sendOptions.ReplyTo = &tb.Message{ID: int(el.threadId)}
	}
	_, err := el.bot.Telegram.Send(recipient, message, sendOptions)
	if err != nil {
		log.Errorf("[ErrorLogger] Failed to send HTML error log to Telegram: %v", err)
	}
}

// LogTransactionError logs transaction-related errors with sender/receiver info
func (el *ErrorLogger) LogTransactionError(err error, transactionType string, amount int64, fromUser, toUser *tb.User) {
	context := fmt.Sprintf("Transaction Error - Type: %s, Amount: %s", transactionType, utils.FormatSats(amount))

	var userDetails []string
	if fromUser != nil {
		userDetails = append(userDetails, fmt.Sprintf("> *From:* %s \\(ID: %d\\)", el.getUserStrV2(fromUser), fromUser.ID))
	}
	if toUser != nil {
		userDetails = append(userDetails, fmt.Sprintf("> *To:* %s \\(ID: %d\\)", el.getUserStrV2(toUser), toUser.ID))
	}

	transactionDetails := fmt.Sprintf("*Transaction Details:*\n%s\n> *Amount:* `%s`\n> *Transaction Error:* `%s`",
		strings.Join(userDetails, "\n"), utils.FormatSats(amount), el.escapeMarkdownV2(err.Error()))

	var logUsers []interface{}
	if fromUser != nil {
		logUsers = append(logUsers, fromUser)
	}
	if toUser != nil {
		logUsers = append(logUsers, toUser)
	}
	logUsers = append(logUsers, transactionDetails)

	el.LogError(err, context, logUsers...)
}

// LogDatabaseError logs database-related errors
func (el *ErrorLogger) LogDatabaseError(err error, operation string, user *tb.User) {
	context := fmt.Sprintf("Database Error - Operation: %s", operation)
	el.LogError(err, context, user)
}

// LogAPIError logs API-related errors
func (el *ErrorLogger) LogAPIError(err error, endpoint string, user *tb.User) {
	context := fmt.Sprintf("API Error - Endpoint: %s", endpoint)
	el.LogError(err, context, user)
}

// LogLNURLError logs LNURL-related errors with request details
func (el *ErrorLogger) LogLNURLError(err error, operation string, username string, requestDetails map[string]interface{}) {
	context := fmt.Sprintf("LNURL Error - Operation: %s, User: %s", operation, username)

	var details []string
	for key, value := range requestDetails {
		details = append(details, fmt.Sprintf("> *%s:* `%v`", el.escapeMarkdownV2(key), el.escapeMarkdownV2(fmt.Sprintf("%v", value))))
	}

	requestInfo := fmt.Sprintf("*Request Details:*\n%s", strings.Join(details, "\n"))

	el.LogError(err, context, requestInfo)
}
