// Package admin serves the passkey-only operator console.
package admin

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/httpguard"
	"github.com/iaia/telegramgw/internal/registry"
)

const (
	adminCookie    = "__Host-telegramgw-admin"
	ceremonyCookie = "__Host-telegramgw-ceremony"
	csrfCookie     = "__Host-telegramgw-csrf"
	maxBody        = 1 << 20
)

//go:embed static/*
var assets embed.FS

// Config defines the public origin used for browser and WebAuthn checks.
type Config struct {
	Origin           string
	BotAPI           BotAPI
	BotUsername      string
	AllowedUserCount int
	AllowedChatCount int
	Redactor         *auth.Redactor
	WebUI            *gateway.Hub
}

type Server struct {
	store         *registry.Store
	origin        string
	webauthn      *wa.WebAuthn
	mux           *http.ServeMux
	bot           *botMonitor
	redactor      *auth.Redactor
	loginBegins   *httpguard.Limiter
	loginFinishes *httpguard.Limiter
	webui         *gateway.Hub
}

// New builds an isolated admin handler. Origin must be the configured public
// HTTPS origin; it is both the browser request origin and WebAuthn RP origin.
func New(store *registry.Store, cfg Config) (*Server, error) {
	if store == nil {
		return nil, errors.New("admin: registry store is required")
	}
	u, err := config.ParseHTTPSOrigin(cfg.Origin)
	if err != nil {
		return nil, errors.New("admin: origin must be an exact https origin")
	}
	origin := u.String()
	w, err := wa.New(&wa.Config{RPID: u.Hostname(), RPDisplayName: "Codex Gateway", RPOrigins: []string{origin}, RPTopOrigins: []string{origin}, AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}})
	if err != nil {
		return nil, fmt.Errorf("admin: configure webauthn: %w", err)
	}
	s := &Server{store: store, origin: origin, webauthn: w, mux: http.NewServeMux()}
	s.loginBegins = httpguard.NewLimiter(10, 120, time.Minute)
	s.loginFinishes = httpguard.NewLimiter(20, 240, time.Minute)
	s.bot = newBotMonitor(cfg)
	s.redactor = cfg.Redactor
	s.webui = cfg.WebUI
	s.routes()
	return s, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)
	s.mux.ServeHTTP(w, r)
}
func (s *Server) routes() {
	s.webuiRoutes()
	s.mux.HandleFunc("/tgw/admin/", s.ui)
	s.mux.HandleFunc("/tgw/admin/static/app.css", s.css)
	s.mux.HandleFunc("/tgw/admin/static/app.js", s.js)
	s.mux.HandleFunc("/tgw/api/v1/admin/bootstrap", s.bootstrapBegin)
	s.mux.HandleFunc("/tgw/api/v1/admin/passkeys/register/begin", s.registrationBegin)
	s.mux.HandleFunc("/tgw/api/v1/admin/passkeys/register/finish", s.registrationFinish)
	s.mux.HandleFunc("/tgw/api/v1/admin/login/begin", s.loginBegin)
	s.mux.HandleFunc("/tgw/api/v1/admin/login/finish", s.loginFinish)
	s.mux.HandleFunc("/tgw/api/v1/admin/session", s.session)
	s.mux.HandleFunc("/tgw/api/v1/admin/sessions", s.browserSessions)
	s.mux.HandleFunc("/tgw/api/v1/admin/sessions/", s.browserSession)
	s.mux.HandleFunc("/tgw/api/v1/admin/logout", s.logout)
	s.mux.HandleFunc("/tgw/api/v1/admin/passkeys", s.passkeys)
	s.mux.HandleFunc("/tgw/api/v1/admin/passkeys/", s.passkey)
	s.mux.HandleFunc("/tgw/api/v1/admin/dashboard", s.dashboard)
	s.mux.HandleFunc("/tgw/api/v1/admin/workers", s.workers)
	s.mux.HandleFunc("/tgw/api/v1/admin/workers/", s.worker)
}
func (s *Server) securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; object-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
}
func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/tgw/admin/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		method(w)
		return
	}
	s.setCSRF(w)
	serveAsset(w, r, "static/index.html", "text/html; charset=utf-8")
}
func (s *Server) css(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, "static/app.css", "text/css; charset=utf-8")
}
func (s *Server) js(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, "static/app.js", "application/javascript; charset=utf-8")
}
func serveAsset(w http.ResponseWriter, r *http.Request, name, typ string) {
	if r.Method != "GET" {
		method(w)
		return
	}
	b, err := assets.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Write(b)
}

func (s *Server) bootstrapBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w)
		return
	}
	if !s.csrfOK(w, r) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.store.CheckAdminBootstrap(r.Context(), in.Token); err != nil {
		forbidden(w)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
func (s *Server) registrationBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w)
		return
	}
	if !s.csrfOK(w, r) {
		return
	}
	var in struct {
		BootstrapToken string `json:"bootstrap_token"`
	}
	if !decode(w, r, &in) {
		return
	}
	var handle []byte
	bootstrap := in.BootstrapToken != ""
	if bootstrap {
		if err := s.store.CheckAdminBootstrap(r.Context(), in.BootstrapToken); err != nil {
			forbidden(w)
			return
		}
		handle = make([]byte, 32)
		if _, err := rand.Read(handle); err != nil {
			fail(w, err)
			return
		}
	} else {
		if _, ok := s.requireFreshAuth(w, r); !ok {
			return
		}
		var err error
		handle, _, err = s.store.AdminUser(r.Context())
		if err != nil {
			forbidden(w)
			return
		}
	}
	user := adminUser{handle: handle, name: "gateway-admin"}
	if !bootstrap {
		_, cs, err := s.store.AdminUser(r.Context())
		if err == nil {
			user.creds = decodeCredentials(cs)
		}
	}
	creation, sd, err := s.webauthn.BeginRegistration(user, wa.WithRegistrationOrigin(s.origin), wa.WithAuthenticatorSelection(protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}), wa.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired))
	if err != nil {
		fail(w, err)
		return
	}
	raw, _ := json.Marshal(sd)
	c, err := s.store.NewAdminCeremony(r.Context(), "registration", handle, raw)
	if err != nil {
		if errors.Is(err, registry.ErrAdminCeremonyLimit) {
			tooManyRequests(w)
			return
		}
		fail(w, err)
		return
	}
	s.setCeremony(w, c)
	writeJSON(w, http.StatusOK, map[string]any{"ceremony_id": c.ID.String(), "publicKey": creation.Response})
}
func (s *Server) registrationFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w)
		return
	}
	if !s.csrfOK(w, r) {
		return
	}
	var in struct {
		CeremonyID     string          `json:"ceremony_id"`
		BootstrapToken string          `json:"bootstrap_token"`
		Credential     json.RawMessage `json:"credential"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.BootstrapToken == "" {
		if _, ok := s.requireFreshAuth(w, r); !ok {
			return
		}
	}
	id, err := uuid.Parse(in.CeremonyID)
	if err != nil {
		bad(w)
		return
	}
	c, binding, err := s.readCeremony(r, id, "registration")
	if err != nil {
		forbidden(w)
		return
	}
	var sd wa.SessionData
	if json.Unmarshal(c.SessionData, &sd) != nil {
		forbidden(w)
		return
	}
	user := adminUser{handle: c.UserHandle, name: "gateway-admin"}
	req := requestWithJSON(r, in.Credential)
	credential, err := s.webauthn.FinishRegistration(user, sd, req)
	if err != nil {
		bad(w)
		return
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		fail(w, err)
		return
	}
	record := registry.AdminCredential{ID: credential.ID, UserHandle: c.UserHandle, CredentialJSON: raw}
	if in.BootstrapToken != "" {
		err = s.store.CompleteBootstrapCredential(r.Context(), id, binding, in.BootstrapToken, record)
	} else {
		if _, ok := s.requireFreshAuth(w, r); !ok {
			return
		}
		err = s.store.CompleteAdminCredential(r.Context(), id, binding, record)
	}
	if err != nil {
		forbidden(w)
		return
	}
	clearCeremony(w)
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}
func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w)
		return
	}
	if !s.csrfOK(w, r) {
		return
	}
	if !s.allowLogin(w, r, s.loginBegins) {
		return
	}
	assertion, sd, err := s.webauthn.BeginDiscoverableLogin(wa.WithLoginOrigin(s.origin), wa.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		fail(w, err)
		return
	}
	raw, _ := json.Marshal(sd)
	predecessor := ""
	if cookie, cookieErr := r.Cookie(adminCookie); cookieErr == nil {
		predecessor = cookie.Value
	}
	c, err := s.store.NewAdminLoginCeremony(r.Context(), raw, predecessor, r.UserAgent())
	if err != nil {
		if errors.Is(err, registry.ErrAdminCeremonyLimit) {
			tooManyRequests(w)
			return
		}
		fail(w, err)
		return
	}
	s.setCeremony(w, c)
	writeJSON(w, http.StatusOK, map[string]any{"ceremony_id": c.ID.String(), "publicKey": assertion.Response})
}
func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w)
		return
	}
	if !s.csrfOK(w, r) {
		return
	}
	if !s.allowLogin(w, r, s.loginFinishes) {
		return
	}
	var in struct {
		CeremonyID string          `json:"ceremony_id"`
		Credential json.RawMessage `json:"credential"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, err := uuid.Parse(in.CeremonyID)
	if err != nil {
		bad(w)
		return
	}
	c, binding, err := s.readCeremony(r, id, "authentication")
	if err != nil {
		forbidden(w)
		return
	}
	var sd wa.SessionData
	if json.Unmarshal(c.SessionData, &sd) != nil {
		forbidden(w)
		return
	}
	handle, records, err := s.store.AdminUser(r.Context())
	if err != nil {
		forbidden(w)
		return
	}
	user := adminUser{handle: handle, name: "gateway-admin", creds: decodeCredentials(records)}
	_, credential, err := s.webauthn.FinishPasskeyLogin(func(rawID, userHandle []byte) (wa.User, error) {
		if !equal(userHandle, handle) {
			return nil, errors.New("unknown passkey")
		}
		for _, r := range records {
			if r.RevokedAt == nil && equal(rawID, r.ID) {
				return user, nil
			}
		}
		return nil, errors.New("unknown passkey")
	}, sd, requestWithJSON(r, in.Credential))
	if err != nil {
		bad(w)
		return
	}
	raw, _ := json.Marshal(credential)
	token, err := s.store.CompleteAdminLogin(r.Context(), id, binding, registry.AdminCredential{ID: credential.ID, CredentialJSON: raw})
	if err != nil {
		if errors.Is(err, registry.ErrAdminCeremonyInvalid) || errors.Is(err, registry.ErrAdminSessionInvalid) || errors.Is(err, registry.ErrAdminCredentialGone) {
			forbidden(w)
			return
		}
		fail(w, err)
		return
	}
	s.setSession(w, token)
	clearCeremony(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) allowLogin(w http.ResponseWriter, r *http.Request, limiter *httpguard.Limiter) bool {
	if !limiter.Allow(httpguard.ClientIP(r), time.Now()) {
		tooManyRequests(w)
		return false
	}
	return true
}

func tooManyRequests(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	http.Error(w, "try again later", http.StatusTooManyRequests)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, true); !ok {
		return
	}
	if c, err := r.Cookie(adminCookie); err == nil {
		if err := s.store.RevokeAdminSession(r.Context(), c.Value); err != nil {
			fail(w, err)
			return
		}
	}
	s.clearSession(w)
	writeJSON(w, http.StatusNoContent, nil)
}
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	d, err := s.store.AdminDashboardSnapshot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	s.redactDashboard(&d)
	writeJSON(w, http.StatusOK, struct {
		registry.AdminDashboard
		Bot       BotInfo   `json:"bot"`
		UpdatedAt time.Time `json:"updated_at"`
	}{d, s.bot.snapshot(r.Context()), time.Now().UTC()})
}
func (s *Server) workers(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 160 {
		bad(w)
		return
	}
	token, err := auth.GenerateWorkerToken()
	if err != nil {
		fail(w, err)
		return
	}
	wkr, err := s.store.CreateWorker(r.Context(), registry.CreateWorkerInput{ID: uuid.New(), Name: strings.TrimSpace(in.Name), OS: "unknown", Arch: "unknown", TokenHash: auth.HashWorkerToken(token)})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"worker_id": wkr.ID.String(), "token": token})
}
func (s *Server) worker(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/tgw/api/v1/admin/workers/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		bad(w)
		return
	}
	id, err := uuid.Parse(parts[0])
	if err != nil {
		bad(w)
		return
	}
	if r.Method == "DELETE" && len(parts) == 1 {
		if err = s.store.RevokeWorker(r.Context(), id); err != nil {
			if errors.Is(err, registry.ErrWorkerNotFound) {
				http.NotFound(w, r)
			} else {
				fail(w, err)
			}
			return
		}
		writeJSON(w, http.StatusNoContent, nil)
		return
	}
	if r.Method == "POST" && len(parts) == 2 && parts[1] == "rotate-token" {
		var body struct{}
		if !decode(w, r, &body) {
			return
		}
		token, e := auth.GenerateWorkerToken()
		if e != nil {
			fail(w, e)
			return
		}
		if e = s.store.RotateWorkerToken(r.Context(), id, auth.HashWorkerToken(token)); e != nil {
			if errors.Is(e, registry.ErrWorkerNotFound) {
				http.NotFound(w, r)
			} else {
				fail(w, e)
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
		return
	}
	method(w)
}
func (s *Server) passkeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	items, err := s.store.AdminCredentials(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	type item struct {
		ID         string     `json:"id"`
		CreatedAt  time.Time  `json:"created_at"`
		LastUsedAt *time.Time `json:"last_used_at"`
		RevokedAt  *time.Time `json:"revoked_at"`
	}
	out := make([]item, 0, len(items))
	for _, c := range items {
		out = append(out, item{base64.RawURLEncoding.EncodeToString(c.ID), c.CreatedAt, c.LastUsedAt, c.RevokedAt})
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) passkey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		method(w)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	id, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(r.URL.Path, "/tgw/api/v1/admin/passkeys/"))
	if err != nil || len(id) == 0 {
		bad(w)
		return
	}
	if err = s.store.RevokeAdminCredential(r.Context(), id); err != nil {
		if errors.Is(err, registry.ErrAdminCredentialGone) {
			http.NotFound(w, r)
		} else {
			bad(w)
		}
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != s.origin {
		forbidden(w)
		return false
	}
	return true
}
func (s *Server) csrfOK(w http.ResponseWriter, r *http.Request) bool {
	if !s.sameOrigin(w, r) {
		return false
	}
	csrf, err := r.Cookie(csrfCookie)
	if err != nil || csrf.Value == "" || r.Header.Get("X-CSRF-Token") == "" || !equal([]byte(csrf.Value), []byte(r.Header.Get("X-CSRF-Token"))) {
		forbidden(w)
		return false
	}
	return true
}
func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request, mutate bool) (registry.AdminCredential, bool) {
	if mutate {
		if !s.csrfOK(w, r) {
			return registry.AdminCredential{}, false
		}
	}
	c, err := r.Cookie(adminCookie)
	if err != nil {
		unauthorized(w)
		return registry.AdminCredential{}, false
	}
	credential, err := s.store.ValidateAdminSession(r.Context(), c.Value)
	if err != nil {
		unauthorized(w)
		return registry.AdminCredential{}, false
	}
	return credential, true
}
func (s *Server) readCeremony(r *http.Request, id uuid.UUID, purpose string) (registry.AdminCeremony, string, error) {
	c, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return registry.AdminCeremony{}, "", registry.ErrAdminCeremonyInvalid
	}
	v, err := s.store.ReadAdminCeremony(r.Context(), id, purpose, c.Value)
	return v, c.Value, err
}
func cookie(name, value string, httpOnly bool) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: httpOnly, SameSite: http.SameSiteStrictMode, MaxAge: 0}
}
func (s *Server) setCeremony(w http.ResponseWriter, c registry.AdminCeremony) {
	http.SetCookie(w, cookie(ceremonyCookie, c.Binding, true))
}
func clearCeremony(w http.ResponseWriter) {
	c := cookie(ceremonyCookie, "", true)
	c.MaxAge = -1
	http.SetCookie(w, c)
}
func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, cookie(adminCookie, token, true))
	s.setCSRF(w)
}
func (s *Server) setCSRF(w http.ResponseWriter) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err == nil {
		http.SetCookie(w, cookie(csrfCookie, base64.RawURLEncoding.EncodeToString(b), false))
	}
}
func (s *Server) clearSession(w http.ResponseWriter) {
	for _, name := range []string{adminCookie, csrfCookie} {
		c := cookie(name, "", name != csrfCookie)
		c.MaxAge = -1
		http.SetCookie(w, c)
	}
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		bad(w)
		return false
	}
	if d.Decode(&struct{}{}) != io.EOF {
		bad(w)
		return false
	}
	return true
}
func requestWithJSON(r *http.Request, b []byte) *http.Request {
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(strings.NewReader(string(b)))
	clone.ContentLength = int64(len(b))
	clone.Header.Set("Content-Type", "application/json")
	return clone
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func method(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, POST, DELETE")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
func bad(w http.ResponseWriter)       { http.Error(w, "invalid request", http.StatusBadRequest) }
func forbidden(w http.ResponseWriter) { http.Error(w, "forbidden", http.StatusForbidden) }
func unauthorized(w http.ResponseWriter) {
	http.Error(w, "authentication required", http.StatusUnauthorized)
}
func fail(w http.ResponseWriter, err error) {
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

type adminUser struct {
	handle []byte
	name   string
	creds  []wa.Credential
}

func (u adminUser) WebAuthnID() []byte                   { return u.handle }
func (u adminUser) WebAuthnName() string                 { return u.name }
func (u adminUser) WebAuthnDisplayName() string          { return u.name }
func (u adminUser) WebAuthnCredentials() []wa.Credential { return u.creds }
func decodeCredentials(records []registry.AdminCredential) []wa.Credential {
	out := make([]wa.Credential, 0, len(records))
	for _, r := range records {
		if r.RevokedAt != nil {
			continue
		}
		var c wa.Credential
		if json.Unmarshal(r.CredentialJSON, &c) == nil {
			out = append(out, c)
		}
	}
	return out
}
func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var n byte
	for i := range a {
		n |= a[i] ^ b[i]
	}
	return n == 0
}
