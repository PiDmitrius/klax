// Package vk provides a minimal VK Community Bot API client.
// Uses Bots Long Poll API for updates and VK API for sending messages.
// No external dependencies — only net/http and encoding/json.
package vk

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/transport"
)

const (
	apiBase    = "https://api.vk.ru/method/"
	apiVersion = "5.199"
)

type Bot struct {
	token   string
	groupID int
	client  *http.Client

	// Long Poll state
	lpServer string
	lpKey    string
	lpTs     string
}

func New(token string) *Bot {
	return &Bot{
		token:  token,
		client: &http.Client{Timeout: 90 * time.Second},
	}
}

// --- Types ---

type Update struct {
	Type    string          `json:"type"`
	Object  json.RawMessage `json:"object"`
	GroupID int             `json:"group_id"`
}

type MessageNew struct {
	Message Message `json:"message"`
}

type Message struct {
	ID                    int    `json:"id"`
	FromID                int    `json:"from_id"`
	PeerID                int    `json:"peer_id"`
	Text                  string `json:"text"`
	ConversationMessageID int    `json:"conversation_message_id"`
}

type GroupInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// --- API helpers ---

func (b *Bot) call(method string, params url.Values) (json.RawMessage, error) {
	params.Set("access_token", b.token)
	params.Set("v", apiVersion)

	resp, err := b.client.PostForm(apiBase+method, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Code int    `json:"error_code"`
			Msg  string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("vk: parse error: %v", err)
	}
	if result.Error != nil {
		// VK error_code 6 = "Too many requests per second"
		retryAfter := 0
		if result.Error.Code == 6 {
			retryAfter = 1
		}
		return nil, &transport.APIError{
			Platform:    "vk",
			Code:        result.Error.Code,
			Description: result.Error.Msg,
			RetryAfter:  retryAfter,
		}
	}
	return result.Response, nil
}

// GetMe validates token and retrieves group info.
func (b *Bot) GetMe() (*GroupInfo, error) {
	raw, err := b.call("groups.getById", url.Values{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Groups []GroupInfo `json:"groups"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.Groups) == 0 {
		return nil, fmt.Errorf("vk: no groups returned")
	}
	b.groupID = resp.Groups[0].ID
	return &resp.Groups[0], nil
}

// --- Long Poll ---

func (b *Bot) initLongPoll() error {
	raw, err := b.call("groups.getLongPollServer", url.Values{
		"group_id": {strconv.Itoa(b.groupID)},
	})
	if err != nil {
		return err
	}
	var lp struct {
		Server string `json:"server"`
		Key    string `json:"key"`
		Ts     string `json:"ts"`
	}
	if err := json.Unmarshal(raw, &lp); err != nil {
		return err
	}
	b.lpServer = lp.Server
	b.lpKey = lp.Key
	b.lpTs = lp.Ts
	return nil
}

// DrainUpdates initializes long poll and skips pending updates.
func (b *Bot) DrainUpdates() error {
	return b.initLongPoll()
}

// GetUpdates performs a single long-poll call and returns new message updates.
func (b *Bot) GetUpdates() ([]Update, error) {
	if b.lpServer == "" {
		if err := b.initLongPoll(); err != nil {
			return nil, err
		}
	}

	u := fmt.Sprintf("%s?act=a_check&key=%s&ts=%s&wait=25", b.lpServer, b.lpKey, b.lpTs)
	resp, err := b.client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var lpResp struct {
		Ts      string   `json:"ts"`
		Updates []Update `json:"updates"`
		Failed  int      `json:"failed"`
	}
	if err := json.Unmarshal(body, &lpResp); err != nil {
		return nil, err
	}

	switch lpResp.Failed {
	case 0:
		b.lpTs = lpResp.Ts
		return lpResp.Updates, nil
	case 1:
		// ts outdated, update ts
		b.lpTs = lpResp.Ts
		return nil, nil
	case 2, 3:
		// key expired or lost info, reinit
		if err := b.initLongPoll(); err != nil {
			return nil, err
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("vk longpoll: unknown failed=%d", lpResp.Failed)
	}
}

// ParseMessageNew extracts a MessageNew from an Update.
func ParseMessageNew(u Update) (*MessageNew, error) {
	var mn MessageNew
	return &mn, json.Unmarshal(u.Object, &mn)
}

// --- Send / Edit ---

// SendMessage sends a text message to a peer.
func (b *Bot) SendMessage(chatID, text, replyTo, format string) error {
	_, err := b.sendMsg(chatID, text, replyTo)
	return err
}

// SendMessageReturnID sends a message and returns its ID as string.
func (b *Bot) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	return b.sendMsg(chatID, text, replyTo)
}

func (b *Bot) sendMsg(peerID, text, replyTo string) (string, error) {
	params := url.Values{
		"peer_id":   {peerID},
		"message":   {text},
		"random_id": {strconv.Itoa(rand.Intn(1e9))},
	}
	if replyTo != "" {
		params.Set("reply_to", replyTo)
	}
	raw, err := b.call("messages.send", params)
	if err != nil {
		return "", err
	}
	// Response is the message_id as integer.
	var msgID int
	if err := json.Unmarshal(raw, &msgID); err != nil {
		// Might be in new format with peer_ids
		return strings.Trim(string(raw), "\""), nil
	}
	return strconv.Itoa(msgID), nil
}

// EditMessage edits an existing message.
func (b *Bot) EditMessage(chatID, messageID, text, format string) error {
	_, err := b.call("messages.edit", url.Values{
		"peer_id":    {chatID},
		"message_id": {messageID},
		"message":    {text},
	})
	return err
}
