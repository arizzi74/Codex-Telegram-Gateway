package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func testTelegramImage(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type mediaWebhookAPI struct {
	TelegramAPI
	files []string
	data  []byte
	err   error
}

func (a *mediaWebhookAPI) DownloadImage(_ context.Context, fileID string) (protocol.Image, error) {
	a.files = append(a.files, fileID)
	return protocol.Image{MIMEType: "image/png", Data: a.data}, a.err
}

func TestWebhookImagesAndCaptions(t *testing.T) {
	for _, tc := range []struct {
		name, fields, caption, file, code string
	}{
		{"photo", `"photo":[{"file_id":"small","width":20,"height":20},{"file_id":"big","width":200,"height":200}]`, "", "big", ""},
		{"caption", `"photo":[{"file_id":"photo","width":200,"height":200}],"caption":"What is this?"`, "What is this?", "photo", ""},
		{"slash caption", `"photo":[{"file_id":"photo","width":200,"height":200}],"caption":"/status"`, "/status", "photo", ""},
		{"document", `"document":{"file_id":"original"},"caption":"Screenshot"`, "Screenshot", "original", ""},
		{"large photo fallback", fmt.Sprintf(`"photo":[{"file_id":"fit","width":100,"height":100},{"file_id":"large","width":200,"height":200,"file_size":%d}]`, protocol.MaxImageBytes+1), "", "fit", ""},
		{"too large", fmt.Sprintf(`"document":{"file_id":"large","file_size":%d}`, protocol.MaxImageBytes+1), "", "", "image_too_large"},
		{"voice", `"voice":{"file_id":"voice"}`, "", "", "image_unsupported"},
		{"animation", `"animation":{"file_id":"animation"},"document":{"file_id":"animation"}`, "", "", "image_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &webhookStore{}
			api := &mediaWebhookAPI{data: testTelegramImage(t)}
			h := NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}}, "secret", api, slog.New(slog.NewTextHandler(io.Discard, nil)))
			body := `{"update_id":1,"message":{"message_id":1,"from":{"id":7},"chat":{"id":7},"message_thread_id":11,"reply_to_message":{"message_id":22},` + tc.fields + `}}`
			r := httptest.NewRequest("POST", "/", strings.NewReader(body))
			r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || len(store.calls) != 1 {
				t.Fatalf("code=%d calls=%d", w.Code, len(store.calls))
			}
			in := store.calls[0]
			if in.TopicID != 11 || in.ReplyToMessageID != 22 || in.Text != tc.caption || in.MediaError != tc.code {
				t.Fatalf("unexpected routing or media result: %+v", in)
			}
			if tc.code == "" {
				if in.Action != "text" || len(in.Images) != 1 || !bytes.Equal(in.Images[0].Data, api.data) || len(api.files) != 1 || api.files[0] != tc.file {
					t.Fatal("image/caption was not forwarded intact")
				}
			} else if in.Action != "media_error" || len(in.Images) != 0 || len(api.files) != 0 {
				t.Fatal("unsupported media must produce durable feedback without download")
			}
		})
	}
}

func TestWebhookAuthorizesBeforeImageDownload(t *testing.T) {
	store := &webhookStore{}
	api := &mediaWebhookAPI{data: testTelegramImage(t)}
	h := NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}, AllowedChatIDs: []int64{9}}, "secret", api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		user, chat int
		secret     string
	}{{8, 9, "secret"}, {7, 8, "secret"}, {7, 9, "wrong"}} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(fmt.Sprintf(`{"update_id":1,"message":{"from":{"id":%d},"chat":{"id":%d},"document":{"file_id":"image"}}}`, tc.user, tc.chat)))
		r.Header.Set("X-Telegram-Bot-Api-Secret-Token", tc.secret)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 && w.Code != 403 {
			t.Fatalf("status=%d", w.Code)
		}
	}
	if len(api.files) != 0 || len(store.calls) != 0 {
		t.Fatal("unauthorized media was processed")
	}
}

func TestWebhookImageFailureProducesFeedback(t *testing.T) {
	store := &webhookStore{}
	api := &mediaWebhookAPI{err: fmt.Errorf("sensitive download URL must not escape")}
	h := NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}}, "secret", api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"update_id":1,"message":{"from":{"id":7},"chat":{"id":7},"document":{"file_id":"image"}}}`))
	r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if len(store.calls) != 1 || store.calls[0].Action != "media_error" || store.calls[0].MediaError != "image_download_failed" {
		t.Fatal("download failure was silently discarded")
	}
}

type mediaPreflightStore struct {
	webhookStore
	code string
	err  error
}

func (s *mediaPreflightStore) PrepareTelegramImage(_ context.Context, in registry.IncomingUpdate) (registry.IncomingUpdate, error) {
	in.MediaError = s.code
	return in, s.err
}

func TestWebhookImagePreflightFailureSkipsDownload(t *testing.T) {
	for _, tc := range []struct {
		code          string
		err           error
		status, calls int
	}{
		{"target_unavailable", nil, 200, 1},
		{"image_worker_upgrade", nil, 200, 1},
		{"", fmt.Errorf("registry unavailable"), 503, 0},
	} {
		store := &mediaPreflightStore{code: tc.code, err: tc.err}
		api := &mediaWebhookAPI{}
		h := NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}}, "secret", api, slog.New(slog.NewTextHandler(io.Discard, nil)))
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"update_id":1,"message":{"from":{"id":7},"chat":{"id":7},"document":{"file_id":"image"}}}`))
		r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || len(store.calls) != tc.calls || len(api.files) != 0 {
			t.Fatal("invalid image target reached download")
		}
		if tc.calls > 0 && (store.calls[0].MediaError != tc.code || store.calls[0].Action != "media_error") {
			t.Fatal("missing durable preflight feedback")
		}
	}
}

func TestDownloadTelegramImage(t *testing.T) {
	data := testTelegramImage(t)
	for _, tc := range []struct {
		name, path, code string
		declared         int
		content          []byte
		status           int
	}{
		{"success", "photos/file_1.png", "", len(data), data, 200},
		{"bad file", "documents/file.txt", "image_unsupported", 4, []byte("text"), 200},
		{"declared oversize", "photos/file.png", "image_too_large", protocol.MaxImageBytes + 1, nil, 200},
		{"actual oversize", "photos/file.png", "image_too_large", 0, bytes.Repeat([]byte{'x'}, protocol.MaxImageBytes+1), 200},
		{"traversal", "../secret", "image_download_failed", 0, nil, 200},
		{"absolute URL", "https://example.invalid/file", "image_download_failed", 0, nil, 200},
		{"expired", "photos/file.png", "image_download_failed", 0, nil, 404},
		{"redirect", "photos/file.png", "image_download_failed", 0, nil, 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/botsecret/getFile" {
					var in map[string]string
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["file_id"] != "chosen" {
						t.Error("wrong getFile request")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": tc.path, "file_size": tc.declared}})
					return
				}
				downloads++
				if r.URL.Path != "/file/botsecret/"+tc.path {
					t.Error("unexpected download path")
				}
				if tc.status == 302 {
					w.Header().Set("Location", "/must-not-follow")
				}
				w.WriteHeader(tc.status)
				if tc.name == "actual oversize" {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(tc.content)
			}))
			defer server.Close()
			client := NewTelegramClient("secret")
			client.endpoint = server.URL
			img, err := client.DownloadImage(context.Background(), "chosen")
			if tc.code == "" {
				if err != nil || img.MIMEType != "image/png" || !bytes.Equal(img.Data, data) {
					t.Fatalf("download failed: %v", err)
				}
			} else if err == nil || err.Error() != tc.code {
				t.Fatalf("error=%v want=%s", err, tc.code)
			}
			if downloads > 1 {
				t.Fatal("followed redirect")
			}
		})
	}
}

func TestTelegramImageErrorDoesNotExposeToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"bad /botsecret-token/ URL"}`)
	}))
	defer server.Close()
	client := NewTelegramClient("secret-token")
	client.endpoint = server.URL
	_, err := client.DownloadImage(context.Background(), "image")
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("unsafe error: %v", err)
	}
}
