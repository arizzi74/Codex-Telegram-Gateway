package admin

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Server) webuiSessionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, r.Method == http.MethodPost); !ok {
		return
	}
	var in struct {
		SessionID string `json:"session_id"`
		RequestID string `json:"request_id"`
		Confirmed bool   `json:"confirmed"`
	}
	if r.Method == http.MethodPost {
		if !decode(w, r, &in) {
			return
		}
		if !in.Confirmed {
			http.Error(w, "Confirm conversation deletion first. The working directory and files will be kept.", http.StatusBadRequest)
			return
		}
	} else {
		in.SessionID, in.RequestID = r.URL.Query().Get("session_id"), r.URL.Query().Get("request_id")
		commandID := r.URL.Query().Get("command_id")
		if in.RequestID == "" {
			in.RequestID = commandID
		} else if commandID != "" && commandID != in.RequestID {
			bad(w)
			return
		}
	}
	sessionID, sessionErr := uuid.Parse(in.SessionID)
	requestID, requestErr := uuid.Parse(in.RequestID)
	if sessionErr != nil || requestErr != nil || sessionID == uuid.Nil || requestID == uuid.Nil {
		bad(w)
		return
	}
	var result registry.WebUISessionDeletion
	var err error
	if r.Method == http.MethodPost {
		result, err = s.store.QueueWebUISessionDeletion(r.Context(), sessionID, requestID)
	} else {
		result, err = s.store.WebUISessionDeletion(r.Context(), sessionID, requestID)
	}
	if errors.Is(err, registry.ErrWebUIDeletionNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		var operationErr *protocol.Error
		if errors.As(err, &operationErr) {
			writeJSON(w, http.StatusConflict, map[string]string{"error_code": operationErr.Code, "message": operationErr.Message})
		} else {
			fail(w, err)
		}
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost && result.Pending {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}
