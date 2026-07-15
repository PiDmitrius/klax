// Package ym provides a minimal Yandex Messenger (Яндекс 360) Bot API client.
// No external dependencies — only net/http and encoding/json.
package ym

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/transport"
)

// apiBase is a var (not const) so tests can point it at an httptest.Server.
var apiBase = "https://botapi.messenger.yandex.net/bot/v1/"

type Bot struct {
	token  string
	client *http.Client
	offset int
}

func New(token string) *Bot {
	return &Bot{
		token:  token,
		client: &http.Client{Timeout: 35 * time.Second},
	}
}

// --- Yandex Messenger types (minimal, see ../../YM_API_NOTES.md) ---

// Chat identifies the conversation an update belongs to. Type is the
// authoritative signal for addressing — NOT whether ID is empty: contrary to
// the documented shape, a private update's ID is populated too, but with a
// composite per-dialog value ("<bot_id>_<user_id>") that is neither a login
// nor a routable chat_id (confirmed empirically 2026-07-15). A private chat
// must be addressed by the sender's Sender.Login instead; group/channel by ID
// (see IsLogin/IsGroup, and the caller in cmd/klax/daemon.go pollYM).
type Chat struct {
	Type string `json:"type"` // "private", "group", "channel"
	ID   string `json:"id,omitempty"`
}

type Sender struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	Robot       bool   `json:"robot"`
}

type File struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Image is one size variant of a sent photo; Update.Images groups variants of
// the same photo together (see BestImage).
type Image struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Size   int64  `json:"size,omitempty"`
	Name   string `json:"name,omitempty"`
}

type Sticker struct {
	ID    string `json:"id"`
	SetID string `json:"set_id"`
}

type Update struct {
	UpdateID  int       `json:"update_id"`
	MessageID int64     `json:"message_id"`
	Timestamp int64     `json:"timestamp"`
	Chat      Chat      `json:"chat"`
	From      Sender    `json:"from"`
	Text      string    `json:"text"`
	Sticker   *Sticker  `json:"sticker"`
	Images    [][]Image `json:"images"`
	File      *File     `json:"file"`
}

// BestImage returns the largest size variant from one images[] group, or nil
// if the group is empty. Prefers the larger `size` (bytes), falling back to
// pixel area (width*height) when sizes tie — `size` is documented as present
// only for the original/largest variant, so every thumbnail variant compares
// as Size==0 and area is the only real signal among them.
func BestImage(variants []Image) *Image {
	if len(variants) == 0 {
		return nil
	}
	best := &variants[0]
	for i := 1; i < len(variants); i++ {
		v := &variants[i]
		if betterImage(v, best) {
			best = v
		}
	}
	return best
}

func betterImage(a, b *Image) bool {
	if a.Size != b.Size {
		return a.Size > b.Size
	}
	return int64(a.Width)*int64(a.Height) > int64(b.Width)*int64(b.Height)
}

// SelfInfo is the response of self/get.
type SelfInfo struct {
	ID            string  `json:"id"`
	Login         string  `json:"login"`
	DisplayName   string  `json:"display_name"`
	WebhookURL    string  `json:"webhook_url"`
	Organizations []int64 `json:"organizations"`
}

// IsLogin reports whether a raw (prefix-stripped) chat address is a user
// login (private chat) rather than a group/channel chat_id. Yandex Messenger
// logins are always "<name>@<domain>"; group/channel chat_id values look like
// "0/0/<guid>" and never contain "@". This is the single place that decides
// the addressing scheme — daemon-side group detection reuses it (IsGroup).
// Relies on the caller having picked the address by Chat.Type (see the Chat
// doc comment) — NOT by whether Chat.ID happens to be empty.
func IsLogin(raw string) bool {
	return strings.Contains(raw, "@")
}

// IsGroup reports whether a raw chat address refers to a group/channel.
func IsGroup(raw string) bool {
	return !IsLogin(raw)
}

// --- API calls ---

// APIError is returned when Yandex Messenger responds with ok=false.
// Network errors are returned as plain errors.
type APIError = transport.APIError

// do performs one HTTP call and unmarshals the common {"ok":...} envelope.
// On success it returns the full raw response body (the envelope is flat —
// unlike Telegram there is no nested "result" field), so callers unmarshal
// whatever fields they need directly out of it.
func (b *Bot) do(method, path string, payload interface{}) (json.RawMessage, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, apiBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+b.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err // network error
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var head struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("parse error: %v", err)
	}
	if !head.OK {
		return nil, &APIError{
			Platform:    "ym",
			Code:        resp.StatusCode,
			Description: head.Description,
		}
	}
	return data, nil
}

// GetMe calls self/get to validate the bot token and fetch its identity.
func (b *Bot) GetMe() (*SelfInfo, error) {
	raw, err := b.do(http.MethodGet, "self/get", nil)
	if err != nil {
		return nil, err
	}
	var info SelfInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

type updatesResp struct {
	Updates []Update `json:"updates"`
}

// nextOffset returns max(update_id)+1 over the page, per the documented
// contract ("offset = max(updates[].update_id) + 1") — NOT the last array
// element's update_id: nothing guarantees the API returns a page sorted by
// update_id, and computing from the last element would misadvance offset (or
// reprocess an update) if it ever isn't.
func nextOffset(updates []Update) int {
	max := updates[0].UpdateID
	for _, u := range updates[1:] {
		if u.UpdateID > max {
			max = u.UpdateID
		}
	}
	return max + 1
}

// DrainUpdates advances the offset past all currently pending updates without
// processing them. Call once on startup to skip accumulated messages.
// Unlike Telegram (whose getUpdates has an offset=-1 "last update only"
// trick), Yandex Messenger has no such shortcut: this fetches one page (up to
// the API max) and advances past it, so a backlog deeper than that is only
// partially skipped — acceptable for the startup-drain use case.
func (b *Bot) DrainUpdates() error {
	raw, err := b.do(http.MethodPost, "messages/getUpdates/", map[string]interface{}{
		"offset": b.offset,
		"limit":  1000,
	})
	if err != nil {
		return err
	}
	var r updatesResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if len(r.Updates) > 0 {
		b.offset = nextOffset(r.Updates)
	}
	return nil
}

// GetUpdates performs a single poll call and returns new updates. Yandex
// Messenger's getUpdates does not document a server-side long-poll wait (no
// "timeout" parameter, unlike Telegram/MAX/VK) — the caller is responsible for
// pacing repeated calls when the result is empty.
func (b *Bot) GetUpdates() ([]Update, error) {
	raw, err := b.do(http.MethodPost, "messages/getUpdates/", map[string]interface{}{
		"offset": b.offset,
		"limit":  100,
	})
	if err != nil {
		return nil, err
	}
	var r updatesResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if len(r.Updates) > 0 {
		b.offset = nextOffset(r.Updates)
	}
	return r.Updates, nil
}

// addressPayload sets the recipient field expected by sendText/getFile-family
// endpoints: "login" for a private chat, "chat_id" for a group/channel.
func addressPayload(raw string, payload map[string]interface{}) {
	if IsLogin(raw) {
		payload["login"] = raw
	} else {
		payload["chat_id"] = raw
	}
}

func (b *Bot) SendMessage(chatID, text, replyTo, format string) error {
	_, err := b.sendMsg(chatID, text, replyTo)
	return err
}

func (b *Bot) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	return b.sendMsg(chatID, text, replyTo)
}

// sendMsg posts messages/sendText. format is intentionally unused: Yandex
// Messenger has a single, always-on markdown-like syntax (see formatting
// notes) — there is no parse_mode/format switch to send, unlike tg/mx.
func (b *Bot) sendMsg(chatID, text, replyTo string) (string, error) {
	payload := map[string]interface{}{"text": text, "disable_web_page_preview": true}
	addressPayload(chatID, payload)
	if replyTo != "" {
		if id, err := strconv.ParseInt(replyTo, 10, 64); err == nil {
			payload["reply_message_id"] = id
		}
	}
	raw, err := b.do(http.MethodPost, "messages/sendText/", payload)
	if err != nil {
		return "", err
	}
	var res struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	return strconv.FormatInt(res.MessageID, 10), nil
}

// EditMessage edits an existing message. Yandex Messenger has no dedicated
// edit endpoint — sendText itself edits when message_id is set (see notes).
func (b *Bot) EditMessage(chatID, messageID, text, format string) error {
	payload := map[string]interface{}{"text": text, "disable_web_page_preview": true}
	addressPayload(chatID, payload)
	if id, err := strconv.ParseInt(messageID, 10, 64); err == nil {
		payload["message_id"] = id
	}
	_, err := b.do(http.MethodPost, "messages/sendText/", payload)
	return err
}

// DownloadFile downloads a file by its file_id and returns the raw bytes. The
// caller already has the filename from the Update that carried the file (File
// or Image), unlike Telegram where DownloadFile itself derives it from
// file_path — Yandex Messenger's getFile response is the file bytes directly,
// not a two-step file_path lookup.
func (b *Bot) DownloadFile(fileID string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, apiBase+"messages/getFile/?file_id="+url.QueryEscape(fileID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+b.token)
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// A failed request comes back as the usual JSON error envelope instead of
	// a binary stream; detect it via Content-Type since there is no other
	// documented signal (HTTP status on error is not specified).
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		var head struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
		}
		if json.Unmarshal(data, &head) == nil && !head.OK {
			return nil, &APIError{Platform: "ym", Code: resp.StatusCode, Description: head.Description}
		}
	}
	return data, nil
}
