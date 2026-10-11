package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendVoiceMultipart(t *testing.T) {
	want := []byte("OggSsynthetic")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/bottest-token/sendVoice" {
			t.Error("wrong voice endpoint")
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		fields := map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(part)
			if part.FormName() == "voice" {
				if !bytes.Equal(data, want) || part.FileName() != "reply.ogg" || part.Header.Get("Content-Type") != "audio/ogg" {
					t.Error("invalid audio attachment")
				}
			} else {
				fields[part.FormName()] = string(data)
			}
		}
		if fields["chat_id"] != "7" || fields["message_thread_id"] != "11" || fields["duration"] != "3" {
			t.Errorf("lost destination or duration: %+v", fields)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	}))
	defer server.Close()
	api := NewTelegramClient("test-token")
	api.endpoint = server.URL
	if id, err := api.SendVoice(context.Background(), 7, 11, want, 3); err != nil || id != 42 {
		t.Fatalf("send: %d %v", id, err)
	}
}

func TestSendVoiceErrorAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redirect {
				w.Header().Set("Location", "https://example.invalid/never-follow")
				w.WriteHeader(302)
				return
			}
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"test-token private","parameters":{"retry_after":8}}`)
		}))
		api := NewTelegramClient("test-token")
		api.endpoint = server.URL
		_, err := api.SendVoice(context.Background(), 7, 0, []byte("OggSsynthetic"), 0)
		server.Close()
		if err == nil || strings.Contains(err.Error(), "test-token") {
			t.Fatalf("unsafe voice error: %v", err)
		}
		if !redirect {
			var telegramErr *TelegramError
			if !errors.As(err, &telegramErr) || telegramErr.RetryAfter != 8*time.Second {
				t.Fatalf("lost retry delay: %v", err)
			}
		}
	}
}
