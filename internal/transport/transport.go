// Package transport defines the messenger abstraction for klax.
package transport

// Transport is the interface that messenger backends implement.
type Transport interface {
	// SendMessage sends text to a chat. Returns error.
	SendMessage(chatID, text, replyTo, format string) error
	// SendMessageReturnID sends text and returns the message ID.
	SendMessageReturnID(chatID, text, replyTo, format string) (string, error)
	// EditMessage edits an existing message.
	EditMessage(chatID, messageID, text, format string) error
}

// APIError represents a messenger API error with enough detail for retry decisions.
type APIError struct {
	Platform    string // "tg", "max", "vk"
	Code        int    // HTTP status or API error code
	Description string
	RetryAfter  int // seconds to wait before retry, 0 if not specified
}

func (e *APIError) Error() string {
	return e.Platform + " API: " + e.Description
}

// IsRetryable returns true for rate limits and server errors.
func (e *APIError) IsRetryable() bool {
	if e.RetryAfter > 0 {
		return true
	}
	// 429 = rate limit, 5xx = server errors
	return e.Code == 429 || e.Code >= 500
}
