package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/tg"
)

var tgMenuCommands = []tg.BotCommand{
	{Command: "status", Description: "Статус"},
	{Command: "sessions", Description: "Сессии"},
	{Command: "new", Description: "Новая сессия"},
	{Command: "model", Description: "Модель"},
	{Command: "abort", Description: "Прервать"},
}

var transportOrder = []string{"tg", "mx", "vk"}

func (d *daemon) handleCommand(chatID, msgID, text string) {
	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])
	// Strip @botname suffix (e.g. /sessions@klax_bot → /sessions)
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at]
	}
	sk := d.sessionKey(chatID)

	switch cmd {
	case "/help":
		d.sendMessage(chatID, msgID, helpText())

	case "/status":
		d.sendMessage(chatID, msgID, d.statusText(sk))

	case "/sessions", "/s":
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
		if len(parts) > 1 {
			name = strings.Join(parts[1:], " ")
		}
		cwd := d.cfg.DefaultCWD
		if cwd == "" {
			cwd, _ = os.UserHomeDir()
		}
		sess := d.store.New(sk, name, cwd)
		d.store.Save()
		d.sendMessage(chatID, msgID, fmt.Sprintf("✅ Новая сессия: <code>%s</code>\n📂 <code>%s</code>", sess.Name, sess.CWD))

	case "/name":
		if len(parts) < 2 {
			d.sendMessage(chatID, msgID, "Использование: /name <имя>")
			return
		}
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		sess.Name = strings.Join(parts[1:], " ")
		d.store.Save()
		d.sendMessage(chatID, msgID, d.sessionsText(sk))

	case "/cwd":
		if len(parts) < 2 {
			sess := d.store.Active(sk)
			if sess != nil {
				d.sendMessage(chatID, msgID, fmt.Sprintf("📂 <code>%s</code>", sess.CWD))
			}
			return
		}
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		sess.CWD = strings.Join(parts[1:], " ")
		d.store.Save()
		d.sendMessage(chatID, msgID, fmt.Sprintf("📂 <code>%s</code>", sess.CWD))

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
				d.sendMessage(chatID, msgID, fmt.Sprintf("📝 <code>%s</code>", sess.AppendSystemPrompt))
			}
			return
		}
		sess.AppendSystemPrompt = text[len("/prompt "):]
		d.store.Save()
		d.sendMessage(chatID, msgID, fmt.Sprintf("📝 <code>%s</code>", sess.AppendSystemPrompt))

	case "/model":
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.sendPlain(chatID, msgID, d.modelText(sess))

	case "/transports":
		d.handleTransports(chatID, msgID, parts)

	case "/bypass":
		if len(parts) < 2 {
			d.sendMessage(chatID, msgID, "Использование: /bypass <команда>")
			return
		}
		prompt := text[len("/bypass "):]
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

	default:
		// /m_MODEL shortcut for model switch
		if strings.HasPrefix(cmd, "/m_") {
			sess := d.store.Active(sk)
			if sess == nil {
				d.sendMessage(chatID, msgID, "Нет активной сессии")
				return
			}
			alias := cmd[3:]
			if alias == "default" {
				sess.ModelOverride = ""
				d.store.Save()
				d.sendPlain(chatID, msgID, d.modelText(sess))
				return
			}
			// Resolve alias to full model name.
			resolved := alias
			for _, m := range knownModels {
				if m.alias == alias {
					resolved = m.model
					break
				}
			}
			sess.ModelOverride = resolved
			d.store.Save()
			d.sendPlain(chatID, msgID, d.modelText(sess))
			return
		}
		// /sN shortcut for /switch N
		if strings.HasPrefix(cmd, "/s") {
			if n, err := strconv.Atoi(cmd[2:]); err == nil {
				sess := d.store.Switch(sk, n-1)
				if sess == nil {
					d.sendMessage(chatID, msgID, fmt.Sprintf("Нет сессии #%d", n))
					return
				}
				d.store.Save()
				d.sendMessage(chatID, msgID, d.sessionsText(sk))
				return
			}
		}
		// /dN shortcut for deleting session N
		if strings.HasPrefix(cmd, "/d") {
			if n, err := strconv.Atoi(cmd[2:]); err == nil {
				idx := n - 1
				sessions := d.store.SessionsFor(sk)
				if idx < 0 || idx >= len(sessions) {
					d.sendMessage(chatID, msgID, fmt.Sprintf("Нет сессии #%d", n))
					return
				}
				if sessions[idx].Active {
					d.sendMessage(chatID, msgID, "Нельзя удалить активную сессию.")
					return
				}
				d.store.Delete(sk, idx)
				d.store.Save()
				d.sendMessage(chatID, msgID, d.cleanupText(sk))
				return
			}
		}
		d.sendMessage(chatID, msgID, fmt.Sprintf("Неизвестная команда: %s\nНапиши /help", cmd))
	}
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

// saveDisabled persists the disabled transports set to config.
func (d *daemon) saveDisabled() {
	d.mu.Lock()
	var list []string
	for name := range d.disabled {
		list = append(list, name)
	}
	d.mu.Unlock()
	d.cfg.DisabledTransports = list
	config.Save(d.cfg)
}
