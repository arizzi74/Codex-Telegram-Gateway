package admin

import (
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUINotificationAssetsAreScopedAndUncached(t *testing.T) {
	s, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, contentType string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), contentType) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status=%d headers=%v", path, w.Code, w.Header())
		}
		return w
	}
	w := request("/tgw/webui/manifest.webmanifest", "application/manifest+json")
	var manifest struct {
		ID, Scope, Display string
		StartURL           string `json:"start_url"`
		Icons              []struct{ Src string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "/tgw/webui/" || manifest.Scope != manifest.ID || manifest.StartURL != manifest.ID || manifest.Display != "standalone" {
		t.Fatalf("unexpected install scope: %+v", manifest)
	}
	if len(manifest.Icons) == 0 {
		t.Fatal("install manifest has no icons")
	}
	for _, icon := range manifest.Icons {
		if !strings.HasPrefix(icon.Src, "/tgw/webui/static/webui-icon") {
			t.Fatalf("icon escaped the public UI assets: %q", icon.Src)
		}
	}
	w = request("/tgw/webui/sw.js", "application/javascript")
	if w.Header().Get("Service-Worker-Allowed") != "/tgw/webui/" {
		t.Fatal("notification worker scope is not restricted to the web UI")
	}
	for _, icon := range []struct {
		path string
		size int
	}{{"/tgw/webui/static/webui-icon-192.png", 192}, {"/tgw/webui/static/webui-icon-512.png", 512}, {"/tgw/webui/static/webui-icon-180.png", 180}} {
		w := request(icon.path, "image/png")
		cfg, err := png.DecodeConfig(w.Body)
		if err != nil || cfg.Width != icon.size || cfg.Height != icon.size {
			t.Fatalf("invalid install icon %s: %+v %v", icon.path, cfg, err)
		}
	}
}
