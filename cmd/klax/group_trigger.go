package main

// Group admission reads a bounded visible prefix; the queued message keeps its source text.

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

func hasGroupTrigger(text string) bool {
	addressed, _ := readGroupTrigger(text)
	return addressed
}

func readGroupTrigger(text string) (addressed, content bool) {
	view := groupPrefixReader{prefixes: triggerPrefixes()}
	view.read(text, false, true)
	if !view.done && !view.addressed {
		lower := strings.ToLower(string(view.text))
		for _, prefix := range view.prefixes {
			if lower == prefix {
				view.addressed = true
				break
			}
		}
	}
	return view.addressed, view.content
}

var (
	groupBlockPrefix = regexp.MustCompile(`^(?:#{1,6}[ \t]+|>[ \t]*|[-+*][ \t]+(?:\[[ xX]\][ \t]+)?|[0-9]+[.)][ \t]+|\[\^[^\]\r\n]+\]:[ \t]*)`)
	groupHTMLTag     = regexp.MustCompile(`(?i)^</?([a-z][a-z0-9-]*)(?:\s[^<>]*?)?\s*/?>`)
)

type groupPrefixReader struct {
	prefixes  []string
	text      []rune
	addressed bool
	content   bool
	space     bool
	done      bool
}

func (p *groupPrefixReader) emit(text string) {
	for _, r := range text {
		if p.done {
			return
		}
		if unicode.Is(unicode.Cf, r) || len(p.text) == 0 && unicode.IsSpace(r) {
			continue
		}
		if p.addressed {
			if !p.space && strings.ContainsRune(",.!?:;—", r) {
				continue
			}
			if unicode.IsSpace(r) {
				p.space = true
				continue
			}
			p.content, p.done = true, true
			return
		}
		p.text = append(p.text, r)
		lower := strings.ToLower(string(p.text))
		pending := false
		for _, prefix := range p.prefixes {
			pending = pending || strings.HasPrefix(prefix, lower)
			if strings.HasPrefix(lower, prefix) && len(lower) > len(prefix) {
				boundary, _ := utf8.DecodeRuneInString(lower[len(prefix):])
				if unicode.IsSpace(boundary) || strings.ContainsRune(",.!?:;—", boundary) {
					p.addressed = true
					p.space = unicode.IsSpace(boundary)
					break
				}
			}
		}
		if !pending && !p.addressed {
			p.done = true
		}
	}
}

func (p *groupPrefixReader) read(text string, code, lineStart bool) {
	table := false
	for i := 0; i < len(text) && !p.done; {
		rest := text[i:]
		if !code {
			if lineStart {
				table = strings.HasPrefix(rest, "|") && !strings.HasPrefix(rest, "||")
				if marker := groupBlockPrefix.FindString(rest); marker != "" {
					i += len(marker)
					continue
				}
			}
			if table && rest[0] == '|' {
				p.emit(" ")
				lineStart = false
				i++
				continue
			}
			if rest[0] == '\\' && len(rest) > 1 && strings.ContainsRune(`\`+"`*_{}[]()#+-.!~|<>=", rune(rest[1])) {
				p.emit(rest[1:2])
				lineStart = false
				i += 2
				continue
			}
			if rest[0] == '`' || strings.HasPrefix(rest, "~~~") {
				width := len(rest) - len(strings.TrimLeft(rest, rest[:1]))
				marker := rest[:width]
				if end := strings.Index(rest[width:], marker); end >= 0 {
					body := rest[width : width+end]
					if line := strings.IndexByte(body, '\n'); width >= 3 && line >= 0 {
						body = body[line+1:]
					} else {
						body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", " "), "\n", " ")
						if strings.HasPrefix(body, " ") && strings.HasSuffix(body, " ") && strings.Trim(body, " ") != "" {
							body = body[1 : len(body)-1]
						}
					}
					p.emit(body)
					lineStart = false
					i += width + end + width
					continue
				}
			}
			if rest[0] == '*' || rest[0] == '_' {
				before := ' '
				if i > 0 {
					before, _ = utf8.DecodeLastRuneInString(text[:i])
				}
				if body, end, ok := groupEmphasis(rest, before); ok {
					p.read(body, false, false)
					i += end
					lineStart = false
					continue
				}
			}
			matched := false
			for _, marker := range []string{"~~", "++", "||", "==", "$$", "$"} {
				if !strings.HasPrefix(rest, marker) {
					continue
				}
				if end := strings.Index(rest[len(marker):], marker); end >= 0 {
					p.read(rest[len(marker):len(marker)+end], false, false)
					i += len(marker) + end + len(marker)
					lineStart = false
					matched = true
				}
				break
			}
			if matched {
				continue
			}
			if rest[0] == '[' {
				if strings.HasPrefix(rest, "[^") {
					if end := strings.IndexByte(rest, ']'); end >= 0 {
						i += end + 1
						continue
					}
				}
				if labelEnd := groupLinkEnd(rest, 1, '[', ']'); labelEnd >= 0 && strings.HasPrefix(rest[labelEnd+1:], "(") {
					if targetEnd := groupLinkEnd(rest, labelEnd+2, '(', ')'); targetEnd >= 0 {
						p.read(rest[1:labelEnd], false, false)
						i += targetEnd + 1
						lineStart = false
						continue
					}
				}
			}
		}
		if rest[0] == '<' {
			if tag := groupHTMLTag.FindStringSubmatch(rest); tag != nil {
				name := strings.ToLower(tag[1])
				switch name {
				case "b", "strong", "i", "em", "u", "s", "del", "strike", "mark", "sub", "sup", "a", "span", "tg-spoiler", "p", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "details", "summary", "ul", "ol", "li", "table", "thead", "tbody", "tr", "th", "td", "br", "hr", "pre", "code":
					if name == "pre" || name == "code" {
						closing := "</" + name + ">"
						if end := strings.Index(strings.ToLower(rest[len(tag[0]):]), closing); end >= 0 {
							p.read(rest[len(tag[0]):len(tag[0])+end], true, false)
							i += len(tag[0]) + end + len(closing)
							continue
						}
					}
					if strings.Contains(" p h1 h2 h3 h4 h5 h6 blockquote li tr th td br hr ", " "+name+" ") {
						p.emit(" ")
					}
					i += len(tag[0])
					continue
				}
			}
		}
		if rest[0] == '&' {
			if end := strings.IndexByte(rest[:min(len(rest), 32)], ';'); end >= 0 {
				entity := rest[:end+1]
				if decoded := html.UnescapeString(entity); decoded != entity {
					p.emit(decoded)
					i += len(entity)
					continue
				}
			}
		}
		r, width := utf8.DecodeRuneInString(rest)
		p.emit(string(r))
		if r == '\n' {
			lineStart = true
		} else if !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) {
			lineStart = false
		}
		i += width
	}
}

func groupEmphasis(text string, before rune) (string, int, bool) {
	type delimiter struct {
		start, left, size int
		closes            bool
	}
	marker := text[0]
	var stack []delimiter
	for i := 0; i < len(text); {
		if text[i] == '\\' {
			i += min(2, len(text)-i)
			continue
		}
		if text[i] == '`' {
			width := len(text[i:]) - len(strings.TrimLeft(text[i:], "`"))
			if end := strings.Index(text[i+width:], text[i:i+width]); end >= 0 {
				i += width + end + width
				continue
			}
		}
		if text[i] != marker {
			_, width := utf8.DecodeRuneInString(text[i:])
			i += width
			continue
		}
		width := len(text[i:]) - len(strings.TrimLeft(text[i:], string(marker)))
		prev := before
		if i > 0 {
			prev, _ = utf8.DecodeLastRuneInString(text[:i])
		}
		next := ' '
		if i+width < len(text) {
			next, _ = utf8.DecodeRuneInString(text[i+width:])
		}
		prevSpace, nextSpace := unicode.IsSpace(prev), unicode.IsSpace(next)
		prevPunct, nextPunct := unicode.IsPunct(prev) || unicode.IsSymbol(prev), unicode.IsPunct(next) || unicode.IsSymbol(next)
		opens := !nextSpace && (!nextPunct || prevSpace || prevPunct)
		closes := !prevSpace && (!prevPunct || nextSpace || nextPunct)
		if marker == '*' && width >= 2 {
			closes = !prevSpace
		}
		if marker == '_' {
			opens, closes = opens && (!closes || prevPunct), closes && (!opens || nextPunct)
		}
		left := width
		for closes && left > 0 && len(stack) > 0 {
			opener := &stack[len(stack)-1]
			if (opener.closes || opens) && (opener.size+width)%3 == 0 && (opener.size%3 != 0 || width%3 != 0) {
				break
			}
			used := min(2, min(opener.left, left))
			closing := i + width - left
			opener.left -= used
			left -= used
			if opener.left == 0 {
				if opener.start == 0 {
					return text[used:closing], closing + used, true
				}
				stack = stack[:len(stack)-1]
			}
		}
		if opens && left > 0 {
			stack = append(stack, delimiter{start: i + width - left, left: left, size: width, closes: closes})
		}
		if i == 0 && len(stack) == 0 {
			return "", 0, false
		}
		i += width
	}
	return "", 0, false
}

func groupLinkEnd(text string, start int, open, close byte) int {
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '<':
			if open == '(' {
				if end := strings.IndexByte(text[i:], '>'); end >= 0 {
					i += end
				}
			}
		case open:
			depth++
		case close:
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}
