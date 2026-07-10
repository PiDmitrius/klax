package history

import "strings"

// Codex's new `custom_tool_call(name="exec")` wrapper carries free-form JavaScript in
// its `input` that orchestrates one or more `tools.<name>(...)` actions. klax NEVER
// executes that source; it does a bounded, string-literal-aware scan to recover the
// invoked actions and their previews, then maps each through the SAME
// codexResponseToolCall used for structured function calls (one canonical name→action
// mapping, not a parallel one). A shell command or a patch routinely contains text like
// "tools.exec_command", so matching MUST skip string/template literals and comments.

const (
	maxExecInput = 128 * 1024 // cap the wrapper source we scan
	maxExecCalls = 32          // cap actions decoded from one wrapper
)

type codexToolCall struct {
	name string
	arg  string // raw text between the call's outer ( )
}

// decodeCodexExecTools decodes an exec wrapper's JS `input` into tool rows. It returns nil
// when no `tools.*` call is found, so the caller keeps the visible `🔧 exec` fallback
// rather than dropping the action. Unknown nested tools stay visible as `🔧 <name>`.
func decodeCodexExecTools(src string) []ToolCall {
	if len(src) > maxExecInput {
		src = src[:maxExecInput]
	}
	calls := scanCodexToolCalls(src)
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, codexExecChildTool(src, c.name, c.arg))
	}
	return out
}

// codexExecChildTool maps one decoded `tools.<name>(<arg>)` to a ToolCall, extracting only
// the field each canonical action needs and delegating the actual labelling to
// codexResponseToolCall. Unresolved/unknown names route through with no args, so they show
// as a visible generic row and never leak raw source.
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
	}
	return codexResponseToolCall("", name, "")
}

// scanCodexToolCalls finds top-level `tools.<ident>(...)` calls, skipping string/template
// literals and comments so identifiers inside a command or patch are never matched.
func scanCodexToolCalls(src string) []codexToolCall {
	var out []codexToolCall
	i, n := 0, len(src)
	for i < n && len(out) < maxExecCalls {
		c := src[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			i = skipCodexString(src, i)
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i += 2
		case strings.HasPrefix(src[i:], "tools."):
			j := i + len("tools.")
			k := j
			for k < n && isIdentByte(src[k]) {
				k++
			}
			m := k
			for m < n && isSpaceByte(src[m]) {
				m++
			}
			if k > j && m < n && src[m] == '(' {
				arg, end := balancedParens(src, m)
				out = append(out, codexToolCall{name: src[j:k], arg: strings.TrimSpace(arg)})
				i = end
			} else {
				i++
			}
		default:
			i++
		}
	}
	return out
}

// balancedParens returns the text inside the parenthesis group opened at `open` and the
// index just past its close, balancing nested (){}[] and skipping string literals.
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
						return readStringValue(obj, m+1)
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
						return readStringValue(obj, m+1)
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
	return ""
}

// resolveCodexPatch recovers apply_patch's patch text from its argument: an inline string,
// an object `{input|patch: "..."}`, or a bare identifier assigned earlier via
// `const/let/var name = "..."` (the observed form: `const patch = "*** Begin Patch..."`).
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
					return readStringValue(src, m2+1)
				}
			}
			i = k
			continue
		}
		i++
	}
	return ""
}

// readStringValue skips whitespace then reads a following string literal (unescaped); "" if
// the value is not a string.
func readStringValue(src string, i int) string {
	i = skipSpaceIdx(src, i)
	if i < len(src) && (src[i] == '"' || src[i] == '\'' || src[i] == '`') {
		s, _ := readCodexString(src, i)
		return s
	}
	return ""
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
