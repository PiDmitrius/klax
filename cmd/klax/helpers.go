package main

import (
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/pathutil"
	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/session"
)

// tildePath replaces $HOME prefix with ~ for display.
func tildePath(path string) string {
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// formatToolLines wraps each tool line in monospace.
func formatToolLines(lines []string, format string) string {
	var out []string
	for _, l := range lines {
		l = pathutil.TildePathsInText(l)
		if format == "html" {
			escaped := strings.ReplaceAll(l, "&", "&amp;")
			escaped = strings.ReplaceAll(escaped, "<", "&lt;")
			escaped = strings.ReplaceAll(escaped, ">", "&gt;")
			out = append(out, "<code>"+escaped+"</code>")
		} else {
			escaped := strings.ReplaceAll(l, "`", "'")
			out = append(out, "`"+escaped+"`")
		}
	}
	return strings.Join(out, "\n")
}

var reHTMLTag = regexp.MustCompile(`<[^>]+>`)

// stripHTML removes HTML tags and unescapes entities for plain-text transports.
func stripHTML(s string) string {
	s = reHTMLTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

type modelEntry struct {
	alias string
	model string // actual --model value
	label string
}

var claudeModels = []modelEntry{
	{"opus", "claude-opus-4-6[1m]", "Claude Opus 1M"},
	{"sonnet", "claude-sonnet-4-6[1m]", "Claude Sonnet 1M"},
	{"haiku", "claude-haiku-4-5-20251001", "Claude Haiku 200k"},
}

var codexModels = []modelEntry{
	{"54", "gpt-5.4", "GPT-5.4"},
	{"mini", "gpt-5.4-mini", "GPT-5.4-Mini"},
	{"codex", "gpt-5.3-codex", "GPT-5.3-Codex"},
	{"spark", "gpt-5.3-codex-spark", "GPT-5.3-Codex-Spark"},
	{"52", "gpt-5.2", "GPT-5.2"},
}

func modelsForBackend(backend string) []modelEntry {
	if backend == "codex" {
		return codexModels
	}
	return claudeModels
}

var claudeEfforts = []modelEntry{
	{"low", "low", "Low"},
	{"med", "medium", "Medium"},
	{"high", "high", "High"},
	{"max", "max", "Max"},
}

var codexEfforts = []modelEntry{
	{"low", "low", "Low"},
	{"med", "medium", "Medium"},
	{"high", "high", "High"},
	{"xhigh", "xhigh", "Extra High"},
}

func effortsForBackend(backend string) []modelEntry {
	if backend == "codex" {
		return codexEfforts
	}
	return claudeEfforts
}

func (d *daemon) backendText(sk string, sess *session.Session) string {
	current := resolveSessionBackend(sess, d.scopeDefaults(sk), d.cfg.GetDefaultBackend())
	backends := []string{"claude", "codex"}
	var sb strings.Builder
	for _, name := range backends {
		if name == current {
			fmt.Fprintf(&sb, "<b>/backend_%s ✅</b>\n", name)
		} else {
			fmt.Fprintf(&sb, "/backend_%s\n", name)
		}
	}
	return sb.String()
}

func (d *daemon) modelText(sk string, sess *session.Session) string {
	def := d.scopeDefaults(sk)
	backend := resolveSessionBackend(sess, def, d.cfg.GetDefaultBackend())
	models := modelsForBackend(backend)

	var sb strings.Builder
	current := sess.ModelOverride
	for _, m := range models {
		if m.model == current {
			fmt.Fprintf(&sb, "<b>/m_%s %s ✅</b>\n", m.alias, m.label)
		} else {
			fmt.Fprintf(&sb, "/m_%s %s\n", m.alias, m.label)
		}
	}
	if current == "" {
		fmt.Fprintf(&sb, "<b>/m_default По умолчанию ✅</b>\n")
	} else {
		fmt.Fprintf(&sb, "/m_default По умолчанию\n")
	}
	return sb.String()
}

func (d *daemon) thinkText(sk string, sess *session.Session) string {
	def := d.scopeDefaults(sk)
	backend := resolveSessionBackend(sess, def, d.cfg.GetDefaultBackend())
	efforts := effortsForBackend(backend)

	var sb strings.Builder
	current := sess.ThinkOverride
	for _, e := range efforts {
		if e.model == current {
			fmt.Fprintf(&sb, "<b>/t_%s %s ✅</b>\n", e.alias, e.label)
		} else {
			fmt.Fprintf(&sb, "/t_%s %s\n", e.alias, e.label)
		}
	}
	if current == "" {
		fmt.Fprintf(&sb, "<b>/t_default По умолчанию ✅</b>\n")
	} else {
		fmt.Fprintf(&sb, "/t_default По умолчанию\n")
	}
	return sb.String()
}

func (d *daemon) groupModeText(chatID string) string {
	if !isGroupChatID(chatID) {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<b>Режим группы</b>\n")
	if d.isGroupChat(chatID) {
		sb.WriteString("<b>/group_on ✅</b>\n")
		sb.WriteString("/group_off\n")
	} else {
		sb.WriteString("/group_on\n")
		sb.WriteString("<b>/group_off ✅</b>\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func (d *daemon) settingsText(chatID, sk string, sess *session.Session) string {
	sections := []string{
		"⚙️ Движок:\n" + strings.TrimSuffix(d.backendText(sk, sess), "\n"),
		"🤖 Модель:\n" + strings.TrimSuffix(d.modelText(sk, sess), "\n"),
		"🧠 Мышление:\n" + strings.TrimSuffix(d.thinkText(sk, sess), "\n"),
	}
	if groupText := d.groupModeText(chatID); groupText != "" {
		sections = append(sections, groupText)
	}
	return strings.Join(sections, "\n\n")
}

// saveRateLimit stores a rate limit event into global state.
func (d *daemon) saveRateLimit(backendName string, rl *runner.RateLimitInfo) {
	d.state.SetRateLimit(backendName, rl.RateLimitType, &session.RateLimitState{
		Status:         rl.Status,
		ResetsAt:       rl.ResetsAt,
		Utilization:    rl.Utilization,
		IsUsingOverage: rl.IsUsingOverage,
	})
	d.saveState()
}

// rateLimitText returns rate limit lines for /status.
func (d *daemon) rateLimitText(backendName string) string {
	rl5h, rlWk := d.state.RateLimits(backendName)
	var lines []string
	for _, entry := range []struct {
		label string
		rl    *session.RateLimitState
	}{
		{"5ч", rl5h},
		{"нед", rlWk},
	} {
		if entry.rl == nil || entry.rl.ResetsAt == 0 {
			continue
		}
		resetsIn := time.Until(time.Unix(entry.rl.ResetsAt, 0))
		if resetsIn <= 0 {
			continue
		}
		remaining := formatDuration(resetsIn)
		switch entry.rl.Status {
		case "throttled", "rejected":
			line := fmt.Sprintf("🚫 Лимит (%s) %s", entry.label, remaining)
			if entry.rl.IsUsingOverage {
				line += " (overage)"
			}
			lines = append(lines, line)
		case "allowed_warning":
			pct := int(entry.rl.Utilization * 100)
			lines = append(lines, fmt.Sprintf("⚠️ Лимит (%s) %d%% %s", entry.label, pct, remaining))
		case "allowed":
			pct := int(entry.rl.Utilization * 100)
			if pct > 0 {
				lines = append(lines, fmt.Sprintf("⏱ Лимит (%s) %d%% %s", entry.label, pct, remaining))
			}
		}
	}
	if len(lines) > 0 {
		return "\n" + strings.Join(lines, "\n")
	}
	return ""
}

// --- Text helpers ---

func helpText() string {
	return `<b>klax</b> — AI messaging bridge

<b>Команды:</b>
/status — статус
/sessions — сессии
/new [имя] — новая сессия
/settings — backend, model, think
/name — переименовать сессию
/cleanup — управление сессиями
/cwd [путь] — рабочая директория
/model — выбор модели
/think — уровень мышления
/prompt [текст] — системный промпт
/groups — режим группы
/transports — управление транспортами
/bypass — прямая команда
/abort — прервать исполнение
/backend — backend (claude/codex)
/update — обновить`
}

func (d *daemon) statusText(chatID string) string {
	sess := d.store.Active(chatID)
	if sess == nil {
		return "❌ Нет активной сессии"
	}

	sr := d.getRunner(chatID)
	sr.mu.Lock()
	qlen := len(sr.queue)
	sr.mu.Unlock()

	tool, toolElapsed, totalElapsed := sr.runner.Status()
	var statusLine string
	if sr.runner.IsBusy() {
		totalSec := int(totalElapsed.Seconds())
		if tool.Name != "" {
			toolSec := int(toolElapsed.Seconds())
			statusLine = fmt.Sprintf("🔄 %s (%ds / %ds)", tool.String(), toolSec, totalSec)
		} else {
			statusLine = fmt.Sprintf("🔄 Работает (%ds)", totalSec)
		}
		if qlen > 0 {
			statusLine += fmt.Sprintf(" 📬 %d", qlen)
		}
	} else {
		statusLine = "💤 Свободен"
	}

	backend := resolveSessionBackend(sess, d.scopeDefaults(chatID), d.cfg.GetDefaultBackend())
	model := sess.ModelOverride
	if model == "" {
		model = "по умолчанию"
	}
	think := sess.ThinkOverride
	if think == "" {
		think = "по умолчанию"
	}

	var contextLine string
	if sess.ContextWindow > 0 {
		pct := sess.ContextUsed * 100 / sess.ContextWindow
		contextLine = fmt.Sprintf("\n📊 Контекст: %d%% (%dk/%dk)",
			pct,
			sess.ContextUsed/1000, sess.ContextWindow/1000)
	}

	rateLine := d.rateLimitText(backend)

	return fmt.Sprintf(
		"<b>klax</b> v%s\n\n📌 Сессия: <code>%s</code>\n🧩 Тип: <code>%s</code>\n⚙️ Движок: <code>%s</code>\n🤖 Модель: <code>%s</code>\n🧠 Мышление: <code>%s</code>\n%s%s%s\n💬 Сообщений: %d",
		version, html.EscapeString(sess.Name), sessionModeLabel(chatID), backend, model, think, statusLine, contextLine, rateLine, sess.Messages,
	)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "менее минуты"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		if hours > 0 {
			return fmt.Sprintf("%dд%dч", days, hours)
		}
		return fmt.Sprintf("%dд", days)
	}
	if hours > 0 {
		return fmt.Sprintf("%dч%dм", hours, mins)
	}
	return fmt.Sprintf("%dм", mins)
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dм назад", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dч назад", h)
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dд назад", days)
	}
}

// hasMultipleBackends checks if sessions use more than one backend.
func hasMultipleBackends(sessions []*session.Session) bool {
	seen := ""
	for _, s := range sessions {
		b := resolveSessionBackend(s, nil, "claude")
		if seen == "" {
			seen = b
		} else if seen != b {
			return true
		}
	}
	return false
}

// formatSessionLine renders one session line.
// activePrefix/inactiveCmd control per-mode differences.
// showBackend adds backend name after message count when multiple backends are used.
func formatSessionLine(sb *strings.Builder, i int, s *session.Session, activePrefix, inactiveCmd string, showBackend bool) {
	ctx := ""
	if s.ContextWindow > 0 {
		pct := s.ContextUsed * 100 / s.ContextWindow
		ctx = fmt.Sprintf("%d%%", pct)
	}
	backendSuffix := ""
	if showBackend {
		b := resolveSessionBackend(s, nil, "claude")
		backendSuffix = fmt.Sprintf(" (%s)", b)
	}
	if s.Active {
		detail := "активна"
		if ctx != "" {
			detail += " " + ctx
		}
		fmt.Fprintf(sb, "%s<b>/s%d</b> <code>%s</code> <b>(%s)</b> <b>%d💬</b>%s\n",
			activePrefix, i+1, html.EscapeString(s.Name), detail, s.Messages, backendSuffix)
	} else {
		ago := ""
		if s.LastUsed > 0 {
			ago = timeAgo(time.Unix(s.LastUsed, 0))
		}
		var parts []string
		if ago != "" {
			parts = append(parts, ago)
		}
		if ctx != "" {
			parts = append(parts, ctx)
		}
		detail := ""
		if len(parts) > 0 {
			detail = " (" + strings.Join(parts, " ") + ")"
		}
		fmt.Fprintf(sb, "%s%d <code>%s</code>%s %d💬%s\n",
			inactiveCmd, i+1, html.EscapeString(s.Name), detail, s.Messages, backendSuffix)
	}
}

func (d *daemon) cleanupText(chatID string) string {
	sessions := d.store.SessionsFor(chatID)
	if len(sessions) == 0 {
		return "Нет сессий."
	}
	multi := hasMultipleBackends(sessions)
	var sb strings.Builder
	inactive := 0
	for i, s := range sessions {
		if !s.Active {
			inactive++
		}
		formatSessionLine(&sb, i, s, "✅ ", "❌ /d", multi)
	}
	if inactive == 0 {
		sb.WriteString("\nНечего удалять.")
	}
	return sb.String()
}

func (d *daemon) sessionsText(chatID string) string {
	sessions := d.store.SessionsFor(chatID)
	if len(sessions) == 0 {
		return "Нет сессий. Напиши /new"
	}
	multi := hasMultipleBackends(sessions)
	var sb strings.Builder
	for i, s := range sessions {
		formatSessionLine(&sb, i, s, "", "/s", multi)
	}
	sb.WriteString("\n/cleanup — управление сессиями")
	return sb.String()
}
