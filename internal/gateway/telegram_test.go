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
