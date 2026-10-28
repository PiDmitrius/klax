package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/inbound"
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/tg"
)

func TestFormattedGroupTrigger(t *testing.T) {
	d := newTestDaemon(t)
	accepted := []string{
		"кл тест", "Кл! тест", "кл\u202fтест", "\u2066КЛ\u2069 тест", "\ufeff# кл тест",
		"`кл тест`", "`кл` тест", "`кл тест` и ещё текст", "`кл`",
		"```\nкл тест\n```", "```python\nкл print(1)\n  print(2)\n```\nпосле",
		"````go\nкл `тест`\n````", "~~~text\nкл тест\n~~~",
		"**кл** тест", "__кл__ тест", "~~кл~~ тест", "++кл++ тест", "||кл|| тест", "==кл== тест",
		"***кл*** тест", "**_кл_** тест", "**к**__л__ тест", "`к`**л** тест",
		"**к*л*** тест", "***к*л** тест", "*к**л*** тест", "***к**л* тест", "` кл ` тест",
		"**кл тест**", "**кл,**тест", "**кл**", "  \n**КЛ**\u00a0тест",
		"<b>Кл</b> тест", "<u><i>КЛ</i></u> тест", "<code>&#1082;&#1083;</code> тест",
		"<pre><code class=\"language-go\">кл тест\n</code></pre>", "<a id=\"top\"></a>кл тест",
		"<p>кл</p><p>тест</p>", "<table><tr><td><b>кл</b></td><td>тест</td></tr></table>",
		"[кл](https://example.com) тест", "[**кл**](<https://example.com/a)b>) тест",
		"# **кл** тест", "> кл тест", "> > ## кл тест", "- [ ] **кл** тест", "1. кл тест",
		"| **кл** тест | value |\n| --- | --- |", "&#1082;&#1083; тест",
		"кл[^note] тест", "[^note]: **кл** тест",
	}
	rejected := []string{
		"пример **кл**", "класс", "**кл**асс", "**к**__ласс__", "`кл`асс",
		"`print(1)`", "`пример кл тест`", "пример `кл тест`", "`кл тест", "**кл тест",
		"`**кл**` тест", "```text\n**кл**\n```", "[пример](https://example.com/кл)",
		"![кл](https://example.com/image.png)", `\**кл** тест`, `\<b>кл</b> тест`,
		"<config>кл</config> тест", "Клакса тест", "**Клаксик** тест",
		"**# кл** тест", "[кл](https://example.com)асс", "<b>кл</b>асс",
		"` кл `асс",
	}
	for _, chatID := range []string{"tg:-1", "mx:-1", "vk:2000000001", "ym:0/0/group"} {
		for _, text := range accepted {
			if prompt, _, ok := d.groupPrompt(chatID, text, false); !ok || prompt != text {
				t.Errorf("%s: groupPrompt(%q) = %q, %v; want unchanged accepted text", chatID, text, prompt, ok)
			}
		}
		for _, text := range rejected {
			if _, _, ok := d.groupPrompt(chatID, text, false); ok {
				t.Errorf("%s: accepted unaddressed message %q", chatID, text)
			}
		}
		d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) { def.GroupAttachmentMode = "off" })
		for _, text := range accepted {
			if blocked, addressed := d.groupAttachmentsBlocked(chatID, text, true); !blocked || !addressed {
				t.Errorf("%s: addressed off-mode attachment %q: blocked=%v, addressed=%v", chatID, text, blocked, addressed)
			}
		}
	}
}

func TestDispatchInboundFormattedGroupTrigger(t *testing.T) {
	for _, chatID := range []string{"ym:0/0/group", "tg:-1"} {
		for _, direct := range []bool{false, true} {
			t.Run(chatID, func(t *testing.T) {
				t.Setenv("KLAX_DATA_DIR", t.TempDir())
				d := newTestDeliveryDaemon(&fakeTransport{})
				d.groupChats = map[string]string{chatID: t.TempDir()}
				d.store = newStoreWithChat(chatID, "one")
				d.runners = make(map[runnerKey]*sessionRunner)
				klaxID := d.store.SessionsFor(chatID)[0].KlaxID
				sr := d.getRunner(chatID, klaxID)
				sr.processing = true
				origin := inbound.Origin{Transport: strings.SplitN(chatID, ":", 2)[0]}
				text := " \n**КЛ** [тест](https://example.com)\n```go\n  file_name := 1\n```\n "
				if direct {
					if !d.handleInbound(Inbound{ChatID: chatID, MsgID: "1", Text: text, Origin: origin}) {
						t.Fatal("direct admission rejected the trigger")
					}
				} else {
					d.dispatchInbound(chatID, "1", text, nil, nil, false, origin)
				}
				log, err := sr.store.InboundLog()
				if err != nil || len(log) != 1 {
					t.Fatalf("inbound log: %+v, %v", log, err)
				}
				if log[0].Text != text || log[0].OriginalText != text || log[0].Origin != origin {
					t.Fatalf("formatted message changed: %+v", log[0])
				}
			})
		}
	}
}

func TestRichMessageFormattedGroupTrigger(t *testing.T) {
	for _, blocks := range []string{
		`[{"type":"heading","size":2,"text":{"type":"bold","text":"КЛ тест"}}]`,
		`[{"type":"paragraph","text":["к",{"type":"bold","text":"л"}," тест"]}]`,
		`[{"type":"paragraph","text":{"type":"bold","text":["к",{"type":"italic","text":"л"}]}}]`,
		`[{"type":"paragraph","text":{"type":"bold","text":[{"type":"italic","text":"к"},"л"]}}]`,
		`[{"type":"paragraph","text":{"type":"italic","text":["к",{"type":"bold","text":"л"}]}}]`,
		`[{"type":"paragraph","text":{"type":"italic","text":[{"type":"bold","text":"к"},"л"]}}]`,
		`[{"type":"pre","language":"go","text":"кл тест\n  next()"}]`,
		`[{"type":"blockquote","blocks":[{"type":"paragraph","text":"кл тест"}]}]`,
		`[{"type":"list","items":[{"has_checkbox":true,"blocks":[{"type":"paragraph","text":"кл тест"}]}]}]`,
		`[{"type":"table","cells":[[{"text":"кл тест"},{"text":"value"}]]}]`,
	} {
		var rich tg.RichMessage
		if err := json.Unmarshal([]byte(`{"blocks":`+blocks+`}`), &rich); err != nil {
			t.Fatal(err)
		}
		msg := tg.Message{RichMessage: &rich}
		text, _ := msg.InboundContent()
		if !hasGroupTrigger(text) {
			t.Errorf("rich trigger lost: %s -> %q", blocks, text)
		}
	}
	var rich tg.RichMessage
	if err := json.Unmarshal([]byte(`{"blocks":[{"type":"paragraph","text":[{"type":"code","text":"кл"},"асс"]}]}`), &rich); err != nil {
		t.Fatal(err)
	}
	msg := tg.Message{RichMessage: &rich}
	if text, _ := msg.InboundContent(); hasGroupTrigger(text) {
		t.Fatalf("inline-code padding created a word boundary: %q", text)
	}
}

func TestDispatchInboundBareGroupTrigger(t *testing.T) {
	for _, text := range []string{"кл", "кл.", "**кл**", "` кл `", "> кл"} {
		for _, mode := range []string{"empty", "download_error", "file", "direct"} {
			t.Run(mode+"/"+text, func(t *testing.T) {
				t.Setenv("KLAX_DATA_DIR", t.TempDir())
				const chatID = "tg:-1"
				tp := &fakeTransport{}
				d := newTestDeliveryDaemon(tp)
				d.groupChats = map[string]string{chatID: t.TempDir()}
				d.store = newStoreWithChat(chatID, "one")
				d.runners = make(map[runnerKey]*sessionRunner)
				sr := d.getRunner(chatID, d.store.SessionsFor(chatID)[0].KlaxID)
				sr.processing = true
				var files []attachment
				var errors []string
				if mode == "download_error" {
					errors = []string{"фото: unavailable"}
				} else if mode == "file" {
					files = []attachment{{filename: "report.txt", data: []byte("content")}}
				}
				if mode == "direct" {
					if d.handleInbound(Inbound{ChatID: chatID, MsgID: "1", Text: text}) {
						t.Fatal("direct admission accepted a bare trigger")
					}
				} else {
					d.dispatchInbound(chatID, "1", text, files, errors, false, inbound.Origin{Transport: "tg"})
				}
				log, err := sr.store.InboundLog()
				if err != nil {
					t.Fatal(err)
				}
				if mode == "file" {
					if len(log) != 1 || log[0].Text != text || len(log[0].Files) != 1 {
						t.Fatalf("addressed file lost: %+v", log)
					}
				} else if len(log) != 0 {
					t.Fatalf("bare trigger queued without a file: %+v", log)
				}
				if mode == "download_error" && (len(tp.sendLog) != 1 || !strings.Contains(tp.sendLog[0].text, errors[0])) {
					t.Fatalf("download error not reported: %+v", tp.sendLog)
				}
			})
		}
	}
}

func TestGroupTriggerContent(t *testing.T) {
	for _, tc := range []struct {
		text               string
		addressed, content bool
	}{
		{"кл", true, false},
		{"**кл**", true, false},
		{"кл.  ", true, false},
		{"кл ,", true, true},
		{"кл" + strings.Repeat(" ", 128) + "**тест**", true, true},
		{"` кл `асс", false, false},
	} {
		if addressed, content := readGroupTrigger(tc.text); addressed != tc.addressed || content != tc.content {
			t.Errorf("%q: addressed=%v, content=%v; want %v, %v", tc.text, addressed, content, tc.addressed, tc.content)
		}
	}
}
