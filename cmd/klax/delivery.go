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
func splitMessage(text string, limit int, format string) []string {
	if format == "html" {
		return splitHTMLMessage(text, limit)
	}
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

type htmlOpenTag struct {
	name string
	raw  string
}

func splitHTMLMessage(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}

	var chunks []string
	var current strings.Builder
	var stack []htmlOpenTag
	i := 0
	for i < len(text) {
		if text[i] == '<' {
			end := strings.IndexByte(text[i:], '>')
			if end != -1 {
				token := text[i : i+end+1]
				name, closing, selfClosing, ok := parseHTMLTag(token)
				if ok {
					nextStack := stack
					if closing {
						if len(nextStack) > 0 && nextStack[len(nextStack)-1].name == name {
							nextStack = nextStack[:len(nextStack)-1]
						}
					} else if !selfClosing {
						nextStack = append(append([]htmlOpenTag(nil), stack...), htmlOpenTag{name: name, raw: token})
					}

					if current.Len() > 0 && current.Len()+len(token)+len(renderClosingTags(nextStack)) > limit {
						chunks = append(chunks, current.String()+renderClosingTags(stack))
						current.Reset()
						current.WriteString(renderOpeningTags(stack))
					}

					current.WriteString(token)
					stack = nextStack
					i += end + 1
					continue
				}
			}
		}

		nextTag := strings.IndexByte(text[i:], '<')
		end := len(text)
		if nextTag != -1 {
			end = i + nextTag
		}
		segment := text[i:end]
		for len(segment) > 0 {
			remaining := limit - current.Len() - len(renderClosingTags(stack))
			if remaining <= 0 && current.Len() > 0 {
				chunks = append(chunks, current.String()+renderClosingTags(stack))
				current.Reset()
				current.WriteString(renderOpeningTags(stack))
				continue
			}
			if len(segment) <= remaining {
				current.WriteString(segment)
				segment = ""
				continue
			}

			cut := htmlTextCut(segment, remaining)
			if cut <= 0 {
				if current.Len() > 0 {
					chunks = append(chunks, current.String()+renderClosingTags(stack))
					current.Reset()
					current.WriteString(renderOpeningTags(stack))
					continue
				}
				cut = remaining
			}

			current.WriteString(segment[:cut])
			segment = segment[cut:]
			chunks = append(chunks, current.String()+renderClosingTags(stack))
			current.Reset()
			current.WriteString(renderOpeningTags(stack))
		}
		i = end
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String()+renderClosingTags(stack))
	}
	return chunks
}

func parseHTMLTag(token string) (name string, closing bool, selfClosing bool, ok bool) {
	if len(token) < 3 || token[0] != '<' || token[len(token)-1] != '>' {
		return "", false, false, false
	}
	body := strings.TrimSpace(token[1 : len(token)-1])
	if body == "" {
		return "", false, false, false
	}
	if body[0] == '/' {
		closing = true
		body = strings.TrimSpace(body[1:])
	}
	if strings.HasSuffix(body, "/") {
		selfClosing = true
		body = strings.TrimSpace(strings.TrimSuffix(body, "/"))
	}
	if body == "" {
		return "", false, false, false
	}
	if idx := strings.IndexAny(body, " \t\r\n"); idx != -1 {
		body = body[:idx]
	}
	return strings.ToLower(body), closing, selfClosing, true
}

func renderOpeningTags(stack []htmlOpenTag) string {
	var sb strings.Builder
	for _, tag := range stack {
		sb.WriteString(tag.raw)
	}
	return sb.String()
}

func renderClosingTags(stack []htmlOpenTag) string {
	var sb strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		sb.WriteString("</")
		sb.WriteString(stack[i].name)
		sb.WriteString(">")
	}
	return sb.String()
}

func htmlTextCut(text string, limit int) int {
	if len(text) <= limit {
		return len(text)
	}
	cut := limit
	if idx := strings.LastIndex(text[:limit], "\n"); idx > 0 {
		cut = idx
	} else if idx := strings.LastIndex(text[:limit], " "); idx > 0 {
		cut = idx
	}
	cut = avoidEntitySplit(text, cut)
	if cut <= 0 || cut > limit {
		cut = avoidEntitySplit(text, limit)
	}
	return cut
}

func avoidEntitySplit(text string, cut int) int {
	if cut <= 0 || cut >= len(text) {
		return cut
	}
	amp := strings.LastIndex(text[:cut], "&")
	if amp == -1 {
		return cut
	}
	if semi := strings.LastIndex(text[:cut], ";"); semi > amp {
		return cut
	}
	if end := strings.IndexByte(text[amp:], ';'); end != -1 && amp < cut {
		return amp
	}
	return cut
}

// --- Retry logic ---

const (
	baseBackoff = 2 * time.Second
	maxBackoff  = 60 * time.Second
	sendTimeout = 2 * time.Minute
)

func withDeliveryTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= sendTimeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, sendTimeout)
}

func transportPauseBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := 30 * time.Second
	for i := 1; i < failures; i++ {
		d *= 2
	}
	if d > time.Minute {
		return time.Minute
	}
	return d
}

func (d *daemon) noteSendResult(name string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.sendPause, name)
		d.sendFails[name] = 0
		return
	}
	d.sendFails[name]++
	wait := transportPauseBackoff(d.sendFails[name])
	d.sendPause[name] = time.Now().Add(wait)
	log.Printf("transport %s outbound degraded, pausing inbound reads for %v: %v", name, wait, err)
}

// waitOutboundReady gates long-polling while outbound delivery is degraded.
// If we cannot answer, reading more updates only increases backlog and hides the
// actual failure mode from operators, so we intentionally pause intake here.
func (d *daemon) waitOutboundReady(ctx context.Context, name string) bool {
	for {
		d.mu.Lock()
		until, paused := d.sendPause[name]
		d.mu.Unlock()
		if !paused {
			return true
		}
		wait := time.Until(until)
		if wait <= 0 {
			d.mu.Lock()
			if current, ok := d.sendPause[name]; ok && !time.Now().Before(current) {
				delete(d.sendPause, name)
			}
			d.mu.Unlock()
			return true
		}
		if !sleepCtx(ctx, wait) {
			return false
		}
	}
}

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

// trySendReturnID sends text with format and returns the created message ID.
func trySendReturnID(ctx context.Context, t transport.Transport, chatID, replyTo, text, format string) (string, error) {
	var msgID string
	err := retryDo(ctx, func() error {
		var err error
		msgID, err = t.SendMessageReturnID(chatID, text, replyTo, format)
		return err
	})
	if err != nil && format != "" && ctx.Err() == nil {
		log.Printf("send error (%s): %v, retrying plain", format, err)
		err = retryDo(ctx, func() error {
			var err error
			msgID, err = t.SendMessageReturnID(chatID, text, replyTo, "")
			return err
		})
	}
	return msgID, err
}

// tryEdit edits text with format, retrying on transient errors.
// Falls back to plain text if formatted edit fails with a permanent error.
func tryEdit(ctx context.Context, t transport.Transport, chatID, msgID, text, format string) error {
	err := retryDo(ctx, func() error {
		return t.EditMessage(chatID, msgID, text, format)
	})
	if err == nil {
		return nil
	}
	// "message is not modified" means the content is already correct — not an error.
	var apiErr *transport.APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.Description, "not modified") {
		return nil
	}
	if format != "" && ctx.Err() == nil {
		log.Printf("edit error (%s): %v, retrying plain", format, err)
		return retryDo(ctx, func() error {
			return t.EditMessage(chatID, msgID, text, "")
		})
	}
	return err
}

type messageChain struct {
	ids  []string
	msgs map[string]string
}

func newMessageChain(ids ...string) *messageChain {
	chain := &messageChain{msgs: make(map[string]string)}
	for _, id := range ids {
		if id == "" {
			continue
		}
		chain.ids = append(chain.ids, id)
	}
	return chain
}

func (mc *messageChain) ensure() *messageChain {
	if mc == nil {
		return newMessageChain()
	}
	if mc.msgs == nil {
		mc.msgs = make(map[string]string)
	}
	return mc
}

// deliverFinal sends the final response, splitting into chunks if needed.
// The first chunk edits the progress message; remaining chunks are new messages.
// On total failure, attempts a last-resort plain error notification.
func (d *daemon) deliverFinal(ctx context.Context, fullChatID string, t transport.Transport, chatID string, chain *messageChain, text, format string) {
	_, _ = d.syncMessageChain(ctx, fullChatID, t, chatID, "", chain, text, format)
}

// syncMessageChain keeps a chunked message chain in sync with the provided text.
// It is shared by progress updates and final delivery, so HTML-safe splitting and
// message-length handling live in exactly one place.
func (d *daemon) syncMessageChain(ctx context.Context, fullChatID string, t transport.Transport, chatID, replyTo string, chain *messageChain, text, format string) (*messageChain, error) {
	if format == "" {
		text = stripHTML(text)
	}
	chunks := splitMessage(text, maxMessageLen, format)
	transportName := transportPrefix(fullChatID)
	chain = chain.ensure()

	for i, chunk := range chunks {
		if ctx.Err() != nil {
			return chain, ctx.Err()
		}
		var err error
		sendCtx, sendCancel := withDeliveryTimeout(ctx)
		if i < len(chain.ids) && chain.ids[i] != "" {
			cacheKey := chain.ids[i]
			cacheVal := chunk + "\x00" + format
			if chain.msgs[cacheKey] == cacheVal {
				sendCancel()
				continue
			}
			err = tryEdit(sendCtx, t, chatID, chain.ids[i], chunk, format)
			if err == nil {
				chain.msgs[cacheKey] = cacheVal
			}
		} else {
			var msgID string
			chunkReplyTo := ""
			if i == 0 {
				chunkReplyTo = replyTo
			}
			msgID, err = trySendReturnID(sendCtx, t, chatID, chunkReplyTo, chunk, format)
			if err == nil {
				chain.ids = append(chain.ids, msgID)
				chain.msgs[msgID] = chunk + "\x00" + format
			}
		}
		sendCancel()
		if err != nil {
			d.noteSendResult(transportName, err)
			log.Printf("deliver error (chunk %d/%d): %v", i+1, len(chunks), err)
			// Last resort: try to notify user about the failure.
			if i == 0 && ctx.Err() == nil {
				notifyCtx, notifyCancel := withDeliveryTimeout(ctx)
				notifyErr := retryDo(notifyCtx, func() error {
					return t.SendMessage(chatID, "Ошибка доставки ответа. Попробуйте /status", "", "")
				})
				notifyCancel()
				d.noteSendResult(transportName, notifyErr)
			}
			return chain, err
		}
		d.noteSendResult(transportName, nil)
	}
	return chain, nil
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
	ctx, cancel := withDeliveryTimeout(context.Background())
	err := trySend(ctx, t, raw, replyTo, text, fmtStr)
	cancel()
	d.noteSendResult(transportPrefix(chatID), err)
	if err != nil {
		log.Printf("send error: %v", err)
	}
}

func (d *daemon) sendPlain(chatID, replyTo, text string) {
	t, raw, _ := d.transportFor(chatID)
	if t == nil {
		log.Printf("no transport for %s", chatID)
		return
	}
	ctx, cancel := withDeliveryTimeout(context.Background())
	err := retryDo(ctx, func() error {
		return t.SendMessage(raw, text, replyTo, "")
	})
	cancel()
	d.noteSendResult(transportPrefix(chatID), err)
	if err != nil {
		log.Printf("send error: %v", err)
	}
}
