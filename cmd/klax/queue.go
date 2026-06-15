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
	d.enqueueToSession(chatID, msgID, text, attachments, 0)
}

// enqueueToSession queues a message against a session in the chat. targetCreated
// selects which: 0 binds to whichever session is active right now (every
// messenger path — /switch and /new after this only affect future messages),
// while a positive value binds to exactly that session (a web-UI tab), validated
// up front so a stale tab gets a clear error instead of silently hitting the
// active session.
func (d *daemon) enqueueToSession(chatID, msgID, text string, attachments []attachment, targetCreated int64) {
	if text == "" && len(attachments) == 0 {
		d.sendMessage(chatID, msgID, "∅")
		return
	}
	if d.isDraining() {
		d.sendMessage(chatID, msgID, "🔄 klax перезапускается, новые задачи не принимаются.")
		return
	}

	sk := d.sessionKey(chatID)
	var sess *session.Session
	if targetCreated > 0 {
		// Explicit target (a UI tab): bind to exactly that session.
		sess = d.store.Get(sk, targetCreated)
		if sess == nil {
			d.sendMessage(chatID, msgID, "❌ Сессия не найдена.")
			return
		}
	} else {
		// Bind the message to whichever session is active right now. /switch and
		// /new after this point only affect future messages — this one will run
		// against the captured session even if the user moves on.
		sess = d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "❌ Нет активной сессии. Напиши /new")
			return
		}
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
	// The "В очереди" notice is a messenger placeholder later reused as the
	// progress message and edited in place. The UI streams independently (the
	// busy dot, typing indicator and unread badge already show the queued/working
	// state), and uiDelivery never touches this placeholder — so on the UI it
	// would just linger as a stale notice that never updates. Skip it there.
	if busy && transportPrefix(chatID) != uiPrefix {
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

	// Surface the new queue depth (the UI shows how many are waiting).
	d.broadcastSessions(sk)

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

		// A message just left the queue to run — refresh the queue depth.
		d.broadcastSessions(msg.sessKey)

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

	// Output for this turn goes through a per-turn Delivery, picked by the
	// chat's transport. The messenger delivery owns the progress message chain,
	// the rate-limited edit worker and the final render/split; the UI delivery
	// streams raw events. Persisting the session below stays here (not in the
	// delivery) so business logic is not duplicated across delivery backends.
	verbose := d.chatVerboseEnabled(msg.chatID)
	del := d.deliveryFor(ctx, msg, verbose)
	defer del.Close()

	prompt, tmpDir := d.buildTurnPrompt(msg)
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
	}, del.Progress)

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

	del.Final(result)
	// Refresh any UI tab strip watching this session (new id, message count,
	// model/ctx). The live busy dot is driven client-side by turn_start/final.
	d.broadcastSessions(sk)
}

// buildTurnPrompt writes the message's attachments to a temp directory and
// folds their paths into the prompt. It returns the prompt and the temp dir
// (empty when there are no attachments or the dir could not be created); the
// caller owns removing a non-empty tmpDir.
func (d *daemon) buildTurnPrompt(msg queuedMsg) (prompt, tmpDir string) {
	prompt = msg.text
	if len(msg.attachments) == 0 {
		return prompt, ""
	}
	var err error
	tmpDir, err = os.MkdirTemp("", "klax-attach-*")
	if err != nil {
		log.Printf("failed to create temp dir for attachments: %v", err)
		return prompt, ""
	}
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
	return prompt, tmpDir
}
