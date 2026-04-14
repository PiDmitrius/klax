package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/tg"
)

var tgMenuCommands = []tg.BotCommand{
	{Command: "status", Description: "Статус"},
	{Command: "sessions", Description: "Сессии"},
	{Command: "new", Description: "Новая сессия"},
	{Command: "settings", Description: "Настройки"},
	{Command: "abort", Description: "Прервать"},
}

var transportOrder = []string{"tg", "mx", "vk"}

func normalizeCommand(cmd string, args []string) (string, []string) {
	switch {
	case strings.HasPrefix(cmd, "/backend_") && len(cmd) > len("/backend_"):
		return "/backend", append([]string{cmd[len("/backend_"):]}, args...)
	case strings.HasPrefix(cmd, "/groups_") && len(cmd) > len("/groups_"):
		return "/groups", append([]string{cmd[len("/groups_"):]}, args...)
	case strings.HasPrefix(cmd, "/group_") && len(cmd) > len("/group_"):
		return "/groups", append([]string{cmd[len("/group_"):]}, args...)
	case strings.HasPrefix(cmd, "/m_") && len(cmd) > len("/m_"):
		return "/__set_model", []string{cmd[len("/m_"):]}
	case strings.HasPrefix(cmd, "/t_") && len(cmd) > len("/t_"):
		return "/__set_think", []string{cmd[len("/t_"):]}
	case strings.HasPrefix(cmd, "/sandbox_") && len(cmd) > len("/sandbox_"):
		return "/__set_sandbox", []string{cmd[len("/sandbox_"):]}
	case strings.HasPrefix(cmd, "/v_") && len(cmd) > len("/v_"):
		return "/__install_version", []string{cmd[len("/v_"):]}
	case hasNumericSuffixCommand(cmd, "/s"):
		return "/__switch_session", []string{cmd[len("/s"):]}
	case hasNumericSuffixCommand(cmd, "/d"):
		return "/__delete_session", []string{cmd[len("/d"):]}
	default:
		return cmd, args
	}
}

func hasNumericSuffixCommand(cmd, prefix string) bool {
	if !strings.HasPrefix(cmd, prefix) || len(cmd) <= len(prefix) {
		return false
	}
	for _, c := range cmd[len(prefix):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func sessionModeLabel(chatID string) string {
	if isGroupChatID(chatID) {
		return "групповая"
	}
	return "личная"
}

func effectiveBackendName(cfg *config.Config, def *session.ScopeDefaults, sess *session.Session) string {
	return resolveSessionBackend(sess, def, cfg.GetDefaultBackend())
}

func sessionCreatedText(cfg *config.Config, chatID string, def *session.ScopeDefaults, sess *session.Session) string {
	backend := effectiveBackendName(cfg, def, sess)
	model := sess.ModelOverride
	if model == "" {
		model = "по умолчанию"
	}
	think := sess.ThinkOverride
	if think == "" {
		think = "по умолчанию"
	}
	sandbox := effectiveSandboxMode(def, sess)
	return fmt.Sprintf(
		"✅ Новая сессия: <code>%s</code>\n🧩 Тип: <code>%s</code>\n⚙️ Движок: <code>%s</code>\n🤖 Модель: <code>%s</code>\n🧠 Мышление: <code>%s</code>\n🔒 Sandbox: <code>%s</code>\n\nНастроить: /settings",
		html.EscapeString(sess.Name),
		sessionModeLabel(chatID),
		backend,
		model,
		think,
		sandbox,
	)
}

func sessionSwitchedText(cfg *config.Config, chatID string, def *session.ScopeDefaults, sess *session.Session, sessionsText string) string {
	backend := effectiveBackendName(cfg, def, sess)
	model := sess.ModelOverride
	if model == "" {
		model = "по умолчанию"
	}
	think := sess.ThinkOverride
	if think == "" {
		think = "по умолчанию"
	}
	sandbox := effectiveSandboxMode(def, sess)
	return fmt.Sprintf(
		"📌 Сессия: <code>%s</code>\n🧩 Тип: <code>%s</code>\n⚙️ Движок: <code>%s</code>\n🤖 Модель: <code>%s</code>\n🧠 Мышление: <code>%s</code>\n🔒 Sandbox: <code>%s</code>\n\n%s",
		html.EscapeString(sess.Name),
		sessionModeLabel(chatID),
		backend,
		model,
		think,
		sandbox,
		sessionsText,
	)
}

func (d *daemon) handleBackendSet(chatID, msgID, sk, name string) {
	sess := d.store.Active(sk)
	if sess == nil {
		d.sendMessage(chatID, msgID, "Нет активной сессии")
		return
	}
	if sess.Messages > 0 {
		d.sendMessage(chatID, msgID, "Backend нельзя изменить после первого сообщения.")
		return
	}
	if name == "" {
		d.sendMessage(chatID, msgID, d.backendText(sk, sess))
		return
	}
	if name != "claude" && name != "codex" {
		d.sendMessage(chatID, msgID, "Доступные backend: claude, codex")
		return
	}
	current := effectiveBackendName(d.cfg, d.scopeDefaults(sk), sess)
	d.store.UpdateScopeDefaults(sk, func(def *session.ScopeDefaults) {
		def.Backend = name
		if current != name {
			def.Model = ""
			def.Think = ""
		}
	})
	sess = d.store.UpdateActive(sk, func(sess *session.Session) {
		sess.Backend = name
		if current != name {
			sess.ModelOverride = ""
			sess.ThinkOverride = ""
		}
	})
	d.saveStore()
	d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))
}

func (d *daemon) handleModelSet(chatID, msgID, sk, alias string) {
	sess := d.store.Active(sk)
	if sess == nil {
		d.sendMessage(chatID, msgID, "Нет активной сессии")
		return
	}
	if alias == "default" {
		d.store.UpdateScopeDefaults(sk, func(def *session.ScopeDefaults) {
			def.Model = ""
		})
		sess = d.store.UpdateActive(sk, func(sess *session.Session) {
			sess.ModelOverride = ""
		})
		d.saveStore()
		d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))
		return
	}
	def := d.scopeDefaults(sk)
	backend := effectiveBackendName(d.cfg, def, sess)
	resolved := alias
	for _, m := range modelsForBackend(backend) {
		if m.alias == alias {
			resolved = m.model
			break
		}
	}
	d.store.UpdateScopeDefaults(sk, func(def *session.ScopeDefaults) {
		def.Model = resolved
	})
	sess = d.store.UpdateActive(sk, func(sess *session.Session) {
		sess.ModelOverride = resolved
	})
	d.saveStore()
	d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))
}

func (d *daemon) handleThinkSet(chatID, msgID, sk, alias string) {
	sess := d.store.Active(sk)
	if sess == nil {
		d.sendMessage(chatID, msgID, "Нет активной сессии")
		return
	}
	if alias == "default" {
		d.store.UpdateScopeDefaults(sk, func(def *session.ScopeDefaults) {
			def.Think = ""
		})
		sess = d.store.UpdateActive(sk, func(sess *session.Session) {
			sess.ThinkOverride = ""
		})
		d.saveStore()
		d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))
		return
	}
	def := d.scopeDefaults(sk)
	backend := effectiveBackendName(d.cfg, def, sess)
	resolved := alias
	for _, e := range effortsForBackend(backend) {
		if e.alias == alias {
			resolved = e.model
			break
		}
	}
	d.store.UpdateScopeDefaults(sk, func(def *session.ScopeDefaults) {
		def.Think = resolved
	})
	sess = d.store.UpdateActive(sk, func(sess *session.Session) {
		sess.ThinkOverride = resolved
	})
	d.saveStore()
	d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))
}

func (d *daemon) handleSandboxSet(chatID, msgID, sk, mode string) {
	sess := d.store.Active(sk)
	if sess == nil {
		d.sendMessage(chatID, msgID, "Нет активной сессии")
		return
	}
	if mode != "on" && mode != "off" {
		d.sendMessage(chatID, msgID, d.sandboxText(sk, sess))
		return
	}
	d.store.UpdateScopeDefaults(sk, func(def *session.ScopeDefaults) {
		def.Sandbox = mode
	})
	sess = d.store.UpdateActive(sk, func(sess *session.Session) {
		sess.Sandbox = mode
	})
	d.saveStore()
	d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))
}

func (d *daemon) handleSessionSwitch(chatID, msgID, sk, n string) {
	idx, err := strconv.Atoi(n)
	if err != nil {
		d.sendMessage(chatID, msgID, fmt.Sprintf("Нет сессии #%s", n))
		return
	}
	sess := d.store.Switch(sk, idx-1)
	if sess == nil {
		d.sendMessage(chatID, msgID, fmt.Sprintf("Нет сессии #%d", idx))
		return
	}
	d.saveStore()
	d.sendMessage(chatID, msgID, sessionSwitchedText(d.cfg, chatID, d.scopeDefaults(sk), sess, d.sessionsText(sk)))
}

func (d *daemon) handleSessionDelete(chatID, msgID, sk, n string) {
	idx, err := strconv.Atoi(n)
	if err != nil {
		d.sendMessage(chatID, msgID, fmt.Sprintf("Нет сессии #%s", n))
		return
	}
	pos := idx - 1
	sessions := d.store.SessionsFor(sk)
	if pos < 0 || pos >= len(sessions) {
		d.sendMessage(chatID, msgID, fmt.Sprintf("Нет сессии #%d", idx))
		return
	}
	if sessions[pos].Active {
		d.sendMessage(chatID, msgID, "Нельзя удалить активную сессию.")
		return
	}
	d.store.Delete(sk, pos)
	d.saveStore()
	d.sendMessage(chatID, msgID, d.cleanupText(sk))
}

// argPayload returns everything after the first whitespace-delimited word in text.
// Handles @botname suffixes correctly: "/prompt@bot hello" → "hello".
func argPayload(text string) string {
	i := strings.IndexFunc(text, unicode.IsSpace)
	if i == -1 {
		return ""
	}
	return strings.TrimLeftFunc(text[i:], unicode.IsSpace)
}

func (d *daemon) handleCommand(chatID, msgID, text string) {
	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])
	// Strip @botname suffix (e.g. /sessions@klax_bot → /sessions)
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at]
	}
	args := parts[1:]
	cmd, args = normalizeCommand(cmd, args)
	parts = append([]string{cmd}, args...)
	sk := d.sessionKey(chatID)

	switch cmd {
	case "/start", "/help", "/h":
		d.sendMessage(chatID, msgID, helpText())

	case "/status", "/?":
		d.sendMessage(chatID, msgID, d.statusText(sk))

	case "/sessions", "/session", "/s":
		d.sendMessage(chatID, msgID, d.sessionsText(sk))

	case "/cleanup":
		d.sendMessage(chatID, msgID, d.cleanupText(sk))

	case "/menu":
		prefix := transportPrefix(chatID)
		if prefix != "tg" {
			d.sendMessage(chatID, msgID, "Команда /menu доступна только в Telegram.")
			return
		}
		bot, ok := d.transports["tg"].(*tg.Bot)
		if !ok {
			d.sendMessage(chatID, msgID, "Telegram транспорт недоступен.")
			return
		}
		_, rawID, _ := d.transportFor(chatID)
		if err := bot.SetMyCommandsForChat(rawID, tgMenuCommands); err != nil {
			d.sendMessage(chatID, msgID, fmt.Sprintf("Ошибка: %v", err))
			return
		}
		d.sendMessage(chatID, msgID, "✅ Меню установлено.")

	case "/new":
		name := "session"
		if len(args) > 0 {
			name = strings.Join(args, " ")
		}
		cwd := d.sessionCWD(chatID)
		if cwd == "" {
			cwd = d.cfg.DefaultCWD
		}
		if cwd == "" {
			cwd, _ = os.UserHomeDir()
		}
		def := d.scopeDefaults(sk)
		sess := d.store.New(sk, name, cwd, *def)
		d.saveStore()
		d.sendMessage(chatID, msgID, sessionCreatedText(d.cfg, chatID, def, sess))

	case "/name":
		if len(args) == 0 {
			d.sendMessage(chatID, msgID, "Использование: /name <имя>")
			return
		}
		sess := d.store.UpdateActive(sk, func(sess *session.Session) {
			sess.Name = strings.Join(args, " ")
		})
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.saveStore()
		d.sendMessage(chatID, msgID, d.sessionsText(sk))

	case "/cwd":
		if len(args) == 0 {
			sess := d.store.Active(sk)
			if sess != nil {
				d.sendMessage(chatID, msgID, fmt.Sprintf("📂 <code>%s</code>", html.EscapeString(tildePath(sess.CWD))))
			}
			return
		}
		sess := d.store.UpdateActive(sk, func(sess *session.Session) {
			sess.CWD = strings.Join(args, " ")
		})
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.saveStore()
		d.sendMessage(chatID, msgID, fmt.Sprintf("📂 <code>%s</code>", html.EscapeString(tildePath(sess.CWD))))

	case "/prompt":
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		if len(parts) < 2 {
			if sess.AppendSystemPrompt == "" {
				d.sendMessage(chatID, msgID, "Системный промпт не задан.")
			} else {
				d.sendMessage(chatID, msgID, fmt.Sprintf("📝 <code>%s</code>", html.EscapeString(sess.AppendSystemPrompt)))
			}
			return
		}
		sess = d.store.UpdateActive(sk, func(sess *session.Session) {
			sess.AppendSystemPrompt = argPayload(text)
		})
		d.saveStore()
		d.sendMessage(chatID, msgID, fmt.Sprintf("📝 <code>%s</code>", html.EscapeString(sess.AppendSystemPrompt)))

	case "/model", "/models", "/m":
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.sendMessage(chatID, msgID, d.modelText(sk, sess))

	case "/think", "/thinking", "/t":
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.sendMessage(chatID, msgID, d.thinkText(sk, sess))

	case "/sandbox":
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.sendMessage(chatID, msgID, d.sandboxText(sk, sess))

	case "/settings", "/setting":
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))

	case "/groups", "/group", "/g":
		d.handleGroups(chatID, msgID, parts)

	case "/transports":
		d.handleTransports(chatID, msgID, parts)

	case "/backend":
		name := ""
		if len(args) > 0 {
			name = strings.ToLower(args[0])
		}
		d.handleBackendSet(chatID, msgID, sk, name)

	case "/update":
		go func() {
			text, err := updateText()
			if err != nil {
				d.sendMessage(chatID, msgID, fmt.Sprintf("❌ %v", err))
				return
			}
			d.sendMessage(chatID, msgID, text)
		}()

	case "/bypass":
		if len(parts) < 2 {
			d.sendMessage(chatID, msgID, "Использование: /bypass <команда>")
			return
		}
		prompt := argPayload(text)
		d.enqueue(chatID, msgID, prompt)

	case "/abort":
		sr := d.getRunner(sk)
		sr.mu.Lock()
		cancelFn := sr.cancel
		busy := sr.runner.IsBusy()
		sr.mu.Unlock()
		dropped := d.clearSessionQueue(sk)
		if !busy && cancelFn == nil && dropped == 0 {
			d.sendMessage(chatID, msgID, "Нет активных задач.")
			return
		}
		// Cancel context first (stops retry loops), then kill claude process.
		if cancelFn != nil {
			cancelFn()
		}
		sr.runner.Abort()
		reply := "❌ Прерван."
		if dropped > 0 {
			reply += fmt.Sprintf(" Очередь очищена (%d сообщений удалено).", dropped)
		}
		d.sendMessage(chatID, msgID, reply)

	case "/__set_model":
		d.handleModelSet(chatID, msgID, sk, args[0])

	case "/__set_think":
		d.handleThinkSet(chatID, msgID, sk, args[0])

	case "/__set_sandbox":
		d.handleSandboxSet(chatID, msgID, sk, args[0])

	case "/__switch_session":
		d.handleSessionSwitch(chatID, msgID, sk, args[0])

	case "/__delete_session":
		d.handleSessionDelete(chatID, msgID, sk, args[0])

	case "/__install_version":
		tag := tagFromAlias(args[0])
		go d.runChatOp(chatID, msgID, "fallback "+tag, fmt.Sprintf("⏳ Устанавливаю %s...", tag))

	default:
		d.sendMessage(chatID, msgID, fmt.Sprintf("Неизвестная команда: %s\nНапиши /help", cmd))
	}
}

func (d *daemon) handleGroups(chatID, msgID string, parts []string) {
	// /groups — list all enabled group chats
	if len(parts) < 2 {
		d.sendMessage(chatID, msgID, d.groupsText())
		return
	}
	switch strings.ToLower(parts[1]) {
	case "on":
		if !isGroupChatID(chatID) {
			d.sendMessage(chatID, msgID, "❌ Команда /groups on работает только в групповых чатах.")
			return
		}
		cwd := d.sessionCWD(chatID)
		if cwd == "" {
			d.sendMessage(chatID, msgID, "❌ Не удалось определить директорию группы.")
			return
		}
		d.enableGroupChat(chatID, cwd)
		// Create a fresh session for group mode with the correct CWD.
		// Any pre-existing sessions (from before group mode) are left inactive.
		sk := d.sessionKey(chatID)
		sessions := d.store.SessionsFor(sk)
		// Check if there's already a session with group CWD.
		hasGroupSession := false
		for _, s := range sessions {
			if s.CWD == cwd {
				hasGroupSession = true
				break
			}
		}
		if !hasGroupSession {
			d.store.New(sk, "group", cwd, *d.scopeDefaults(sk))
		}
		d.saveStore()
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))
	case "off":
		target := chatID
		if len(parts) >= 3 {
			target = parts[2]
		}
		d.disableGroupChat(target)
		if target == chatID && isGroupChatID(chatID) {
			sk := d.sessionKey(chatID)
			sess := d.store.Active(sk)
			if sess == nil {
				d.sendMessage(chatID, msgID, "Нет активной сессии")
				return
			}
			d.sendMessage(chatID, msgID, d.settingsText(chatID, sk, sess))
			return
		}
		d.sendMessage(chatID, msgID, fmt.Sprintf("❌ Режим группы выключен: <code>%s</code>", html.EscapeString(target)))
	default:
		d.sendMessage(chatID, msgID, "Использование: /groups [on | off [id]]")
	}
}

func (d *daemon) groupsText() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.groupChats) == 0 {
		return "Нет активных групп."
	}
	keys := make([]string, 0, len(d.groupChats))
	for id := range d.groupChats {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("Группы:\n")
	for _, id := range keys {
		sb.WriteString(fmt.Sprintf("- <code>%s</code>\n  📂 <code>%s</code>\n", html.EscapeString(id), html.EscapeString(tildePath(d.groupChats[id]))))
	}
	return sb.String()
}

func (d *daemon) handleTransports(chatID, msgID string, parts []string) {
	// /transports — list
	if len(parts) == 1 {
		d.sendPlain(chatID, msgID, d.transportsText())
		return
	}

	// /transports on|off <name>
	if len(parts) == 3 {
		action := strings.ToLower(parts[1])
		name := strings.ToLower(parts[2])

		// Normalize aliases
		switch name {
		case "max":
			name = "mx"
		case "telegram":
			name = "tg"
		}

		if _, ok := d.transports[name]; !ok {
			d.sendMessage(chatID, msgID, fmt.Sprintf("Транспорт %q не настроен.", name))
			return
		}

		current := transportPrefix(chatID)

		switch action {
		case "on":
			d.mu.Lock()
			delete(d.disabled, name)
			d.mu.Unlock()
			d.saveDisabled()
			d.startPoll(name)
			d.sendPlain(chatID, msgID, d.transportsText())
		case "off":
			if name == current {
				d.sendMessage(chatID, msgID, "Нельзя отключить транспорт, через который идёт команда.")
				return
			}
			d.stopPoll(name)
			d.mu.Lock()
			d.disabled[name] = true
			d.mu.Unlock()
			d.saveDisabled()
			d.sendPlain(chatID, msgID, d.transportsText())
		default:
			d.sendMessage(chatID, msgID, "Использование: /transports [on|off <tg|max|vk>]")
		}
		return
	}

	d.sendMessage(chatID, msgID, "Использование: /transports [on|off <tg|max|vk>]")
}

func (d *daemon) transportsText() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var sb strings.Builder
	sb.WriteString("Транспорты:\n")
	for _, name := range transportOrder {
		if _, ok := d.transports[name]; !ok {
			continue
		}
		if d.disabled[name] {
			sb.WriteString(fmt.Sprintf("  %s — выкл\n", name))
		} else {
			sb.WriteString(fmt.Sprintf("  %s — вкл\n", name))
		}
	}
	return sb.String()
}

// runChatOp sends a progress message, runs the given klax subcommand,
// and edits the progress message with the result.
func (d *daemon) runChatOp(chatID, msgID, subcmd, progressText string) {
	t, rawChatID, fmtStr := d.transportFor(chatID)
	if t == nil {
		return
	}

	ctx, cancel := withDeliveryTimeout(context.Background())
	defer cancel()

	messageIDs, err := d.syncMessageChain(ctx, chatID, t, rawChatID, msgID, nil, progressText, fmtStr)
	if err != nil {
		return
	}

	bin, _ := os.Executable()
	args := strings.Fields(subcmd)
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		detail := lines[len(lines)-1]
		_, _ = d.syncMessageChain(ctx, chatID, t, rawChatID, "", messageIDs, fmt.Sprintf("%s\n❌ %s", progressText, detail), "")
		return
	}

	if strings.HasPrefix(subcmd, "fallback") {
		_, _ = d.syncMessageChain(ctx, chatID, t, rawChatID, "", messageIDs, progressText+"\n✅ Релизная версия установлена, перезапускаюсь...", "")
	} else {
		_, _ = d.syncMessageChain(ctx, chatID, t, rawChatID, "", messageIDs, progressText+"\n✅ Обновлено, перезапускаюсь...", "")
	}
}

// saveDisabled persists the disabled transports set to config.
func (d *daemon) saveDisabled() {
	d.mu.Lock()
	list := make([]string, 0, len(d.disabled))
	for name := range d.disabled {
		list = append(list, name)
	}
	sort.Strings(list)
	d.mu.Unlock()
	d.cfg.DisabledTransports = list
	if err := config.Save(d.cfg); err != nil {
		log.Printf("save config: %v", err)
	}
}
