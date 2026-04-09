package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/transport"
)

const maxMessageLen = 4000 // safe limit under Telegram's 4096

// splitMessage splits text into chunks that fit within the message limit.
// Splits on newlines when possible, otherwise hard-cuts.
func splitMessage(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= limit {
			chunks = append(chunks, text)
			break
		}
		cut := limit
		// Try to split on a newline.
		if idx := strings.LastIndex(text[:limit], "\n"); idx > 0 {
			cut = idx
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
		// Skip the newline we split on.
		if len(text) > 0 && text[0] == '\n' {
			text = text[1:]
		}
	}
	return chunks
}

// --- Retry logic ---

const (
	baseBackoff = 2 * time.Second
	maxBackoff  = 60 * time.Second
)

// retryDo executes fn with retries on transient/rate-limit errors.
// Rate limits and network errors retry indefinitely with backoff.
// Permanent API errors (400, 401, 403) return immediately.
// Cancelling ctx aborts the retry loop (used by /abort).
func retryDo(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := fn()
		if err == nil {
			return nil
		}

		var apiErr *transport.APIError
		if errors.As(err, &apiErr) {
			if apiErr.RetryAfter > 0 {
				wait := time.Duration(apiErr.RetryAfter) * time.Second
				log.Printf("rate limited, retry after %v: %v", wait, err)
				if !sleepCtx(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
			if apiErr.IsRetryable() {
				wait := backoff(attempt)
				log.Printf("server error, retry in %v: %v", wait, err)
				if !sleepCtx(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
			// Permanent API error (400, 401, 403, etc.) — don't retry.
			return err
		}

		// Network error (timeout, DNS, connection refused) — retry indefinitely.
		wait := backoff(attempt)
		log.Printf("network error, retry in %v: %v", wait, err)
		if !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns true if slept fully.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func backoff(attempt int) time.Duration {
	d := baseBackoff
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// trySend sends text with format, retrying on transient errors.
// Falls back to plain text if formatted send fails with a permanent error.
func trySend(ctx context.Context, t transport.Transport, chatID, replyTo, text, format string) error {
	err := retryDo(ctx, func() error {
		return t.SendMessage(chatID, text, replyTo, format)
	})
	if err != nil && format != "" && ctx.Err() == nil {
		log.Printf("send error (%s): %v, retrying plain", format, err)
		return retryDo(ctx, func() error {
			return t.SendMessage(chatID, text, replyTo, "")
		})
	}
	return err
}

// tryEdit edits text with format, retrying on transient errors.
// Falls back to plain text if formatted edit fails with a permanent error.
func tryEdit(ctx context.Context, t transport.Transport, chatID, msgID, text, format string) error {
	err := retryDo(ctx, func() error {
		return t.EditMessage(chatID, msgID, text, format)
	})
	if err != nil && format != "" && ctx.Err() == nil {
		log.Printf("edit error (%s): %v, retrying plain", format, err)
		return retryDo(ctx, func() error {
			return t.EditMessage(chatID, msgID, text, "")
		})
	}
	return err
}

// deliverFinal sends the final response, splitting into chunks if needed.
// The first chunk edits the progress message; remaining chunks are new messages.
// On total failure, attempts a last-resort plain error notification.
func (d *daemon) deliverFinal(ctx context.Context, t transport.Transport, chatID, progressMsgID, text, format string) {
	if format == "" {
		text = stripHTML(text)
	}
	chunks := splitMessage(text, maxMessageLen)

	for i, chunk := range chunks {
		if ctx.Err() != nil {
			return
		}
		var err error
		if i == 0 && progressMsgID != "" {
			err = tryEdit(ctx, t, chatID, progressMsgID, chunk, format)
		} else {
			err = trySend(ctx, t, chatID, "", chunk, format)
		}
		if err != nil {
			log.Printf("deliver error (chunk %d/%d): %v", i+1, len(chunks), err)
			// Last resort: try to notify user about the failure.
			if i == 0 && ctx.Err() == nil {
				_ = retryDo(ctx, func() error {
					return t.SendMessage(chatID, "Ошибка доставки ответа. Попробуйте /status", "", "")
				})
			}
			return
		}
	}
}

func (d *daemon) sendMessage(chatID, replyTo, text string) {
	t, raw, fmtStr := d.transportFor(chatID)
	if t == nil {
		log.Printf("no transport for %s", chatID)
		return
	}
	if fmtStr == "" {
		text = stripHTML(text)
	}
	if err := trySend(context.Background(), t, raw, replyTo, text, fmtStr); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (d *daemon) sendPlain(chatID, replyTo, text string) {
	t, raw, _ := d.transportFor(chatID)
	if t == nil {
		log.Printf("no transport for %s", chatID)
		return
	}
	if err := retryDo(context.Background(), func() error {
		return t.SendMessage(raw, text, replyTo, "")
	}); err != nil {
		log.Printf("send error: %v", err)
	}
}
