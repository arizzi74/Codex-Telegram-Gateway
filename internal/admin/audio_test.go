package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestAudioHTTPAuthenticationAndInputBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := registry.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	token := webuiTestLogin(t, store)
	manager, err := audio.New(filepath.Join(filepath.Dir(path), "audio"), config.VoiceConfig{Enabled: true, ModelPath: "/usr/local/share/codex-whisper/ggml-small.bin", Threads: 3})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(store, Config{Origin: passkeyTestOrigin, Audio: manager})
	if err != nil {
		t.Fatal(err)
	}
	const body = `{"enabled":false,"model":"small","threads":3,"reply_with_audio":false,"tts_voice":"auto"}`
	for _, endpoint := range []string{"/tgw/api/v1/admin/audio", "/tgw/api/v1/admin/audio/settings"} {
		method := http.MethodGet
		if strings.HasSuffix(endpoint, "/settings") {
			method = http.MethodPost
		}
		got := webUICommandRequestForTest(server, method, endpoint, body, "", passkeyTestOrigin, "csrf", "csrf")
		if got.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated audio access: %d %s", got.Code, got.Body.String())
		}
	}
	for _, tc := range []struct{ origin, header, cookie string }{{"https://evil.example", "csrf", "csrf"}, {"", "csrf", "csrf"}, {passkeyTestOrigin, "wrong", "csrf"}, {passkeyTestOrigin, "", ""}} {
		got := webUICommandRequestForTest(server, http.MethodPost, "/tgw/api/v1/admin/audio/settings", body, token, tc.origin, tc.header, tc.cookie)
		if got.Code != http.StatusForbidden {
			t.Fatalf("unsafe audio write accepted: %d", got.Code)
		}
	}
	read := webUICommandRequestForTest(server, http.MethodGet, "/tgw/api/v1/admin/audio", "", token, "", "", "")
	var status audio.Status
	if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &status) != nil || status.Settings.Model != "small" || status.Settings.Threads != 3 {
		t.Fatalf("existing settings lost: %d %s", read.Code, read.Body.String())
	}
	if strings.Contains(read.Body.String(), "/usr/local/") || strings.Contains(read.Body.String(), "huggingface.co") {
		t.Fatal("audio status exposed internal paths or URLs")
	}
	for _, invalid := range []string{`{"model":"tiny","threads":2}`, `{"model":"base","threads":33}`, `{"model":"base","threads":0}`, `{"model":"base","threads":2,"tts_engine":"pocket"}`, `{"model":"base","threads":2,"tts_engine":"../qwen3"}`, `{"model":"base","threads":2,"url":"https://evil.example/model"}`, `{"model":"base","threads":2,"helper_path":"/bin/evil"}`, `{"model":"base","threads":2} {}`, `{"model":"base","threads":2,"tts_voice":"` + strings.Repeat("x", 5000) + `"}`} {
		got := webUICommandRequestForTest(server, http.MethodPost, "/tgw/api/v1/admin/audio/settings", invalid, token, passkeyTestOrigin, "csrf", "csrf")
		if got.Code != 400 {
			t.Fatalf("invalid audio settings accepted: %d %s", got.Code, got.Body.String())
		}
	}
	// A sensitive change follows passwordSettings' five-minute policy for both
	// supported authentication methods; recent auth is checked before admission.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	stale := webUICommandRequestForTest(server, http.MethodPost, "/tgw/api/v1/admin/audio/settings", body, token, passkeyTestOrigin, "csrf", "csrf")
	if stale.Code != 403 || !strings.Contains(stale.Body.String(), `"reauthentication_required"`) {
		t.Fatalf("stale session changed audio settings: %d %s", stale.Code, stale.Body.String())
	}
	if manager.Snapshot().Busy {
		t.Fatal("rejected request reached audio manager")
	}
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	accepted := webUICommandRequestForTest(server, http.MethodPost, "/tgw/api/v1/admin/audio/settings", body, token, passkeyTestOrigin, "csrf", "csrf")
	if accepted.Code != 202 || !strings.Contains(accepted.Body.String(), `"busy":true`) {
		t.Fatalf("valid settings not admitted asynchronously: %d %s", accepted.Code, accepted.Body.String())
	}
	busy := webUICommandRequestForTest(server, http.MethodPost, "/tgw/api/v1/admin/audio/settings", body, token, passkeyTestOrigin, "csrf", "csrf")
	if busy.Code != 409 || !strings.Contains(busy.Body.String(), `"audio_busy"`) {
		t.Fatalf("concurrent apply accepted: %d %s", busy.Code, busy.Body.String())
	}
}

func TestAudioUnavailableRequiresAuthenticationAndMethodsAreExact(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"audio", "audio/settings"} {
		method := http.MethodGet
		allow := "GET"
		if endpoint == "audio/settings" {
			method = http.MethodPost
			allow = "POST"
		}
		got := webUICommandRequestForTest(server, method, "/tgw/api/v1/admin/"+endpoint, "{}", token, passkeyTestOrigin, "csrf", "csrf")
		if got.Code != 503 || !strings.Contains(got.Body.String(), `"audio_unavailable"`) {
			t.Fatalf("missing manager response: %d %s", got.Code, got.Body.String())
		}
		wrong := webUICommandRequestForTest(server, http.MethodPut, "/tgw/api/v1/admin/"+endpoint, "{}", token, passkeyTestOrigin, "csrf", "csrf")
		if wrong.Code != 405 || wrong.Header().Get("Allow") != allow {
			t.Fatalf("wrong methods accepted: %d %s", wrong.Code, wrong.Header().Get("Allow"))
		}
	}
}
