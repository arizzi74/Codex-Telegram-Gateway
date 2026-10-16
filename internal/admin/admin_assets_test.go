package admin

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/registry"
)

func TestAdminBotProfilePreviewAssetIsServed(t *testing.T) {
	s, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/tgw/admin/static/webui-icon.svg"
	page := httptest.NewRecorder()
	s.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/tgw/admin/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `src="`+path+`"`) {
		t.Fatal("Admin creation form does not reference the expected profile preview")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/svg+xml" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview asset: status=%d headers=%v", w.Code, w.Header())
	}
	var svg struct{ XMLName xml.Name }
	if err := xml.Unmarshal(w.Body.Bytes(), &svg); err != nil || svg.XMLName.Local != "svg" {
		t.Fatalf("profile preview is not valid SVG: %v", err)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("preview asset accepts writes: %d", w.Code)
	}
}
