package history

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Codex's `custom_tool_call(name="exec")` wrapper carries a small JavaScript snippet in its
// `input` that orchestrates one or more `tools.<name>(...)` actions, e.g.
//
//	const r = await tools.exec_command({cmd:"..."}); text(r.output);
//
// klax NEVER executes that source. This is a deliberately small, best-effort decoder for the
// finite shapes Codex actually emits — NOT a general JavaScript parser. It does a
// string-literal-aware scan for `tools.<name>(...)` calls (a shell command or patch routinely
// contains text like "tools.exec_command", so string skipping is required to not match it) and
// maps each through the SAME codexResponseToolCall used for structured function calls. Anything
// it cannot confidently decode falls back to a visible `🔧 exec` / `🔧 <name>` row — never an
// executed action, never a silent drop. We do not pre-harden against JavaScript constructs this
// format does not contain (comments, regex, …); if a genuinely new shape ever shows up in real
// transcripts, extend the decoder for that shape then.

const (
	maxExecInput  = 128 * 1024 // cap the wrapper source we scan
	maxExecCalls  = 32         // cap actions decoded from one wrapper
	maxExecOutput = 4 * 1024   // cap the output tail a finished command row carries
)

type codexToolCall struct {
	name string
	arg  string // raw text between the call's outer ( )
}

// codexExecRow is one decoded action of a wrapper. An exec_command row is a slot that the
// command's completion event replaces. An argument that is not one plain string literal
// (cmd == "") takes a command of the wrapper that matches no decoded one: one that has its
// literal parts in order, or any command when it has none.
type codexExecRow struct {
	tool    ToolCall
	exec    bool
	cmd     string
	pattern []string
	done    *ToolCall
}

// decodeCodexExecTools decodes an exec wrapper's JS `input` into tool rows. It returns nil when
// no `tools.*` call is found, so the caller keeps the visible `🔧 exec` fallback rather than
// dropping the action. Unknown nested tools stay visible as `🔧 <name>`.
func decodeCodexExecTools(src string) []codexExecRow {
	if len(src) > maxExecInput {
		src = src[:maxExecInput]
	}
	calls := scanCodexToolCalls(src)
	if len(calls) == 0 {
		return nil
	}
	out := make([]codexExecRow, 0, len(calls))
	for _, c := range calls {
		row := codexExecRow{tool: codexExecChildTool(src, c.name, c.arg)}
		if c.name == "exec_command" {
			cmd, parts := jsObjectField(c.arg, "cmd", "command")
			if parts == nil {
				row.cmd = cmd
			} else {
				row.pattern = parts
			}
			row.exec = true
		}
		out = append(out, row)
	}
	return out
}

// codexExecWrap is one exec wrapper of the current turn: its timeline item and the rows
// completed commands fill. A command belongs to the wrapper during whose call it started:
// call_id ties the wrapper to its output, and the command's start is its completion minus its
// duration, so a long command finishing under a later wrapper still completes its own row.
type codexExecWrap struct {
	callID   string
	item     int
	from, to time.Time
	closed   bool
	rows     []codexExecRow
}

// codexClockSlack absorbs the millisecond rounding of rollout timestamps.
const codexClockSlack = time.Millisecond

// appendExec adds a wrapper as one item whose tools are its statically decoded actions. If
// nothing decodes, the raw source is shown as an Exec row (or `🔧 exec` when empty) until
// completed commands replace it; a wrapper is never silently dropped.
func (c *codexParser) appendExec(callID, src string, at time.Time, ts string) {
	rows := decodeCodexExecTools(src)
	if len(rows) == 0 {
		tool := ToolCall{Name: "exec", Label: "🔧 exec"}
		if src != "" {
			tool = toolCall("Exec", jsonObject("command", src))
		}
		rows = []codexExecRow{{tool: tool, exec: true}}
	}
	w := &codexExecWrap{callID: callID, from: at, rows: rows}
	c.appendAssistant(Item{Role: "assistant", Tools: w.tools(), Time: ts})
	w.item = len(c.items) - 1
	c.execs = append(c.execs, w)
}

// execCompleted places a completed command into the wrapper that started it. A command whose
// start is unknown or that no wrapper of the turn started (a direct tool call, a user shell)
// leaves the timeline as is. Once later items follow the wrapper, its rows are only replaced,
// never added, so the positions of the blocks after it stay put.
func (c *codexParser) execCompleted(it *codexEventItem, done time.Time) {
	if it.Duration == nil || done.IsZero() {
		return
	}
	w := c.execWrapAt(done.Add(-time.Duration(it.Duration.Secs)*time.Second - time.Duration(it.Duration.Nanos)))
	if w == nil {
		return
	}
	cmd, tool := codexCommandTool(it)
	if !w.complete(cmd, tool, w.item == len(c.items)-1) {
		return
	}
	item := c.at(w.item)
	item.Tools = w.tools()
	item.Rev++
}

func (c *codexParser) execWrapAt(start time.Time) *codexExecWrap {
	for _, w := range slices.Backward(c.execs) {
		if w.from.IsZero() || w.from.After(start.Add(codexClockSlack)) {
			continue
		}
		if w.closed && (w.to.IsZero() || start.After(w.to.Add(codexClockSlack))) {
			continue
		}
		return w
	}
	return nil
}

// complete fills the open row of the decoded command, else an open undecoded exec_command
// matching the command, one with literal parts first. Any other command is appended at the
// end when grow is set, so rows already shown keep their positions. It reports whether a row
// changed.
func (w *codexExecWrap) complete(cmd string, tool ToolCall, grow bool) bool {
	cmd = strings.TrimSpace(cmd)
	for _, pick := range []func(r *codexExecRow) bool{
		func(r *codexExecRow) bool { return r.cmd != "" && strings.TrimSpace(r.cmd) == cmd },
		func(r *codexExecRow) bool {
			return r.cmd == "" && r.pattern != nil && codexPatternMatch(r.pattern, cmd)
		},
		func(r *codexExecRow) bool { return r.cmd == "" && r.pattern == nil },
	} {
		for i := range w.rows {
			if r := &w.rows[i]; r.exec && r.done == nil && pick(r) {
				r.done = &tool
				return true
			}
		}
	}
	if grow {
		w.rows = append(w.rows, codexExecRow{tool: tool})
	}
	return grow
}

// codexTemplateParts splits a template literal's text into its literal parts around ${...};
// an unclosed ${ leaves the last part open.
func codexTemplateParts(s string) []string {
	var parts []string
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			return append(parts, s)
		}
		parts = append(parts, s[:i])
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return append(parts, "")
		}
		s = s[i+j+1:]
	}
}

// codexPatternMatch reports whether cmd starts with the first part, contains the middle ones
// in order and ends with the last.
func codexPatternMatch(parts []string, cmd string) bool {
	rest, ok := strings.CutPrefix(cmd, parts[0])
	if !ok || len(parts) == 1 {
		return ok && rest == ""
	}
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, p)
		if i < 0 {
			return false
		}
		rest = rest[i+len(p):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

func (w *codexExecWrap) tools() []ToolCall {
	var out []ToolCall
	for _, r := range w.rows {
		if r.done != nil {
			out = append(out, *r.done)
		} else {
			out = append(out, r.tool)
		}
	}
	return out
}

// codexCommandTool renders a completed command: the Exec row of its command, marked when it
// failed, with its directory, outcome and output tail as the row's detail.
func codexCommandTool(it *codexEventItem) (string, ToolCall) {
	var cmd string
	var argv []string
	if json.Unmarshal(it.Command, &argv) == nil {
		if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
			cmd = argv[2]
		} else {
			cmd = strings.Join(argv, " ")
		}
	} else {
		_ = json.Unmarshal(it.Command, &cmd)
	}
	tool := toolCall("Exec", jsonObject("command", cmd))
	outcome := it.Status
	if it.ExitCode != nil {
		outcome = fmt.Sprintf("exit %d", *it.ExitCode)
	}
	if it.Status != "" && it.Status != "completed" || it.ExitCode != nil && *it.ExitCode != 0 {
		tool.Label += " ✗ (" + outcome + ")"
	}
	var b strings.Builder
	b.WriteString("$ " + cmd)
	if dir := it.Cwd; dir != "" {
		if u, err := url.Parse(dir); err == nil && u.Scheme == "file" {
			dir = u.Path
		}
		b.WriteString("\ncwd: " + dir)
	}
	status := it.Status
	if it.ExitCode != nil {
		status = strings.TrimPrefix(status+", "+outcome, ", ")
	}
	if status != "" {
		b.WriteString("\nstatus: " + status)
	}
	if out := strings.TrimRight(it.Output, "\n"); out != "" {
		if len(out) > maxExecOutput {
			cut := len(out) - maxExecOutput
			for cut < len(out) && !utf8.RuneStart(out[cut]) {
				cut++
			}
			out = "…" + out[cut:]
		}
		b.WriteString("\n\n" + out)
	}
	tool.Detail = b.String()
	return cmd, tool
}

func codexTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// codexExecChildTool maps one decoded `tools.<name>(<arg>)` to a ToolCall, extracting only the
// field each canonical action needs and delegating the labelling to codexResponseToolCall.
// Unresolved/unknown names route through with no args, so they show as a visible generic row.
func codexExecChildTool(src, name, arg string) ToolCall {
	switch name {
	case "exec_command":
		if cmd := jsObjectString(arg, "cmd", "command"); cmd != "" {
			return codexResponseToolCall("", "exec_command", jsonObject("cmd", cmd))
		}
	case "view_image":
		if path := jsObjectString(arg, "path"); path != "" {
			return codexResponseToolCall("", "view_image", jsonObject("path", path))
		}
	case "apply_patch":
		if patch := resolveCodexPatch(src, arg); patch != "" {
			return codexResponseToolCall("", "apply_patch", patch)
		}
	case "write_stdin":
		// tools.write_stdin({session_id: …, chars: "…"}) — the session id is an internal handle we
		// don't surface; only `chars` distinguishes a poll-wait (empty) from real stdin input.
		return codexWriteStdinTool(jsObjectString(arg, "chars"))
	}
	return codexResponseToolCall("", name, "")
}

// scanCodexToolCalls finds `tools.<ident>(...)` calls, skipping string/template literals so an
// identifier inside a command or patch string is never matched. A batch `<array>.map(x =>
// tools.exec_command(x))` over an array literal of objects, inline or bound by const/let/var,
// yields one exec_command call per element.
func scanCodexToolCalls(src string) []codexToolCall {
	var out []codexToolCall
	i, n := 0, len(src)
	for i < n && len(out) < maxExecCalls {
		c := src[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			i = skipCodexString(src, i)
		case c == '[':
			inner, end := balancedParens(src, i)
			if calls, next, ok := codexMapBatch(src, inner, end); ok {
				out = append(out, calls...)
				i = next
			} else {
				i++
			}
		case isIdentByte(c):
			k := i
			for k < n && isIdentByte(src[k]) {
				k++
			}
			word := src[i:k]
			i = k
			if word == "tools" && k < n && src[k] == '.' {
				j := k + 1
				k = j
				for k < n && isIdentByte(src[k]) {
					k++
				}
				m := skipSpaceIdx(src, k)
				if k > j && m < n && src[m] == '(' {
					arg, end := balancedParens(src, m)
					out = append(out, codexToolCall{name: src[j:k], arg: strings.TrimSpace(arg)})
					i = end
				}
			} else if strings.HasPrefix(src[skipSpaceIdx(src, k):], ".map") {
				if v := codexDeclValue(src, word); v >= 0 && src[v] == '[' && codexArrayEndsDecl(src, v) {
					inner, _ := balancedParens(src, v)
					if calls, next, ok := codexMapBatch(src, inner, k); ok {
						out = append(out, calls...)
						i = next
					}
				}
			}
		default:
			i++
		}
	}
	return out[:min(len(out), maxExecCalls)]
}

// codexMapBatch recognizes `.map(x => tools.exec_command(x))` (optionally async/await, with a
// parenthesized parameter) right after an array literal whose body is arr and whose end is at
// after. Every element must be an object literal.
func codexMapBatch(src, arr string, after int) ([]codexToolCall, int, bool) {
	m := skipSpaceIdx(src, after)
	if !strings.HasPrefix(src[m:], ".map") {
		return nil, 0, false
	}
	m = skipSpaceIdx(src, m+len(".map"))
	if m >= len(src) || src[m] != '(' {
		return nil, 0, false
	}
	body, end := balancedParens(src, m)
	if !isCodexExecCallback(body) {
		return nil, 0, false
	}
	var calls []codexToolCall
	for i := skipSpaceIdx(arr, 0); i < len(arr); {
		if arr[i] != '{' {
			return nil, 0, false
		}
		obj, next := balancedParens(arr, i)
		calls = append(calls, codexToolCall{name: "exec_command", arg: "{" + obj + "}"})
		i = skipSpaceIdx(arr, next)
		if i < len(arr) {
			if arr[i] != ',' {
				return nil, 0, false
			}
			i = skipSpaceIdx(arr, i+1)
		}
	}
	return calls, end, true
}

// isCodexExecCallback reports whether body is `[async] x => [await] tools.exec_command(x)`.
func isCodexExecCallback(body string) bool {
	s := cutCodexWord(strings.TrimSpace(body), "async")
	var param string
	if rest, ok := strings.CutPrefix(s, "("); ok {
		param, s, ok = strings.Cut(rest, ")")
		if !ok {
			return false
		}
		param = strings.TrimSpace(param)
	} else {
		k := 0
		for k < len(s) && isIdentByte(s[k]) {
			k++
		}
		param, s = s[:k], s[k:]
	}
	s, ok := strings.CutPrefix(strings.TrimSpace(s), "=>")
	if !ok || !isIdentifier(param) {
		return false
	}
	s = cutCodexWord(strings.TrimSpace(s), "await")
	for _, tok := range []string{"tools.exec_command", "(", param, ")"} {
		if s, ok = strings.CutPrefix(strings.TrimSpace(s), tok); !ok {
			return false
		}
	}
	return strings.TrimSpace(s) == ""
}

// codexArrayEndsDecl reports whether the array literal at v is the whole initializer: what
// follows its close is `;`, `}`, a statement's first word or the end of src.
func codexArrayEndsDecl(src string, v int) bool {
	_, end := balancedParens(src, v)
	end = skipSpaceIdx(src, end)
	return end == len(src) || src[end] == ';' || src[end] == '}' || isIdentByte(src[end])
}

// cutCodexWord removes a leading keyword followed by a non-identifier byte.
func cutCodexWord(s, word string) string {
	if rest, ok := strings.CutPrefix(s, word); ok && rest != "" && !isIdentByte(rest[0]) {
		return strings.TrimSpace(rest)
	}
	return s
}

// balancedParens returns the text inside the parenthesis group opened at `open` and the index
// just past its close, balancing nested (){}[] and skipping string literals.
func balancedParens(src string, open int) (string, int) {
	depth, i, n := 0, open, len(src)
	start := open + 1
	for i < n {
		switch c := src[i]; {
		case c == '"' || c == '\'' || c == '`':
			i = skipCodexString(src, i)
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
			if depth == 0 {
				return src[start:i], i + 1
			}
		}
		i++
	}
	return src[start:], n
}

// jsObjectString extracts a string-valued field at the TOP level of a JS object literal,
// accepting quoted or bare keys (`{"cmd":"x"}` and `{cmd:"x"}`). Non-string values yield "".
func jsObjectString(obj string, keys ...string) string {
	s, _ := jsObjectField(obj, keys...)
	return s
}

// jsObjectField is jsObjectString that also returns the value's literal parts when it is not
// exactly the literal (see readStringPattern).
func jsObjectField(obj string, keys ...string) (string, []string) {
	i, n := 0, len(obj)
	depth := 0
	for i < n {
		c := obj[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			s, end := readCodexString(obj, i)
			if depth == 1 {
				if m := skipSpaceIdx(obj, end); m < n && obj[m] == ':' {
					if matchKey(s, keys) {
						return readStringPattern(obj, m+1)
					}
				}
			}
			i = end
			continue
		case c == '{' || c == '[' || c == '(':
			depth++
		case c == '}' || c == ']' || c == ')':
			depth--
		default:
			if depth == 1 && isIdentByte(c) {
				k := i
				for k < n && isIdentByte(obj[k]) {
					k++
				}
				if m := skipSpaceIdx(obj, k); m < n && obj[m] == ':' {
					if matchKey(obj[i:k], keys) {
						return readStringPattern(obj, m+1)
					}
					i = m + 1
					continue
				}
				i = k
				continue
			}
		}
		i++
	}
	return "", nil
}

// resolveCodexPatch recovers apply_patch's patch text from its argument: an inline string, an
// object `{input|patch: "..."}`, or a bare identifier assigned earlier via `const/let/var name
// = "..."` (the observed form: `const patch = "*** Begin Patch..."`).
func resolveCodexPatch(src, arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}
	switch arg[0] {
	case '"', '\'', '`':
		s, _ := readCodexString(arg, 0)
		return s
	case '{':
		return jsObjectString(arg, "input", "patch")
	}
	if isIdentifier(arg) {
		return resolveCodexConst(src, arg)
	}
	return ""
}

// resolveCodexConst finds `const|let|var <name> = "<string>"` in src and returns the string.
func resolveCodexConst(src, name string) string {
	if v := codexDeclValue(src, name); v >= 0 {
		return readStringValue(src, v)
	}
	return ""
}

// codexDeclValue returns the index of the value in the first `const|let|var <name> = <value>`
// of src, or -1.
func codexDeclValue(src, name string) int {
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' || c == '`' {
			i = skipCodexString(src, i)
			continue
		}
		if isIdentByte(c) {
			k := i
			for k < n && isIdentByte(src[k]) {
				k++
			}
			if w := src[i:k]; w == "const" || w == "let" || w == "var" {
				m := skipSpaceIdx(src, k)
				ns := m
				for m < n && isIdentByte(src[m]) {
					m++
				}
				decl := src[ns:m]
				if m2 := skipSpaceIdx(src, m); decl == name && m2 < n && src[m2] == '=' {
					if v := skipSpaceIdx(src, m2+1); v < n {
						return v
					}
					return -1
				}
			}
			i = k
			continue
		}
		i++
	}
	return -1
}

// readStringPattern is readStringValue that also returns the literal parts of a value that is
// not exactly the literal: the parts around a template's ${...}, and an open last part when an
// operator continues the value. They are nil for a plain literal or a non-string value.
func readStringPattern(src string, i int) (string, []string) {
	i = skipSpaceIdx(src, i)
	if i >= len(src) || src[i] != '"' && src[i] != '\'' && src[i] != '`' {
		return "", nil
	}
	s, end := readCodexString(src, i)
	var parts []string
	if src[i] == '`' && strings.Contains(s, "${") {
		parts = codexTemplateParts(s)
	}
	if end = skipSpaceIdx(src, end); end < len(src) && strings.IndexByte(",})]", src[end]) < 0 {
		if parts == nil {
			parts = []string{s}
		}
		parts = append(parts, "")
	}
	return s, parts
}

// readStringValue skips whitespace then reads a following string literal (unescaped); "" if the
// value is not a string.
func readStringValue(src string, i int) string {
	s, _ := readStringPattern(src, i)
	return s
}

// readCodexString reads the string literal opened at i (src[i] is the quote) and returns its
// unescaped content plus the index just past the close.
func readCodexString(src string, i int) (string, int) {
	q := src[i]
	i++
	var b strings.Builder
	for i < len(src) {
		c := src[i]
		if c == '\\' && i+1 < len(src) {
			b.WriteByte(unescapeCodexByte(src[i+1]))
			i += 2
			continue
		}
		if c == q {
			return b.String(), i + 1
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), len(src)
}

func unescapeCodexByte(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	}
	return c // \" \' \` \\ \/ and anything else → the literal char
}

// skipCodexString returns the index just past the string literal opened at i.
func skipCodexString(src string, i int) int {
	q := src[i]
	i++
	for i < len(src) {
		if src[i] == '\\' {
			i += 2
			continue
		}
		if src[i] == q {
			return i + 1
		}
		i++
	}
	return len(src)
}

func skipSpaceIdx(src string, i int) int {
	for i < len(src) && isSpaceByte(src[i]) {
		i++
	}
	return i
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$'
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return true
}

func matchKey(k string, keys []string) bool {
	for _, want := range keys {
		if k == want {
			return true
		}
	}
	return false
}
