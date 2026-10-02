package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func enrollmentHTTPServer(t *testing.T, origin string) (*Server, *registry.Store, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := registry.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server, err := New(store, Config{Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	return server, store, db, webuiTestLogin(t, store)
}

func publicEnrollmentRequest(s *Server, method, path, body, origin, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Real-IP", ip)
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func createHTTPEnrollment(t *testing.T, s *Server, token, body string) protocol.CreateWorkerEnrollmentResponse {
	t.Helper()
	w := webUICommandRequestForTest(s, http.MethodPost, "/tgw/api/v1/admin/worker-enrollments", body, token, s.origin, "csrf", "csrf")
	var response protocol.CreateWorkerEnrollmentResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatalf("create enrollment: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("enrollment URL response is cacheable")
	}
	return response
}

func redemptionBody(t *testing.T, code string) string {
	t.Helper()
	body, err := json.Marshal(protocol.RedeemWorkerEnrollmentRequest{Code: code, Name: "Installer worker", OS: "darwin", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestWorkerEnrollmentHTTPCreateLandingRedeemAndPrivacy(t *testing.T) {
	origin := "https://gateway.example.com:8443"
	s, store, db, token := enrollmentHTTPServer(t, origin)
	e := createHTTPEnrollment(t, s, token, `{"service_access":"full"}`)
	u, err := url.Parse(e.EnrollmentURL)
	if err != nil || e.EnrollmentID == "" || e.ServiceAccess != "full" || u.Scheme != "https" || u.Host != "gateway.example.com:8443" || u.Path != "/tgw/enroll/" || len(u.Fragment) != 12 || u.RawQuery != "" || time.Until(e.ExpiresAt) < 9*time.Minute {
		t.Fatal("create response did not contain the configured origin and ten-minute fragment URL")
	}
	workers, err := store.ListWorkers(t.Context())
	if err != nil || len(workers) != 0 {
		t.Fatal("link creation registered a worker")
	}
	// The browser sends RequestURI, which omits the fragment capability.
	if strings.Contains(u.RequestURI(), u.Fragment) {
		t.Fatal("enrollment code would be sent in the browser request target")
	}
	for _, target := range []string{u.RequestURI(), "/tgw/enroll/?code=" + u.Fragment} {
		w := publicEnrollmentRequest(s, http.MethodGet, target, "", "", "198.51.100.1")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), u.Fragment) || !strings.Contains(w.Body.String(), "worker installer") || strings.Contains(w.Body.String(), "<script") || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("landing page exposed the code or lacked safe instructions: status=%d path=%s", w.Code, u.Path)
		}
	}
	var consumed *string
	if err = db.QueryRow(`SELECT consumed_at FROM worker_enrollments WHERE enrollment_id=?`, e.EnrollmentID).Scan(&consumed); err != nil || consumed != nil {
		t.Fatal("GET consumed the enrollment")
	}
	w := publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, redemptionBody(t, strings.ToLower(u.Fragment)), "", "198.51.100.1")
	var credentials protocol.RedeemWorkerEnrollmentResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &credentials) != nil || credentials.GatewayURL != "wss://gateway.example.com:8443/tgw/api/v1/workers/connect" || credentials.ServiceAccess != "full" {
		t.Fatalf("redeem response: %d", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" || len(w.Result().Cookies()) != 0 {
		t.Fatal("public credentials response is cacheable or created cookies")
	}
	worker, err := store.AuthenticateWorker(t.Context(), credentials.Token)
	if err != nil || worker.ID.String() != credentials.WorkerID || worker.Name != "Installer worker" || worker.OS != "darwin" || worker.Arch != "arm64" {
		t.Fatal("redeemed credentials cannot authenticate their named worker")
	}
	w = webUICommandRequestForTest(s, http.MethodGet, "/tgw/api/v1/admin/dashboard", "", token, "", "", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), credentials.Token) || strings.Contains(w.Body.String(), u.Fragment) || strings.Contains(w.Body.String(), e.EnrollmentURL) {
		t.Fatal("dashboard exposed enrollment credentials")
	}
	w = publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, redemptionBody(t, u.Fragment), origin, "198.51.100.1")
	if w.Code != http.StatusGone || strings.Contains(w.Body.String(), credentials.Token) || strings.Contains(w.Body.String(), u.Fragment) {
		t.Fatal("replay succeeded or exposed enrollment credentials")
	}
	defaultEnrollment := createHTTPEnrollment(t, s, token, "")
	if defaultEnrollment.ServiceAccess != "restricted" {
		t.Fatal("empty creation request did not default to restricted access")
	}
}

func TestWorkerEnrollmentAdminFreshAuthenticationOriginAndCSRF(t *testing.T) {
	s, _, db, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	e := createHTTPEnrollment(t, s, token, "{}")
	for _, target := range []struct{ method, path, body string }{
		{http.MethodPost, "/tgw/api/v1/admin/worker-enrollments", "{}"},
		{http.MethodDelete, "/tgw/api/v1/admin/worker-enrollments/" + e.EnrollmentID, ""},
	} {
		for _, tc := range []struct {
			name, token, origin, header, cookie string
			status                              int
		}{
			{"no login", "", passkeyTestOrigin, "csrf", "csrf", http.StatusUnauthorized},
			{"invalid login", "invalid", passkeyTestOrigin, "csrf", "csrf", http.StatusUnauthorized},
			{"foreign origin", token, "https://evil.example", "csrf", "csrf", http.StatusForbidden},
			{"different port", token, passkeyTestOrigin + ":9443", "csrf", "csrf", http.StatusForbidden},
			{"missing origin", token, "", "csrf", "csrf", http.StatusForbidden},
			{"missing csrf", token, passkeyTestOrigin, "", "csrf", http.StatusForbidden},
			{"csrf mismatch", token, passkeyTestOrigin, "wrong", "csrf", http.StatusForbidden},
		} {
			t.Run(target.method+"/"+tc.name, func(t *testing.T) {
				w := webUICommandRequestForTest(s, target.method, target.path, target.body, tc.token, tc.origin, tc.header, tc.cookie)
				if w.Code != tc.status {
					t.Fatalf("status=%d want=%d", w.Code, tc.status)
				}
			})
		}
	}
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ method, path, body string }{
		{http.MethodPost, "/tgw/api/v1/admin/worker-enrollments", "{}"},
		{http.MethodDelete, "/tgw/api/v1/admin/worker-enrollments/" + e.EnrollmentID, ""},
	} {
		w := webUICommandRequestForTest(s, target.method, target.path, target.body, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauthentication_required") {
			t.Fatal("enrollment mutation did not require a fresh passkey")
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM worker_enrollments WHERE revoked_at IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatal("rejected admin requests mutated enrollments")
	}
}

func TestWorkerEnrollmentGoneResponsesAndAdminRevocation(t *testing.T) {
	s, _, db, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	expired := createHTTPEnrollment(t, s, token, "{}")
	revoked := createHTTPEnrollment(t, s, token, "{}")
	used := createHTTPEnrollment(t, s, token, "{}")
	if _, err := db.Exec(`UPDATE worker_enrollments SET created_at=?,expires_at=? WHERE enrollment_id=?`, time.Now().Add(-20*time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z"), time.Now().Add(-time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z"), expired.EnrollmentID); err != nil {
		t.Fatal(err)
	}
	w := webUICommandRequestForTest(s, http.MethodDelete, "/tgw/api/v1/admin/worker-enrollments/"+revoked.EnrollmentID, "", token, passkeyTestOrigin, "csrf", "csrf")
	if w.Code != http.StatusNoContent {
		t.Fatal("revocation failed")
	}
	usedURL, _ := url.Parse(used.EnrollmentURL)
	w = publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, redemptionBody(t, usedURL.Fragment), "", "198.51.100.1")
	if w.Code != http.StatusOK {
		t.Fatal("initial redemption failed")
	}
	var body string
	for _, link := range []string{expired.EnrollmentURL, revoked.EnrollmentURL, used.EnrollmentURL, passkeyTestOrigin + "/tgw/enroll/#AAAAAAAAAAAA"} {
		u, _ := url.Parse(link)
		w = publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, redemptionBody(t, u.Fragment), "", "198.51.100.1")
		if w.Code != http.StatusGone || (body != "" && body != w.Body.String()) || strings.Contains(w.Body.String(), u.Fragment) {
			t.Fatal("unavailable enrollment cases were distinguishable or leaked their code")
		}
		body = w.Body.String()
	}
	for _, id := range []string{used.EnrollmentID, uuid.NewString()} {
		w = webUICommandRequestForTest(s, http.MethodDelete, "/tgw/api/v1/admin/worker-enrollments/"+id, "", token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != http.StatusNotFound {
			t.Fatal("revocation of missing or consumed enrollment should return 404")
		}
	}
}

func TestWorkerEnrollmentMalformedJSONAndForeignOriginDoNotConsume(t *testing.T) {
	s, _, _, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	e := createHTTPEnrollment(t, s, token, "{}")
	u, _ := url.Parse(e.EnrollmentURL)
	valid := redemptionBody(t, u.Fragment)
	for _, origin := range []string{"https://evil.example", passkeyTestOrigin + ":8443", "null"} {
		if w := publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, valid, origin, "198.51.100.1"); w.Code != http.StatusForbidden {
			t.Fatal("foreign browser origin accepted")
		}
	}
	for i, body := range []string{
		"", "null", "[]", valid + " {}", strings.TrimSuffix(valid, "}") + `,"extra":"secret-input"}`,
		`{"code":"` + u.Fragment + `","name":"line\nbreak","os":"linux","arch":"amd64"}`,
		`{"code":"` + u.Fragment + `","name":"` + string([]byte{0xff}) + `","os":"linux","arch":"amd64"}`,
		`{"code":"` + u.Fragment + `","name":"` + strings.Repeat("a", maxEnrollmentBody) + `","os":"linux","arch":"amd64"}`,
		`{"code":"` + u.Fragment + `","name":"worker","os":"windows","arch":"amd64"}`,
	} {
		w := publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, body, "", fmt.Sprintf("198.51.100.%d", i+2))
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), u.Fragment) || strings.Contains(w.Body.String(), "secret-input") {
			t.Fatalf("malformed request status=%d", w.Code)
		}
	}
	for _, body := range []string{`{"service_access":"root"}`, `{"service_access":"full","extra":true}`, "{} {}", "null"} {
		w := webUICommandRequestForTest(s, http.MethodPost, "/tgw/api/v1/admin/worker-enrollments", body, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != http.StatusBadRequest {
			t.Fatal("malformed creation request accepted")
		}
	}
	w := publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, valid, passkeyTestOrigin, "198.51.100.100")
	if w.Code != http.StatusOK {
		t.Fatal("rejected input consumed the valid link")
	}
}

func TestWorkerEnrollmentPublicLimitsPerProxyClientAndGlobally(t *testing.T) {
	body := redemptionBody(t, "AAAAAAAAAAAA")
	t.Run("per client", func(t *testing.T) {
		s, _, _, _ := enrollmentHTTPServer(t, passkeyTestOrigin)
		for range 20 {
			if w := publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, body, "", "198.51.100.1"); w.Code != http.StatusGone {
				t.Fatalf("request status=%d", w.Code)
			}
		}
		w := publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, body, "", "198.51.100.1")
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
			t.Fatal("client limiter lacked 429 and Retry-After")
		}
		if w = publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, body, "", "203.0.113.1"); w.Code != http.StatusGone {
			t.Fatal("independent client blocked")
		}
	})
	t.Run("global", func(t *testing.T) {
		s, _, _, _ := enrollmentHTTPServer(t, passkeyTestOrigin)
		for i := range 240 {
			if w := publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, body, "", fmt.Sprintf("2001:db8::%x", i+1)); w.Code != http.StatusGone {
				t.Fatalf("request %d status=%d", i, w.Code)
			}
		}
		w := publicEnrollmentRequest(s, http.MethodPost, protocol.WorkerEnrollmentRedeemPath, body, "", "203.0.113.200")
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
			t.Fatal("global limiter did not bound rotating clients")
		}
	})
}
