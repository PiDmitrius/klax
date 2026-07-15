package main

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/fmtutil"
	"github.com/PiDmitrius/klax/internal/mdhtml"
	"github.com/PiDmitrius/klax/internal/pathutil"
	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/session"
)

// formatLogItems renders the pre-answer progress log. Tool invocations stay
// as inline monospace ("техлог"). Narration blocks — the intermediate
// assistant text demoted by the runner — are full-format text rendered
// through the same markdown-to-HTML path as the final answer, so lists,
// code fences and emphasis survive. Adjacent tool labels share a single
// newline so they stack tightly; any transition involving narration gets a
// blank line so the formatted text breathes.
//
// Narration items are rendered one at a time because the runner cuts only
// on paragraph boundaries — the "\n\n" between two narration items here is
// the same separator that was consumed at the cut, so the original
// paragraph structure of the model's reply is reproduced exactly.
func formatLogItems(items []runner.ProgressEvent, format string) string {
	var out strings.Builder
	var prevKind runner.ProgressKind
	for i, item := range items {
		if i > 0 {
			tight := prevKind == runner.ProgressKindTool && item.Kind == runner.ProgressKindTool
			out.WriteString(logSeparator(format, tight))
		}
		out.WriteString(formatLogItem(item, format))
		prevKind = item.Kind
	}
	return out.String()
}

// richSpacerBlock is an empty (zero-width) paragraph used as a visual gap between
// rich blocks. Rich renderers ignore inter-block whitespace, so the "\n\n" blank
// line that breathes legacy log sections apart has no effect — the gap must be a
// real block.
const richSpacerBlock = "<p>\u200b</p>" // zero-width-space spacer (\u200b escape, so the plain fallback shows nothing, not "&#8203;")

// logSeparator is the separator placed before a progress-log segment. A tight
// separator (tool→tool) is a single newline; otherwise legacy/plain get a blank
// line and rich gets a spacer block so the gap actually renders.
func logSeparator(format string, tight bool) string {
	if tight {
		return "\n"
	}
	if format == "rich" {
		return "\n" + richSpacerBlock + "\n"
	}
	return "\n\n"
}

func formatLogItem(item runner.ProgressEvent, format string) string {
	text := pathutil.TildePathsInText(item.Text)
	switch item.Kind {
	case runner.ProgressKindNarration:
		switch format {
		case "rich":
			return mdhtml.ConvertRich(text)
		case "html":
			// Progress-log narration keeps <pre> for quotes (rare, ephemeral); the
			// final answer is what switches to <blockquote> on Telegram.
			return mdhtml.Convert(text, false)
		}
		return text
	default:
		switch format {
		case "rich":
			// Tool log as a paragraph block (top-level inline isn't a valid rich
			// block); collapse newlines so it stays a single block for the splitter.
			return "<p><code>" + strings.ReplaceAll(htmlEscapeLogText(text), "\n", " ") + "</code></p>"
		case "html":
			return "<code>" + htmlEscapeLogText(text) + "</code>"
		}
		return "`" + strings.ReplaceAll(text, "`", "'") + "`"
	}
}

func formatLogChunks(items []runner.ProgressEvent, tail, format string, limit int) []string {
	if len(items) == 0 {
		if tail == "" {
			return nil
		}
		return splitMessage(tail, limit, format)
	}

	var chunks []string
	var current strings.Builder
	var prevKind runner.ProgressKind
	for i, item := range items {
		segment := formatLogItem(item, format)
		if i > 0 {
			tight := prevKind == runner.ProgressKindTool && item.Kind == runner.ProgressKindTool
			segment = logSeparator(format, tight) + segment
		}
		appendLogSegment(&chunks, &current, segment, format, limit)
		prevKind = item.Kind
	}
	if tail != "" {
		appendLogSegment(&chunks, &current, logSeparator(format, false)+tail, format, limit)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func appendLogSegment(chunks *[]string, current *strings.Builder, segment, format string, limit int) {
	if current.Len() > 0 && current.Len()+len(segment) > limit {
		*chunks = append(*chunks, current.String())
		current.Reset()
		segment = strings.TrimLeft(segment, "\n")
	}
	if len(segment) <= limit {
		current.WriteString(segment)
		return
	}
	for _, chunk := range splitMessage(segment, limit, format) {
		if current.Len() > 0 {
			*chunks = append(*chunks, current.String())
			current.Reset()
		}
		if len(chunk) > limit {
			*chunks = append(*chunks, chunk)
			continue
		}
		current.WriteString(chunk)
	}
}

func withProgressEllipsis(chunks []string, format string, limit int) []string {
	if format == "rich" {
		// Keep the "still working" marker a real block (rich ignores loose
		// top-level text). If appending it to the last chunk would push it over the
		// limit — e.g. the last chunk is an over-soft-limit single block — emit the
		// marker as its own chunk instead of overflowing the block.
		const block = "<p>…</p>"
		if len(chunks) == 0 {
			return []string{block}
		}
		out := append([]string(nil), chunks...)
		last := len(out) - 1
		if len(out[last])+1+len(block) > limit {
			return append(out, block)
		}
		out[last] += "\n" + block
		return out
	}
	if len(chunks) == 0 {
		return []string{"..."}
	}
	out := append([]string(nil), chunks...)
	out[len(out)-1] += "\n\n..."
	return out
}

func htmlEscapeLogText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
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

var reBlockClose = regexp.MustCompile(`(?i)</(p|li|h[1-6]|tr|blockquote|pre|ul|ol|table|figure|details)>|<br\s*/?>|<hr\s*/?>`)

// htmlToPlain flattens HTML / rich markup into readable plain text for the
// markup→plain fallback: block-closing tags become newlines, then all tags are
// stripped. Without it, a rejected rich message would reach the user as a wall of
// literal <p>/<ul>/<li> tags (stripHTML alone collapses block boundaries to a
// run-on line).
func htmlToPlain(s string) string {
	s = reBlockClose.ReplaceAllString(s, "$0\n")
	s = stripHTML(s)
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// plainFallback turns formatted text into the plain text used when a formatted
// send is rejected. Only HTML-shaped formats are flattened; any other format
// (e.g. markdown, were it ever wired up) is passed through unchanged.
func plainFallback(text, format string) string {
	if format == "html" || format == "rich" {
		return htmlToPlain(text)
	}
	return text
}

var (
	reYMBold = regexp.MustCompile(`(?is)<b>(.*?)</b>`)
	reYMCode = regexp.MustCompile(`(?is)<code>(.*?)</code>`)
	reYMPre  = regexp.MustCompile(`(?is)<pre>(.*?)</pre>`)
	reYMLink = regexp.MustCompile(`(?is)<a href="([^"]*)">(.*?)</a>`)
)

// htmlToYMMarkdown converts klax's internal command-output HTML into Yandex
// Messenger's own always-on markdown-like syntax (**bold**, `code`, fenced
// code blocks, [text](url) links — see YM_API_NOTES.md), which the client
// actually renders. This is why ym gets its own converter instead of sharing
// VK's stripHTML: VK has no text formatting at all, so stripping is correct
// there, but doing the same for ym would throw away real, confirmed-working
// capability (e.g. the active-session bold marker in /sessions).
func htmlToYMMarkdown(s string) string {
	s = reYMPre.ReplaceAllString(s, "```\n$1\n```")
	s = reYMBold.ReplaceAllString(s, "**$1**")
	s = reYMCode.ReplaceAllString(s, "`$1`")
	s = reYMLink.ReplaceAllString(s, "[$2]($1)")
	s = reBlockClose.ReplaceAllString(s, "$0\n")
	s = stripHTML(s)
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// plainRenderForChat renders text for a chat whose transport format is ""
// (no HTML/markdown parse_mode negotiated by klax): ym has real formatting
// support of its own, so klax's internal HTML converts to ym's syntax instead
// of being stripped like it is for VK (which has none).
func plainRenderForChat(fullChatID, text string) string {
	if transportPrefix(fullChatID) == "ym" {
		return htmlToYMMarkdown(text)
	}
	return stripHTML(text)
}

type modelEntry struct {
	alias string
	model string // actual --model value
	label string
}

// Claude models are launched by their bare CLI alias — the alias resolves to the
// current model on its own (fable→claude-fable-5, opus→claude-opus-4-8, …), so
// klax carries no window markers or per-model logic.
var claudeModels = []modelEntry{
	{"fable", "fable", "Fable"},
	{"opus", "opus", "Opus"},
	{"sonnet", "sonnet", "Sonnet"},
	{"haiku", "haiku", "Haiku"},
}

// Codex: the three GPT-5.6 variants (most-capable first) plus GPT-5.5 as an
// explicit fallback. "По умолчанию" (empty) covers "let Codex decide". Bare
// gpt-5.6 is intentionally absent — the local ChatGPT-account Codex rejects it.
var codexModels = []modelEntry{
	{"sol", "gpt-5.6-sol", "GPT-5.6 Sol"},
	{"terra", "gpt-5.6-terra", "GPT-5.6 Terra"},
	{"luna", "gpt-5.6-luna", "GPT-5.6 Luna"},
	{"55", "gpt-5.5", "GPT-5.5"},
}

func modelsForBackend(backend string) []modelEntry {
	if backend == "codex" {
		return codexModels
	}
	return claudeModels
}

// Effort levels start at High: low/medium go unused in practice, and the
// separate "По умолчанию" (empty) choice already covers "let the CLI decide".
// The CLI enum still accepts the lower levels — klax simply doesn't offer them.
var claudeEfforts = []modelEntry{
	{"high", "high", "High"},
	{"xhigh", "xhigh", "Extra High"},
	{"max", "max", "Max"},
}

// Codex GPT-5.6 exposes the deeper Max/Ultra reasoning levels on top of High/
// Extra High; low/medium stay omitted, "По умолчанию" covers the CLI default.
var codexEfforts = []modelEntry{
	{"high", "high", "High"},
	{"xhigh", "xhigh", "Extra High"},
	{"max", "max", "Max"},
	{"ultra", "ultra", "Ultra"},
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

func effectiveSandboxMode(def *session.ScopeDefaults, sess *session.Session) string {
	if sess != nil && sess.Sandbox != "" {
		return sess.Sandbox
	}
	if def != nil && def.Sandbox != "" {
		return def.Sandbox
	}
	return "off"
}

func claudeTTYLabel(sess *session.Session) string {
	if sess != nil && sess.ClaudeTTY {
		return "on"
	}
	return "off"
}

func (d *daemon) sandboxText(sk string, sess *session.Session) string {
	current := effectiveSandboxMode(d.scopeDefaults(sk), sess)
	var sb strings.Builder
	if current == "on" {
		sb.WriteString("<b>/sandbox_on ✅</b>\n")
		sb.WriteString("/sandbox_off\n")
	} else {
		sb.WriteString("/sandbox_on\n")
		sb.WriteString("<b>/sandbox_off ✅</b>\n")
	}
	return sb.String()
}

func (d *daemon) ttyText(sk string, sess *session.Session) string {
	if resolveSessionBackend(sess, d.scopeDefaults(sk), d.cfg.GetDefaultBackend()) != "claude" {
		return "TTY (только claude)"
	}
	var sb strings.Builder
	if sess != nil && sess.ClaudeTTY {
		sb.WriteString("<b>/tty_on ✅</b>\n")
		sb.WriteString("/tty_off\n")
	} else {
		sb.WriteString("/tty_on\n")
		sb.WriteString("<b>/tty_off ✅</b>\n")
	}
	return sb.String()
}

func (d *daemon) verboseText(chatID string) string {
	var sb strings.Builder
	if d.chatVerboseEnabled(chatID) {
		sb.WriteString("<b>/verbose_on ✅</b>\n")
		sb.WriteString("/verbose_off\n")
	} else {
		sb.WriteString("/verbose_on\n")
		sb.WriteString("<b>/verbose_off ✅</b>\n")
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
		"🔒 Sandbox:\n" + strings.TrimSuffix(d.sandboxText(sk, sess), "\n"),
		"🖥 TTY:\n" + strings.TrimSuffix(d.ttyText(sk, sess), "\n"),
	}
	if groupText := d.groupModeText(chatID); groupText != "" {
		sections = append(sections, groupText)
		sections = append(sections, "🗣 Verbose:\n"+strings.TrimSuffix(d.verboseText(chatID), "\n"))
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
		remaining := fmtutil.Duration(resetsIn)
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
/nuke [имя] — новая сессия, остальные снести (с прерыванием)
/settings — backend, model, think
/name — переименовать сессию
/cleanup — управление сессиями
/cwd [путь] — рабочая директория
/model [name] — выбор модели
/think — уровень мышления
/tty — TTY (только claude)
/prompt [текст] — системный промпт
/groups — режим группы
/verbose — промежуточный вывод группы
/rich — Rich-форматирование Telegram (глобально)
/transports — управление транспортами
/bypass — прямая команда
/abort — прервать исполнение
/backend — backend (claude/codex)
/usage — лимиты backend
/update — обновить`
}

func (d *daemon) statusText(chatID string) string {
	sess := d.store.Active(chatID)
	if sess == nil {
		return "❌ Нет активной сессии"
	}

	sr := d.lookupRunner(chatID, sess.Created)
	var statusLine string
	if sr == nil {
		statusLine = "💤 Свободен"
	} else {
		sr.mu.Lock()
		qlen := len(sr.queue)
		sr.mu.Unlock()
		tool, toolElapsed, totalElapsed := sr.runner.Status()
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
		} else if qlen > 0 {
			statusLine = fmt.Sprintf("📬 В очереди: %d", qlen)
		} else {
			statusLine = "💤 Свободен"
		}
	}

	backend := resolveSessionBackend(sess, d.scopeDefaults(chatID), d.cfg.GetDefaultBackend())
	model := sess.Model
	if model == "" {
		model = sess.ModelOverride
	}
	if model == "" {
		model = "по умолчанию"
	}
	think := sess.ThinkOverride
	if think == "" {
		think = "по умолчанию"
	}
	sandbox := effectiveSandboxMode(d.scopeDefaults(chatID), sess)
	tty := claudeTTYLabel(sess)
	verboseLine := ""
	if isGroupChatID(chatID) {
		verbose := "off"
		if d.chatVerboseEnabled(chatID) {
			verbose = "on"
		}
		verboseLine = fmt.Sprintf("🗣 Verbose: <code>%s</code>\n", verbose)
	}

	var contextLine string
	if sess.ContextWindow > 0 {
		pct := sess.ContextUsed * 100 / sess.ContextWindow
		contextLine = fmt.Sprintf("\n📊 Контекст: %d%% (%dk/%dk)",
			pct,
			sess.ContextUsed/1000, sess.ContextWindow/1000)
	}

	rateLine := d.rateLimitText(backend)
	versionPad := strings.Repeat("\u2800", 8)
	statusBlankLine := strings.Repeat("\u2800", 16)

	return fmt.Sprintf(
		"<b>✅ klax</b> v%s%s\n%s\n📌 Сессия: <code>%s</code>\n🧩 Тип: <code>%s</code>\n⚙️ Движок: <code>%s</code>\n🤖 Модель: <code>%s</code>\n🧠 Мышление: <code>%s</code>\n🔒 Sandbox: <code>%s</code>\n🖥 TTY: <code>%s</code>\n%s%s%s%s\n💬 Сообщений: %d",
		version, versionPad, statusBlankLine, html.EscapeString(sess.Name), sessionModeLabel(chatID), backend, model, think, sandbox, tty, verboseLine, statusLine, contextLine, rateLine, sess.Messages,
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
	} else {
		sb.WriteString("\n/nuke — новая сессия, остальные снести (с прерыванием)")
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
