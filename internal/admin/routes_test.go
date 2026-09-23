package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/registry"
)

func TestAdminRoutesUseTGWPrefix(t *testing.T) {
	s, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path     string
		code     int
		contains string
	}{
		{"/tgw/admin", http.StatusTemporaryRedirect, "/tgw/admin/"},
		{"/tgw/admin/", http.StatusOK, `/tgw/admin/static/app.js`},
		{"/tgw/admin/static/app.css", http.StatusOK, "font-family"},
		{"/tgw/admin/static/app.js", http.StatusOK, "const api = '/tgw/api/v1/admin'"},
		{"/tgw/api/v1/admin/dashboard", http.StatusUnauthorized, ""},
		{"/tgadmin", http.StatusNotFound, ""},
		{"/tgadmin/", http.StatusNotFound, ""},
		{"/tgadmin/static/app.js", http.StatusNotFound, ""},
		{"/tgapi/v1/admin/dashboard", http.StatusNotFound, ""},
		{"/admin", http.StatusNotFound, ""},
		{"/admin/", http.StatusNotFound, ""},
		{"/admin/static/app.js", http.StatusNotFound, ""},
		{"/api/v1/admin/dashboard", http.StatusNotFound, ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("GET %s: status=%d want=%d, expected content %q", tc.path, w.Code, tc.code, tc.contains)
			}
			if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("admin route lost security headers")
			}
			if tc.code == http.StatusNotFound && w.Header().Get("Location") != "" {
				t.Fatal("old admin route must not redirect")
			}
		})
	}
}
