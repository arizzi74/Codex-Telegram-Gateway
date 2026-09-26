package admin

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

const freshAuthenticationTTL = 5 * time.Minute

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	cookie, _ := r.Cookie(adminCookie)
	info, err := s.store.AdminSessionInfo(r.Context(), cookie.Value)
	if err != nil {
		unauthorized(w)
		return
	}
	ownerID := sha256.Sum256(info.UserHandle)
	writeJSON(w, http.StatusOK, map[string]any{
		"owner_id":      base64.RawURLEncoding.EncodeToString(ownerID[:]),
		"authenticated": true, "session_id": info.ID, "server_time": time.Now().UTC(),
		"expires_at": info.ExpiresAt, "reauthenticated_at": info.ReauthenticatedAt,
	})
}

func (s *Server) requireFreshAuth(w http.ResponseWriter, r *http.Request) (registry.AdminCredential, bool) {
	credential, ok := s.requireAuth(w, r, true)
	if !ok {
		return registry.AdminCredential{}, false
	}
	cookie, _ := r.Cookie(adminCookie)
	info, err := s.store.AdminSessionInfo(r.Context(), cookie.Value)
	if err != nil {
		unauthorized(w)
		return registry.AdminCredential{}, false
	}
	if !freshAuthentication(info.ReauthenticatedAt, time.Now()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "reauthentication_required", "message": "Confirm with your passkey to continue."})
		return registry.AdminCredential{}, false
	}
	return credential, true
}

func freshAuthentication(verified, now time.Time) bool {
	age := now.Sub(verified)
	return age >= 0 && age < freshAuthenticationTTL
}

func (s *Server) browserSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	cookie, _ := r.Cookie(adminCookie)
	sessions, err := s.store.AdminBrowserSessions(r.Context(), cookie.Value)
	if err != nil {
		if errors.Is(err, registry.ErrAdminSessionInvalid) {
			unauthorized(w)
		} else {
			fail(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) browserSession(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/tgw/api/v1/admin/sessions/")
	var target *uuid.UUID
	if path == "revoke-all" {
		if r.Method != http.MethodPost {
			method(w)
			return
		}
	} else {
		if r.Method != http.MethodDelete {
			method(w)
			return
		}
		id, err := uuid.Parse(path)
		if err != nil {
			bad(w)
			return
		}
		target = &id
	}
	if _, ok := s.requireAuth(w, r, true); !ok {
		return
	}
	cookie, _ := r.Cookie(adminCookie)
	info, err := s.store.AdminSessionInfo(r.Context(), cookie.Value)
	if err != nil {
		unauthorized(w)
		return
	}
	if err = s.store.RevokeAdminBrowserSessions(r.Context(), cookie.Value, target); err != nil {
		if errors.Is(err, registry.ErrAdminSessionInvalid) {
			http.NotFound(w, r)
		} else {
			fail(w, err)
		}
		return
	}
	if target == nil || *target == info.ID {
		s.clearSession(w)
	}
	writeJSON(w, http.StatusNoContent, nil)
}
