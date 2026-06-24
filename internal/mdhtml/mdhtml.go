// Package mdhtml converts a subset of Markdown to HTML.
// Supports: code blocks, blockquotes (as <pre>), inline code,
// bold, italic, links, and headers.
package mdhtml

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Convert transforms Markdown text into HTML suitable for the Telegram and MAX
// "html" parse mode. useBlockquote renders ">" quotes as <blockquote> (Telegram
// parse_mode=HTML supports it since Bot API 7.0); when false, quotes stay as the
// legacy <pre> block (MAX, whose blockquote support is unverified).
func Convert(md string, useBlockquote bool) string {
	// Normalize line endings.
	md = strings.ReplaceAll(md, "\r\n", "\n")

	// Split into lines for block-level processing.
	lines := strings.Split(md, "\n")

	var out []string
	i := 0
	for i < len(lines) {
		line := lines[i]

		// Fenced code block: ```
		if strings.HasPrefix(line, "```") {
			var block []string
			i++ // skip opening ```
			for i < len(lines) && !strings.HasPrefix(lines[i], "```") {
				block = append(block, escapeHTML(lines[i]))
				i++
			}
			if i < len(lines) {
				i++ // skip closing ```
			}
			out = append(out, "<pre>"+strings.Join(block, "\n")+"</pre>")
			continue
		}

		// Blockquote block: consecutive lines starting with >
		if strings.HasPrefix(line, ">") {
			var ql []string
			for i < len(lines) && strings.HasPrefix(lines[i], ">") {
				ql = append(ql, lines[i])
				i++
			}
			if useBlockquote {
				// Telegram: real <blockquote> — strip the > markers, keep inline
				// formatting. Legacy parse_mode=HTML does NOT support <br>; line
				// breaks inside the quote are plain "\n" (see the docs' example).
				var b []string
				for _, l := range ql {
					q := strings.TrimPrefix(strings.TrimPrefix(l, ">"), " ")
					b = append(b, convertInline(escapeHTML(q)))
				}
				out = append(out, "<blockquote>"+strings.Join(b, "\n")+"</blockquote>")
			} else {
				// MAX: keep the legacy <pre> with raw markers (blockquote unverified).
				var b []string
				for _, l := range ql {
					b = append(b, escapeHTML(l))
				}
				out = append(out, "<pre>"+strings.Join(b, "\n")+"</pre>")
			}
			continue
		}

		// Header: lines starting with #
		// NB: "#" markers are kept intentionally — Telegram has no header tag,
		// so the raw marker serves as a visual cue alongside <b>.
		if strings.HasPrefix(line, "#") {
			out = append(out, "<b>"+convertInline(escapeHTML(line))+"</b>")
			i++
			continue
		}

		// Regular line: inline conversion.
		out = append(out, convertInline(escapeHTML(line)))
		i++
	}

	return strings.Join(out, "\n")
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// convertInline handles inline formatting on an already HTML-escaped line. A
// markdown link is emitted only for a target validLinkURL accepts, with its href
// quote-escaped; an unsupported target (relative path, bare word like "url", a
// value with spaces, or a local file path like /home/u/notes.md) falls back to
// underlined link text. Emitting a link the transport rejects would fail the
// whole send — Telegram drops a rich message (RICH_MESSAGE_URL_INVALID) or
// rejects the HTML parse — forcing a plain-text fallback that strips every other
// entity in the chunk along with it.
func convertInline(line string) string {
	// 1. Inline code: `...` — atomic, no further parsing inside.
	var codeParts []string
	line = reInlineCode.ReplaceAllStringFunc(line, func(m string) string {
		inner := m[1 : len(m)-1]
		codeParts = append(codeParts, inner)
		return "\x00CODE\x00"
	})

	// 2. Links: [text](url) — only when validLinkURL accepts the target (see above).
	line = reLink.ReplaceAllStringFunc(line, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		if !validLinkURL(sub[2]) {
			return "<u>" + sub[1] + "</u>"
		}
		return `<a href="` + strings.ReplaceAll(sub[2], `"`, "&quot;") + `">` + sub[1] + `</a>`
	})

	// 3. Bold: **text** (before italic)
	line = reBold.ReplaceAllString(line, "<b>$1</b>")

	// 4. Strikethrough: ~~text~~
	line = reStrike.ReplaceAllString(line, "<s>$1</s>")

	// 5. Italic: *text* (but not inside words for _text_)
	line = reItalicStar.ReplaceAllString(line, "<i>$1</i>")
	line = reItalicUnderscore.ReplaceAllString(line, "${1}<i>$2</i>${3}")

	// Restore inline code placeholders.
	for _, code := range codeParts {
		line = strings.Replace(line, "\x00CODE\x00", "<code>"+code+"</code>", 1)
	}

	return line
}

// validLinkURL reports whether u is a link target Telegram accepts in a message.
// Anything else is rendered as plain text rather than a link, so one bad URL can't
// make the transport reject the whole message. u is already HTML-escaped, so a
// literal space or control char (which Telegram rejects) survives here and must be
// caught — a valid scheme prefix alone is not enough.
func validLinkURL(u string) bool {
	if u == "" {
		return false
	}
	for _, r := range u {
		// No URL Telegram accepts contains whitespace or control/zero-width chars;
		// these survive HTML-escaping and would still trigger a whole-message reject.
		// unicode.Cf (format) covers zero-width spaces / BOM that IsSpace misses.
		if r <= ' ' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	// Validate per scheme, not just by prefix: an empty host/payload (e.g.
	// "https://", "https://?q=1", "mailto:", "tel:") would also be rejected.
	switch {
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		p, err := url.Parse(u)
		return err == nil && p.Host != ""
	case strings.HasPrefix(u, "mailto:"):
		return len(u) > len("mailto:")
	case strings.HasPrefix(u, "tel:"):
		return len(u) > len("tel:")
	case strings.HasPrefix(u, "tg://"):
		return len(u) > len("tg://")
	case strings.HasPrefix(u, "#"):
		return len(u) > len("#")
	}
	return false
}

var (
	reInlineCode       = regexp.MustCompile("`[^`]+`")
	reLink             = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reBold             = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reStrike           = regexp.MustCompile(`~~(.+?)~~`)
	reItalicStar       = regexp.MustCompile(`\*(.+?)\*`)
	reItalicUnderscore = regexp.MustCompile(`(^|[\s(])_(.+?)_([\s).,!?:;]|$)`)
)
