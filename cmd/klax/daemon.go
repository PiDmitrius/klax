package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/max"
	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/tg"
	"github.com/PiDmitrius/klax/internal/transport"
	"github.com/PiDmitrius/klax/internal/vk"
)

// sessionRunner holds a per-session runner and message queue.
// Different sessions run Claude in parallel; within a session, messages are serialized.
type sessionRunner struct {
	runner     *runner.Runner
	mu         sync.Mutex
	queue      []queuedMsg
	processing bool
	cancel     context.CancelFunc // cancels current run (claude process + retry loops)
}

type daemon struct {
	cfg        *config.Config
	transports map[string]transport.Transport // "tg" -> tg.Bot, "mx" -> max.Bot
	formats    map[string]string              // "tg" -> "html", "vk" -> ""
	disabled   map[string]bool                // disabled transports
	pollCtx    map[string]context.CancelFunc  // cancel functions for poll goroutines
	store      *session.Store
	runners    map[string]*sessionRunner // sessionKey -> runner+queue
	runnersMu  sync.Mutex
	mu         sync.Mutex
	draining   bool             // stop accepting new tasks, wait for current to finish
	drainWg    sync.WaitGroup   // tracks active sessionRunners for drain
	identities map[int64]string // telegram userID -> canonical user ID
	maxIdents  map[int64]string // max userID -> canonical user ID
	vkIdents   map[int]string   // vk userID -> canonical user ID
	groupChats map[string]string // chatID -> CWD for group mode chats
}

// getRunner returns the sessionRunner for the given key, creating one if needed.
func (d *daemon) getRunner(sessionKey string) *sessionRunner {
	d.runnersMu.Lock()
	defer d.runnersMu.Unlock()
	sr, ok := d.runners[sessionKey]
	if !ok {
		sr = &sessionRunner{runner: runner.New()}
		d.runners[sessionKey] = sr
	}
	return sr
}

// transportPrefix extracts the transport prefix from a chatID (e.g. "tg" from "tg:123").
func transportPrefix(chatID string) string {
	if idx := strings.Index(chatID, ":"); idx != -1 {
		return chatID[:idx]
	}
	return "tg" // legacy chatIDs without prefix are telegram
}

// transportFor returns the transport, raw chatID (prefix stripped), and format for a prefixed chatID.
func (d *daemon) transportFor(chatID string) (transport.Transport, string, string) {
	if idx := strings.Index(chatID, ":"); idx != -1 {
		prefix := chatID[:idx]
		raw := chatID[idx+1:]
		if t, ok := d.transports[prefix]; ok {
			return t, raw, d.formats[prefix]
		}
	}
	// Fallback to tg
	if t, ok := d.transports["tg"]; ok {
		return t, chatID, d.formats["tg"]
	}
	return nil, chatID, ""
}

// sessionKey resolves a chatID to the key used for session storage.
// For DMs from known users, maps to canonical user ID so sessions are shared cross-platform.
func (d *daemon) sessionKey(chatID string) string {
	if idx := strings.Index(chatID, ":"); idx != -1 {
		prefix := chatID[:idx]
		raw := chatID[idx+1:]
		switch prefix {
		case "tg":
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
				// Positive IDs are DMs, negative are groups
				if id > 0 {
					if canonical, ok := d.identities[id]; ok {
						return "user:" + canonical
					}
				}
			}
		case "mx":
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
				if id > 0 {
					if canonical, ok := d.maxIdents[id]; ok {
						return "user:" + canonical
					}
				}
			}
		case "vk":
			if id, err := strconv.Atoi(raw); err == nil {
				// VK DMs: peer_id == user_id (< 2000000000), groups: peer_id >= 2000000000
				if id < 2000000000 {
					if canonical, ok := d.vkIdents[id]; ok {
						return "user:" + canonical
					}
				}
			}
		}
	}
	return chatID
}

// attachment is a file downloaded from a messenger, to be saved to a temp dir before running Claude.
type attachment struct {
	filename string // original filename (e.g. "photo.jpg")
	data     []byte
}

type queuedMsg struct {
	chatID      string
	msgID       string // user's message ID (for replyTo)
	text        string
	progressID  string // ID of "В очереди" message to reuse as progress
	attachments []attachment
}

func runDaemon() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("cannot load config: %v\nRun 'klax setup' first.", err)
	}
	if cfg.TelegramToken == "" && cfg.MaxToken == "" {
		log.Fatal("no tokens configured. Run 'klax setup'.")
	}

	transports := make(map[string]transport.Transport)

	// --- Telegram ---
	var tgBot *tg.Bot
	if cfg.TelegramToken != "" {
		tgBot = tg.New(cfg.TelegramToken)
		for {
			err := tgBot.GetMe()
			if err == nil {
				if err := tgBot.DrainUpdates(); err != nil {
					log.Printf("warning: tg drain updates: %v", err)
				}
				tgBot.SetMyCommands(tgMenuCommands)
				break
			}
			var apiErr *tg.APIError
			if errors.As(err, &apiErr) {
				log.Fatalf("telegram auth failed: %v", err)
			}
			log.Printf("telegram unreachable: %v (retry in 10s)", err)
			time.Sleep(10 * time.Second)
		}
		transports["tg"] = tgBot
		log.Println("[OK] Telegram connected")
	}

	// --- MAX ---
	var maxBot *max.Bot
	if cfg.MaxToken != "" {
		maxBot = max.New(cfg.MaxToken)
		me, err := maxBot.GetMe()
		if err != nil {
			log.Fatalf("MAX auth failed: %v", err)
		}
		if err := maxBot.DrainUpdates(); err != nil {
			log.Printf("warning: max drain updates: %v", err)
		}
		transports["mx"] = maxBot
		log.Printf("[OK] MAX bot: [%d] %s (@%s)", me.UserID, me.Name, me.Username)
	}

	// --- VK ---
	var vkBot *vk.Bot
	if cfg.VKToken != "" {
		vkBot = vk.New(cfg.VKToken)
		group, err := vkBot.GetMe()
		if err != nil {
			log.Fatalf("VK auth failed: %v", err)
		}
		if err := vkBot.DrainUpdates(); err != nil {
			log.Printf("warning: vk drain updates: %v", err)
		}
		transports["vk"] = vkBot
		log.Printf("[OK] VK group: [%d] %s", group.ID, group.Name)
	}

	store, err := session.LoadStore()
	if err != nil {
		log.Fatalf("cannot load sessions: %v", err)
	}

	// Build identity maps from config.
	tgIdents := make(map[int64]string)
	maxIdents := make(map[int64]string)
	vkIdents := make(map[int]string)
	for _, u := range cfg.Users {
		if u.TelegramID != 0 {
			tgIdents[u.TelegramID] = u.ID
		}
		if u.MaxID != 0 {
			maxIdents[u.MaxID] = u.ID
		}
		if u.VKID != 0 {
			vkIdents[int(u.VKID)] = u.ID
		}
	}

	// Migrate legacy flat sessions to first user's canonical ID or tg chatID.
	if len(cfg.AllowedUsers) > 0 {
		uid := cfg.AllowedUsers[0]
		migrateKey := fmt.Sprintf("tg:%d", uid)
		if canonical, ok := tgIdents[uid]; ok {
			migrateKey = "user:" + canonical
		}
		if store.MigrateTo(migrateKey) {
			store.Save()
			log.Printf("migrated legacy sessions to %s", migrateKey)
		}
	}

	// Merge platform-specific session keys into canonical user keys.
	for _, u := range cfg.Users {
		targetKey := "user:" + u.ID
		var oldKeys []string
		if u.TelegramID != 0 {
			oldKeys = append(oldKeys, fmt.Sprintf("tg:%d", u.TelegramID))
			oldKeys = append(oldKeys, fmt.Sprintf("%d", u.TelegramID))
		}
		if u.MaxID != 0 {
			oldKeys = append(oldKeys, fmt.Sprintf("mx:%d", u.MaxID))
			oldKeys = append(oldKeys, fmt.Sprintf("max:%d", u.MaxID)) // legacy prefix
		}
		if u.VKID != 0 {
			oldKeys = append(oldKeys, fmt.Sprintf("vk:%d", u.VKID))
		}
		if store.MergeKeys(targetKey, oldKeys) {
			store.Save()
			log.Printf("merged sessions into %s", targetKey)
		}
	}

	disabled := make(map[string]bool)
	for _, name := range cfg.DisabledTransports {
		disabled[name] = true
	}

	groupChats := make(map[string]string)
	for _, gc := range cfg.GroupChats {
		groupChats[gc.ID] = gc.CWD
	}

	d := &daemon{
		cfg:        cfg,
		transports: transports,
		formats:    map[string]string{"tg": "html", "mx": "html", "vk": ""},
		disabled:   disabled,
		pollCtx:    make(map[string]context.CancelFunc),
		store:      store,
		runners:    make(map[string]*sessionRunner),
		identities: tgIdents,
		maxIdents:  maxIdents,
		vkIdents:   vkIdents,
		groupChats: groupChats,
	}

	writePID()
	log.Printf("klax %s started (pid %d)", version, os.Getpid())

	// Startup notification: if restart marker exists, notify users we're back.
	if m := readMarker(); m != nil {
		var text string
		switch m.Reason {
		case "update":
			text = fmt.Sprintf("✅ klax обновлён (v%s)", version)
		default:
			text = fmt.Sprintf("✅ klax перезапущен (v%s)", version)
		}
		d.notifyAllUsers(text)
		removeMarker()
	}

	// Handle SIGTERM/SIGINT for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, draining...", sig)
		d.startDrain("signal")
	}()

	// Watch for restart marker (runs after startup marker is cleared).
	go d.watchMarker()

	// Start polling loops for enabled transports.
	if tgBot != nil && !disabled["tg"] {
		d.startPoll("tg")
	}
	if maxBot != nil && !disabled["mx"] {
		d.startPoll("mx")
	}
	if vkBot != nil && !disabled["vk"] {
		d.startPoll("vk")
	}

	// Block forever (goroutines do the work).
	select {}
}

// startPoll starts the polling goroutine for the given transport.
func (d *daemon) startPoll(name string) {
	d.mu.Lock()
	// Stop existing poll if running.
	if cancel, ok := d.pollCtx[name]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.pollCtx[name] = cancel
	d.mu.Unlock()

	switch name {
	case "tg":
		go d.pollTG(ctx)
	case "mx":
		go d.pollMAX(ctx)
	case "vk":
		go d.pollVK(ctx)
	}
	log.Printf("poll started: %s", name)
}

// stopPoll stops the polling goroutine for the given transport.
func (d *daemon) stopPoll(name string) {
	d.mu.Lock()
	if cancel, ok := d.pollCtx[name]; ok {
		cancel()
		delete(d.pollCtx, name)
	}
	d.mu.Unlock()
	log.Printf("poll stopped: %s", name)
}

// notifyAllUsers sends a message to all allowed users on enabled platforms.
// These are self-initiated messages (no replyTo).
func (d *daemon) notifyAllUsers(text string) {
	if _, ok := d.transports["tg"]; ok && !d.disabled["tg"] {
		for _, uid := range d.cfg.AllowedUsers {
			chatID := fmt.Sprintf("tg:%d", uid)
			log.Printf("notify %s", chatID)
			d.sendMessage(chatID, "", text)
		}
	}
	if _, ok := d.transports["mx"]; ok && !d.disabled["mx"] {
		for _, uid := range d.cfg.MaxAllowedUsers {
			chatID := fmt.Sprintf("mx:%d", uid)
			log.Printf("notify %s", chatID)
			d.sendMessage(chatID, "", text)
		}
	}
	if _, ok := d.transports["vk"]; ok && !d.disabled["vk"] {
		for _, uid := range d.cfg.VKAllowedUsers {
			chatID := fmt.Sprintf("vk:%d", uid)
			log.Printf("notify %s", chatID)
			d.sendMessage(chatID, "", text)
		}
	}
}

// startDrain puts the daemon into draining mode.
// Waits for current task AND queued tasks to finish before shutting down.
func (d *daemon) startDrain(reason string) {
	d.mu.Lock()
	if d.draining {
		d.mu.Unlock()
		return
	}
	d.draining = true
	d.mu.Unlock()

	log.Printf("drain started (reason: %s)", reason)
	if readMarker() == nil {
		writeMarker(reason)
	}
	d.notifyAllUsers("🔄 klax перезапускается...")

	// Kick processing on all session runners that have queued messages.
	d.runnersMu.Lock()
	for _, sr := range d.runners {
		sr.mu.Lock()
		hasItems := len(sr.queue) > 0 && !sr.processing
		sr.mu.Unlock()
		if hasItems {
			go d.processSessionQueue(sr)
		}
	}
	d.runnersMu.Unlock()

	// Wait for all active session runners to finish.
	log.Println("waiting for all sessions to drain...")
	d.drainWg.Wait()
	d.shutdown()
}

func (d *daemon) shutdown() {
	log.Println("shutting down")
	removePID()
	os.Exit(0)
}

func (d *daemon) isDraining() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.draining
}

func (d *daemon) watchMarker() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if d.isDraining() {
			return
		}
		if m := readMarker(); m != nil {
			log.Printf("restart marker found (reason: %s)", m.Reason)
			d.startDrain(m.Reason)
			return
		}
	}
}

func (d *daemon) pollTG(ctx context.Context) {
	bot := d.transports["tg"].(*tg.Bot)
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := bot.GetUpdates()
		if err != nil {
			log.Printf("tg: getUpdates error: %v (retry in 5s)", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, u := range updates {
			msg := u.Message
			if msg == nil {
				continue
			}
			// /id — respond to anyone, even unauthenticated
			if strings.TrimSpace(msg.Text) == "/id" || strings.HasPrefix(msg.Text, "/id@") {
				reply := fmt.Sprintf("user_id: %d\nchat_id: %d", msg.From.ID, msg.Chat.ID)
				bot.SendMessage(fmt.Sprintf("%d", msg.Chat.ID), reply, "", "")
				continue
			}
			chatID := fmt.Sprintf("tg:%d", msg.Chat.ID)
			msgID := fmt.Sprintf("%d", msg.MessageID)

			// Extract text: prefer Text, fall back to Caption for media messages.
			text := msg.Text
			if text == "" {
				text = msg.Caption
			}

			// Download attachments (photo or document).
			var attachments []attachment
			if photo := msg.BestPhoto(); photo != nil {
				if data, name, err := bot.DownloadFile(photo.FileID); err == nil {
					attachments = append(attachments, attachment{filename: name, data: data})
				} else {
					log.Printf("tg: download photo: %v", err)
				}
			}
			if msg.Document != nil {
				if data, name, err := bot.DownloadFile(msg.Document.FileID); err == nil {
					if name == "" {
						name = msg.Document.FileName
					}
					attachments = append(attachments, attachment{filename: name, data: data})
				} else {
					log.Printf("tg: download document: %v", err)
				}
			}

			if d.isTGAllowed(msg.From.ID) {
				d.handleMessageWithAttachments(chatID, msgID, text, attachments)
			} else if d.isGroupChat(chatID) {
				if strings.HasPrefix(text, "/") && isGroupCommand(text) {
					d.ensureSessionWithCWD(d.sessionKey(chatID), d.groupCWD(chatID))
					d.handleCommand(chatID, msgID, text)
				} else if prompt, ok := stripGroupTrigger(strings.TrimSpace(text)); ok {
					d.ensureSessionWithCWD(d.sessionKey(chatID), d.groupCWD(chatID))
					d.enqueueWithAttachments(chatID, msgID, prompt, attachments)
				}
			}
		}
	}
}

func (d *daemon) pollMAX(ctx context.Context) {
	bot := d.transports["mx"].(*max.Bot)
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := bot.GetUpdates()
		if err != nil {
			log.Printf("mx: getUpdates error: %v (retry in 5s)", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, upd := range updates {
			if upd.UpdateType != "message_created" {
				continue
			}
			senderID := upd.Message.Sender.UserID
			text := upd.Message.Body.Text
			// /id — respond to anyone
			if strings.TrimSpace(text) == "/id" {
				reply := fmt.Sprintf("user_id: %d", senderID)
				if upd.Message.Recipient.ChatID != 0 {
					reply += fmt.Sprintf("\nchat_id: %d", upd.Message.Recipient.ChatID)
				}
				if upd.Message.Recipient.ChatType == "dialog" {
					bot.SendMessage(fmt.Sprintf("%d", senderID), reply, "", "")
				} else {
					bot.SendMessage(fmt.Sprintf("%d", upd.Message.Recipient.ChatID), reply, "", "")
				}
				continue
			}
			var chatID string
			if upd.Message.Recipient.ChatType == "dialog" {
				chatID = fmt.Sprintf("mx:%d", senderID)
			} else {
				chatID = fmt.Sprintf("mx:%d", upd.Message.Recipient.ChatID)
			}
			msgID := upd.Message.Body.Mid

			// Download attachments from MAX message.
			var attachments []attachment
			for _, att := range upd.Message.Body.ParseAttachments() {
				data, err := bot.DownloadURL(att.URL)
				if err != nil {
					log.Printf("mx: download %s: %v", att.Type, err)
					continue
				}
				attachments = append(attachments, attachment{filename: att.Filename, data: data})
			}

			if text == "" && len(attachments) == 0 {
				continue
			}

			if d.isMAXAllowed(senderID) {
				d.handleMessageWithAttachments(chatID, msgID, text, attachments)
			} else if d.isGroupChat(chatID) {
				if strings.HasPrefix(text, "/") && isGroupCommand(text) {
					d.ensureSessionWithCWD(d.sessionKey(chatID), d.groupCWD(chatID))
					d.handleCommand(chatID, msgID, text)
				} else if prompt, ok := stripGroupTrigger(strings.TrimSpace(text)); ok {
					d.ensureSessionWithCWD(d.sessionKey(chatID), d.groupCWD(chatID))
					d.enqueueWithAttachments(chatID, msgID, prompt, attachments)
				}
			}
		}
	}
}

func (d *daemon) isTGAllowed(id int64) bool {
	for _, uid := range d.cfg.AllowedUsers {
		if uid == id {
			return true
		}
	}
	return false
}

func (d *daemon) pollVK(ctx context.Context) {
	bot := d.transports["vk"].(*vk.Bot)
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := bot.GetUpdates()
		if err != nil {
			log.Printf("vk: getUpdates error: %v (retry in 5s)", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, upd := range updates {
			if upd.Type != "message_new" {
				continue
			}
			mn, err := vk.ParseMessageNew(upd)
			if err != nil {
				log.Printf("vk: parse message_new: %v", err)
				continue
			}
			msg := mn.Message
			// /id — respond to anyone
			if strings.TrimSpace(msg.Text) == "/id" {
				reply := fmt.Sprintf("from_id: %d\npeer_id: %d", msg.FromID, msg.PeerID)
				bot.SendMessage(strconv.Itoa(msg.PeerID), reply, "", "")
				continue
			}
			if msg.Text == "" {
				continue
			}
			chatID := fmt.Sprintf("vk:%d", msg.PeerID)
			msgID := strconv.Itoa(msg.ID)
			if d.isVKAllowed(msg.FromID) {
				d.handleMessage(chatID, msgID, msg.Text)
			} else if d.isGroupChat(chatID) {
				if prompt, ok := stripGroupTrigger(strings.TrimSpace(msg.Text)); ok {
					d.ensureSessionWithCWD(d.sessionKey(chatID), d.groupCWD(chatID))
					d.enqueue(chatID, msgID, prompt)
				}
			}
		}
	}
}

func (d *daemon) isVKAllowed(id int) bool {
	for _, uid := range d.cfg.VKAllowedUsers {
		if uid == id {
			return true
		}
	}
	return false
}

func (d *daemon) isMAXAllowed(id int64) bool {
	for _, uid := range d.cfg.MaxAllowedUsers {
		if uid == id {
			return true
		}
	}
	return false
}

// isGroupChat returns true if the chat has group mode enabled.
func (d *daemon) isGroupChat(chatID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.groupChats[chatID]
	return ok
}

// isGroupChatID returns true if the chatID refers to a group (not a DM).
// TG: negative chat ID. MAX: negative chat ID. VK: peer_id >= 2000000000.
func isGroupChatID(chatID string) bool {
	idx := strings.Index(chatID, ":")
	if idx == -1 {
		return false
	}
	prefix := chatID[:idx]
	raw := chatID[idx+1:]
	if prefix == "vk" {
		if id, err := strconv.Atoi(raw); err == nil {
			return id >= 2000000000
		}
		return false
	}
	return len(raw) > 0 && raw[0] == '-'
}

// groupCWD returns the CWD for a group chat, or "" if not a group.
func (d *daemon) groupCWD(chatID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.groupChats[chatID]
}

// enableGroupChat enables group mode for a chat with the given CWD.
func (d *daemon) enableGroupChat(chatID, cwd string) {
	d.mu.Lock()
	d.groupChats[chatID] = cwd
	d.mu.Unlock()
	d.saveGroupChats()
}

// disableGroupChat disables group mode for a chat.
func (d *daemon) disableGroupChat(chatID string) {
	d.mu.Lock()
	delete(d.groupChats, chatID)
	d.mu.Unlock()
	d.saveGroupChats()
}

func (d *daemon) saveGroupChats() {
	d.mu.Lock()
	var list []config.GroupChat
	for id, cwd := range d.groupChats {
		list = append(list, config.GroupChat{ID: id, CWD: cwd})
	}
	d.mu.Unlock()
	d.cfg.GroupChats = list
	config.Save(d.cfg)
}

// groupTriggerPrefixes are the recognized prefixes for group mode messages.
// Checked case-insensitively. Must be followed by comma or any whitespace.
var groupTriggerPrefixes = []string{
	"klax", "клакс", "клэкс", "клац",
	"kl", "кл",
}

// stripGroupTrigger checks if text starts with a group trigger prefix.
// Returns the remaining text (trimmed) and true, or "" and false.
func stripGroupTrigger(text string) (string, bool) {
	lower := strings.ToLower(text)
	for _, prefix := range groupTriggerPrefixes {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		rest := text[len(prefix):]
		// Trigger alone (e.g. caption "кл" with attachment) — valid, empty prompt.
		if len(rest) == 0 {
			return "", true
		}
		// Must be followed by punctuation or any whitespace.
		r := rune(rest[0])
		if strings.ContainsRune(",.!?:;—", r) {
			rest = strings.TrimLeft(rest, ",.!?:;—")
		} else if !unicode.IsSpace(r) {
			continue
		}
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		return rest, true
	}
	return "", false
}

// isGroupCommand checks if text starts with a command allowed for non-admin group members.
func isGroupCommand(text string) bool {
	cmd := strings.Fields(text)[0]
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at]
	}
	switch cmd {
	case "/status", "/sessions", "/s", "/new", "/model", "/abort", "/help", "/start":
		return true
	}
	// /s<n> shortcuts
	if strings.HasPrefix(cmd, "/s") {
		for _, c := range cmd[2:] {
			if c < '0' || c > '9' {
				return false
			}
		}
		return len(cmd) > 2
	}
	return false
}

func (d *daemon) handleMessage(chatID, msgID, text string) {
	d.handleMessageWithAttachments(chatID, msgID, text, nil)
}

func (d *daemon) handleMessageWithAttachments(chatID, msgID, text string, attachments []attachment) {
	text = strings.TrimSpace(text)
	if text == "" && len(attachments) == 0 {
		return
	}

	// Ensure chat has at least one session.
	sk := d.sessionKey(chatID)
	d.ensureSessionWithCWD(sk, d.groupCWD(chatID))

	// Handle built-in commands (allowed users only — enforced by poll loops)
	if strings.HasPrefix(text, "/") {
		d.handleCommand(chatID, msgID, text)
		return
	}

	// In group mode, require trigger prefix for all users
	if d.isGroupChat(chatID) {
		if prompt, ok := stripGroupTrigger(text); ok {
			d.enqueueWithAttachments(chatID, msgID, prompt, attachments)
		}
		// No prefix — ignore silently
		return
	}

	// Queue for Claude
	d.enqueueWithAttachments(chatID, msgID, text, attachments)
}

func (d *daemon) ensureSession(sessionKey string) {
	d.ensureSessionWithCWD(sessionKey, "")
}

func (d *daemon) ensureSessionWithCWD(sessionKey, forceCWD string) {
	if sess := d.store.Active(sessionKey); sess != nil {
		// If group CWD is set and session CWD doesn't match, update it.
		if forceCWD != "" && sess.CWD != forceCWD {
			sess.CWD = forceCWD
			d.store.Save()
		}
		return
	}
	cwd := forceCWD
	if cwd == "" {
		cwd = d.cfg.DefaultCWD
	}
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	d.store.New(sessionKey, "default", cwd)
	d.store.Save()
}
