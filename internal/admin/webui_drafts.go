package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Server) webuiDraftRoutes() { s.mux.HandleFunc("/tgw/api/v1/webui/drafts/", s.webuiDraft) }

func (s *Server) webuiDraft(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, r.Method != http.MethodGet); !ok {
		return
	}
	part := strings.TrimPrefix(r.URL.Path, "/tgw/api/v1/webui/drafts/")
	id, err := uuid.Parse(part)
	if err != nil || id == uuid.Nil || id.String() != part {
		bad(w)
		return
	}
	token, _ := r.Cookie(adminCookie)
	if r.Method == http.MethodGet {
		draft, err := s.store.GetAdminDraft(r.Context(), token.Value, id)
		if err != nil {
			s.webuiDraftError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, draft)
		return
	}
	var in struct {
		Ciphertext string `json:"ciphertext,omitempty"`
		Revision   int64  `json:"revision"`
	}
	limit := int64(registry.AdminDraftMaxBytes*4/3 + 2048)
	if r.Method == http.MethodDelete {
		limit = 1024
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		bad(w)
		return
	}
	if r.Method == http.MethodDelete {
		if in.Ciphertext != "" {
			bad(w)
			return
		}
		if err := s.store.DeleteAdminDraft(r.Context(), token.Value, id, in.Revision); err != nil {
			s.webuiDraftError(w, err)
			return
		}
		writeJSON(w, http.StatusNoContent, nil)
		return
	}
	draft, err := s.store.PutAdminDraft(r.Context(), token.Value, id, in.Revision, in.Ciphertext)
	if err != nil {
		s.webuiDraftError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": draft.Revision, "expires_at": draft.ExpiresAt})
}

func (s *Server) webuiDraftError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrAdminSessionInvalid):
		unauthorized(w)
	case errors.Is(err, registry.ErrAdminDraftInvalid):
		bad(w)
	case errors.Is(err, registry.ErrAdminDraftNotFound):
		http.NotFound(w, nil)
	case errors.Is(err, registry.ErrAdminDraftConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"message": "This recovery copy was cleared or replaced."})
	case errors.Is(err, registry.ErrAdminDraftLimit):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "Text draft recovery is temporarily full. Try again later."})
	default:
		fail(w, err)
	}
}
