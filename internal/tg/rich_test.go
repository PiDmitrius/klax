package tg

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestRichMessageFromGetUpdates(t *testing.T) {
	bot := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":2,"from":{"id":3},"chat":{"id":3},"rich_message":{"blocks":[{"type":"heading","size":2,"text":"Заголовок"},{"type":"paragraph","text":["Текст ",{"type":"bold","text":{"type":"italic","text":"важно"}}," и ",{"type":"url","text":"ссылка","url":"https://example.com/a(b)"}]}]}}}]}`))
	})
	updates, err := bot.GetUpdates()
	if err != nil {
		t.Fatal(err)
	}
	text, media := updates[0].Message.InboundContent()
	want := "## Заголовок\n\nТекст ***важно*** и [ссылка](<https://example.com/a(b)>)"
	if text != want || len(media) != 0 {
		t.Fatalf("got %q, %+v; want %q, no media", text, media, want)
	}
}

func TestRichTextVariants(t *testing.T) {
	cases := []struct{ input, want string }{
		{`["a",["b","c"]]`, "abc"},
		{`{"type":"underline","text":"a"}`, "<u>a</u>"},
		{`{"type":"strikethrough","text":"a"}`, "~~a~~"},
		{`{"type":"spoiler","text":"a"}`, "||a||"},
		{`{"type":"marked","text":"a"}`, "==a=="},
		{`{"type":"subscript","text":"a"}`, "<sub>a</sub>"},
		{`{"type":"superscript","text":"a"}`, "<sup>a</sup>"},
		{`{"type":"custom_emoji","alternative_text":"🙂"}`, "🙂"},
		{`{"type":"mathematical_expression","expression":"x^2"}`, "$x^2$"},
		{`{"type":"date_time","text":"tomorrow","unix_time":1}`, "tomorrow"},
		{`{"type":"text_mention","text":"Alice","user":{"id":42}}`, "[Alice](<tg://user?id=42>)"},
		{`{"type":"email_address","text":"mail","email_address":"a@example.com"}`, "[mail](<mailto:a@example.com>)"},
		{`{"type":"phone_number","text":"call","phone_number":"+123"}`, "[call](<tel:+123>)"},
		{`{"type":"mention","text":"@bot","username":"bot"}`, "@bot"},
		{`{"type":"bot_command","text":"/help","bot_command":"/help"}`, "/help"},
		{`{"type":"anchor","name":"top"}`, `<a id="top"></a>`},
		{`{"type":"anchor_link","text":"up","anchor_name":"top"}`, "[up](<#top>)"},
		{`{"type":"reference","name":"note","text":"source"}`, "[^note]: source"},
		{`{"type":"reference_link","reference_name":"note","text":"see"}`, "see[^note]"},
		{`{"type":"button","button":{"text":"Open","url":"https://example.com"}}`, "[Open](<https://example.com>)"},
		{`{"type":"future_text","text":["still ","readable"]}`, "still readable"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			if got := richText(json.RawMessage(tc.input), false); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRichBlocksAndMedia(t *testing.T) {
	var msg Message
	err := json.Unmarshal([]byte(`{"rich_message":{"blocks":[
 {"type":"list","items":[{"label":"3.","blocks":[{"type":"paragraph","text":"ordered"},{"type":"list","items":[{"has_checkbox":true,"is_checked":true,"blocks":[{"type":"paragraph","text":"done"}]}]}]}]},
 {"type":"blockquote","credit":"Author","blocks":[{"type":"paragraph","text":"quote"}]},
 {"type":"details","summary":"More","blocks":[{"type":"table","caption":{"type":"bold","text":"Caption"},"cells":[[{"text":"A"},{"text":"B"}],[{"text":"x|y\nz","colspan":2}]]}]},
 {"type":"slideshow","caption":{"text":"Album","credit":"Photographer"},"blocks":[
  {"type":"photo","photo":[{"file_id":"small","width":10,"height":10},{"file_id":"large","width":100,"height":100}],"caption":{"text":"A photo"}},
  {"type":"document","document":{"file_id":"doc","file_name":"report.pdf"}},
  {"type":"audio","audio":{"file_id":"audio","file_name":"song.mp3"}},
  {"type":"video","video":{"file_id":"video","file_name":"clip.mp4"}},
  {"type":"animation","animation":{"file_id":"gif","file_name":"motion.gif"}},
  {"type":"voice_note","voice_note":{"file_id":"voice"}}
 ]},
 {"type":"map","location":{"latitude":1.5,"longitude":2.5}},
 {"type":"buttons","buttons":[{"text":"Open","url":"https://example.com"},{"text":"Action","callback_data":"ignored"}]},
 {"type":"future","blocks":[{"type":"paragraph","text":"nested future text"}]},
 {"type":"unknown"}
 ]}}`), &msg)
	if err != nil {
		t.Fatal(err)
	}
	text, media := msg.InboundContent()
	for _, want := range []string{
		"3. ordered\n   \n   - [x] done", "> quote\n> \n> — Author", "More\n\n| A | B |\n| --- | --- |\n| x\\|y<br>z (span 2×1) |  |",
		"**Caption**", "[photo]\n\nA photo", "[document: report.pdf]", "Album\n\n— Photographer", "Location: 1.5, 2.5",
		"[Open](<https://example.com>) | Action", "nested future text", "[Unsupported Telegram rich block: unknown]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	wantMedia := []RichMedia{{"large", "", "photo"}, {"doc", "report.pdf", "document"}, {"audio", "song.mp3", "audio"}, {"video", "clip.mp4", "video"}, {"gif", "motion.gif", "animation"}, {"voice", "", "voice_note"}}
	if !reflect.DeepEqual(media, wantMedia) {
		t.Fatalf("media=%+v, want %+v", media, wantMedia)
	}
}

func TestRichCodeKeepsLiteralContent(t *testing.T) {
	literal := "`x` **plain**\n```\nend"
	raw, _ := json.Marshal(literal)
	msg := Message{RichMessage: &RichMessage{Blocks: []RichBlock{{Type: "pre", Text: raw, Language: "go"}}}}
	text, _ := msg.InboundContent()
	want := "````go\n" + literal + "\n````"
	if text != want {
		t.Fatalf("got %q, want %q", text, want)
	}
	inline := json.RawMessage("{\"type\":\"code\",\"text\":\"`literal`\"}")
	if got := richText(inline, false); got != "`` `literal` ``" {
		t.Fatalf("inline code: %q", got)
	}
}

func TestInboundContentLegacyAndMediaOnly(t *testing.T) {
	for _, tc := range []struct {
		msg  Message
		want string
	}{
		{Message{Text: "plain **literal**", Caption: "caption"}, "plain **literal**"},
		{Message{Caption: "caption"}, "caption"},
		{Message{}, ""},
	} {
		text, media := tc.msg.InboundContent()
		if text != tc.want || len(media) != 0 {
			t.Fatalf("got %q, %+v; want %q", text, media, tc.want)
		}
	}
	var msg Message
	if err := json.Unmarshal([]byte(`{"rich_message":{"blocks":[{"type":"photo","photo":[{"file_id":"pic","width":20,"height":20}]}]}}`), &msg); err != nil {
		t.Fatal(err)
	}
	text, media := msg.InboundContent()
	if text == "" || len(media) != 1 || media[0].FileID != "pic" {
		t.Fatalf("media-only message lost: %q, %+v", text, media)
	}
}

func TestRichLiteralTextDoesNotBecomeMarkdown(t *testing.T) {
	cases := []struct{ input, want string }{
		{`"<config> **literal** [x](https://example.com)"`, `\<config\> \*\*literal\*\* \[x\](https://example.com)`},
		{`"# heading\n- item\n1. item\n&copy;"`, "\\# heading\n\\- item\n1\\. item\n\\&copy;"},
		{`{"type":"bold","text":"a*b"}`, `**a\*b**`},
		{`"snake_case @klax_dev_bot имя_файла _literal_ ~~literal~~ ~/my-app/file.md"`, `snake_case @klax_dev_bot имя_файла \_literal\_ \~\~literal\~\~ ~/my-app/file.md`},
		{`"version 1.2.3 (see https://example.com/a-b?q=x&n=2)"`, `version 1.2.3 (see https://example.com/a-b?q=x&n=2)`},
		{`"+ item\n2) item\n## heading\n#hashtag\nx - y"`, "\\+ item\n2\\) item\n\\## heading\n#hashtag\nx - y"},
		{`{"type":"url","text":"a]b [c]","url":"https://example.com"}`, `[a\]b \[c\]](<https://example.com>)`},
		{`{"type":"button","button":{"text":"Open [report]","url":"https://example.com"}}`, `[Open \[report\]](<https://example.com>)`},
	}
	for _, tc := range cases {
		if got := richText(json.RawMessage(tc.input), false); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.input, got, tc.want)
		}
	}
	literal := "<config> **literal** [x](https://example.com)"
	raw, _ := json.Marshal(literal)
	if got := richText(raw, true); got != literal {
		t.Fatalf("plain/code text changed: %q", got)
	}
	row := [][]richTableCell{{{Text: json.RawMessage(`"x|y\\z"`)}}}
	if got := renderRichTable(row); got != "| x\\|y\\\\z |\n| --- |" {
		t.Fatalf("table escaped twice: %q", got)
	}
}

func TestRichButtonURLVariants(t *testing.T) {
	var msg Message
	err := json.Unmarshal([]byte(`{"rich_message":{"blocks":[
 {"type":"buttons","buttons":[
 {"text":"Direct","url":"https://example.com/direct"},
 {"text":"Web [app]","web_app":{"url":"https://example.com/app"}},
 {"text":"Login","login_url":{"url":"https://example.com/login"}},
 {"text":"Action","callback_data":"action"}]},
 {"type":"paragraph","text":[
 {"type":"button","button":{"text":"Inline app","web_app":{"url":"https://example.com/app"}}}," ",
 {"type":"button","button":{"text":"Inline login","login_url":{"url":"https://example.com/login"}}}]}]}}`), &msg)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := msg.InboundContent()
	for _, want := range []string{
		"[Direct](<https://example.com/direct>)", `[Web \[app\]](<https://example.com/app>)`, "[Login](<https://example.com/login>)",
		"[Inline app](<https://example.com/app>)", "[Inline login](<https://example.com/login>)", " | Action",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
}
