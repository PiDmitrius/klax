package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PiDmitrius/klax/internal/transport"
)

const maxMessageLen = 2048

// splitMessage splits text into chunks that fit within the message limit.
// Splits on newlines when possible, otherwise hard-cuts.
func splitMessage(text string, limit int, format string) []string {
	if format == "html" {
		return splitHTMLMessage(text, limit)
	}
	if format == "rich" {
		return splitRichMessage(text, limit)
	}
	// format=="" (vk, ym) has no tag-stack to keep balanced across chunks —
	// except ym actually renders ``` fenced code blocks (VK has no formatting
	// at all), so a fence spanning a chunk boundary needs the same care
	// splitHTMLMessage gives <pre>: close it at the cut, reopen it after.
	// Only reached for text containing a fence at all, so the common
	// (fence-free) case keeps using the plain splitter untouched.
	if strings.Contains(text, "```") {
		return splitPlainFencedMessage(text, limit)
	}
	return splitPlainMessage(text, limit)
}

// splitPlainMessage is the byte/newline-based splitter for plain (format=="")
// text with no ``` fence to keep balanced.
func splitPlainMessage(text string, limit int) []string {
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
		if idx := strings.LastIndex(text[:limit], "\n"); isGoodSoftCut(idx, limit) {
			cut = idx
		}
		cut = alignUTF8Cut(text, cut)
		if cut <= 0 {
			_, size := utf8.DecodeRuneInString(text)
			cut = size
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

// splitPlainFencedMessage splits format=="" text that contains at least one
// ``` fence, line by line, keeping every fence balanced across chunks: a
// chunk that would end while still inside a fence gets a closing ``` appended
// and the next chunk reopens it with the same language tag — otherwise one
// chunk would render an unterminated code block and the next would start
// mid-block with no opening fence.
func splitPlainFencedMessage(text string, limit int) []string {
	lines := strings.Split(text, "\n")
	var chunks []string
	var cur []string
	curLen := 0
	inFence := false
	fenceLang := ""

	fenceMarker := func() string {
		return "```" + fenceLang
	}
	flush := func() {
		if len(cur) == 0 {
			return
		}
		body := strings.Join(cur, "\n")
		if inFence {
			body += "\n```"
		}
		chunks = append(chunks, body)
		cur = nil
		curLen = 0
		if inFence {
			cur = append(cur, fenceMarker())
			curLen = len(fenceMarker())
		}
	}
	// appendLine adds one already-limit-sized piece, flushing first if it
	// would overflow. Shared by the normal per-line path and the oversized-
	// line hard-split below, so both go through the same fence bookkeeping.
	appendLine := func(line string) {
		sep := 0
		if len(cur) > 0 {
			sep = 1
		}
		reserve := 0
		if inFence {
			reserve = len("\n```")
		}
		// len(cur) > 1 guards forward progress: never flush a chunk that
		// holds nothing but a just-reopened fence marker.
		if curLen+sep+len(line)+reserve > limit && len(cur) > 1 {
			flush()
			sep = 0
			if len(cur) > 0 {
				sep = 1
			}
		}
		cur = append(cur, line)
		curLen += sep + len(line)
	}

	for _, line := range lines {
		reserve := 0
		if inFence {
			reserve = len("\n```")
		}
		maxLine := limit - reserve
		// A line that can never fit any chunk on its own (e.g. a huge
		// unbroken tool-output line inside a fence) must be hard-split here,
		// through appendLine/flush, so the fence stays balanced across the
		// pieces — deferring to the fence-oblivious byte splitter (as a
		// post-pass) would split an already-fence-wrapped chunk with no idea
		// where the markers were. A static per-piece budget can overshoot
		// `limit` by the reopened fence marker's own size (a few bytes) —
		// accepted deliberately: maxMessageLen is a soft one-message target,
		// not a hard API ceiling (real platform limits sit well above it),
		// so the extra logic to shave that off isn't worth the complexity.
		if maxLine > 0 && len(line) > maxLine {
			flush() // start the oversized line in its own fresh chunk
			rest := line
			for len(rest) > 0 {
				cut := maxLine
				if cut > len(rest) {
					cut = len(rest)
				}
				cut = alignUTF8Cut(rest, cut)
				if cut <= 0 {
					_, size := utf8.DecodeRuneInString(rest)
					cut = size
				}
				appendLine(rest[:cut])
				rest = rest[cut:]
				if len(rest) > 0 {
					flush()
				}
			}
		} else {
			appendLine(line)
		}

		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence = true
				fenceLang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			} else {
				inFence = false
				fenceLang = ""
			}
		}
	}
	flush()
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
				opening := renderOpeningTags(stack)
				if current.Len() > len(opening) {
					chunks = append(chunks, current.String()+renderClosingTags(stack))
					current.Reset()
					current.WriteString(opening)
					continue
				}
				// Tag overhead alone meets the limit — re-flushing can't make
				// progress, so write one rune to guarantee forward progress
				// regardless of how unbalanced the open-tag stack is.
				_, size := utf8.DecodeRuneInString(segment)
				current.WriteString(segment[:size])
				segment = segment[size:]
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

// splitRichMessage splits a Rich HTML message (mdhtml.ConvertRich output) into
// chunks of at most `limit` bytes. It cuts on top-level block boundaries so a
// table, list or code block is normally kept intact; a block that is itself larger
// than the limit is split internally (so no chunk — nor the plain-text fallback —
// overruns Telegram's ceiling). Our 2048 is a soft target for one-message-per-answer.
func splitRichMessage(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	// Split any over-limit block so it can't ship as one over-ceiling chunk: a
	// <pre> is split on its internal newlines; any other oversized block (a giant
	// paragraph, list, table or blockquote) goes through the inline-tag-aware
	// splitter, which re-balances tags across the cut.
	var blocks []string
	for _, b := range topLevelRichBlocks(text) {
		switch {
		case len(b) <= limit:
			blocks = append(blocks, b)
		case strings.HasPrefix(b, "<pre>"):
			blocks = append(blocks, splitPreBlock(b, limit)...)
		default:
			// Any other oversized block (a huge paragraph, list, or table) — split
			// with the inline-tag-aware splitter, which re-closes/re-opens tags so
			// each piece stays balanced. This keeps every chunk (and therefore the
			// plain-text fallback) under the limit, so no single block can overrun
			// Telegram's ceiling.
			blocks = append(blocks, splitHTMLMessage(b, limit)...)
		}
	}
	var chunks []string
	var cur strings.Builder
	for _, b := range blocks {
		switch {
		case cur.Len() == 0:
			cur.WriteString(b)
		case cur.Len()+1+len(b) > limit:
			chunks = append(chunks, cur.String())
			cur.Reset()
			cur.WriteString(b)
		default:
			cur.WriteString("\n")
			cur.WriteString(b)
		}
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

// splitPreBlock breaks an over-limit <pre><code…> block into several smaller <pre>
// blocks on its internal newlines, re-wrapping each piece with the same opening
// tag. A single line longer than the limit is kept whole (rare).
func splitPreBlock(block string, limit int) []string {
	codeOpen := strings.Index(block, "<code")
	closeIdx := strings.LastIndex(block, "</code></pre>")
	if codeOpen < 0 || closeIdx < 0 {
		return []string{block}
	}
	openEnd := strings.IndexByte(block[codeOpen:], '>')
	if openEnd < 0 {
		return []string{block}
	}
	prefix := block[:codeOpen+openEnd+1] // "<pre><code …>"
	const suffix = "</code></pre>"
	inner := block[codeOpen+openEnd+1 : closeIdx]

	budget := limit - len(prefix) - len(suffix)
	if budget < 1 {
		budget = 1
	}
	var out []string
	var cur []string
	curLen := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, prefix+strings.Join(cur, "\n")+suffix)
			cur, curLen = nil, 0
		}
	}
	for _, line := range strings.Split(inner, "\n") {
		// A single code line longer than the budget is hard-split (UTF-8/entity-safe
		// via htmlTextCut) so one unbroken line can't ship as an over-ceiling chunk.
		for len(line) > budget {
			flush()
			cut := htmlTextCut(line, budget)
			out = append(out, prefix+line[:cut]+suffix)
			line = line[cut:]
		}
		add := len(line)
		if len(cur) > 0 {
			add++ // newline joiner
		}
		if len(cur) > 0 && curLen+add > budget {
			flush()
			add = len(line)
		}
		cur = append(cur, line)
		curLen += add
	}
	flush()
	if len(out) == 0 {
		return []string{block}
	}
	return out
}

// topLevelRichBlocks splits ConvertRich output back into its top-level blocks.
// Blocks are newline-separated; a <pre>…</pre> code block is the only block that
// may itself span multiple lines, so its lines are kept together.
func topLevelRichBlocks(text string) []string {
	var blocks []string
	var pre []string
	inPre := false
	for _, line := range strings.Split(text, "\n") {
		if inPre {
			pre = append(pre, line)
			if strings.Contains(line, "</pre>") {
				blocks = append(blocks, strings.Join(pre, "\n"))
				pre = nil
				inPre = false
			}
			continue
		}
		opens := strings.Count(line, "<pre>") + strings.Count(line, "<pre ")
		if opens > strings.Count(line, "</pre>") {
			inPre = true
			pre = []string{line}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		blocks = append(blocks, line)
	}
	if len(pre) > 0 {
		blocks = append(blocks, strings.Join(pre, "\n"))
	}
	return blocks
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
	name = strings.ToLower(body)
	if voidHTMLElements[name] {
		// Void elements (e.g. <br>) have no close tag, so they must never be pushed
		// onto the open-tag stack — otherwise an unclosed <br> per line accumulates
		// unbounded and can spin the splitter forever (rich blockquotes use <br>).
		selfClosing = true
	}
	return name, closing, selfClosing, true
}

var voidHTMLElements = map[string]bool{
	"br": true, "hr": true, "img": true, "wbr": true, "area": true, "base": true,
	"col": true, "embed": true, "input": true, "link": true, "meta": true,
	"source": true, "track": true,
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
	if idx := strings.LastIndex(text[:limit], "\n"); isGoodSoftCut(idx, limit) {
		cut = idx
	} else if idx := strings.LastIndex(text[:limit], " "); isGoodSoftCut(idx, limit) {
		cut = idx
	}
	cut = avoidEntitySplit(text, cut)
	if cut <= 0 || cut > limit {
		cut = avoidEntitySplit(text, limit)
	}
	cut = alignUTF8Cut(text, cut)
	if cut <= 0 {
		_, size := utf8.DecodeRuneInString(text)
		return size
	}
	return cut
}

func isGoodSoftCut(idx, limit int) bool {
	if idx <= 0 {
		return false
	}
	if limit <= 64 {
		return true
	}
	return idx >= limit*3/4
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

func alignUTF8Cut(text string, cut int) int {
	if cut <= 0 || cut >= len(text) {
		return cut
	}
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
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

// isReplyTargetError reports whether err is a permanent API error caused by
// the reply-to message being deleted/inaccessible. Such errors should not
// fail the whole send — we drop reply_to and retry as a plain message.
func isReplyTargetError(err error) bool {
	var apiErr *transport.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	desc := strings.ToLower(apiErr.Description)
	if strings.Contains(desc, "message to be replied") {
		return true
	}
	if strings.Contains(desc, "reply") &&
		(strings.Contains(desc, "not found") ||
			strings.Contains(desc, "invalid") ||
			strings.Contains(desc, "deleted")) {
		return true
	}
	return false
}

// trySend sends text with format, retrying on transient errors.
// Falls back to plain text if formatted send fails with a permanent error.
// If the reply-to target is gone, retries once without reply_to.
func trySend(ctx context.Context, t transport.Transport, chatID, replyTo, text, format string) error {
	err := sendWithFormatFallback(ctx, t, chatID, replyTo, text, format)
	if err != nil && replyTo != "" && isReplyTargetError(err) && ctx.Err() == nil {
		log.Printf("send error (reply target gone): %v, retrying without reply_to", err)
		err = sendWithFormatFallback(ctx, t, chatID, "", text, format)
	}
	return err
}

func sendWithFormatFallback(ctx context.Context, t transport.Transport, chatID, replyTo, text, format string) error {
	err := retryDo(ctx, func() error {
		return t.SendMessage(chatID, text, replyTo, format)
	})
	if err != nil && format != "" && ctx.Err() == nil && !isReplyTargetError(err) {
		log.Printf("send error (%s): %v, retrying plain", format, err)
		return retryDo(ctx, func() error {
			return t.SendMessage(chatID, plainFallback(text, format), replyTo, "")
		})
	}
	return err
}

// trySendReturnID sends text with format and returns the created message ID.
func trySendReturnID(ctx context.Context, t transport.Transport, chatID, replyTo, text, format string) (string, error) {
	msgID, err := sendReturnIDWithFormatFallback(ctx, t, chatID, replyTo, text, format)
	if err != nil && replyTo != "" && isReplyTargetError(err) && ctx.Err() == nil {
		log.Printf("send error (reply target gone): %v, retrying without reply_to", err)
		msgID, err = sendReturnIDWithFormatFallback(ctx, t, chatID, "", text, format)
	}
	return msgID, err
}

func sendReturnIDWithFormatFallback(ctx context.Context, t transport.Transport, chatID, replyTo, text, format string) (string, error) {
	var msgID string
	err := retryDo(ctx, func() error {
		var err error
		msgID, err = t.SendMessageReturnID(chatID, text, replyTo, format)
		return err
	})
	if err != nil && format != "" && ctx.Err() == nil && !isReplyTargetError(err) {
		log.Printf("send error (%s): %v, retrying plain", format, err)
		err = retryDo(ctx, func() error {
			var err error
			msgID, err = t.SendMessageReturnID(chatID, plainFallback(text, format), replyTo, "")
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
			return t.EditMessage(chatID, msgID, plainFallback(text, format), "")
		})
	}
	return err
}

type transportOp struct {
	fullChatID string
	messageID  string
	replyTo    string
	text       string
	format     string
	returnID   bool
	useDefault bool
}

type transportResult struct {
	messageID string
	activity  uint64
}

func (d *daemon) performTransportOp(ctx context.Context, op transportOp) (transportResult, error) {
	t, rawChatID, fmtStr := d.transportFor(op.fullChatID)
	if t == nil {
		err := errors.New("no transport for " + op.fullChatID)
		log.Printf("%v", err)
		return transportResult{}, err
	}

	text := op.text
	format := op.format
	if op.useDefault {
		format = fmtStr
	}
	if format == "" && op.useDefault {
		text = plainRenderForChat(op.fullChatID, text)
	}

	var (
		res transportResult
		err error
	)
	if op.messageID != "" {
		err = tryEdit(ctx, t, rawChatID, op.messageID, text, format)
	} else if op.returnID {
		res.messageID, err = trySendReturnID(ctx, t, rawChatID, op.replyTo, text, format)
		if err == nil {
			res.activity = d.bumpChatActivity(op.fullChatID)
		}
	} else {
		err = trySend(ctx, t, rawChatID, op.replyTo, text, format)
		if err == nil {
			res.activity = d.bumpChatActivity(op.fullChatID)
		}
	}

	d.noteSendResult(transportPrefix(op.fullChatID), err)
	return res, err
}

type messageChain struct {
	ids                []string
	msgs               map[string]string
	anchorReplyTo      string
	lastCreateActivity uint64
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

// syncMessageChain keeps a chunked message chain in sync with the provided text.
// It is shared by progress updates and final delivery, so HTML-safe splitting and
// message-length handling live in exactly one place.
func (d *daemon) syncMessageChain(ctx context.Context, fullChatID, replyTo string, chain *messageChain, text, format string) (*messageChain, error) {
	if format == "" {
		text = plainRenderForChat(fullChatID, text)
	}
	chunks := splitMessage(text, maxMessageLen, format)
	return d.syncMessageChainChunks(ctx, fullChatID, replyTo, chain, chunks, format)
}

func (d *daemon) syncMessageChainChunks(ctx context.Context, fullChatID, replyTo string, chain *messageChain, chunks []string, format string) (*messageChain, error) {
	chain = chain.ensure()
	if replyTo != "" {
		chain.anchorReplyTo = replyTo
	}

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
			_, err = d.performTransportOp(sendCtx, transportOp{
				fullChatID: fullChatID,
				messageID:  chain.ids[i],
				text:       chunk,
				format:     format,
			})
			if err == nil {
				chain.msgs[cacheKey] = cacheVal
			}
		} else {
			chunkReplyTo := ""
			if len(chain.ids) == 0 {
				chunkReplyTo = chain.anchorReplyTo
			} else if chain.anchorReplyTo != "" && d.chatActivity(fullChatID) != chain.lastCreateActivity {
				chunkReplyTo = chain.anchorReplyTo
			}
			res, sendErr := d.performTransportOp(sendCtx, transportOp{
				fullChatID: fullChatID,
				replyTo:    chunkReplyTo,
				text:       chunk,
				format:     format,
				returnID:   true,
			})
			err = sendErr
			if err == nil {
				chain.ids = append(chain.ids, res.messageID)
				chain.msgs[res.messageID] = chunk + "\x00" + format
				chain.lastCreateActivity = res.activity
			}
		}
		sendCancel()
		if err != nil {
			log.Printf("deliver error (chunk %d/%d): %v", i+1, len(chunks), err)
			// Last resort: try to notify user about the failure.
			if i == 0 && ctx.Err() == nil {
				notifyCtx, notifyCancel := withDeliveryTimeout(ctx)
				_, _ = d.performTransportOp(notifyCtx, transportOp{
					fullChatID: fullChatID,
					text:       "Ошибка доставки ответа. Попробуйте /status",
				})
				notifyCancel()
			}
			return chain, err
		}
	}
	return chain, nil
}

func (d *daemon) syncFinalMessageChainChunks(fullChatID, replyTo string, chain *messageChain, chunks []string, format string) (*messageChain, error) {
	ctx, cancel := withDeliveryTimeout(context.Background())
	defer cancel()
	return d.syncMessageChainChunks(ctx, fullChatID, replyTo, chain, chunks, format)
}

func (d *daemon) sendMessage(chatID, replyTo, text string) {
	ctx, cancel := withDeliveryTimeout(context.Background())
	_, err := d.performTransportOp(ctx, transportOp{
		fullChatID: chatID,
		replyTo:    replyTo,
		text:       text,
		useDefault: true,
	})
	cancel()
	if err != nil {
		log.Printf("send error: %v", err)
	}
}

func (d *daemon) sendPlain(chatID, replyTo, text string) {
	ctx, cancel := withDeliveryTimeout(context.Background())
	_, err := d.performTransportOp(ctx, transportOp{
		fullChatID: chatID,
		replyTo:    replyTo,
		text:       text,
		format:     "",
	})
	cancel()
	if err != nil {
		log.Printf("send error: %v", err)
	}
}
