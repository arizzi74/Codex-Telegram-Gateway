package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

const maxEnrollmentBody = 4096

func (s *Server) workerEnrollments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		method(w)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	var in protocol.CreateWorkerEnrollmentRequest
	if !decodeEnrollmentJSON(w, r, &in, true) {
		return
	}
	enrollment, code, err := s.store.CreateWorkerEnrollment(r.Context(), in.ServiceAccess)
	if err != nil {
		if errors.Is(err, registry.ErrWorkerEnrollmentInput) {
			bad(w)
		} else {
			fail(w, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, protocol.CreateWorkerEnrollmentResponse{
		EnrollmentID: enrollment.ID.String(), EnrollmentURL: s.origin + "/tgw/enroll/#" + code,
		ExpiresAt: enrollment.ExpiresAt, ServiceAccess: enrollment.ServiceAccess,
	})
}

func (s *Server) workerEnrollment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		method(w)
		return
	}
	if _, ok := s.requireFreshAuth(w, r); !ok {
		return
	}
	id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/tgw/api/v1/admin/worker-enrollments/"))
	if err != nil || id == uuid.Nil {
		bad(w)
		return
	}
	if err = s.store.RevokeWorkerEnrollment(r.Context(), id); err != nil {
		if errors.Is(err, registry.ErrWorkerEnrollmentGone) {
			http.NotFound(w, r)
		} else {
			fail(w, err)
		}
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) redeemWorkerEnrollment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		method(w)
		return
	}
	if !s.allowLogin(w, r, s.enrollments) {
		return
	}
	// A command-line installer has no Origin. A browser request must come from
	// the configured origin, including its exact port.
	if origins := r.Header.Values("Origin"); len(origins) > 0 && (len(origins) != 1 || origins[0] != s.origin) {
		forbidden(w)
		return
	}
	var in protocol.RedeemWorkerEnrollmentRequest
	if !decodeEnrollmentJSON(w, r, &in, false) {
		return
	}
	result, err := s.store.RedeemWorkerEnrollment(r.Context(), registry.RedeemWorkerEnrollmentInput{Code: in.Code, Name: in.Name, OS: in.OS, Arch: in.Arch})
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrWorkerEnrollmentInput):
			bad(w)
		case errors.Is(err, registry.ErrWorkerEnrollmentGone):
			http.Error(w, "enrollment unavailable", http.StatusGone)
		default:
			fail(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, protocol.RedeemWorkerEnrollmentResponse{
		WorkerID: result.WorkerID.String(), Token: result.Token,
		GatewayURL:    "wss://" + strings.TrimPrefix(s.origin, "https://") + "/tgw/api/v1/workers/connect",
		ServiceAccess: result.ServiceAccess,
	})
}

func (s *Server) workerEnrollmentLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/tgw/enroll/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Enroll a worker</title><link rel="stylesheet" href="/tgw/admin/static/app.css"><body><main><section class="card"><h1>Enroll a worker</h1><p>Run the Codex Gateway worker installer on the computer you want to connect. Enter a name for the worker and paste the complete enrollment URL when prompted.</p><p>Run this command as your project account, without sudo:</p><p class="session-path">curl -fsSL https://raw.githubusercontent.com/arizzi74/Codex-Telegram-Gateway/main/scripts/install.sh | sh</p><p>The link expires after 10 minutes and can enroll one worker. Opening this page leaves the link available for the installer.</p><p>If the link has expired or has already been used, create a new enrollment link in the gateway admin console.</p><p><a href="/tgw/admin/">Open the admin console</a></p></section></main></body></html>`)
}

// Enrollment requests are deliberately small. Validate raw UTF-8 before JSON
// decoding, which would otherwise silently replace malformed name bytes.
func decodeEnrollmentJSON(w http.ResponseWriter, r *http.Request, dst any, optional bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxEnrollmentBody)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil || !utf8.Valid(body) {
		bad(w)
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 && optional {
		return true
	}
	if len(body) == 0 || body[0] != '{' {
		bad(w)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		bad(w)
		return false
	}
	return true
}
