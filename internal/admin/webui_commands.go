package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/buildinfo"
	"github.com/iaia/telegramgw/internal/registry"
)

type webUICommandRequest struct {
	Command   string `json:"command"`
	SessionID string `json:"session_id,omitempty"`
}

type webUICommandInstance struct {
	WorkerID     string `json:"worker_id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	State        string `json:"state"`
	RuntimeID    string `json:"runtime_id,omitempty"`
	RuntimeName  string `json:"runtime_name,omitempty"`
	RuntimeState string `json:"runtime_state,omitempty"`
	CodexVersion string `json:"codex_version,omitempty"`
}

type webUICommandResponse struct {
	Command        string                        `json:"command"`
	Text           string                        `json:"text"`
	Workers        []registry.WorkerUpdateStatus `json:"workers,omitempty"`
	Instances      []webUICommandInstance        `json:"instances,omitempty"`
	GatewayVersion string                        `json:"gateway_version,omitempty"`
	Session        *registry.AdminSession        `json:"session,omitempty"`
}

// Gateway commands are deliberately separate from the session RPC relay. They
// can be used while a worker is offline, but never impersonate a Telegram user
// or alter Telegram's selected session. Only the named operations are accepted.
func (s *Server) webuiCommands(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, r.Method == http.MethodPost); !ok {
		return
	}
	var in webUICommandRequest
	if r.Method == http.MethodGet {
		in.Command, in.SessionID = r.URL.Query().Get("command"), r.URL.Query().Get("session_id")
		if in.Command == "" {
			writeJSON(w, http.StatusOK, map[string]any{"commands": []map[string]string{
				{"name": "tgstatus", "description": "Gateway, worker and queued update status"},
				{"name": "tginstances", "description": "List enrolled workers and Codex runtimes"},
				{"name": "tgupdateworkers", "description": "Queue updates for all workers at their next idle point"},
			}})
			return
		}
	} else if !decode(w, r, &in) {
		return
	}
	if in.Command != "tgstatus" && in.Command != "tginstances" && in.Command != "tgupdateworkers" {
		http.Error(w, "unsupported gateway command", http.StatusBadRequest)
		return
	}
	if in.Command == "tgupdateworkers" && r.Method != http.MethodPost {
		method(w)
		return
	}
	if in.SessionID != "" {
		if _, err := uuid.Parse(in.SessionID); err != nil {
			bad(w)
			return
		}
	}
	response := webUICommandResponse{Command: in.Command}
	if in.Command == "tgupdateworkers" {
		workers, err := s.store.QueueWebUIWorkerUpdates(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		response.Workers = workers
		if len(workers) == 0 {
			response.Text = "No enabled workers are enrolled."
		} else {
			lines := []string{"Worker updates"}
			for _, worker := range workers {
				lines = append(lines, worker.Name+": "+webUIWorkerUpdateText(worker))
			}
			lines = append(lines, "Each worker will check for an update at its next idle point, after its turns and pending work finish. Offline workers receive the request when they reconnect. Workers already on the latest version are not restarted. Use /tgstatus to check progress.")
			response.Text = strings.Join(lines, "\n")
		}
	} else {
		dashboard, err := s.store.AdminDashboardSnapshot(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		updates, err := s.store.WorkerUpdateSnapshot(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		response.Workers, response.GatewayVersion = updates, buildinfo.Version
		lines := []string{fmt.Sprintf("Gateway %s · %d workers · %d runtimes · %d sessions", buildinfo.Version, len(dashboard.Workers), len(dashboard.Runtimes), len(dashboard.Sessions))}
		for _, worker := range dashboard.Workers {
			state := worker.Connectivity
			if !worker.Enabled {
				state = "disabled"
			}
			instance := webUICommandInstance{WorkerID: worker.ID.String(), Name: worker.Name, Version: worker.Version, State: state}
			lines = append(lines, fmt.Sprintf("%s · %s · worker %s", worker.Name, state, worker.Version))
			withRuntime := false
			for _, runtime := range dashboard.Runtimes {
				if runtime.WorkerID != worker.ID.String() {
					continue
				}
				withRuntime = true
				instance.RuntimeID, instance.RuntimeName = runtime.ID, runtime.Name
				instance.RuntimeState, instance.CodexVersion = runtime.State, runtime.CodexVersion
				response.Instances = append(response.Instances, instance)
				lines = append(lines, fmt.Sprintf("  %s · %s · Codex %s", runtime.Name, runtime.State, runtime.CodexVersion))
			}
			if !withRuntime {
				response.Instances = append(response.Instances, instance)
			}
		}
		if in.Command == "tgstatus" {
			lines = append(lines, fmt.Sprintf("Pending approvals: %d · Queued commands: %d", dashboard.PendingApprovals, dashboard.QueuedCommands))
			for _, update := range updates {
				lines = append(lines, update.Name+" update: "+webUIWorkerUpdateText(update))
			}
			if in.SessionID != "" {
				for i := range dashboard.Sessions {
					if dashboard.Sessions[i].ID == in.SessionID {
						response.Session = &dashboard.Sessions[i]
						lines = append(lines, fmt.Sprintf("Selected session: %s · %s", response.Session.Name, response.Session.State))
						break
					}
				}
				if response.Session == nil {
					http.NotFound(w, r)
					return
				}
			}
		}
		response.Text = strings.Join(lines, "\n")
	}
	// Redact the complete allowlisted response, including version/name fields.
	// Never expose credential hashes, raw metadata, or Telegram subscriptions.
	raw, err := json.Marshal(response)
	if err != nil {
		fail(w, err)
		return
	}
	raw, err = s.redactWebUI(raw)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(raw))
}

func webUIWorkerUpdateText(worker registry.WorkerUpdateStatus) string {
	switch worker.State {
	case "queued":
		return "queued"
	case "already_queued":
		return "already queued"
	case "pending":
		return "queued; waiting for the worker to finish its update check"
	case "completed":
		return "updated and restarted · " + worker.Version
	case "up_to_date":
		return "up to date · " + worker.Version + " · no restart"
	case "failed":
		if worker.ErrorCode == "unsupported_worker" || worker.ErrorCode == "update_unavailable" {
			return "local update required: codex-telegramgw update worker"
		}
		return "update request failed; check the worker update service logs"
	default:
		return worker.State
	}
}
