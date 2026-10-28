package tg

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// RichMessage is the received Bot API representation, not InputRichMessage:
// Telegram returns blocks even when the sender supplied Markdown or HTML.
type RichMessage struct {
	Blocks []RichBlock `json:"blocks"`
}

// RichBlock holds the fields needed to read the Bot API's block union.
type RichBlock struct {
	Type       string            `json:"type"`
	Text       json.RawMessage   `json:"text"`
	Size       int               `json:"size"`
	Language   string            `json:"language"`
	Expression string            `json:"expression"`
	Name       string            `json:"name"`
	Blocks     []RichBlock       `json:"blocks"`
	Items      []richListItem    `json:"items"`
	Cells      [][]richTableCell `json:"cells"`
	Summary    json.RawMessage   `json:"summary"`
	Credit     json.RawMessage   `json:"credit"`
	Caption    json.RawMessage   `json:"caption"`
	Location   *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"location"`
	Buttons   []richButton `json:"buttons"`
	Photo     []PhotoSize  `json:"photo"`
	Document  *richFile    `json:"document"`
	Audio     *richFile    `json:"audio"`
	Video     *richFile    `json:"video"`
	Animation *richFile    `json:"animation"`
	VoiceNote *richFile    `json:"voice_note"`
}

type richListItem struct {
	Label       string      `json:"label"`
	Blocks      []RichBlock `json:"blocks"`
	HasCheckbox bool        `json:"has_checkbox"`
	IsChecked   bool        `json:"is_checked"`
}

type richTableCell struct {
	Text    json.RawMessage `json:"text"`
	Colspan int             `json:"colspan"`
	Rowspan int             `json:"rowspan"`
}

type richFile struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
}

type richButton struct {
	Text   json.RawMessage `json:"text"`
	URL    string          `json:"url"`
	WebApp *struct {
		URL string `json:"url"`
	} `json:"web_app"`
	LoginURL *struct {
		URL string `json:"url"`
	} `json:"login_url"`
}

// RichMedia is downloaded through the same path as ordinary TG attachments.
type RichMedia struct {
	FileID   string
	FileName string
	Kind     string
}

func (m *Message) InboundContent() (string, []RichMedia) {
	if m.RichMessage != nil {
		var media []RichMedia
		text := renderRichBlocks(m.RichMessage.Blocks, &media)
		return text, media
	}
	if m.Text != "" {
		return m.Text, nil
	}
	return m.Caption, nil
}

func richText(raw json.RawMessage, plain bool) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if plain {
			return s
		}
		return escapeRichLiteral(s)
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		var out strings.Builder
		for _, part := range parts {
			out.WriteString(richText(part, plain))
		}
		return out.String()
	}
	var t struct {
		Type            string          `json:"type"`
		Text            json.RawMessage `json:"text"`
		URL             string          `json:"url"`
		EmailAddress    string          `json:"email_address"`
		PhoneNumber     string          `json:"phone_number"`
		Expression      string          `json:"expression"`
		AlternativeText string          `json:"alternative_text"`
		Name            string          `json:"name"`
		AnchorName      string          `json:"anchor_name"`
		ReferenceName   string          `json:"reference_name"`
		User            User            `json:"user"`
		Button          richButton      `json:"button"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return ""
	}
	text := richText(t.Text, plain || t.Type == "code")
	if t.Type == "custom_emoji" {
		return t.AlternativeText
	}
	if t.Type == "mathematical_expression" {
		if plain {
			return t.Expression
		}
		return "$" + t.Expression + "$"
	}
	if t.Type == "button" {
		return renderRichButton(t.Button, plain)
	}
	if plain {
		return text
	}
	switch t.Type {
	case "bold":
		return "**" + text + "**"
	case "italic":
		return "*" + text + "*"
	case "underline":
		return "<u>" + text + "</u>"
	case "strikethrough":
		return "~~" + text + "~~"
	case "spoiler":
		return "||" + text + "||"
	case "marked":
		return "==" + text + "=="
	case "subscript":
		return "<sub>" + text + "</sub>"
	case "superscript":
		return "<sup>" + text + "</sup>"
	case "code":
		fence := codeFence(text, "`")
		// Padding prevents a literal backtick from merging into the delimiter.
		return fence + " " + text + " " + fence
	case "url":
		return richLink(text, t.URL)
	case "email_address":
		return richLink(text, "mailto:"+t.EmailAddress)
	case "phone_number":
		return richLink(text, "tel:"+t.PhoneNumber)
	case "text_mention":
		return richLink(text, fmt.Sprintf("tg://user?id=%d", t.User.ID))
	case "anchor":
		return "<a id=\"" + escapeRichAttribute(t.Name) + "\"></a>"
	case "anchor_link":
		return richLink(text, "#"+t.AnchorName)
	case "reference":
		return "[^" + t.Name + "]: " + text
	case "reference_link":
		return text + "[^" + t.ReferenceName + "]"
	default:
		return text
	}
}

var (
	richLiteralUnderscore = regexp.MustCompile(`(?m)(^|[^\pL\pN_])_|_($|[^\pL\pN_])`)
	richLiteralBlock      = regexp.MustCompile(`(?m)^[ \t]{0,3}(#{1,6}|[-+]|[0-9]+[.)])([ \t]|$)`)
	richLiteralEntity     = regexp.MustCompile(`&(?:#[0-9]+|#[xX][0-9A-Fa-f]+|[A-Za-z][A-Za-z0-9]+);`)
)

// Preserve punctuation in commands, paths and mentions; escape formatting syntax.
// Pipes are escaped by renderRichTable so table content is escaped once.
func escapeRichLiteral(s string) string {
	s = strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "[", "\\[", "]", "\\]", "<", "\\<", ">", "\\>",
	).Replace(s)
	s = strings.ReplaceAll(s, "~~", `\~\~`)
	s = richLiteralUnderscore.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ReplaceAll(m, "_", `\_`)
	})
	s = richLiteralEntity.ReplaceAllStringFunc(s, func(m string) string { return `\` + m })
	return richLiteralBlock.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.IndexAny(m, "#+-.)")
		return m[:i] + `\` + m[i:]
	})
}

func escapeRichAttribute(s string) string {
	r := strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func richLink(text, target string) string {
	if target == "" {
		return text
	}
	target = strings.NewReplacer("\n", "%0A", "\r", "%0D", "<", "%3C", ">", "%3E").Replace(target)
	return "[" + text + "](<" + target + ">)"
}

func renderRichButton(b richButton, plain bool) string {
	text := richText(b.Text, plain)
	if plain {
		return text
	}
	target := b.URL
	if b.WebApp != nil {
		target = b.WebApp.URL
	}
	if b.LoginURL != nil {
		target = b.LoginURL.URL
	}
	return richLink(text, target)
}

func codeFence(text, minimum string) string {
	fence := minimum
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence
}

func renderRichBlocks(blocks []RichBlock, media *[]RichMedia) string {
	var parts []string
	for _, b := range blocks {
		text := richText(b.Text, false)
		switch b.Type {
		case "paragraph", "footer", "thinking":
		case "heading":
			text = strings.Repeat("#", max(1, min(6, b.Size))) + " " + text
		case "pre":
			text = richText(b.Text, true)
			fence := codeFence(text, "```")
			text = fence + strings.ReplaceAll(b.Language, "\n", "") + "\n" + text + "\n" + fence
		case "divider":
			text = "---"
		case "mathematical_expression":
			text = "$$\n" + b.Expression + "\n$$"
		case "anchor":
			text = "<a id=\"" + escapeRichAttribute(b.Name) + "\"></a>"
		case "list":
			var items []string
			for _, item := range b.Items {
				prefix := item.Label
				if prefix == "" || prefix == "•" {
					prefix = "-"
				}
				if item.HasCheckbox {
					prefix = "- [ ]"
					if item.IsChecked {
						prefix = "- [x]"
					}
				}
				prefix += " "
				body := renderRichBlocks(item.Blocks, media)
				items = append(items, prefix+strings.ReplaceAll(body, "\n", "\n"+strings.Repeat(" ", len(prefix))))
			}
			text = strings.Join(items, "\n")
		case "blockquote", "expandable_blockquote", "pullquote":
			if len(b.Blocks) > 0 {
				text = renderRichBlocks(b.Blocks, media)
			}
			if credit := richText(b.Credit, false); credit != "" {
				text += "\n\n— " + credit
			}
			text = "> " + strings.ReplaceAll(text, "\n", "\n> ")
		case "details":
			text = richText(b.Summary, false) + "\n\n" + renderRichBlocks(b.Blocks, media)
		case "collage", "slideshow":
			text = renderRichBlocks(b.Blocks, media)
		case "table":
			text = renderRichTable(b.Cells)
		case "map":
			if b.Location != nil {
				text = fmt.Sprintf("Location: %g, %g", b.Location.Latitude, b.Location.Longitude)
			}
		case "buttons":
			var buttons []string
			for _, button := range b.Buttons {
				buttons = append(buttons, renderRichButton(button, false))
			}
			text = strings.Join(buttons, " | ")
		case "photo", "document", "audio", "video", "animation", "voice_note":
			f := b.Document
			switch b.Type {
			case "photo":
				var best PhotoSize
				for _, photo := range b.Photo {
					if photo.Width*photo.Height >= best.Width*best.Height {
						best = photo
					}
				}
				f = &richFile{FileID: best.FileID}
			case "audio":
				f = b.Audio
			case "video":
				f = b.Video
			case "animation":
				f = b.Animation
			case "voice_note":
				f = b.VoiceNote
			}
			text = "[" + b.Type + "]"
			if f != nil && f.FileID != "" {
				*media = append(*media, RichMedia{FileID: f.FileID, FileName: f.FileName, Kind: b.Type})
				if f.FileName != "" {
					text = "[" + b.Type + ": " + f.FileName + "]"
				}
			}
		default:
			text = strings.TrimSpace(text + "\n\n" + renderRichBlocks(b.Blocks, media))
			if text == "" {
				text = "[Unsupported Telegram rich block: " + b.Type + "]"
			}
		}
		if caption := richCaption(b.Caption); caption != "" {
			text += "\n\n" + caption
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func richCaption(raw json.RawMessage) string {
	// Table captions are RichText; media captions have separate text and credit.
	var caption struct {
		Text   json.RawMessage `json:"text"`
		Credit json.RawMessage `json:"credit"`
		Type   string          `json:"type"`
	}
	if json.Unmarshal(raw, &caption) == nil && caption.Type == "" && (len(caption.Text) > 0 || len(caption.Credit) > 0) {
		text := richText(caption.Text, false)
		if credit := richText(caption.Credit, false); credit != "" {
			text += "\n\n— " + credit
		}
		return strings.TrimSpace(text)
	}
	return richText(raw, false)
}

func renderRichTable(rows [][]richTableCell) string {
	// Markdown has no merged cells; retain their dimensions explicitly.
	width := 0
	for _, row := range rows {
		width = max(width, len(row))
	}
	var lines []string
	for i, row := range rows {
		cells := make([]string, width)
		for j, cell := range row {
			text := richText(cell.Text, false)
			text = strings.NewReplacer("|", "\\|", "\n", "<br>").Replace(text)
			if cell.Colspan > 1 || cell.Rowspan > 1 {
				text += fmt.Sprintf(" (span %d×%d)", max(1, cell.Colspan), max(1, cell.Rowspan))
			}
			cells[j] = text
		}
		lines = append(lines, "| "+strings.Join(cells, " | ")+" |")
		if i == 0 {
			sep := make([]string, width)
			for j := range sep {
				sep[j] = "---"
			}
			lines = append(lines, "| "+strings.Join(sep, " | ")+" |")
		}
	}
	return strings.Join(lines, "\n")
}
