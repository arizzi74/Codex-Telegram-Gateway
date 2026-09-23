package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginLimitsPerProxyClientAndPhase(t *testing.T) {
	s, err := New(adminIntegrationStore(t), Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if w := loginLimitRequest(s, "begin", "198.51.100.1"); w.Code != http.StatusOK {
			t.Fatalf("begin %d = %d: %s", i, w.Code, w.Body.String())
		}
	}
	if w := loginLimitRequest(s, "begin", "198.51.100.1"); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("begin limit = %d, headers=%v", w.Code, w.Header())
	}
	if w := loginLimitRequest(s, "begin", "203.0.113.1"); w.Code != http.StatusOK {
		t.Fatalf("independent client blocked: %d", w.Code)
	}
	// A full begin budget leaves finish available; malformed finishes still
	// consume their own budget before JSON parsing or credential verification.
	for i := range 20 {
		if w := loginLimitRequest(s, "finish", "198.51.100.1"); w.Code != http.StatusBadRequest {
			t.Fatalf("finish %d = %d", i, w.Code)
		}
	}
	if w := loginLimitRequest(s, "finish", "198.51.100.1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("finish limit = %d", w.Code)
	}
	if w := loginLimitRequest(s, "finish", "203.0.113.1"); w.Code != http.StatusBadRequest {
		t.Fatalf("independent finish blocked: %d", w.Code)
	}
}

func TestLoginGlobalBudgetsBoundRotatingClients(t *testing.T) {
	for _, tc := range []struct {
		phase       string
		limit, want int
	}{
		{"begin", 120, http.StatusOK}, {"finish", 240, http.StatusBadRequest},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			s, err := New(adminIntegrationStore(t), Config{Origin: passkeyTestOrigin})
			if err != nil {
				t.Fatal(err)
			}
			for i := range tc.limit {
				if w := loginLimitRequest(s, tc.phase, fmt.Sprintf("2001:db8::%x", i+1)); w.Code != tc.want {
					t.Fatalf("request %d = %d: %s", i, w.Code, w.Body.String())
				}
			}
			if w := loginLimitRequest(s, tc.phase, "203.0.113.200"); w.Code != http.StatusTooManyRequests {
				t.Fatalf("global limit = %d", w.Code)
			}
		})
	}
}

func loginLimitRequest(s *Server, phase, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/tgw/api/v1/admin/login/"+phase, strings.NewReader("{}"))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Real-IP", ip)
	r.Header.Set("Origin", passkeyTestOrigin)
	r.Header.Set("X-CSRF-Token", "csrf-test-token")
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "csrf-test-token"})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
