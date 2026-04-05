// Package max provides a minimal MAX (max.ru) Bot API client.
// Uses long-polling for updates and REST for sending messages.
package max

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/PiDmitrius/klax/internal/transport"
)

const apiBase = "https://platform-api.max.ru"

type Bot struct {
	token  string
	client *http.Client
	marker *int64
}

func New(token string) *Bot {
	return &Bot{
		token:  token,
		client: &http.Client{Timeout: 90 * time.Second},
	}
}

// --- Types ---

type User struct {
	UserID   int64  `json:"user_id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

type Recipient struct {
	ChatID   int64  `json:"chat_id,omitempty"`
	ChatType string `json:"chat_type,omitempty"`
	UserID   int64  `json:"user_id,omitempty"`
}

type MessageBody struct {
	Mid  string `json:"mid"`
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

type Message struct {
	Sender    User        `json:"sender"`
	Recipient Recipient   `json:"recipient"`
	Timestamp int64       `json:"timestamp"`
	Body      MessageBody `json:"body"`
}

type Update struct {
	UpdateType string  `json:"update_type"`
	Timestamp  int64   `json:"timestamp"`
	Message    Message `json:"message"`
}

type updatesResp struct {
	Updates []Update `json:"updates"`
	Marker  *int64   `json:"marker"`
}

type sentMessage struct {
	Message struct {
		Body struct {
			Mid string `json:"mid"`
		} `json:"body"`
	} `json:"message"`
}

// --- API helpers ---

func httpError(code int, desc string) *transport.APIError {
	return &transport.APIError{
		Platform:    "max",
		Code:        code,
		Description: desc,
	}
}

func (b *Bot) request(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, apiBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", b.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return b.client.Do(req)
}

// GetMe validates the bot token.
func (b *Bot) GetMe() (*User, error) {
	resp, err := b.request("GET", "/me", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return nil, httpError(resp.StatusCode, "GET /me: "+string(data))
	}
	var u User
	return &u, json.NewDecoder(resp.Body).Decode(&u)
}

// GetUpdates performs a single long-poll call and returns new updates.
func (b *Bot) GetUpdates() ([]Update, error) {
	path := "/updates?timeout=30&types=message_created"
	if b.marker != nil {
		path += "&marker=" + strconv.FormatInt(*b.marker, 10)
	}
	resp, err := b.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return nil, httpError(resp.StatusCode, "GET /updates: "+string(data))
	}
	var r updatesResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	b.marker = r.Marker
	return r.Updates, nil
}

// DrainUpdates advances past all pending updates.
func (b *Bot) DrainUpdates() error {
	// Do a poll with timeout=0 to get current marker.
	path := "/updates?timeout=0&types=message_created"
	if b.marker != nil {
		path += "&marker=" + strconv.FormatInt(*b.marker, 10)
	}
	resp, err := b.request("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r updatesResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	b.marker = r.Marker
	return nil
}

// SendMessage sends a text message. For DM use userID>0, chatID=0. For group use chatID.
func (b *Bot) SendMessage(chatID, text, replyTo, format string) error {
	_, err := b.sendMsg(chatID, text, replyTo, format)
	return err
}

// SendMessageReturnID sends a message and returns its mid.
func (b *Bot) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	return b.sendMsg(chatID, text, replyTo, format)
}

func (b *Bot) sendMsg(chatID, text, replyTo, format string) (string, error) {
	payload := map[string]interface{}{
		"text": text,
	}
	if format == "markdown" || format == "html" {
		payload["format"] = format
	}
	if replyTo != "" {
		payload["link"] = map[string]string{
			"type": "reply",
			"mid":  replyTo,
		}
	}
	body, _ := json.Marshal(payload)

	// Positive IDs are user IDs (DMs), negative are chat IDs (groups).
	var query string
	if id, err := strconv.ParseInt(chatID, 10, 64); err == nil && id > 0 {
		query = "/messages?user_id=" + chatID
	} else {
		query = "/messages?chat_id=" + chatID
	}
	resp, err := b.request("POST", query, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return "", httpError(resp.StatusCode, "POST /messages: "+string(data))
	}
	var msg sentMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return "", err
	}
	return msg.Message.Body.Mid, nil
}

// EditMessage edits an existing message by mid.
func (b *Bot) EditMessage(chatID, messageID, text, format string) error {
	payload := map[string]interface{}{
		"text": text,
	}
	if format == "markdown" || format == "html" {
		payload["format"] = format
	}
	body, _ := json.Marshal(payload)
	resp, err := b.request("PUT", "/messages?message_id="+messageID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return httpError(resp.StatusCode, "PUT /messages: "+string(data))
	}
	return nil
}
