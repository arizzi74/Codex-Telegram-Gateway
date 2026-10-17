package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type fileWebhookAPI struct {
	mediaWebhookAPI
	fileDownloads []string
}

func (a *fileWebhookAPI) DownloadFile(_ context.Context, id string) (protocol.File, error) {
	a.fileDownloads = append(a.fileDownloads, id)
	return protocol.File{Name: "ignored API name", Data: a.data}, a.err
}

type filePreflightStore struct{ mediaPreflightStore }

func (s *filePreflightStore) PrepareTelegramFile(ctx context.Context, in registry.IncomingUpdate) (registry.IncomingUpdate, error) {
	return s.PrepareTelegramImage(ctx, in)
}

func TestWebhookDocumentsClassifyContentAndPreserveCaption(t *testing.T) {
	for _, tc := range []struct {
		name, fields, caption, action, text, filename, code string
		data                                                []byte
		image                                               bool
		err                                                 error
	}{
		{name: "generic file", fields: `"document":{"file_id":"doc","file_name":"report.pdf"}`, action: "text", filename: "report.pdf", data: []byte("%PDF-binary\x00data")},
		{name: "caption", fields: `"document":{"file_id":"doc","file_name":"résumé.csv"}`, caption: "Summarize this", action: "text", text: "Summarize this", filename: "résumé.csv", data: []byte("a,b\n1,2")},
		{name: "literal slash", fields: `"document":{"file_id":"doc","file_name":"data"}`, caption: "/model gpt-6", action: "text", text: "/model gpt-6", filename: "data", data: []byte("data")},
		{name: "explicit steer", fields: `"document":{"file_id":"doc","file_name":"data"}`, caption: "/tgsteer use file", action: "steer", text: "use file", filename: "data", data: []byte("data")},
		{name: "explicit steer alone", fields: `"document":{"file_id":"doc","file_name":"data"}`, caption: "/tgsteer", action: "steer", filename: "data", data: []byte("data")},
		{name: "missing filename", fields: `"document":{"file_id":"doc"}`, action: "text", filename: "file", data: []byte("data")},
		{name: "empty file", fields: `"document":{"file_id":"doc","file_name":"empty"}`, action: "text", filename: "empty"},
		{name: "safe filename", fields: `"document":{"file_id":"doc","file_name":"../../bad\\name\n.txt"}`, action: "text", filename: ".._.._bad_name_.txt", data: []byte("data")},
		{name: "fake image metadata", fields: `"document":{"file_id":"doc","file_name":"screenshot.png","mime_type":"image/png"}`, action: "text", filename: "screenshot.png", data: []byte("plain text")},
		{name: "actual image", fields: `"document":{"file_id":"doc","file_name":"data.bin","mime_type":"application/octet-stream"}`, action: "text", data: testTelegramImage(t), image: true},
		{name: "unsupported image format is file", fields: `"document":{"file_id":"doc","file_name":"icon.svg","mime_type":"image/svg+xml"}`, action: "text", filename: "icon.svg", data: []byte("<svg/>")},
		{name: "oversize metadata", fields: fmt.Sprintf(`"document":{"file_id":"doc","file_size":%d}`, protocol.MaxFileBytes+1), code: "file_too_large"},
		{name: "oversize downloaded", fields: `"document":{"file_id":"doc"}`, data: make([]byte, protocol.MaxFileBytes+1), code: "file_too_large"},
		{name: "failed download", fields: `"document":{"file_id":"doc"}`, err: errors.New("https://private-token/download"), code: "file_download_failed"},
		{name: "animation excluded", fields: `"document":{"file_id":"doc"},"animation":{"file_id":"doc"}`, code: "image_unsupported"},
		{name: "video excluded", fields: `"video":{"file_id":"doc"}`, code: "image_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &filePreflightStore{}
			api := &fileWebhookAPI{mediaWebhookAPI: mediaWebhookAPI{data: tc.data, err: tc.err}}
			h := NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}, Secrets: config.BotSecrets{BotName: "bot"}}, "secret", api, slog.New(slog.NewTextHandler(io.Discard, nil)))
			caption, _ := json.Marshal(tc.caption)
			body := `{"update_id":1,"message":{"from":{"id":7},"chat":{"id":7},"caption":` + string(caption) + `,` + tc.fields + `}}`
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || len(store.calls) != 1 {
				t.Fatalf("status=%d calls=%d", w.Code, len(store.calls))
			}
			in := store.calls[0]
			if tc.code != "" {
				if in.Action != "media_error" || in.MediaError != tc.code || len(in.Files) != 0 || len(in.Images) != 0 {
					t.Fatalf("missing safe feedback: %#v", in)
				}
				return
			}
			if in.Action != tc.action || in.Text != tc.text || len(api.fileDownloads) != 1 || api.fileDownloads[0] != "doc" {
				t.Fatalf("lost caption or download: %#v", in)
			}
			if tc.image {
				if len(in.Images) != 1 || len(in.Files) != 0 || !bytes.Equal(in.Images[0].Data, tc.data) {
					t.Fatal("image document lost vision behavior")
				}
			} else if len(in.Files) != 1 || len(in.Images) != 0 || in.Files[0].Name != tc.filename || !bytes.Equal(in.Files[0].Data, tc.data) {
				t.Fatalf("document bytes or name changed: %#v", in.Files)
			}
		})
	}
}

func TestWebhookFilePreflightAndACLPrecedeDownload(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		user, chat int64
		failure    error
		status     int
	}{
		{name: "worker upgrade", code: "file_worker_upgrade", user: 7, chat: 9, status: 200},
		{name: "owner rejected", code: "file_unauthorized", user: 7, chat: 9, status: 200},
		{name: "registry unavailable", failure: errors.New("unavailable"), user: 7, chat: 9, status: 503},
		{name: "user ACL", user: 8, chat: 9, status: 403},
		{name: "chat ACL", user: 7, chat: 8, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &filePreflightStore{mediaPreflightStore: mediaPreflightStore{code: tc.code, err: tc.failure}}
			api := &fileWebhookAPI{}
			h := NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}, AllowedChatIDs: []int64{9}}, "secret", api, slog.New(slog.NewTextHandler(io.Discard, nil)))
			body := fmt.Sprintf(`{"update_id":1,"message":{"from":{"id":%d},"chat":{"id":%d},"document":{"file_id":"doc"}}}`, tc.user, tc.chat)
			r := httptest.NewRequest("POST", "/", strings.NewReader(body))
			r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || len(api.fileDownloads) != 0 || len(api.files) != 0 {
				t.Fatalf("status=%d file downloads=%d image downloads=%d", w.Code, len(api.fileDownloads), len(api.files))
			}
		})
	}
}

func TestDownloadTelegramFileBoundsAndProtectsToken(t *testing.T) {
	for _, tc := range []struct {
		name, path, code string
		declared         int
		data             []byte
		status           int
	}{
		{name: "binary document", path: "documents/file.bin", data: []byte("\x00\xffbinary"), status: 200},
		{name: "empty document", path: "documents/empty", status: 200},
		{name: "exact limit", path: "documents/file.bin", data: make([]byte, protocol.MaxFileBytes), status: 200},
		{name: "metadata too large", path: "documents/file.bin", declared: protocol.MaxFileBytes + 1, code: "file_too_large", status: 200},
		{name: "actual too large", path: "documents/file.bin", data: make([]byte, protocol.MaxFileBytes+1), code: "file_too_large", status: 200},
		{name: "unsafe path", path: "../secret-token", code: "file_download_failed", status: 200},
		{name: "redirect", path: "documents/file.bin", code: "file_download_failed", status: 302},
		{name: "download error", path: "documents/file.bin", code: "file_download_failed", status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/botsecret-token/getFile" {
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": tc.path, "file_size": tc.declared}})
					return
				}
				downloads++
				if tc.status == 302 {
					w.Header().Set("Location", "https://example.invalid/private-token")
				}
				w.WriteHeader(tc.status)
				if tc.name == "actual too large" {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(tc.data)
			}))
			defer server.Close()
			client := NewTelegramClient("secret-token")
			client.endpoint = server.URL
			file, err := client.DownloadFile(t.Context(), "id")
			if tc.code == "" {
				if err != nil || !bytes.Equal(file.Data, tc.data) {
					t.Fatalf("file changed: %v", err)
				}
			} else if err == nil || err.Error() != tc.code || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("unsafe or incorrect error: %v", err)
			}
			if downloads > 1 {
				t.Fatal("download followed redirect")
			}
		})
	}
}
