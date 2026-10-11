package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/voice"
)

type voiceWebhookStore struct {
	webhookStore
	recordings []registry.TelegramVoice
	incoming   []registry.IncomingUpdate
}

func (s *voiceWebhookStore) AcceptTelegramVoice(_ context.Context, in registry.IncomingUpdate, v registry.TelegramVoice) (registry.AcceptResult, error) {
	s.recordings = append(s.recordings, v)
	s.incoming = append(s.incoming, in)
	return registry.AcceptResult{View: "voice_transcribing"}, nil
}

func TestVoiceWebhookQueuesWithoutDownloading(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enabled  bool
		actor    int
		metadata string
		code     string
		http     int
	}{
		{"allowed", true, 7, `{"file_id":"recording","duration":4}`, "", 200},
		{"unauthorized", true, 8, `{"file_id":"recording","duration":4}`, "", 403},
		{"disabled", false, 7, `{"file_id":"recording","duration":4}`, "voice_disabled", 200},
		{"large", true, 7, fmt.Sprintf(`{"file_id":"recording","file_size":%d}`, voice.MaxInputBytes+1), "voice_too_large", 200},
		{"long", true, 7, `{"file_id":"recording","duration":301}`, "voice_too_long", 200},
		{"malformed", true, 7, `{"file_id":7}`, "voice_invalid", 200},
		{"empty id", true, 7, `{"duration":1}`, "voice_invalid", 200},
		{"negative duration", true, 7, `{"file_id":"recording","duration":-1}`, "voice_invalid", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &voiceWebhookStore{}
			cfg := config.GatewayConfig{AllowedUserIDs: []int64{7}, Voice: config.VoiceConfig{Enabled: tc.enabled}}
			// A nil API proves that durable acceptance never downloads or sends.
			h := NewWebhook(store, cfg, "secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			body := fmt.Sprintf(`{"update_id":42,"message":{"from":{"id":%d},"chat":{"id":7},"message_thread_id":11,"reply_to_message":{"message_id":22},"voice":%s}}`, tc.actor, tc.metadata)
			r := httptest.NewRequest("POST", "/", strings.NewReader(body))
			r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.http {
				t.Fatalf("status=%d", w.Code)
			}
			if tc.http != 200 {
				if len(store.calls)+len(store.recordings) != 0 {
					t.Fatal("unauthorized update reached registry")
				}
				return
			}
			if tc.code != "" {
				if len(store.calls) != 1 || store.calls[0].MediaError != tc.code || len(store.recordings) != 0 {
					t.Fatalf("invalid media response: %+v", store)
				}
			} else if len(store.recordings) != 1 || store.incoming[0].TopicID != 11 || store.incoming[0].ReplyToMessageID != 22 || store.incoming[0].UpdateID != 42 {
				t.Fatalf("queue lost routing: %+v", store)
			}
		})
	}
}

func TestDownloadVoiceChecksBoundariesAndNeverLeaksToken(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		declared   int64
		length     int
		status     int
		want       string
	}{
		{"valid", "voice/file.oga", 3, 3, 200, ""},
		{"unknown size", "voice/file.oga", 0, 3, 200, ""},
		{"truncated body", "voice/file.oga", 4, 3, 200, "voice_download_failed"},
		{"body larger than metadata", "voice/file.oga", 2, 3, 200, "voice_download_failed"},
		{"negative size", "voice/file.oga", -1, 0, 200, "voice_download_failed"},
		{"oversize metadata", "voice/file.oga", voice.MaxInputBytes + 1, 0, 200, "voice_too_large"},
		{"oversize body", "voice/file.oga", 0, voice.MaxInputBytes + 1, 200, "voice_too_large"},
		{"traversal", "../private", 0, 0, 200, "voice_download_failed"},
		{"redirect", "voice/file.oga", 0, 0, 302, "voice_download_failed"},
		{"failure", "voice/file.oga", 0, 0, 500, "voice_download_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if strings.HasSuffix(r.URL.Path, "/getFile") {
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": tc.path, "file_size": tc.declared}})
					return
				}
				if tc.status == 302 {
					w.Header().Set("Location", "https://example.invalid/should-never-follow")
				}
				w.WriteHeader(tc.status)
				if tc.length > 0 {
					_, _ = io.Copy(w, io.LimitReader(&zeroReader{}, int64(tc.length)))
				}
			}))
			defer server.Close()
			api := NewTelegramClient("private-test-token")
			api.endpoint = server.URL
			data, err := api.DownloadVoice(context.Background(), "recording")
			if tc.want == "" {
				if err != nil || !bytes.Equal(data, make([]byte, tc.length)) {
					t.Fatalf("download len=%d err=%v", len(data), err)
				}
			} else if err == nil || err.Error() != tc.want || strings.Contains(err.Error(), "private-test-token") {
				t.Fatalf("error=%v", err)
			}
			if (tc.name == "oversize metadata" || tc.name == "negative size" || tc.name == "traversal") && requests != 1 {
				t.Fatal("invalid file was downloaded")
			}
		})
	}
}

type zeroReader struct{}

func (*zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
