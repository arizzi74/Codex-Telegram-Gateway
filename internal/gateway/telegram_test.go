package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestTelegramClientPayloadAndRateLimit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			var body SendMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.ChatID != 123 || body.TopicID != 9 || body.Text != "hello" {
				t.Error("wrong Telegram target")
			}
			w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
		} else {
			w.WriteHeader(429)
			w.Write([]byte(`{"ok":false,"error_code":429,"description":"slow down","parameters":{"retry_after":3}}`))
		}
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint = server.URL
	client.http = server.Client()
	id, err := client.Send(context.Background(), SendMessage{ChatID: 123, TopicID: 9, Text: "hello"})
	if err != nil || id != 77 {
		t.Fatal(id, err)
	}
	err = client.AnswerCallback(context.Background(), "callback", "ok")
	apiErr, ok := err.(*TelegramError)
	if !ok || apiErr.RetryAfter.Seconds() != 3 {
		t.Fatal("missing retry_after", err)
	}
}

func TestTelegramClientSendsForceReplyMarkup(t *testing.T) {
	placeholder := strings.Repeat("🧪", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botfake-token/sendMessage" {
			t.Errorf("unexpected API method: %s", r.URL.Path)
		}
		var body struct {
			ChatID   int64                      `json:"chat_id"`
			Text     string                     `json:"text"`
			Keyboard map[string]json.RawMessage `json:"reply_markup"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.ChatID != 123 || body.Text != "Type your answer and send it." {
			t.Errorf("unexpected message: %+v", body)
		}
		if len(body.Keyboard) != 2 || string(body.Keyboard["force_reply"]) != "true" {
			t.Errorf("invalid ForceReply markup: %v", body.Keyboard)
		}
		var gotPlaceholder string
		if err := json.Unmarshal(body.Keyboard["input_field_placeholder"], &gotPlaceholder); err != nil || gotPlaceholder != placeholder {
			t.Errorf("placeholder = %q, err = %v", gotPlaceholder, err)
		}
		if _, present := body.Keyboard["inline_keyboard"]; present {
			t.Error("standalone ForceReply must not include inline_keyboard")
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint, client.http = server.URL, server.Client()
	id, err := client.Send(context.Background(), SendMessage{
		ChatID: 123,
		Text:   "Type your answer and send it.",
		Keyboard: &TelegramKeyboard{
			ForceReply:            true,
			InputFieldPlaceholder: placeholder,
		},
	})
	if err != nil || id != 77 {
		t.Fatalf("send: id=%d err=%v", id, err)
	}
}

func TestTelegramClientPreservesInlineKeyboardPayload(t *testing.T) {
	const wantMarkup = `{"inline_keyboard":[[{"text":"Reply with text","callback_data":"text-token"}]]}`
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, strings.TrimPrefix(r.URL.Path, "/botfake-token/"))
		var body struct {
			Keyboard json.RawMessage `json:"reply_markup"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body.Keyboard) != wantMarkup {
			t.Errorf("reply_markup = %s, want %s", body.Keyboard, wantMarkup)
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint, client.http = server.URL, server.Client()
	message := SendMessage{
		ChatID:   123,
		Text:     "Question",
		Keyboard: &TelegramKeyboard{Rows: [][]TelegramButton{{{Text: "Reply with text", Data: "text-token"}}}},
	}
	ctx := context.Background()
	if _, err := client.Send(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := client.Edit(ctx, message.ChatID, 77, message.Text, message.Keyboard); err != nil {
		t.Fatal(err)
	}
	if err := client.EditFormatted(ctx, 77, message); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "sendMessage,editMessageText,editMessageText" {
		t.Fatalf("unexpected Telegram methods: %v", methods)
	}
}

func TestTelegramClientRejectsInvalidReplyMarkupBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid reply markup reached Telegram transport")
		w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint, client.http = server.URL, server.Client()
	for _, test := range []struct {
		name     string
		keyboard TelegramKeyboard
	}{
		{"combined markup", TelegramKeyboard{ForceReply: true, Rows: [][]TelegramButton{{{Text: "Button", Data: "token"}}}}},
		{"placeholder without force reply", TelegramKeyboard{InputFieldPlaceholder: "Answer"}},
		{"long placeholder", TelegramKeyboard{ForceReply: true, InputFieldPlaceholder: strings.Repeat("🧪", 65)}},
		{"invalid UTF-8 placeholder", TelegramKeyboard{ForceReply: true, InputFieldPlaceholder: "Answer\xff"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			id, err := client.Send(context.Background(), SendMessage{ChatID: 123, Text: "Question", Keyboard: &test.keyboard})
			if err == nil || id != 0 {
				t.Fatalf("invalid markup accepted: id=%d err=%v", id, err)
			}
		})
	}
	keyboard := &TelegramKeyboard{ForceReply: true, InputFieldPlaceholder: "Answer"}
	if err := client.Edit(context.Background(), 123, 77, "Question", keyboard); err == nil {
		t.Error("Edit accepted ForceReply")
	}
	if err := client.EditFormatted(context.Background(), 77, SendMessage{ChatID: 123, Text: "Question", Keyboard: keyboard}); err == nil {
		t.Error("EditFormatted accepted ForceReply")
	}
}

func TestTelegramClientChatActionAndMenuMethods(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/botfake-token/")
		methods = append(methods, method)
		switch method {
		case "sendChatAction":
			var body ChatAction
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body != (ChatAction{ChatID: 123, TopicID: 9, Action: "typing"}) {
				t.Errorf("chat action = %#v", body)
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		case "setMyCommands":
			var body struct {
				Commands []BotCommand `json:"commands"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Commands) != 1 || body.Commands[0] != (BotCommand{Command: "tghelp", Description: "Gateway help"}) {
				t.Errorf("commands payload = %#v", body.Commands)
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		case "getMyCommands":
			w.Write([]byte(`{"ok":true,"result":[{"command":"tghelp","description":"Gateway help"}]}`))
		case "setChatMenuButton":
			var body struct {
				ChatID     int64      `json:"chat_id"`
				MenuButton MenuButton `json:"menu_button"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.ChatID != 0 || body.MenuButton.Type != "commands" {
				t.Errorf("menu payload = %#v", body)
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		case "getChatMenuButton":
			w.Write([]byte(`{"ok":true,"result":{"type":"commands"}}`))
		default:
			t.Errorf("unexpected method %q", method)
		}
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint = server.URL
	client.http = server.Client()
	ctx := context.Background()
	if err := client.SendChatAction(ctx, ChatAction{ChatID: 123, TopicID: 9, Action: "typing"}); err != nil {
		t.Fatal(err)
	}
	wantCommands := []BotCommand{{Command: "tghelp", Description: "Gateway help"}}
	if err := client.SetMyCommands(ctx, wantCommands); err != nil {
		t.Fatal(err)
	}
	commands, err := client.GetMyCommands(ctx)
	if err != nil || len(commands) != 1 || commands[0] != wantCommands[0] {
		t.Fatalf("get commands = %#v, %v", commands, err)
	}
	if err := client.SetChatMenuButton(ctx, 0, MenuButton{Type: "commands"}); err != nil {
		t.Fatal(err)
	}
	button, err := client.GetChatMenuButton(ctx, 0)
	if err != nil || button.Type != "commands" {
		t.Fatalf("get menu button = %#v, %v", button, err)
	}
	wantMethods := []string{"sendChatAction", "setMyCommands", "getMyCommands", "setChatMenuButton", "getChatMenuButton"}
	if strings.Join(methods, ",") != strings.Join(wantMethods, ",") {
		t.Fatalf("methods = %v, want %v", methods, wantMethods)
	}
}

func TestSplitTextPreservesUnicode(t *testing.T) {
	text := strings.Repeat("a👩🏽‍💻\n", 1500)
	chunks := SplitText(text, 4000)
	if strings.Join(chunks, "") != text {
		t.Fatal("text lost")
	}
	for _, chunk := range chunks {
		if len(utf16.Encode([]rune(chunk))) > 4000 {
			t.Fatal("oversized Telegram text")
		}
	}
}

func TestTelegramClientDeletesOnlyTheRequestedMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botfake-token/deleteMessage" {
			t.Errorf("unexpected API method: %s", r.URL.Path)
		}
		var body map[string]int64
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 2 || body["chat_id"] != -123 || body["message_id"] != 456 {
			t.Errorf("wrong deletion target: %v", body)
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint = server.URL
	client.http = server.Client()
	if err := client.DeleteMessage(context.Background(), -123, 456); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramClientSendsAndEditsLiteralMonospaceText(t *testing.T) {
	text := "printf '<b>🧪 & `literal`</b>'"
	entity := TelegramEntity{Type: "pre", Offset: 0, Length: len(utf16.Encode([]rune(text)))}
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/botfake-token/")
		methods = append(methods, method)
		var body struct {
			ChatID    int64            `json:"chat_id"`
			MessageID int64            `json:"message_id"`
			Text      string           `json:"text"`
			Entities  []TelegramEntity `json:"entities"`
			ParseMode string           `json:"parse_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.ChatID != -123 || body.Text != text || body.ParseMode != "" || len(body.Entities) != 1 || body.Entities[0] != entity {
			t.Errorf("invalid formatted message payload: %+v", body)
		}
		if method == "editMessageText" && body.MessageID != 77 {
			t.Errorf("wrong edit identity: %d", body.MessageID)
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint, client.http = server.URL, server.Client()
	message := SendMessage{ChatID: -123, Text: text, Entities: []TelegramEntity{entity}}
	id, err := client.Send(context.Background(), message)
	if err != nil || id != 77 {
		t.Fatalf("send: id=%d err=%v", id, err)
	}
	if err := client.EditFormatted(context.Background(), id, message); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "sendMessage,editMessageText" {
		t.Fatalf("unexpected Telegram methods: %v", methods)
	}
}
