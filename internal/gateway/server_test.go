package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type routeReadiness struct {
	pingErr, migrationErr error
}

func (s routeReadiness) Ping(context.Context) error            { return s.pingErr }
func (s routeReadiness) CheckMigrations(context.Context) error { return s.migrationErr }

func TestGatewayRoutesUseTGWPrefix(t *testing.T) {
	mux := NewMux(routeReadiness{}, NewHub(nil, nil, 0, 0))
	for _, tc := range []struct {
		path string
		code int
		body string
	}{
		{"/tgw/healthz", http.StatusOK, "ok\n"},
		{"/tgw/readyz", http.StatusOK, "ready\n"},
		{"/tgw/api/v1/workers/connect", http.StatusUnauthorized, "unauthorized\n"},
		{"/tghealthz", http.StatusNotFound, "404 page not found\n"},
		{"/tgreadyz", http.StatusNotFound, "404 page not found\n"},
		{"/tgapi/v1/workers/connect", http.StatusNotFound, "404 page not found\n"},
		{"/healthz", http.StatusNotFound, "404 page not found\n"},
		{"/readyz", http.StatusNotFound, "404 page not found\n"},
		{"/api/v1/workers/connect", http.StatusNotFound, "404 page not found\n"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.code || w.Body.String() != tc.body {
				t.Fatalf("GET %s: status=%d body=%q", tc.path, w.Code, w.Body.String())
			}
			if w.Header().Get("Location") != "" {
				t.Fatal("route must not redirect to a legacy alias")
			}
		})
	}
}

func TestTGReadinessChecksRegistryAndSchema(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store routeReadiness
		body  string
	}{
		{"registry", routeReadiness{pingErr: errors.New("database unavailable")}, "registry unavailable\n"},
		{"schema", routeReadiness{migrationErr: errors.New("migration missing")}, "schema not ready\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			NewMux(tc.store, NewHub(nil, nil, 0, 0)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tgw/readyz", nil))
			if w.Code != http.StatusServiceUnavailable || w.Body.String() != tc.body {
				t.Fatalf("readiness: status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
}
