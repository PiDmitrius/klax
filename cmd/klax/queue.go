package main

import (
	"context"
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

func sanitizeAttachmentFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "attachment"
	}
	return name
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
		t, rawChatID, _ := d.transportFor(chatID)
		if t != nil {
			qlen := len(sr.queue) + 1 // +1 for this message being added
			if mid, err := t.SendMessageReturnID(rawChatID, fmt.Sprintf("⏳ В очереди: %d", qlen), msgID, ""); err == nil {
				qm.progressID = mid
			}
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
				t, rawChatID, _ := d.transportFor(qm.chatID)
				if t != nil {
					t.EditMessage(rawChatID, qm.progressID, fmt.Sprintf("⏳ В очереди: %d", i+1), "")
				}
			}
		}
		sr.mu.Unlock()

		d.runBackend(msg)
	}
}

func (d *daemon) clearSessionQueue(sk string, created int64) int {
	sr := d.lookupRunner(sk, created)
	if sr == nil {
		return 0
	}
	sr.mu.Lock()
	n := len(sr.queue)
	sr.queue = nil
	sr.mu.Unlock()
	return n
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

	// Progress message — edit in place.
	// If this message was queued, reuse the "В очереди" notification.
	t, rawChatID, chatFmt := d.transportFor(msg.chatID)
	progressChain := newMessageChain(msg.progressID)
	if t != nil {
		var err error
		progressChain, err = d.syncMessageChain(ctx, msg.chatID, t, rawChatID, msg.msgID, progressChain, "...", "")
		if err != nil {
			progressChain = nil
		}
	}

	var toolLines []string
	lastProgress := ""
	onProgress := func(status string) {
		if status == lastProgress {
			return
		}
		lastProgress = status
		toolLines = append(toolLines, status)

		newText := formatToolLines(toolLines, chatFmt) + "\n\n..."
		if progressChain != nil && len(progressChain.ids) > 0 {
			var err error
			progressChain, err = d.syncMessageChain(ctx, msg.chatID, t, rawChatID, "", progressChain, newText, chatFmt)
			if err != nil {
				log.Printf("progress update failed: %v", err)
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
	result := sr.runner.Run(backend, runner.RunOptions{
		Prompt:             prompt,
		SessionID:          sess.ID,
		CWD:                sess.CWD,
		Sandbox:            sess.Sandbox,
		Model:              sess.ModelOverride,
		Effort:             sess.ThinkOverride,
		AppendSystemPrompt: sess.AppendSystemPrompt,
	}, onProgress)

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
		finalText := "..."
		if len(toolLines) > 0 {
			finalText = formatToolLines(toolLines, chatFmt) + "\n\n..."
		}
		finalText += fmt.Sprintf("\n❌ Ошибка: %v", result.Error)
		if progressChain != nil && len(progressChain.ids) > 0 && t != nil {
			_, err := d.syncMessageChain(ctx, msg.chatID, t, rawChatID, "", progressChain, finalText, chatFmt)
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

	// Build final message: tool log + separator + answer.
	var finalText string
	if len(toolLines) > 0 {
		finalText = formatToolLines(toolLines, chatFmt) + "\n\n" + formatted
	} else {
		finalText = formatted
	}

	if t != nil {
		_, err := d.syncMessageChain(ctx, msg.chatID, t, rawChatID, "", progressChain, finalText, chatFmt)
		if err != nil {
			log.Printf("final delivery failed: %v", err)
		}
	}
}
