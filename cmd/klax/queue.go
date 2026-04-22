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
// limit without coupling stdout reading to network latency.
const progressEditInterval = 500 * time.Millisecond

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
	if errors.Is(err, context.Canceled) {
		if len(logItems) > 0 {
			return formatLogItems(logItems, format) + "\n\n❌ Прервано."
		}
		return "❌ Прервано."
	}

	finalText := "..."
	if len(logItems) > 0 {
		finalText = formatLogItems(logItems, format) + "\n\n..."
	}
	return finalText + fmt.Sprintf("\n❌ Ошибка: %v", err)
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
	sr.mu.Unlock()
	defer cancel()
	// Clear the cancel handle once the run is done so a later /abort on an
	// idle session reports "Нет активных сообщений в сессии." instead of the abort text.
	defer func() {
		sr.mu.Lock()
		sr.cancel = nil
		sr.mu.Unlock()
	}()

	// Progress message — edit in place.
	// If this message was queued and nothing happened in the chat since then,
	// reuse the queue notification. Otherwise point to the new answer below.
	t, _, chatFmt := d.transportFor(msg.chatID)
	var progressChain *messageChain
	reuseQueuedProgress := d.shouldReuseQueuedProgress(msg)
	needsRedirectMarker := !reuseQueuedProgress && msg.progressID != ""
	if reuseQueuedProgress {
		progressChain = newMessageChain(msg.progressID)
		progressChain.lastCreateActivity = msg.progressSeq
	}
	if t != nil {
		var err error
		progressChain, err = d.syncMessageChain(ctx, msg.chatID, msg.msgID, progressChain, "...", "")
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
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		var lastSentText string
		for snapshot := range progressCh {
			newText := formatLogItems(snapshot, chatFmt) + "\n\n..."
			if newText == lastSentText {
				continue
			}
			lastSentText = newText
			if progressChain != nil && len(progressChain.ids) > 0 {
				pc, err := d.syncMessageChain(ctx, msg.chatID, msg.msgID, progressChain, newText, chatFmt)
				if err != nil {
					log.Printf("progress update failed: %v", err)
					continue
				}
				progressChain = pc
			}
			// Rate-limit edits so Telegram does not 429 us. Cancellation
			// shortcuts the wait so /abort unblocks quickly.
			select {
			case <-ctx.Done():
			case <-time.After(progressEditInterval):
			}
		}
	}()
	onProgress := func(ev runner.ProgressEvent) {
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
		Prompt:             prompt,
		SessionID:          sess.ID,
		CWD:                sess.CWD,
		Sandbox:            sess.Sandbox,
		Model:              sess.ModelOverride,
		Effort:             sess.ThinkOverride,
		AppendSystemPrompt: sess.AppendSystemPrompt,
	}, onProgress)

	// Flush the progress worker before any final-delivery path runs: the
	// worker mutates progressChain, so reading it here without a barrier
	// would race.
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
		if progressChain != nil && len(progressChain.ids) > 0 && t != nil {
			_, err := d.syncFinalMessageChain(msg.chatID, msg.msgID, progressChain, finalText, chatFmt)
			if err != nil {
				log.Printf("final error delivery failed: %v", err)
			}
		} else {
			d.sendMessage(msg.chatID, msg.msgID, finalText)
		}
		return
	}

	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "✅ Готово."
	}

	// Convert Claude's Markdown to the transport format.
	var formatted string
	if chatFmt == "html" {
		formatted = mdhtml.Convert(text)
	} else {
		formatted = text
	}

	// Build final message: progress log + separator + answer.
	var finalText string
	if len(logItems) > 0 {
		finalText = formatLogItems(logItems, chatFmt) + "\n\n" + formatted
	} else {
		finalText = formatted
	}

	if t != nil {
		_, err := d.syncFinalMessageChain(msg.chatID, msg.msgID, progressChain, finalText, chatFmt)
		if err != nil {
			log.Printf("final delivery failed: %v", err)
		}
	}
}
