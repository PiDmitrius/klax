package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

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

var knownModels = []struct {
	alias string
	model string // actual --model value
	label string
}{
	{"opus", "claude-opus-4-6[1m]", "Claude Opus 1M"},
	{"sonnet", "claude-sonnet-4-6[1m]", "Claude Sonnet 1M"},
	{"haiku", "claude-haiku-4-5-20251001", "Claude Haiku 200k"},
}

func (d *daemon) backendText(sess *session.Session) string {
	current := sess.Backend
	if current == "" {
		current = d.cfg.GetDefaultBackend()
	}
	backends := []string{"claude", "codex"}
	var sb strings.Builder
	for _, name := range backends {
		if name == current {
			fmt.Fprintf(&sb, "/backend_%s ✅\n", name)
		} else {
			fmt.Fprintf(&sb, "/backend_%s\n", name)
		}
	}
	if sess.Messages > 0 {
		sb.WriteString("\n(зафиксирован)")
	}
	return sb.String()
}

func (d *daemon) modelText(sess *session.Session) string {
	var sb strings.Builder
	current := sess.ModelOverride
	for _, m := range knownModels {
		if m.model == current {
			fmt.Fprintf(&sb, "/m_%s %s ✅\n", m.alias, m.label)
		} else {
			fmt.Fprintf(&sb, "/m_%s %s\n", m.alias, m.label)
		}
	}
	if current == "" {
		fmt.Fprintf(&sb, "/m_default По умолчанию ✅\n")
	} else {
		fmt.Fprintf(&sb, "/m_default По умолчанию\n")
	}
	return sb.String()
}

// --- Text helpers ---

func helpText() string {
	return `<b>klax</b> — bridge для Claude Code

<b>Команды:</b>
/status — статус
/sessions — сессии
/new [имя] — новая сессия
/name — переименовать сессию
/cleanup — управление сессиями
/cwd [путь] — рабочая директория
/model — модель (opus/sonnet/haiku)
/prompt [текст] — системный промпт
/groups — режим группы
/transports — управление транспортами
/bypass — команда в Claude
/abort — прервать исполнение
/backend — backend (claude/codex)
/update — обновить
/fallback — установить релизную версию`
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

	backend := sess.Backend
	if backend == "" {
		backend = d.cfg.GetDefaultBackend()
	}

	var contextLine string
	if sess.ContextWindow > 0 {
		pct := sess.ContextUsed * 100 / sess.ContextWindow
		contextLine = fmt.Sprintf("\n🤖 <code>%s</code>\n📊 Контекст: %d%% (%dk/%dk)",
			sess.Model, pct,
			sess.ContextUsed/1000, sess.ContextWindow/1000)
	} else if sess.Model != "" {
		contextLine = fmt.Sprintf("\n🤖 <code>%s</code>", sess.Model)
	}

	var rateLine string
	if sess.RateLimitResets > 0 {
		resetsIn := time.Until(time.Unix(sess.RateLimitResets, 0))
		hours := int(resetsIn.Hours())
		mins := int(resetsIn.Minutes()) % 60
		if sess.RateLimitStatus == "throttled" {
			rateLine = fmt.Sprintf("\n🚫 Лимит исчерпан %dч%dм", hours, mins)
		} else if resetsIn > 0 {
			rateLine = fmt.Sprintf("\n⏱ Сброс лимита %dч%dм", hours, mins)
		}
		if sess.RateLimitOverage {
			rateLine += " (overage)"
		}
	}

	return fmt.Sprintf(
		"<b>klax</b> v%s [%s]\n\n📌 <code>%s</code>\n%s%s%s\n💬 Сообщений: %d",
		version, backend, sess.Name, statusLine, contextLine, rateLine, sess.Messages,
	)
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
		b := s.Backend
		if b == "" {
			b = "claude"
		}
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
		b := s.Backend
		if b == "" {
			b = "claude"
		}
		backendSuffix = fmt.Sprintf(" (%s)", b)
	}
	if s.Active {
		detail := "активна"
		if ctx != "" {
			detail += " " + ctx
		}
		fmt.Fprintf(sb, "%s<b>/s%d</b> <code>%s</code> <b>(%s)</b> <b>%d💬</b>%s\n",
			activePrefix, i+1, s.Name, detail, s.Messages, backendSuffix)
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
			inactiveCmd, i+1, s.Name, detail, s.Messages, backendSuffix)
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
