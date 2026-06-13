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

// Convert transforms Markdown text into HTML suitable for
// Telegram and MAX APIs (format: "html").
func Convert(md string) string {
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
		// NB: ">" markers are kept intentionally — Telegram has no blockquote,
		// so the raw marker serves as a visual cue.
		if strings.HasPrefix(line, ">") {
			var block []string
			for i < len(lines) && strings.HasPrefix(lines[i], ">") {
				block = append(block, escapeHTML(lines[i]))
				i++
			}
			out = append(out, "<pre>"+strings.Join(block, "\n")+"</pre>")
			continue
		}

		// Header: lines starting with #
		// NB: "#" markers are kept intentionally — Telegram has no header tag,
		// so the raw marker serves as a visual cue alongside <b>.
		if strings.HasPrefix(line, "#") {
			out = append(out, "<b>"+convertInline(escapeHTML(line), false)+"</b>")
			i++
			continue
		}

		// Regular line: inline conversion.
		out = append(out, convertInline(escapeHTML(line), false))
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

// convertInline handles inline formatting on an already HTML-escaped line. When
// strict is true (rich messages) a link is only emitted for a URL Telegram
// accepts and its href is quote-escaped, since one bad URL makes Telegram reject
// the whole rich message; legacy callers pass strict=false to keep byte-identical
// output.
func convertInline(line string, strict bool) string {
	// 1. Inline code: `...` — atomic, no further parsing inside.
	var codeParts []string
	line = reInlineCode.ReplaceAllStringFunc(line, func(m string) string {
		inner := m[1 : len(m)-1]
		codeParts = append(codeParts, inner)
		return "\x00CODE\x00"
	})

	// 2. Links: [text](url). In strict (rich) mode only emit a link for a target
	// Telegram accepts — an unsupported URL (relative path, bare word like "url",
	// a value with spaces, etc.) makes Telegram reject the ENTIRE rich message
	// (RICH_MESSAGE_URL_INVALID) and it drops to plain text — so fall back to the
	// link text alone. Legacy keeps its previous unconditional behavior.
	line = reLink.ReplaceAllStringFunc(line, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		if !strict {
			return `<a href="` + sub[2] + `">` + sub[1] + `</a>`
		}
		if !validLinkURL(sub[2]) {
			return sub[1]
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
// make Telegram reject the whole (rich) message. u is already HTML-escaped, so a
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
