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
