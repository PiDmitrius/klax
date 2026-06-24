package mdhtml

import (
	"regexp"
	"strconv"
	"strings"
)

// ConvertRich transforms Markdown into Telegram Rich HTML — the string passed in
// InputRichMessage.html to sendRichMessage / editMessageText. Unlike Convert
// (legacy parse_mode=HTML, inline-only), it emits real document blocks: headings,
// paragraphs, lists, tables, blockquotes, fenced code and horizontal rules.
//
// Output contract relied on by the rich splitter (splitRichMessage in delivery):
// top-level blocks are separated by "\n", and NO block contains a raw "\n" except
// <pre> code blocks (whose internal newlines are real). Lists, tables and
// blockquotes are therefore rendered on a single line — internal breaks use nested
// tags or <br> — so the splitter can cut on top-level boundaries without ever
// tearing a block apart.
func ConvertRich(md string) string {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	lines := strings.Split(md, "\n")

	var out []string
	i := 0
	for i < len(lines) {
		line := lines[i]

		// Fenced code block: ``` — the only block kept multi-line.
		if strings.HasPrefix(line, "```") {
			// Use the fence info string as the language ONLY if its first token is a
			// safe identifier — otherwise a value like `go" onx="y` would break out
			// of the class attribute and make Telegram reject the whole message.
			lang := ""
			if f := strings.Fields(strings.TrimPrefix(line, "```")); len(f) > 0 && reLangSafe.MatchString(f[0]) {
				lang = f[0]
			}
			var block []string
			i++ // skip opening fence
			for i < len(lines) && !strings.HasPrefix(lines[i], "```") {
				block = append(block, escapeHTML(lines[i]))
				i++
			}
			if i < len(lines) {
				i++ // skip closing fence
			}
			open := "<pre><code>"
			if lang != "" {
				open = `<pre><code class="language-` + lang + `">`
			}
			out = append(out, open+strings.Join(block, "\n")+"</code></pre>")
			continue
		}

		// Blank line: top-level block separator.
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}

		// Horizontal rule: a line of only -, * or _ (3+).
		if isHorizontalRule(line) {
			out = append(out, "<hr/>")
			i++
			continue
		}

		// Table: a header row followed by a separator row. The separator's :--:
		// colons set per-column alignment.
		if i+1 < len(lines) && strings.Contains(line, "|") && isTableSeparator(lines[i+1]) {
			headers := parseTableRow(line)
			aligns := parseTableAligns(lines[i+1])
			var rows [][]string
			i += 2
			for i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
				rows = append(rows, parseTableRow(lines[i]))
				i++
			}
			out = append(out, renderTable(headers, aligns, rows))
			continue
		}

		// Heading: # … ###### — wrapped in <b> as well, since small heading sizes
		// (h4–h6) render close to body text and don't read as headings otherwise.
		if m := reHeading.FindStringSubmatch(line); m != nil {
			tag := "h" + strconv.Itoa(len(m[1]))
			out = append(out, "<"+tag+"><b>"+convertInline(escapeHTML(m[2]))+"</b></"+tag+">")
			i++
			continue
		}

		// Blockquote: consecutive lines starting with >
		if strings.HasPrefix(line, ">") {
			var block []string
			for i < len(lines) && strings.HasPrefix(lines[i], ">") {
				q := strings.TrimPrefix(lines[i], ">")
				q = strings.TrimPrefix(q, " ")
				block = append(block, convertInline(escapeHTML(q)))
				i++
			}
			out = append(out, "<blockquote>"+strings.Join(block, "<br>")+"</blockquote>")
			continue
		}

		// List (unordered/ordered, possibly nested via indentation). Collect the
		// whole consecutive list region — including indented sub-items — and build
		// nested <ul>/<ol> from the indentation.
		if reListItem.MatchString(line) {
			var items []listItem
			for i < len(lines) && reListItem.MatchString(lines[i]) {
				m := reListItem.FindStringSubmatch(lines[i])
				items = append(items, listItem{
					indent:  len(strings.ReplaceAll(m[1], "\t", "  ")),
					ordered: strings.HasSuffix(m[2], "."),
					content: convertInline(escapeHTML(m[3])),
				})
				i++
			}
			out = append(out, renderList(items))
			continue
		}

		// Plain paragraph line.
		out = append(out, "<p>"+convertInline(escapeHTML(line))+"</p>")
		i++
	}

	return strings.Join(out, "\n")
}

func isHorizontalRule(line string) bool {
	s := strings.TrimSpace(line)
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			return false
		}
	}
	return true
}

// isTableSeparator reports whether a line is a GFM table alignment row, e.g.
// "| :--- | ---: | :--: |". Every cell must be dashes with optional leading/
// trailing colons.
func isTableSeparator(line string) bool {
	if !strings.Contains(line, "-") {
		return false
	}
	cells := parseTableRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if !reTableSepCell.MatchString(strings.TrimSpace(c)) {
			return false
		}
	}
	return true
}

// parseTableRow splits a "| a | b |" row into trimmed cells, tolerating missing
// outer pipes.
func parseTableRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseTableAligns(line string) []string {
	cells := parseTableRow(line)
	aligns := make([]string, len(cells))
	for i, c := range cells {
		c = strings.TrimSpace(c)
		left := strings.HasPrefix(c, ":")
		right := strings.HasSuffix(c, ":")
		switch {
		case left && right:
			aligns[i] = "center"
		case right:
			aligns[i] = "right"
		case left:
			aligns[i] = "left"
		}
	}
	return aligns
}

// renderTable emits a <table bordered> (the rich attribute that draws cell
// borders for readability), with <th> header cells and <td> body cells carrying
// the markdown column alignment. Cells hold inline formatting only.
func renderTable(headers, aligns []string, rows [][]string) string {
	cols := len(headers)
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r) // never drop cells from an over-wide body row
		}
	}
	cell := func(vals []string, c int) string {
		if c < len(vals) {
			return convertInline(escapeHTML(vals[c]))
		}
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<table bordered><tr>")
	for c := 0; c < cols; c++ {
		// Header cells get no align — the docs only ever put align on <td>, and
		// Telegram styles the header row itself; aligning <th> fights that.
		sb.WriteString("<th>" + cell(headers, c) + "</th>")
	}
	sb.WriteString("</tr>")
	for _, row := range rows {
		sb.WriteString("<tr>")
		for c := 0; c < cols; c++ {
			sb.WriteString("<td" + alignAttr(aligns, c) + ">" + cell(row, c) + "</td>")
		}
		sb.WriteString("</tr>")
	}
	sb.WriteString("</table>")
	return sb.String()
}

func alignAttr(aligns []string, i int) string {
	if i < len(aligns) && aligns[i] != "" {
		return ` align="` + aligns[i] + `"`
	}
	return ""
}

// listItem is one parsed list line: its indentation (leading-space count, tabs
// counted as two), whether the marker is ordered ("N.") and its inline content.
type listItem struct {
	indent  int
	ordered bool
	content string
}

// renderList builds nested <ul>/<ol> from a flat slice of list lines, using each
// line's indent to nest deeper lists inside the parent <li>. Produces a single
// top-level block (no internal newlines), as the rich splitter contract requires.
func renderList(items []listItem) string {
	var sb strings.Builder
	var stack []listItem // one entry per open list level (indent+ordered captured at open)
	closeTop := func() {
		sb.WriteString("</li>")
		if stack[len(stack)-1].ordered {
			sb.WriteString("</ol>")
		} else {
			sb.WriteString("</ul>")
		}
		stack = stack[:len(stack)-1]
	}
	for _, it := range items {
		// Dedent: close every open level deeper than this item.
		for len(stack) > 0 && it.indent < stack[len(stack)-1].indent {
			closeTop()
		}
		if len(stack) > 0 && it.indent == stack[len(stack)-1].indent {
			sb.WriteString("</li>") // sibling — close the previous item's <li>
		} else {
			// Deeper than the current level (or the very first list): open a nested
			// list INSIDE the current still-open <li>. A list is thus only ever
			// opened at the top level or inside an <li> — never directly inside
			// another list (invalid HTML that Telegram would reject). All items are
			// consumed regardless of indent, so none is silently dropped.
			if it.ordered {
				sb.WriteString("<ol>")
			} else {
				sb.WriteString("<ul>")
			}
			stack = append(stack, it)
		}
		sb.WriteString("<li>" + it.content)
	}
	for len(stack) > 0 {
		closeTop()
	}
	return sb.String()
}

var (
	reHeading      = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reListItem     = regexp.MustCompile(`^([ \t]*)([-*+]|\d+\.)\s+(.*)$`)
	reTableSepCell = regexp.MustCompile(`^:?-+:?$`)
	reLangSafe     = regexp.MustCompile(`^[A-Za-z0-9+#._-]+$`)
)
