package admin

import (
	"errors"
	"net/http"

	"github.com/iaia/telegramgw/internal/registry"
)

// loginOptions deliberately exposes neither the account name nor its credential
// record. Password access is opt-in; first-owner setup still uses a passkey.
func (s *Server) loginOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	settings, err := s.store.AdminPasswordSettings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"passkey": true, "password": settings.Enabled})
}

func (s *Server) passwordLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		method(w)
		return
	}
	if !s.csrfOK(w, r) || !s.allowLogin(w, r, s.passwordLogins) {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &input) || !s.acquirePasswordSlot(w) {
		return
	}
	defer s.releasePasswordSlot()
	predecessor := ""
	if cookie, err := r.Cookie(adminCookie); err == nil {
		predecessor = cookie.Value
	}
	token, err := s.store.LoginAdminPassword(r.Context(), input.Username, input.Password, predecessor, r.UserAgent())
	if err != nil {
		if errors.Is(err, registry.ErrAdminPasswordInvalid) || errors.Is(err, registry.ErrAdminPasswordPolicy) || errors.Is(err, registry.ErrAdminSessionInvalid) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "invalid_credentials", "message": "Username or password is incorrect, or password sign-in is disabled."})
			return
		}
		fail(w, err)
		return
	}
	s.setSession(w, token)
	clearCeremony(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) passwordSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := s.requireAuth(w, r, false); !ok {
			return
		}
	} else {
		if _, ok := s.requireFreshAuth(w, r); !ok {
			return
		}
		cookie, _ := r.Cookie(adminCookie)
		var err error
		if r.Method == http.MethodDelete {
			err = s.store.DisableAdminPassword(r.Context(), cookie.Value)
		} else {
			var input struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			if !decode(w, r, &input) || !s.acquirePasswordSlot(w) {
				return
			}
			err = s.store.SetAdminPassword(r.Context(), cookie.Value, input.Username, input.Password)
			s.releasePasswordSlot()
		}
		if err != nil {
			switch {
			case errors.Is(err, registry.ErrAdminPasswordPolicy):
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "password_policy", "message": "Use a username of 1–64 bytes without control characters and a password of 12–256 bytes."})
			case errors.Is(err, registry.ErrAdminReauthenticationRequired):
				writeJSON(w, http.StatusForbidden, map[string]string{"code": "reauthentication_required", "message": "Sign in again to continue."})
			case errors.Is(err, registry.ErrAdminSessionInvalid):
				unauthorized(w)
			default:
				fail(w, err)
			}
			return
		}
		// Replacing or disabling a password revokes sessions authenticated with
		// its previous credential. Clear this browser's cookie if it was one.
		if _, err := s.store.ValidateAdminSession(r.Context(), cookie.Value); errors.Is(err, registry.ErrAdminSessionInvalid) {
			s.clearSession(w)
			// Keep same-page sign-in usable after the credential change. The
			// retired session is gone, but authentication still needs CSRF.
			s.setCSRF(w)
		}
		if r.Method == http.MethodDelete {
			writeJSON(w, http.StatusNoContent, nil)
			return
		}
	}
	settings, err := s.store.AdminPasswordSettings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// Rate limits bound total hashing work; admission additionally caps memory use
// when several clients arrive together. Never queue unbounded Argon2 work.
func (s *Server) acquirePasswordSlot(w http.ResponseWriter) bool {
	select {
	case s.passwordSlots <- struct{}{}:
		return true
	default:
		tooManyRequests(w)
		return false
	}
}

func (s *Server) releasePasswordSlot() { <-s.passwordSlots }
