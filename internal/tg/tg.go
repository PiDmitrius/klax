// Package tg provides a minimal Telegram Bot API client.
// No external dependencies — only net/http and encoding/json.
package tg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/transport"
)

const apiBase = "https://api.telegram.org/bot"

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

// DrainUpdates advances the offset past all pending updates without processing them.
// Call once on startup to skip accumulated messages.
func (b *Bot) DrainUpdates() error {
	payload := map[string]interface{}{
		"offset":  -1,
		"timeout": 0,
	}
	raw, err := b.call("getUpdates", payload)
	if err != nil {
		return err
	}
	var updates []Update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return err
	}
	if len(updates) > 0 {
		b.offset = updates[len(updates)-1].UpdateID + 1
	}
	return nil
}

// --- Telegram types (minimal) ---

type Update struct {
	UpdateID int      `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	MessageID int        `json:"message_id"`
	From      User       `json:"from"`
	Chat      Chat       `json:"chat"`
	Date      int        `json:"date"`
	Text      string     `json:"text"`
	Caption   string     `json:"caption"`
	Photo     []PhotoSize `json:"photo"`
	Document  *Document  `json:"document"`
}

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int    `json:"file_size"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int    `json:"file_size"`
}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// --- API calls ---

// APIError is returned when Telegram responds with ok=false.
// Network errors are returned as plain errors.
type APIError = transport.APIError

func (b *Bot) call(method string, payload interface{}) (json.RawMessage, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s%s/%s", apiBase, b.token, method)
	resp, err := b.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err // network error
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse error: %v", err)
	}
	if !result.OK {
		apiErr := &APIError{
			Platform:    "tg",
			Code:        result.ErrorCode,
			Description: result.Description,
		}
		if result.Parameters != nil {
			apiErr.RetryAfter = result.Parameters.RetryAfter
		}
		return nil, apiErr
	}
	return result.Result, nil
}

// GetMe calls the getMe API to validate the bot token.
func (b *Bot) GetMe() error {
	_, err := b.call("getMe", struct{}{})
	return err
}

// SetMyCommands sets the bot's command menu visible to users.
func (b *Bot) SetMyCommands(commands []BotCommand) error {
	_, err := b.call("setMyCommands", map[string]interface{}{
		"commands": commands,
	})
	return err
}

// SetMyCommandsForChat sets the bot's command menu for a specific chat.
// This overrides any per-chat menu that may have been set by a previous bot using the same token.
func (b *Bot) SetMyCommandsForChat(chatID string, commands []BotCommand) error {
	_, err := b.call("setMyCommands", map[string]interface{}{
		"commands": commands,
		"scope": map[string]interface{}{
			"type":    "chat",
			"chat_id": chatID,
		},
	})
	return err
}

// BotCommand describes a bot command for the Telegram menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// GetUpdates performs a single long-poll call and returns new updates.
func (b *Bot) GetUpdates() ([]Update, error) {
	payload := map[string]interface{}{
		"offset":  b.offset,
		"timeout": 30,
	}
	raw, err := b.call("getUpdates", payload)
	if err != nil {
		return nil, err
	}
	var updates []Update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, err
	}
	// Advance offset so processed updates are not returned again.
	if len(updates) > 0 {
		b.offset = updates[len(updates)-1].UpdateID + 1
	}
	return updates, nil
}

func (b *Bot) SendMessage(chatID, text, replyTo, format string) error {
	_, err := b.sendMsg(chatID, text, replyTo, format)
	return err
}

func (b *Bot) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	return b.sendMsg(chatID, text, replyTo, format)
}

func (b *Bot) sendMsg(chatID, text, replyTo, format string) (string, error) {
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if replyTo != "" {
		if id, err := strconv.Atoi(replyTo); err == nil {
			payload["reply_parameters"] = map[string]interface{}{"message_id": id}
		}
	}
	switch format {
	case "markdown":
		payload["parse_mode"] = "Markdown"
	case "markdownv2":
		payload["parse_mode"] = "MarkdownV2"
	case "html":
		payload["parse_mode"] = "HTML"
	}
	raw, err := b.call("sendMessage", payload)
	if err != nil {
		return "", err
	}
	var msg struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", msg.MessageID), nil
}

// BestPhoto returns the largest photo from the Photo array, or nil.
func (m *Message) BestPhoto() *PhotoSize {
	if len(m.Photo) == 0 {
		return nil
	}
	best := &m.Photo[0]
	for i := range m.Photo {
		if m.Photo[i].FileSize > best.FileSize {
			best = &m.Photo[i]
		}
	}
	return best
}

// DownloadFile downloads a file by its file_id and returns the bytes and filename.
func (b *Bot) DownloadFile(fileID string) ([]byte, string, error) {
	raw, err := b.call("getFile", map[string]interface{}{"file_id": fileID})
	if err != nil {
		return nil, "", err
	}
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, "", err
	}
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.token, f.FilePath)
	resp, err := b.client.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	// Extract filename from file_path (e.g. "photos/file_1.jpg" -> "file_1.jpg")
	parts := strings.Split(f.FilePath, "/")
	name := parts[len(parts)-1]
	return data, name, nil
}

func (b *Bot) EditMessage(chatID, messageID, text, format string) error {
	msgID, _ := strconv.Atoi(messageID)
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
		"text":       text,
	}
	switch format {
	case "markdown":
		payload["parse_mode"] = "Markdown"
	case "markdownv2":
		payload["parse_mode"] = "MarkdownV2"
	case "html":
		payload["parse_mode"] = "HTML"
	}
	_, err := b.call("editMessageText", payload)
	return err
}
