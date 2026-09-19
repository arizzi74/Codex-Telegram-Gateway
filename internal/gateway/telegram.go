package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/telegramcommands"
)

type TelegramUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}
type TelegramChat struct {
	ID int64 `json:"id"`
}
type TelegramMessage struct {
	ID        int64             `json:"message_id"`
	From      *TelegramUser     `json:"from"`
	Chat      TelegramChat      `json:"chat"`
	TopicID   int64             `json:"message_thread_id"`
	Text      string            `json:"text"`
	Caption   string            `json:"caption"`
	Photo     []TelegramPhoto   `json:"photo"`
	Document  *TelegramDocument `json:"document"`
	Animation json.RawMessage   `json:"animation"`
	Video     json.RawMessage   `json:"video"`
	Audio     json.RawMessage   `json:"audio"`
	Voice     json.RawMessage   `json:"voice"`
	VideoNote json.RawMessage   `json:"video_note"`
	Sticker   json.RawMessage   `json:"sticker"`
	ReplyTo   *TelegramMessage  `json:"reply_to_message"`
}
type TelegramPhoto struct {
	FileID   string `json:"file_id"`
	Width    int64  `json:"width"`
	Height   int64  `json:"height"`
	FileSize int64  `json:"file_size"`
}
type TelegramDocument struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
}
type TelegramCallback struct {
	ID      string           `json:"id"`
	From    TelegramUser     `json:"from"`
	Message *TelegramMessage `json:"message"`
	Data    string           `json:"data"`
}
type TelegramUpdate struct {
	ID       int64             `json:"update_id"`
	Message  *TelegramMessage  `json:"message"`
	Callback *TelegramCallback `json:"callback_query"`
}
type TelegramButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type TelegramKeyboard struct {
	Rows [][]TelegramButton `json:"inline_keyboard"`
}
type SendMessage struct {
	ChatID              int64             `json:"chat_id"`
	TopicID             int64             `json:"message_thread_id,omitempty"`
	Text                string            `json:"text"`
	Entities            []TelegramEntity  `json:"entities,omitempty"`
	Keyboard            *TelegramKeyboard `json:"reply_markup,omitempty"`
	DisableNotification bool              `json:"disable_notification,omitempty"`
}

// TelegramEntity offsets and lengths are measured in UTF-16 code units.
type TelegramEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}
type ChatAction struct {
	ChatID  int64  `json:"chat_id"`
	TopicID int64  `json:"message_thread_id,omitempty"`
	Action  string `json:"action"`
}

type BotCommand = telegramcommands.Command

type MenuButton struct {
	Type string `json:"type"`
}

type TelegramAPI interface {
	Send(context.Context, SendMessage) (int64, error)
	Edit(context.Context, int64, int64, string, *TelegramKeyboard) error
	AnswerCallback(context.Context, string, string) error
}
type TelegramClient struct {
	token, endpoint string
	http            *http.Client
}
type TelegramError struct {
	Code        int
	RetryAfter  time.Duration
	Description string
}

func (e *TelegramError) Error() string {
	return fmt.Sprintf("Telegram API error %d: %s", e.Code, e.Description)
}

func NewTelegramClient(token string) *TelegramClient {
	return &TelegramClient{token: token, endpoint: "https://api.telegram.org", http: &http.Client{Timeout: 15 * time.Second}}
}

func (t *TelegramClient) call(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+"/bot"+t.token+"/"+method, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Telegram endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := t.http.Do(req)
	if err != nil {
		return errors.New("Telegram request could not be completed")
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&envelope); err != nil {
		return errors.New("invalid Telegram response")
	}
	if !envelope.OK {
		return &TelegramError{Code: envelope.Code, Description: strings.ReplaceAll(envelope.Description, t.token, "[REDACTED]"), RetryAfter: time.Duration(envelope.Parameters.RetryAfter) * time.Second}
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}
func (t *TelegramClient) Send(ctx context.Context, message SendMessage) (int64, error) {
	var reply struct {
		ID int64 `json:"message_id"`
	}
	err := t.call(ctx, "sendMessage", message, &reply)
	return reply.ID, err
}
func (t *TelegramClient) Edit(ctx context.Context, chatID, messageID int64, text string, keyboard *TelegramKeyboard) error {
	payload := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	return t.call(ctx, "editMessageText", payload, nil)
}

func (t *TelegramClient) EditFormatted(ctx context.Context, messageID int64, message SendMessage) error {
	return t.call(ctx, "editMessageText", struct {
		ChatID    int64             `json:"chat_id"`
		MessageID int64             `json:"message_id"`
		Text      string            `json:"text"`
		Entities  []TelegramEntity  `json:"entities"`
		Keyboard  *TelegramKeyboard `json:"reply_markup,omitempty"`
	}{message.ChatID, messageID, message.Text, message.Entities, message.Keyboard}, nil)
}

func (t *TelegramClient) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	return t.call(ctx, "deleteMessage", struct {
		ChatID    int64 `json:"chat_id"`
		MessageID int64 `json:"message_id"`
	}{ChatID: chatID, MessageID: messageID}, nil)
}

func (t *TelegramClient) AnswerCallback(ctx context.Context, id, text string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

func (t *TelegramClient) SendChatAction(ctx context.Context, action ChatAction) error {
	return t.call(ctx, "sendChatAction", action, nil)
}

func (t *TelegramClient) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	if commands == nil {
		commands = []BotCommand{}
	}
	return t.call(ctx, "setMyCommands", struct {
		Commands []BotCommand `json:"commands"`
	}{Commands: commands}, nil)
}

func (t *TelegramClient) GetMyCommands(ctx context.Context) ([]BotCommand, error) {
	var commands []BotCommand
	err := t.call(ctx, "getMyCommands", struct{}{}, &commands)
	return commands, err
}

// BotCommandScope restricts session aliases to the authenticated conversation.
// Private chats use chat; group members use chat_member so menus stay personal.
type BotCommandScope struct {
	Type   string `json:"type"`
	ChatID int64  `json:"chat_id"`
	UserID int64  `json:"user_id,omitempty"`
}

func (t *TelegramClient) SetScopedCommands(ctx context.Context, scope BotCommandScope, commands []BotCommand) error {
	if commands == nil {
		commands = []BotCommand{}
	}
	return t.call(ctx, "setMyCommands", struct {
		Commands []BotCommand    `json:"commands"`
		Scope    BotCommandScope `json:"scope"`
	}{commands, scope}, nil)
}

func (t *TelegramClient) DeleteScopedCommands(ctx context.Context, scope BotCommandScope) error {
	return t.call(ctx, "deleteMyCommands", struct {
		Scope BotCommandScope `json:"scope"`
	}{scope}, nil)
}

// SetChatMenuButton changes the default menu when chatID is zero, or the menu
// for one private chat otherwise.
func (t *TelegramClient) SetChatMenuButton(ctx context.Context, chatID int64, button MenuButton) error {
	payload := struct {
		ChatID     int64      `json:"chat_id,omitempty"`
		MenuButton MenuButton `json:"menu_button"`
	}{ChatID: chatID, MenuButton: button}
	return t.call(ctx, "setChatMenuButton", payload, nil)
}

func (t *TelegramClient) GetChatMenuButton(ctx context.Context, chatID int64) (MenuButton, error) {
	var button MenuButton
	payload := struct {
		ChatID int64 `json:"chat_id,omitempty"`
	}{ChatID: chatID}
	err := t.call(ctx, "getChatMenuButton", payload, &button)
	return button, err
}

type WebhookInfo struct {
	URL            string `json:"url"`
	PendingUpdates int    `json:"pending_update_count"`
	LastError      string `json:"last_error_message"`
	LastErrorDate  int64  `json:"last_error_date"`
}

func (t *TelegramClient) GetWebhook(ctx context.Context) (WebhookInfo, error) {
	var result WebhookInfo
	err := t.call(ctx, "getWebhookInfo", struct{}{}, &result)
	return result, err
}
func (t *TelegramClient) SetWebhook(ctx context.Context, url, secret string) error {
	return t.call(ctx, "setWebhook", map[string]any{"url": url, "secret_token": secret, "allowed_updates": []string{"message", "callback_query"}, "drop_pending_updates": false}, nil)
}
func (t *TelegramClient) Identity(ctx context.Context) (TelegramUser, error) {
	var result TelegramUser
	err := t.call(ctx, "getMe", struct{}{}, &result)
	return result, err
}

// SplitText uses UTF-16 code units, conservatively respecting Telegram's text
// limit even for astral Unicode characters. Plain text needs no markup escaping.
func SplitText(text string, limit int) []string {
	if limit < 2 {
		limit = 4000
	}
	var chunks []string
	var b strings.Builder
	units := 0
	for _, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			chunks = append(chunks, b.String())
			b.Reset()
			units = 0
		}
		b.WriteRune(r)
		units += width
	}
	if b.Len() > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}
