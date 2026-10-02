package admin

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type webUICreationTargetInput struct {
	WorkerID          string `json:"worker_id"`
	RuntimeID         string `json:"runtime_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
	RequestID         string `json:"request_id"`
}

func (in webUICreationTargetInput) target() (registry.WebUICreationTarget, bool) {
	workerID, workerErr := uuid.Parse(in.WorkerID)
	runtimeID, runtimeErr := uuid.Parse(in.RuntimeID)
	requestID, requestErr := uuid.Parse(in.RequestID)
	target := registry.WebUICreationTarget{WorkerID: workerID, RuntimeID: runtimeID, RuntimeGeneration: in.RuntimeGeneration, RequestID: requestID}
	return target, workerErr == nil && runtimeErr == nil && requestErr == nil && workerID != uuid.Nil && runtimeID != uuid.Nil && requestID != uuid.Nil && in.RuntimeGeneration > 0 && in.RuntimeGeneration <= math.MaxInt64
}

func (s *Server) webuiSessionNew(w http.ResponseWriter, r *http.Request) {
	s.webuiCreation(w, r, protocol.NewSession)
}

func (s *Server) webuiWorkspaces(w http.ResponseWriter, r *http.Request) {
	s.webuiCreation(w, r, protocol.BrowseWorkspace)
}

func (s *Server) webuiCreation(w http.ResponseWriter, r *http.Request, operation protocol.Operation) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, r.Method == http.MethodPost); !ok {
		return
	}
	var result registry.WebUICreationResult
	var err error
	if r.Method == http.MethodGet {
		workerID, workerErr := uuid.Parse(r.URL.Query().Get("worker_id"))
		request := r.URL.Query().Get("request_id")
		command := r.URL.Query().Get("command_id")
		if request == "" {
			request = command
		} else if command != "" && command != request {
			bad(w)
			return
		}
		requestID, requestErr := uuid.Parse(request)
		if workerErr != nil || requestErr != nil || workerID == uuid.Nil || requestID == uuid.Nil {
			bad(w)
			return
		}
		result, err = s.store.WebUICreation(r.Context(), workerID, requestID, operation)
	} else if operation == protocol.NewSession {
		var in struct {
			webUICreationTargetInput
			Name string `json:"name"`
			CWD  string `json:"cwd"`
		}
		if !decode(w, r, &in) {
			return
		}
		target, ok := in.webUICreationTargetInput.target()
		if !ok {
			bad(w)
			return
		}
		result, err = s.store.QueueWebUISessionCreation(r.Context(), target, in.Name, in.CWD)
	} else {
		var in struct {
			webUICreationTargetInput
			Path   string `json:"path,omitempty"`
			Offset int    `json:"offset,omitempty"`
		}
		if !decode(w, r, &in) {
			return
		}
		target, ok := in.webUICreationTargetInput.target()
		request := protocol.WorkspaceRequest{Path: in.Path, Offset: in.Offset}
		if !ok || request.Validate() != nil {
			bad(w)
			return
		}
		result, err = s.store.QueueWebUIWorkspace(r.Context(), target, request)
	}
	if errors.Is(err, registry.ErrWebUICreationNotFound) {
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
	raw, err := json.Marshal(result)
	if err == nil {
		raw, err = s.redactWebUI(raw)
	}
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost && result.Pending {
		status = http.StatusAccepted
	}
	writeJSON(w, status, json.RawMessage(raw))
}
