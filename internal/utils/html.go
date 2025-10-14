package utils

import "strings"

// EscapeHTML escapes special HTML characters for safe Telegram HTML message formatting.
func EscapeHTML(text string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return replacer.Replace(text)
}
