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
)

func (d *daemon) enqueue(chatID, msgID, text string) {
	d.enqueueWithAttachments(chatID, msgID, text, nil)
}

func (d *daemon) enqueueWithAttachments(chatID, msgID, text string, attachments []attachment) {
	if d.isDraining() {
		d.sendMessage(chatID, msgID, "🔄 klax перезапускается, новые задачи не принимаются.")
		return
	}

	sk := d.sessionKey(chatID)
	sr := d.getRunner(sk)

	sr.mu.Lock()
	qm := queuedMsg{chatID: chatID, msgID: msgID, text: text, attachments: attachments}
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

		d.runClaude(msg)
	}
}

func (d *daemon) clearSessionQueue(sk string) int {
	sr := d.getRunner(sk)
	sr.mu.Lock()
	n := len(sr.queue)
	sr.queue = nil
	sr.mu.Unlock()
	return n
}

func (d *daemon) runClaude(msg queuedMsg) {
	sk := d.sessionKey(msg.chatID)
	sess := d.store.Active(sk)
	if sess == nil {
		d.sendMessage(msg.chatID, msg.msgID, "❌ Нет активной сессии. Напиши /new")
		return
	}

	sr := d.getRunner(sk)

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
	progressMsgID := msg.progressID
	if t != nil {
		if progressMsgID != "" {
			// Edit existing queue notification into progress indicator.
			t.EditMessage(rawChatID, progressMsgID, "...", "")
		} else {
			// Send new progress message as reply to user's message.
			retryDo(ctx, func() error {
				mid, err := t.SendMessageReturnID(rawChatID, "...", msg.msgID, "")
				if err == nil {
					progressMsgID = mid
				}
				return err
			})
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
		if progressMsgID != "" {
			t.EditMessage(rawChatID, progressMsgID, newText, chatFmt)
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
				fp := filepath.Join(tmpDir, att.filename)
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

	permMode := sess.PermissionMode
	if permMode == "" {
		permMode = d.cfg.PermissionMode
	}
	result := sr.runner.Run(runner.RunOptions{
		Prompt:             prompt,
		SessionID:          sess.ID,
		CWD:                sess.CWD,
		PermissionMode:     permMode,
		Model:              sess.ModelOverride,
		AppendSystemPrompt: sess.AppendSystemPrompt,
	}, onProgress)

	// Update session metadata.
	sess.Messages++
	sess.LastUsed = time.Now().Unix()
	if result.SessionID != "" {
		sess.ID = result.SessionID
	}
	// Only update model/usage from successful runs.
	// On kill/error, system event may report a wrong default model.
	if result.Error == nil && result.Usage.Model != "" {
		sess.Model = result.Usage.Model
		sess.ContextWindow = result.Usage.ContextWindow
		sess.ContextUsed = result.Usage.ContextUsed
	}
	d.store.Save()

	if result.Error != nil {
		finalText := fmt.Sprintf("❌ Ошибка: %v", result.Error)
		if progressMsgID != "" && t != nil {
			tryEdit(ctx, t, rawChatID, progressMsgID, finalText, "")
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
		d.deliverFinal(ctx, t, rawChatID, progressMsgID, finalText, chatFmt)
	}
}
