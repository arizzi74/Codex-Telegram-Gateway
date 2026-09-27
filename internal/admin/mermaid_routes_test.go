package admin

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUIDiagramRendererIsolation(t *testing.T) {
	console, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	// A containing server may set a general frame prohibition. The renderer is
	// the one endpoint designed for a same-origin, opaque sandboxed iframe.
	response.Header().Set("X-Frame-Options", "DENY")
	console.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/tgw/webui/diagram-renderer", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("renderer status=%d body=%s", response.Code, response.Body)
	}
	if response.Header().Get("X-Frame-Options") != "SAMEORIGIN" || response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("renderer headers=%v", response.Header())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("renderer lost shared security headers: %v", response.Header())
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("renderer must not create an authentication or CSRF session")
	}
	policy := map[string]string{}
	for _, directive := range strings.Split(response.Header().Get("Content-Security-Policy"), ";") {
		fields := strings.Fields(directive)
		if len(fields) < 2 {
			t.Fatalf("invalid CSP directive=%q", directive)
		}
		if _, duplicate := policy[fields[0]]; duplicate {
			t.Fatalf("duplicate CSP directive=%q", directive)
		}
		policy[fields[0]] = strings.Join(fields[1:], " ")
	}
	for name, want := range map[string]string{
		"default-src": "'none'", "base-uri": "'none'", "frame-ancestors": "'self'",
		"form-action": "'none'", "object-src": "'none'", "connect-src": "'none'",
		"script-src": passkeyTestOrigin + "/tgw/webui/static/webui-mermaid-runtime.js",
		"style-src":  "'unsafe-inline'", "img-src": "'none'", "font-src": "'none'",
		"frame-src": "'none'", "worker-src": "'none'", "sandbox": "allow-scripts",
	} {
		if policy[name] != want {
			t.Errorf("%s=%q, want %q", name, policy[name], want)
		}
	}
	for _, forbidden := range []string{"allow-same-origin", "allow-forms", "allow-popups", "allow-top-navigation", "unsafe-eval", "data:", "blob:", "*"} {
		if strings.Contains(response.Header().Get("Content-Security-Policy"), forbidden) {
			t.Errorf("renderer policy allows %s", forbidden)
		}
	}
}

func TestWebUIDiagramAssetsShareReleaseFingerprint(t *testing.T) {
	console, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	console.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/tgw/webui/", nil))
	parentScript := regexp.MustCompile(`/tgw/webui/static/webui-diagrams\.js\?v=([0-9a-f]{20})`).FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || len(parentScript) != 2 {
		t.Fatalf("missing parent diagram script: status=%d", page.Code)
	}
	frame := httptest.NewRecorder()
	console.ServeHTTP(frame, httptest.NewRequest(http.MethodGet, "/tgw/webui/diagram-renderer?v="+parentScript[1], nil))
	runtimeScript := regexp.MustCompile(`/tgw/webui/static/webui-mermaid-runtime\.js\?v=([0-9a-f]{20})`).FindStringSubmatch(frame.Body.String())
	if frame.Code != http.StatusOK || len(runtimeScript) != 2 || runtimeScript[1] != parentScript[1] {
		t.Fatalf("renderer does not share shell release: status=%d script=%v", frame.Code, runtimeScript)
	}
	for _, script := range []string{parentScript[0], runtimeScript[0]} {
		response := httptest.NewRecorder()
		console.ServeHTTP(response, httptest.NewRequest(http.MethodGet, script, nil))
		wantCache := "no-store"
		if script == runtimeScript[0] {
			wantCache = "public, max-age=31536000, immutable"
		}
		if response.Code != http.StatusOK || response.Body.Len() == 0 || response.Header().Get("Content-Type") != "application/javascript; charset=utf-8" || response.Header().Get("Cache-Control") != wantCache {
			t.Fatalf("unavailable bundled script %q: status=%d headers=%v", script, response.Code, response.Header())
		}
		if strings.Contains(response.Header().Get("Content-Security-Policy"), "unsafe-inline") {
			t.Fatalf("renderer-specific policy escaped into static asset %q", script)
		}
	}
}

func TestWebUIDiagramRoutesRejectUnexpectedRequests(t *testing.T) {
	console, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/tgw/webui/diagram-renderer", "/tgw/webui/static/webui-diagrams.js", "/tgw/webui/static/webui-mermaid-runtime.js", "/tgw/webui/static/webui-mermaid-LICENSE.txt"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
			response := httptest.NewRecorder()
			console.ServeHTTP(response, httptest.NewRequest(method, path, nil))
			if response.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status=%d", method, path, response.Code)
			}
		}
	}
	for _, path := range []string{"/tgw/webui/diagram-renderer/", "/tgw/webui/diagram-renderer/unknown", "/tgw/webui/static/webui-mermaid-frame.html", "/tgw/webui/static/mermaid-missing.js"} {
		response := httptest.NewRecorder()
		console.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s: status=%d", path, response.Code)
		}
	}
}

func TestWebUIDiagramRuntimeCompressionAndCaching(t *testing.T) {
	console, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := versionedWebUIAssets()
	if err != nil {
		t.Fatal(err)
	}
	original, err := assets.ReadFile("static/webui-mermaid-runtime.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		accept      []string
		compressed  bool
		immutable   bool
	}{
		{name: "unversioned gzip", accept: []string{"gzip, deflate, br"}, compressed: true},
		{name: "matching release", query: "?v=" + bundle.fingerprint, accept: []string{"gzip"}, compressed: true, immutable: true},
		{name: "stale release", query: "?v=old-release", accept: []string{"gzip"}, compressed: true},
		{name: "duplicate version", query: "?v=" + bundle.fingerprint + "&v=other", accept: []string{"gzip"}, compressed: true},
		{name: "no encoding"},
		{name: "gzip forbidden", accept: []string{"gzip;q=0, br"}},
		{name: "explicit refusal overrides wildcard", accept: []string{"*;q=1, gzip;q=0"}},
		{name: "wildcard permits gzip", accept: []string{"*;q=0.5"}, compressed: true},
		{name: "wildcard denied", accept: []string{"*;q=0"}},
		{name: "quality accepted case insensitive", accept: []string{"GZip; Q=0.5"}, compressed: true},
		{name: "multiple headers", accept: []string{"br", "gzip"}, compressed: true},
		{name: "contradictory gzip denied", accept: []string{"gzip;q=0", "gzip;q=1"}},
		{name: "invalid quality", accept: []string{"gzip;q=NaN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/tgw/webui/static/webui-mermaid-runtime.js"+tc.query, nil)
			for _, accept := range tc.accept {
				request.Header.Add("Accept-Encoding", accept)
			}
			response := httptest.NewRecorder()
			console.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("runtime status=%d headers=%v", response.Code, response.Header())
			}
			wantCache := "no-store"
			if tc.immutable {
				wantCache = "public, max-age=31536000, immutable"
			}
			if response.Header().Get("Cache-Control") != wantCache {
				t.Fatalf("cache-control=%q, want %q", response.Header().Get("Cache-Control"), wantCache)
			}
			body := response.Body.Bytes()
			if tc.compressed {
				if response.Header().Get("Content-Encoding") != "gzip" || len(body) >= len(original) {
					t.Fatalf("compression headers=%v bytes=%d original=%d", response.Header(), len(body), len(original))
				}
				reader, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				body, err = io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
			} else if response.Header().Get("Content-Encoding") != "" {
				t.Fatalf("unexpected encoding=%q", response.Header().Get("Content-Encoding"))
			}
			if !bytes.Equal(body, original) {
				t.Fatal("transfer changed the embedded runtime")
			}
		})
	}
}

func TestWebUIDiagramLicenseAvailable(t *testing.T) {
	console, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	console.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/tgw/webui/static/webui-mermaid-LICENSE.txt", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "Mermaid diagram renderer") || !strings.Contains(response.Body.String(), "MIT License") {
		t.Fatalf("missing bundled license notices: status=%d headers=%v", response.Code, response.Header())
	}
}
