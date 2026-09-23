package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/registry"
)

type webUIRouteReadiness struct{}

func (webUIRouteReadiness) Ping(context.Context) error            { return nil }
func (webUIRouteReadiness) CheckMigrations(context.Context) error { return nil }

// Exercise the outer daemon mux as well as the admin mux: redirects and route
// ownership can differ from invoking the admin handler directly.
func TestComposedGatewayBrowserRoutes(t *testing.T) {
	console, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	mux := gateway.NewMux(webUIRouteReadiness{}, gateway.NewHub(nil, nil, 0, 0))
	for _, prefix := range []string{"/tgw/admin/", "/tgw/api/v1/admin/", "/tgw/webui/", "/tgw/api/v1/webui/"} {
		mux.Handle(prefix, console)
	}
	for _, tc := range []struct {
		method, path, location, marker string
		status                         int
		securityHeaders                bool
	}{
		{method: "GET", path: "/tgw", location: "/tgw/", status: http.StatusTemporaryRedirect},
		{method: "GET", path: "/tgw/", location: "/tgw/webui/", status: http.StatusTemporaryRedirect},
		{method: "GET", path: "/tgw/admin", location: "/tgw/admin/", status: http.StatusTemporaryRedirect},
		{method: "GET", path: "/tgw/webui", location: "/tgw/webui/", status: http.StatusTemporaryRedirect},
		{method: "GET", path: "/tgw/admin/", marker: "/tgw/admin/static/app.js", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/webui/", marker: "/tgw/webui/static/webui.js", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/admin/static/app.css", marker: "font-family", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/admin/static/app.js", marker: "/tgw/api/v1/admin", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/webui/static/webui.css", marker: "font-family", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/webui/static/webui.js", marker: "/tgw/api/v1/webui", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/webui/static/webui-format.js", marker: "CodexFormat", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/webui/static/webui-notifications.js", marker: "/tgw/api/v1/webui/push", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/webui/manifest.webmanifest", marker: "/tgw/webui/", status: http.StatusOK, securityHeaders: true},
		{method: "GET", path: "/tgw/webui/sw.js", marker: "notificationclick", status: http.StatusOK, securityHeaders: true},
		{method: "POST", path: "/tgw/webui/sw.js", status: http.StatusMethodNotAllowed, securityHeaders: true},
		{method: "POST", path: "/tgw/webui/", status: http.StatusMethodNotAllowed, securityHeaders: true},
		{method: "POST", path: "/tgw/webui/static/webui.js", status: http.StatusMethodNotAllowed, securityHeaders: true},
		{method: "GET", path: "/tgw/unknown", status: http.StatusNotFound},
		{method: "GET", path: "/tgw/webui/unknown", status: http.StatusNotFound, securityHeaders: true},
		{method: "GET", path: "/tgw/webui/static/missing.js", status: http.StatusNotFound, securityHeaders: true},
		{method: "GET", path: "/tgw/admin/unknown", status: http.StatusNotFound, securityHeaders: true},
		{method: "GET", path: "/", status: http.StatusNotFound},
		{method: "GET", path: "/tgadmin/", status: http.StatusNotFound},
		{method: "GET", path: "/tgapi/v1/admin/dashboard", status: http.StatusNotFound},
		{method: "GET", path: "/tgui/", status: http.StatusNotFound},
		{method: "GET", path: "/tghealthz", status: http.StatusNotFound},
		{method: "GET", path: "/tgreadyz", status: http.StatusNotFound},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status || response.Header().Get("Location") != tc.location || !strings.Contains(response.Body.String(), tc.marker) {
				t.Fatalf("status=%d location=%q; want status=%d location=%q and content %q", response.Code, response.Header().Get("Location"), tc.status, tc.location, tc.marker)
			}
			if tc.securityHeaders && (response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff") {
				t.Fatalf("browser route lost security headers: %v", response.Header())
			}
		})
	}
}

func TestWebUIPageVersionsEveryEmbeddedAsset(t *testing.T) {
	console, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	console.ServeHTTP(w, httptest.NewRequest("GET", "/tgw/webui/", nil))
	links := regexp.MustCompile(`/tgw/webui/static/[^"?]+\?v=([0-9a-f]{20})`).FindAllStringSubmatch(w.Body.String(), -1)
	if w.Code != 200 || len(links) != 6 {
		t.Fatalf("versioned assets: status=%d links=%v", w.Code, links)
	}
	for _, link := range links {
		if link[1] != links[0][1] {
			t.Fatal("shell assets used different release fingerprints")
		}
		response := httptest.NewRecorder()
		console.ServeHTTP(response, httptest.NewRequest("GET", link[0], nil))
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Body.Len() == 0 {
			t.Fatalf("versioned asset unavailable: %s: %d", link[0], response.Code)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("versioned shell became cacheable")
	}
}

func TestWebUIAttachmentPreviewCSPOnlyAllowsLocalBlobs(t *testing.T) {
	console, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	policy := func(path string) string {
		response := httptest.NewRecorder()
		console.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, response.Code)
		}
		return response.Header().Get("Content-Security-Policy")
	}
	adminPolicy, webPolicy := policy("/tgw/admin/"), policy("/tgw/webui/")
	if strings.Contains(adminPolicy, "blob:") || webPolicy != adminPolicy+"; img-src 'self' blob:; worker-src 'self'; manifest-src 'self'" {
		t.Fatalf("preview policy changed unrelated sources/admin: %q %q", adminPolicy, webPolicy)
	}
	for _, forbidden := range []string{"data:", "https:", "http:", "unsafe-inline", "*"} {
		if strings.Contains(webPolicy, forbidden) {
			t.Fatalf("preview allowed %s", forbidden)
		}
	}
}
