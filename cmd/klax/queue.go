package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/sessfiles"
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
	d.enqueueToSession(chatID, msgID, text, attachments, 0, "")
}

// enqueueToSession queues a message against a session in the chat. targetCreated
// selects which: 0 binds to whichever session is active right now (every
// messenger path — /switch and /new after this only affect future messages),
// while a positive value binds to exactly that session (a web-UI tab), validated
// up front so a stale tab gets a clear error instead of silently hitting the
// active session.
// enqueueToSession returns true if the message was durably accepted (queued, or
// persisted-for-replay while draining), false if it was dropped (empty text and no
// files, no such session, or a durable-write failure) — the web UI's handleSend
// uses this to answer 204 vs roll back its optimistic echo.
func (d *daemon) enqueueToSession(chatID, msgID, text string, attachments []attachment, targetCreated int64, nonce string) bool {
	if text == "" && len(attachments) == 0 {
		d.sendMessage(chatID, msgID, "∅")
		return false
	}
	draining := d.isDraining()

	sk := d.sessionKey(chatID)
	var sess *session.Session
	if targetCreated > 0 {
		// Explicit target (a UI tab): bind to exactly that session.
		sess = d.store.Get(sk, targetCreated)
		if sess == nil {
			d.sendMessage(chatID, msgID, "❌ Сессия не найдена.")
			return false
		}
	} else {
		// Bind the message to whichever session is active right now. /switch and
		// /new after this point only affect future messages — this one will run
		// against the captured session even if the user moves on.
		sess = d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "❌ Нет активной сессии. Напиши /new")
			return false
		}
	}
	sr := d.getRunner(sk, sess.Created)

	// Durably accept the message BEFORE acking: write its files and append a fsynced
	// enq record, holding only the durable-store lock — never sr.mu (the two-lock
	// rule: no disk I/O under sr.mu). The bytes go to disk; the in-memory queue then
	// holds only a reference (turnSeq + stored names + marker).
	readers := make([]sessfiles.NamedReader, 0, len(attachments))
	for _, a := range attachments {
		readers = append(readers, sessfiles.NamedReader{Name: a.filename, R: bytes.NewReader(a.data)})
	}
	turnSeq, marker, files, err := sr.store.Enqueue(chatID, msgID, nonce, text, readers)
	if err != nil {
		log.Printf("durable enqueue (%s/%d): %v", sk, sess.Created, err)
		d.sendMessage(chatID, msgID, "❌ Не удалось сохранить сообщение, попробуйте снова.")
		return false
	}

	echoText := text
	if echoText == "" && len(attachments) > 0 {
		names := make([]string, 0, len(attachments))
		for _, a := range attachments {
			names = append(names, sanitizeAttachmentFilename(a.filename))
		}
		echoText = "📎 " + strings.Join(names, ", ")
	}
	emitEcho := func() {
		if echoText != "" {
			d.uiEmit(uiUserForKey(sk), uiEvent{Type: "user", Session: sess.Created, TurnSeq: turnSeq, State: "enq", Text: echoText, Nonce: nonce})
		}
		d.broadcastSessions(sk)
	}

	// Accept-during-drain: the message is persisted but NOT started here — startup
	// replay re-enqueues it after the restart. No runner is launched, so there is no
	// drainWg Add-after-Wait race. The user still sees it land in the UI.
	if draining {
		emitEcho()
		return true
	}

	sr.mu.Lock()
	qm := queuedMsg{chatID: chatID, msgID: msgID, text: text, turnSeq: turnSeq, files: files, marker: marker, sessKey: sk, sessCreated: sess.Created}
	busy := sr.runner.IsBusy()
	// The "В очереди" notice is a messenger placeholder later reused as the progress
	// message; the UI streams independently, so skip it there.
	if busy && transportPrefix(chatID) != uiPrefix {
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

	emitEcho()

	if busy {
		return true
	}

	go d.processSessionQueue(sr)
	return true
}

// replayDurableQueues runs once at startup, before transports begin polling (so the
// session store and runner map are accessed single-threaded here). For every
// session it re-enqueues messages that were durably accepted but never started
// (enq without run — safe to run) and marks interrupted any run that crashed
// mid-flight (run without terminal — never auto-rerun, the backend may have had
// side effects; transcript-based recovery to "done" is a later refinement).
func (d *daemon) replayDurableQueues() {
	for sk, cs := range d.store.Chats {
		for _, sess := range cs.Sessions {
			sr := d.getRunner(sk, sess.Created)
			reenq, recovered, err := sr.store.Replay()
			if err != nil {
				log.Printf("durable replay (%s/%d): %v", sk, sess.Created, err)
				d.dropRunner(sk, sess.Created)
				continue
			}
			for _, t := range recovered {
				_ = sr.store.MarkErr(t.Seq, "interrupted")
			}
			if len(reenq) == 0 {
				d.dropRunner(sk, sess.Created) // no pending work — don't keep the runner
				continue
			}
			sr.mu.Lock()
			for _, t := range reenq {
				sr.queue = append(sr.queue, queuedMsg{
					chatID: t.ChatID, msgID: t.MsgID, text: t.Text,
					turnSeq: t.Seq, files: t.Files, marker: t.Marker,
					sessKey: sk, sessCreated: sess.Created,
				})
			}
			n := len(sr.queue)
			sr.mu.Unlock()
			log.Printf("durable replay: re-enqueued %d message(s) for %s/%d", n, sk, sess.Created)
			go d.processSessionQueue(sr)
		}
	}
}

func (d *daemon) processSessionQueue(sr *sessionRunner) {
	sr.mu.Lock()
	if sr.processing {
		sr.mu.Unlock()
		return
	}
	sr.processing = true
	sr.mu.Unlock()

	var lastSK string
	d.drainWg.Add(1)
	defer d.drainWg.Done()

	defer func() {
		sr.mu.Lock()
		sr.processing = false
		restart := len(sr.queue) > 0
		sr.mu.Unlock()
		if lastSK != "" {
			d.broadcastSessions(lastSK)
		}
		if restart && !d.isDraining() {
			go d.processSessionQueue(sr)
		}
	}()

	for {
		sr.mu.Lock()
		if len(sr.queue) == 0 {
			sr.mu.Unlock()
			return
		}
		msg := sr.queue[0]
		sr.queue = sr.queue[1:]
		lastSK = msg.sessKey
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
	// Durably mark the dropped queued turns aborted, so a restart's replay does not
	// resurrect work the user just aborted (they were enq-without-run on disk).
	if sr != nil {
		for _, qm := range queued {
			if err := sr.store.MarkErr(qm.turnSeq, "aborted"); err != nil && !errors.Is(err, sessfiles.ErrRemoved) {
				log.Printf("durable MarkErr aborted (%s/%d): %v", sk, created, err)
			}
		}
	}
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

	prompt, tmpDir, err := d.buildTurnPrompt(sr, msg)
	if tmpDir != "" {
		defer os.RemoveAll(tmpDir)
	}
	if err != nil {
		log.Printf("buildTurnPrompt (%s/%d): %v", sk, sess.Created, err)
		if mErr := sr.store.MarkErr(msg.turnSeq, "attachments-missing"); mErr != nil && !errors.Is(mErr, sessfiles.ErrRemoved) {
			log.Printf("durable MarkErr (%s/%d): %v", sk, sess.Created, mErr)
		}
		del.Final(runner.RunResult{Error: errors.New("вложения недоступны, сообщение не обработано")})
		return
	}

	// MarkRun is a hard pre-run fence: if the durable append fails we must NOT run
	// the backend, or a crash would replay the (still enq) turn and duplicate work.
	if err := sr.store.MarkRun(msg.turnSeq); err != nil {
		log.Printf("durable MarkRun (%s/%d): %v", sk, sess.Created, err)
		del.Final(runner.RunResult{Error: errors.New("не удалось зафиксировать запуск, сообщение не обработано")})
		return
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

	// Record the turn's terminal state in the durable queue so a future replay skips
	// it. A failed append is logged, not fatal: ErrRemoved means a concurrent close
	// deleted the session (record is moot); any other error means replay re-classifies.
	var termErr error
	if result.Error != nil {
		termErr = sr.store.MarkErr(msg.turnSeq, result.Error.Error())
	} else {
		termErr = sr.store.MarkDone(msg.turnSeq)
	}
	if termErr != nil && !errors.Is(termErr, sessfiles.ErrRemoved) {
		log.Printf("durable terminal mark (%s/%d): %v", sk, sess.Created, termErr)
	}

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
}

// buildTurnPrompt materializes the message's durable files into a clean per-turn
// run-view (a /tmp dir holding the ORIGINAL names, never the internal store paths),
// folds those paths into the prompt, and appends the opaque turn marker so the
// backend transcript records it for correlation. Returns the prompt and the
// run-view dir (empty when there are no files); the caller owns removing tmpDir.
func (d *daemon) buildTurnPrompt(sr *sessionRunner, msg queuedMsg) (prompt, tmpDir string, err error) {
	prompt = msg.text
	if len(msg.files) > 0 {
		tmpDir, err = os.MkdirTemp("", "klax-attach-*")
		if err != nil {
			return prompt, "", fmt.Errorf("create run-view dir: %w", err)
		}
		paths, mErr := sr.store.Materialize(msg.files, tmpDir)
		if mErr != nil {
			return prompt, tmpDir, fmt.Errorf("materialize attachments: %w", mErr)
		}
		// Never run a turn missing files the user sent (contract §3): a short
		// materialization is a corrupt-message error, not a silent text-only run.
		if len(paths) != len(msg.files) {
			return prompt, tmpDir, fmt.Errorf("materialized %d of %d attachment(s)", len(paths), len(msg.files))
		}
		fileList := strings.Join(paths, "\n")
		if prompt == "" {
			prompt = fmt.Sprintf("Пользователь отправил файлы. Прочитай и проанализируй их:\n%s", fileList)
		} else {
			prompt = fmt.Sprintf("%s\n\nПрикреплённые файлы:\n%s", prompt, fileList)
		}
	}
	if msg.marker != "" {
		// Unobtrusive correlation marker the agent ignores; history.Load strips it.
		prompt = fmt.Sprintf("%s\n\n<!-- klax-turn:%s -->", prompt, msg.marker)
	}
	return prompt, tmpDir, nil
}
