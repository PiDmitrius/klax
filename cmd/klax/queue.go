package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/mdhtml"
	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/session"
)

// progressEditInterval is the minimum gap between two Telegram edits of the
// same progress message. Keeps us well under Telegram's per-chat edit rate
// limit without coupling stdout reading to network latency. Matches the
// openclaw draft-stream default (DEFAULT_THROTTLE_MS = 1000) — at 500ms the
// rapid look-ahead/idle bursts from the runner could flicker.
const progressEditInterval = 1 * time.Second

func sanitizeAttachmentFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "attachment"
	}
	return name
}

func formatRunFailure(logItems []runner.ProgressEvent, format string, err error) string {
	// In rich mode each trailing marker must be its own block, and the error text
	// must be escaped so stray <, > or & don't break the rich parser.
	mark := func(s string) string {
		if format == "rich" {
			return "<p>" + s + "</p>"
		}
		return s
	}
	errLine := func(e error) string {
		s := fmt.Sprintf("❌ Ошибка: %v", e)
		if format == "rich" {
			return "<p>" + htmlEscapeLogText(s) + "</p>"
		}
		return s
	}

	// sep is the breathing gap between the log and the trailing markers — a real
	// spacer block in rich (inter-block whitespace is ignored), a blank line in
	// legacy.
	sep := logSeparator(format, false)

	if errors.Is(err, context.Canceled) {
		if len(logItems) > 0 {
			return formatLogItems(logItems, format) + sep + mark("❌ Прервано.")
		}
		return mark("❌ Прервано.")
	}

	head := mark("...")
	if len(logItems) > 0 {
		head = formatLogItems(logItems, format) + sep + mark("...")
	}
	return head + "\n" + errLine(err)
}

func (d *daemon) syncFinalMessageChain(fullChatID, replyTo string, chain *messageChain, text, format string) (*messageChain, error) {
	ctx, cancel := withDeliveryTimeout(context.Background())
	defer cancel()
	return d.syncMessageChain(ctx, fullChatID, replyTo, chain, text, format)
}

func (d *daemon) enqueue(chatID, msgID, text string) {
	d.enqueueWithAttachments(chatID, msgID, text, nil)
}

func (d *daemon) enqueueWithAttachments(chatID, msgID, text string, attachments []attachment) {
	if text == "" && len(attachments) == 0 {
		d.sendMessage(chatID, msgID, "∅")
		return
	}
	if d.isDraining() {
		d.sendMessage(chatID, msgID, "🔄 klax перезапускается, новые задачи не принимаются.")
		return
	}

	sk := d.sessionKey(chatID)
	// Bind the message to whichever session is active right now. /switch and
	// /new after this point only affect future messages — this one will run
	// against the captured session even if the user moves on.
	sess := d.store.Active(sk)
	if sess == nil {
		d.sendMessage(chatID, msgID, "❌ Нет активной сессии. Напиши /new")
		return
	}
	sr := d.getRunner(sk, sess.Created)

	sr.mu.Lock()
	qm := queuedMsg{
		chatID:      chatID,
		msgID:       msgID,
		text:        text,
		attachments: attachments,
		sessKey:     sk,
		sessCreated: sess.Created,
	}
	busy := sr.runner.IsBusy()
	if busy {
		// Send queue notification and capture its ID for later reuse as progress message.
		qlen := len(sr.queue) + 1 // +1 for this message being added
		ctx, cancel := withDeliveryTimeout(context.Background())
		res, err := d.performTransportOp(ctx, transportOp{
			fullChatID: chatID,
			replyTo:    msgID,
			text:       fmt.Sprintf("⏳ В очереди: %d", qlen),
			returnID:   true,
			format:     "",
		})
		cancel()
		if err == nil {
			qm.progressID = res.messageID
			qm.progressSeq = res.activity
		}
	}
	sr.queue = append(sr.queue, qm)
	sr.mu.Unlock()

	if busy {
		return
	}

	go d.processSessionQueue(sr)
}

func (d *daemon) processSessionQueue(sr *sessionRunner) {
	sr.mu.Lock()
	if sr.processing {
		sr.mu.Unlock()
		return
	}
	sr.processing = true
	sr.mu.Unlock()

	d.drainWg.Add(1)
	defer d.drainWg.Done()

	defer func() {
		sr.mu.Lock()
		sr.processing = false
		sr.mu.Unlock()
	}()

	for {
		sr.mu.Lock()
		if len(sr.queue) == 0 {
			sr.mu.Unlock()
			return
		}
		msg := sr.queue[0]
		sr.queue = sr.queue[1:]
		// Update queue position in remaining messages' progress notifications.
		for i, qm := range sr.queue {
			if qm.progressID != "" {
				ctx, cancel := withDeliveryTimeout(context.Background())
				_, _ = d.performTransportOp(ctx, transportOp{
					fullChatID: qm.chatID,
					messageID:  qm.progressID,
					text:       fmt.Sprintf("⏳ В очереди: %d", i+1),
					format:     "",
				})
				cancel()
			}
		}
		sr.mu.Unlock()

		d.runBackend(msg)
	}
}

func (d *daemon) clearSessionQueue(sk string, created int64) []queuedMsg {
	sr := d.lookupRunner(sk, created)
	if sr == nil {
		return nil
	}
	sr.mu.Lock()
	queued := append([]queuedMsg(nil), sr.queue...)
	sr.queue = nil
	sr.mu.Unlock()
	return queued
}

// abortSession cancels any in-flight run for the session and marks its queued
// messages as aborted. Cancelling the run context makes Runner.Run SIGTERM →
// SIGKILL the backend's process group, so this reaches grandchildren (e.g. rust
// codex behind the npm shim). Returns true if there was anything to abort.
//
// When closing is set (the /nuke teardown path) the runner is flagged so a run
// caught between dequeue and installing its cancel handle bails in runBackend
// instead of launching the backend against a session about to be deleted. On
// that path the active check also counts processing — not just runner.IsBusy()
// — so the same window is reported as aborted. Plain /abort (closing=false)
// keeps the original IsBusy()-only check: it cannot stop a run that has no
// cancel handle yet, so claiming it did would be a lie.
func (d *daemon) abortSession(sk string, created int64, closing bool) bool {
	sr := d.lookupRunner(sk, created)
	var cancelFn context.CancelFunc
	var active bool
	if sr != nil {
		sr.mu.Lock()
		if closing {
			sr.closing = true
		}
		cancelFn = sr.cancel
		active = sr.runner.IsBusy() || (closing && sr.processing)
		sr.mu.Unlock()
	}
	queued := d.clearSessionQueue(sk, created)
	hasWork := active || cancelFn != nil || len(queued) > 0
	if cancelFn != nil {
		cancelFn()
	}
	d.abortQueuedMessages(queued)
	return hasWork
}

func (d *daemon) abortQueuedMessages(msgs []queuedMsg) {
	for _, qm := range msgs {
		if qm.progressID == "" {
			continue
		}
		ctx, cancel := withDeliveryTimeout(context.Background())
		_, err := d.performTransportOp(ctx, transportOp{
			fullChatID: qm.chatID,
			messageID:  qm.progressID,
			text:       "❌ Прервано.",
			format:     "",
		})
		cancel()
		if err != nil {
			log.Printf("failed to mark queued message as aborted: %v", err)
		}
	}
}

func (d *daemon) shouldReuseQueuedProgress(msg queuedMsg) bool {
	return msg.progressID != "" && msg.progressSeq != 0 && d.chatActivity(msg.chatID) == msg.progressSeq
}

func (d *daemon) runBackend(msg queuedMsg) {
	sk := msg.sessKey
	// Look up the session bound at enqueue time. If the user deleted the
	// session in the meantime the message has nowhere to land — surface that
	// instead of silently picking a different session.
	sess := d.store.Get(sk, msg.sessCreated)
	if sess == nil {
		d.sendMessage(msg.chatID, msg.msgID, "❌ Сессия удалена, сообщение не обработано.")
		return
	}

	sr := d.getRunner(sk, sess.Created)

	// Create a cancellable context for this run.
	// /abort cancels it, which stops both claude and retry loops.
	ctx, cancel := context.WithCancel(context.Background())
	sr.mu.Lock()
	sr.cancel = cancel
	closing := sr.closing
	sr.mu.Unlock()
	defer cancel()
	// Clear the cancel handle once the run is done so a later /abort on an
	// idle session reports "Нет активных сообщений в сессии." instead of the abort text.
	defer func() {
		sr.mu.Lock()
		sr.cancel = nil
		sr.mu.Unlock()
	}()

	// Guard against /nuke racing this run. closing and sr.cancel are read/written
	// under the same sr.mu as abortSession, so the two orderings are covered:
	//   - abortSession ran first → we observe closing here and bail;
	//   - we installed cancel first → abortSession sees it and cancels our ctx.
	// The remaining case is /nuke having already dropped the runner, so getRunner
	// above handed us a fresh sr without closing; the store.Get re-check catches
	// it, since dropRunner only happens after the session is Deleted.
	if closing || d.store.Get(sk, sess.Created) == nil {
		d.dropRunner(sk, sess.Created)
		d.abortQueuedMessages([]queuedMsg{msg})
		return
	}

	// Progress message — edit in place.
	// If this message was queued and nothing happened in the chat since then,
	// reuse the queue notification. Otherwise point to the new answer below.
	t, _, _ := d.transportFor(msg.chatID)
	chatFmt := d.answerFormat(msg.chatID)
	// Rich messages have their own message type: a Rich Message can only be
	// edit-streamed from a message that was *born* rich (editMessageText with
	// rich_message). So in rich mode we never reuse the plain/HTML queued
	// notification, and the placeholder itself is created as rich.
	richMode := chatFmt == "rich"
	var progressChain *messageChain
	reuseQueuedProgress := !richMode && d.shouldReuseQueuedProgress(msg)
	needsRedirectMarker := !reuseQueuedProgress && msg.progressID != ""
	if reuseQueuedProgress {
		progressChain = newMessageChain(msg.progressID)
		progressChain.lastCreateActivity = msg.progressSeq
	}
	if t != nil {
		var err error
		placeholder, placeFmt := "...", ""
		if richMode {
			placeholder, placeFmt = "<p>…</p>", "rich"
		}
		progressChain, err = d.syncMessageChain(ctx, msg.chatID, msg.msgID, progressChain, placeholder, placeFmt)
		if err != nil {
			progressChain = nil
		} else if needsRedirectMarker {
			markerCtx, markerCancel := withDeliveryTimeout(ctx)
			_, _ = d.performTransportOp(markerCtx, transportOp{
				fullChatID: msg.chatID,
				messageID:  msg.progressID,
				text:       "↓",
				format:     "",
			})
			markerCancel()
		}
	}

	verbose := d.chatVerboseEnabled(msg.chatID)

	// Progress plumbing. onProgress runs in the Runner's stdout-scanner
	// goroutine and MUST NOT block on network: if it did, the backend's
	// stdout pipe would fill and the child (e.g. rust codex behind the npm
	// shim) would hang in pipe_write. Instead we hand the latest logItems
	// snapshot to a worker via a mailbox channel and rate-limit Telegram
	// edits there. Nothing is dropped: each snapshot is cumulative, so an
	// overwritten pending snapshot loses no history — the next one carries
	// everything the previous one would have shown plus newer entries.
	var logItems []runner.ProgressEvent
	progressCh := make(chan []runner.ProgressEvent, 1)
	progressDone := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		var lastSentText string
		for {
			var snapshot []runner.ProgressEvent
			select {
			case <-progressDone:
				return
			case s, ok := <-progressCh:
				if !ok {
					return
				}
				snapshot = s
			}
			select {
			case <-progressDone:
				return
			default:
			}
			chunks := withProgressEllipsis(formatLogChunks(snapshot, "", chatFmt, maxMessageLen), chatFmt, maxMessageLen)
			cacheKey := fmt.Sprintf("%q", chunks)
			if cacheKey == lastSentText {
				continue
			}
			lastSentText = cacheKey
			if progressChain != nil && len(progressChain.ids) > 0 {
				pc, err := d.syncMessageChainChunks(ctx, msg.chatID, msg.msgID, progressChain, chunks, chatFmt)
				if err != nil {
					log.Printf("progress update failed: %v", err)
					continue
				}
				progressChain = pc
			}
			// Rate-limit edits so Telegram does not 429 us. Cancellation
			// shortcuts the wait so /abort unblocks quickly.
			select {
			case <-progressDone:
				return
			case <-ctx.Done():
			case <-time.After(progressEditInterval):
			}
		}
	}()
	onProgress := func(ev runner.ProgressEvent) {
		if !verbose {
			return
		}
		// No upstream dedup here on purpose: the progress worker already
		// suppresses duplicate edits via its `lastSentText` check, and
		// collapsing equal ProgressEvents at this level would hide real
		// repeats (same tool invoked twice, same rate-limit warning
		// reappearing after a cooldown).
		logItems = append(logItems, ev)
		snapshot := append([]runner.ProgressEvent(nil), logItems...)
		// Non-blocking mailbox: drop any stale pending snapshot in favour
		// of the newer, superset one. Never blocks the scanner.
		select {
		case progressCh <- snapshot:
		default:
			select {
			case <-progressCh:
			default:
			}
			select {
			case progressCh <- snapshot:
			default:
			}
		}
	}

	// Save attachments to a temp directory and build prompt with file paths.
	prompt := msg.text
	var tmpDir string
	if len(msg.attachments) > 0 {
		var err error
		tmpDir, err = os.MkdirTemp("", "klax-attach-*")
		if err != nil {
			log.Printf("failed to create temp dir for attachments: %v", err)
		} else {
			var filePaths []string
			for _, att := range msg.attachments {
				fp := filepath.Join(tmpDir, sanitizeAttachmentFilename(att.filename))
				if err := os.WriteFile(fp, att.data, 0644); err != nil {
					log.Printf("failed to write attachment %s: %v", att.filename, err)
					continue
				}
				filePaths = append(filePaths, fp)
			}
			if len(filePaths) > 0 {
				fileList := strings.Join(filePaths, "\n")
				if prompt == "" {
					prompt = fmt.Sprintf("Пользователь отправил файлы. Прочитай и проанализируй их:\n%s", fileList)
				} else {
					prompt = fmt.Sprintf("%s\n\nПрикреплённые файлы:\n%s", prompt, fileList)
				}
			}
		}
	}
	if tmpDir != "" {
		defer os.RemoveAll(tmpDir)
	}

	backend := d.backendFor(sess)
	result := sr.runner.Run(ctx, backend, runner.RunOptions{
		Prompt:                    prompt,
		SessionID:                 sess.ID,
		CWD:                       sess.CWD,
		Sandbox:                   sess.Sandbox,
		Model:                     sess.ModelOverride,
		Effort:                    sess.ThinkOverride,
		AppendSystemPrompt:        sess.AppendSystemPrompt,
		ClaudeTTY:                 sess.ClaudeTTY,
		SuppressNarrationProgress: !verbose,
	}, onProgress)

	// Flush the progress worker before any final-delivery path runs: the
	// worker mutates progressChain, so reading it here without a barrier
	// would race.
	close(progressDone)
	close(progressCh)
	<-workerDone

	// Persist changes onto the same session record that started the run.
	d.store.UpdateSession(sk, sess.Created, func(current *session.Session) {
		current.Messages++
		current.LastUsed = time.Now().Unix()
		if result.SessionID != "" {
			current.ID = result.SessionID
		}
		// Only update model/usage from successful runs.
		// On kill/error, system event may report a wrong default model.
		if result.Error == nil {
			if result.Usage.Model != "" {
				current.Model = result.Usage.Model
			}
			if result.Usage.ContextWindow > 0 {
				current.ContextWindow = result.Usage.ContextWindow
			}
			if result.Usage.ContextUsed > 0 {
				current.ContextUsed = result.Usage.ContextUsed
			}
		}
	})
	if result.RateLimit != nil {
		d.saveRateLimit(backend.Name(), result.RateLimit)
	}
	d.saveStore()

	if result.Error != nil {
		finalText := formatRunFailure(logItems, chatFmt, result.Error)
		if t != nil {
			// Deliver with chatFmt so a rich-formatted failure goes out as a Rich
			// Message (and reuses the rich-born progress chain when present).
			if _, err := d.syncFinalMessageChain(msg.chatID, msg.msgID, progressChain, finalText, chatFmt); err != nil {
				log.Printf("final error delivery failed: %v", err)
			}
		} else {
			d.sendMessage(msg.chatID, msg.msgID, finalText)
		}
		return
	}

	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "✅"
	}

	// Convert Claude's Markdown to the transport format.
	var formatted string
	switch chatFmt {
	case "rich":
		formatted = mdhtml.ConvertRich(text)
	case "html":
		formatted = mdhtml.Convert(text)
	default:
		formatted = text
	}

	// Build final message: progress log + separator + answer.
	var finalChunks []string
	if len(logItems) > 0 {
		finalChunks = formatLogChunks(logItems, formatted, chatFmt, maxMessageLen)
	} else {
		finalChunks = splitMessage(formatted, maxMessageLen, chatFmt)
	}

	if t != nil {
		_, err := d.syncFinalMessageChainChunks(msg.chatID, msg.msgID, progressChain, finalChunks, chatFmt)
		if err != nil {
			log.Printf("final delivery failed: %v", err)
		}
	}
}
