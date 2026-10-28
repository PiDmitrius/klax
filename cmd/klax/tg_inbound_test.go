package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/tg"
)

type tgInboundTransport func(*http.Request) (*http.Response, error)

func (f tgInboundTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPollTGAcceptsRichMessage(t *testing.T) {
	for _, failDownload := range []bool{false, true} {
		name := "download"
		if failDownload {
			name = "download_error"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("KLAX_DATA_DIR", t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var notices []string
			transport := tgInboundTransport(func(r *http.Request) (*http.Response, error) {
				var body string
				switch r.URL.Path {
				case "/bottest-token/getUpdates":
					cancel() // One batch, then leave the poll loop without a real backend/network.
					body = `{"ok":true,"result":[{"update_id":1,"message":{"message_id":7,"from":{"id":1},"chat":{"id":1,"type":"private"},"rich_message":{"blocks":[{"type":"heading","size":2,"text":"Review"},{"type":"details","summary":"Files","blocks":[{"type":"document","document":{"file_id":"doc","file_name":"report.pdf"}}]}]}}}]}`
				case "/bottest-token/getFile":
					var payload struct {
						FileID string `json:"file_id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.FileID != "doc" {
						t.Fatalf("getFile: %+v, %v", payload, err)
					}
					body = `{"ok":true,"result":{"file_path":"documents/file_1.pdf"}}`
					if failDownload {
						body = `{"ok":false,"error_code":400,"description":"file unavailable"}`
					}
				case "/file/bottest-token/documents/file_1.pdf":
					body = "PDF bytes"
				case "/bottest-token/sendMessage":
					var payload struct {
						Text string `json:"text"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					notices = append(notices, payload.Text)
					body = `{"ok":true,"result":{"message_id":8}}`
				default:
					t.Fatalf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			old := http.DefaultTransport
			http.DefaultTransport = transport
			defer func() { http.DefaultTransport = old }()
			d := newTestDeliveryDaemon(tg.New("test-token"))
			d.cfg.AllowedUsers = []int64{1}
			d.store = newStoreWithChat("tg:1", "one")
			d.runners = make(map[runnerKey]*sessionRunner)
			klaxID := d.store.SessionsFor("tg:1")[0].KlaxID
			sr := d.getRunner("tg:1", klaxID)
			sr.processing = true
			d.pollTG(ctx)
			log, err := sr.store.InboundLog()
			if err != nil || len(log) != 1 {
				t.Fatalf("inbound log: %+v, %v", log, err)
			}
			if !strings.Contains(log[0].Text, "## Review\n\nFiles\n\n[document: report.pdf]") {
				t.Fatalf("rich text lost: %q", log[0].Text)
			}
			if failDownload {
				if len(log[0].Files) != 0 || len(notices) != 1 || !strings.Contains(notices[0], "file unavailable") || !strings.Contains(notices[0], `вложение "report.pdf"`) {
					t.Fatalf("failed download: files=%v, notices=%q", log[0].Files, notices)
				}
			} else {
				if len(log[0].Files) != 1 || !strings.HasSuffix(log[0].Files[0], "report.pdf") {
					t.Fatalf("embedded file lost: %+v", log[0].Files)
				}
				if data, err := os.ReadFile(sr.store.Path(log[0].Files[0])); err != nil || string(data) != "PDF bytes" {
					t.Fatalf("stored file: %q, %v", data, err)
				}
			}
		})
	}
}

func TestRichTextPreservesGroupTriggersAndCommands(t *testing.T) {
	original := triggerPrefixes()
	defer func() { groupTriggerPrefixes.Store(original) }()
	groupTriggerPrefixes.Store([]string{"кл", "@klax_dev_bot"})
	for _, input := range []string{
		"кл. посмотри ~/project/my-app/README.md",
		"кл! версия 1.2.3 (https://example.com/a-b)",
		"@klax_dev_bot посмотри ~/project/my-app/README.md",
	} {
		raw, _ := json.Marshal(input)
		msg := tg.Message{RichMessage: &tg.RichMessage{Blocks: []tg.RichBlock{{Type: "paragraph", Text: raw}}}}
		text, _ := msg.InboundContent()
		want, ok := stripGroupTrigger(input)
		got, accepted := stripGroupTrigger(text)
		if !ok || !accepted || got != want {
			t.Errorf("rich group trigger changed: %q -> %q; accepted=%v, want %q", input, text, accepted, want)
		}
	}
	for _, input := range []string{"/model gpt-5.5", "/m_update", "/cwd ~/project/my-app", "a_b имя_файла a~b a=b x+y {value} (text)"} {
		raw, _ := json.Marshal(input)
		msg := tg.Message{RichMessage: &tg.RichMessage{Blocks: []tg.RichBlock{{Type: "paragraph", Text: raw}}}}
		if text, _ := msg.InboundContent(); text != input {
			t.Errorf("literal command/content changed: %q -> %q", input, text)
		}
	}
}
