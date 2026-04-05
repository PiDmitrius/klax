// Package mdhtml converts a subset of Markdown to HTML.
// Supports: code blocks, blockquotes (as <pre>), inline code,
// bold, italic, links, and headers.
package mdhtml

import (
	"regexp"
	"strings"
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

// convertInline handles inline formatting on an already HTML-escaped line.
func convertInline(line string) string {
	// 1. Inline code: `...` — atomic, no further parsing inside.
	var codeParts []string
	line = reInlineCode.ReplaceAllStringFunc(line, func(m string) string {
		inner := m[1 : len(m)-1]
		codeParts = append(codeParts, inner)
		return "\x00CODE\x00"
	})

	// 2. Links: [text](url)
	line = reLink.ReplaceAllStringFunc(line, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		return `<a href="` + sub[2] + `">` + sub[1] + `</a>`
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

var (
	reInlineCode       = regexp.MustCompile("`[^`]+`")
	reLink             = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reBold             = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reStrike           = regexp.MustCompile(`~~(.+?)~~`)
	reItalicStar       = regexp.MustCompile(`\*(.+?)\*`)
	reItalicUnderscore = regexp.MustCompile(`(^|[\s(])_(.+?)_([\s).,!?:;]|$)`)
)
