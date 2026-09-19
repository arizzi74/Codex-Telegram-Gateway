package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Runtime struct {
	ID           string `json:"runtime_id"`
	WorkerID     string `json:"worker_id,omitempty"`
	ProfileID    string `json:"profile_id"`
	Name         string `json:"name"`
	Generation   uint64 `json:"generation"`
	PID          int    `json:"pid"`
	State        string `json:"state"`
	CodexVersion string `json:"codex_version,omitempty"`
	DefaultCWD   string `json:"default_cwd,omitempty"`
	LocalSocket  string `json:"local_socket,omitempty"`
}

type Session struct {
	ID           string        `json:"session_id"`
	WorkerID     string        `json:"worker_id"`
	RuntimeID    string        `json:"runtime_id"`
	ThreadID     string        `json:"codex_thread_id"`
	Name         string        `json:"name"`
	Preview      string        `json:"preview,omitempty"`
	CWD          string        `json:"cwd"`
	GitBranch    string        `json:"git_branch,omitempty"`
	GitRoot      string        `json:"git_root,omitempty"`
	State        string        `json:"state"`
	ActiveTurnID string        `json:"active_turn_id,omitempty"`
	Loaded       bool          `json:"loaded"`
	Archived     bool          `json:"archived"`
	Deleted      bool          `json:"deleted,omitempty"`
	UpdatedAt    time.Time     `json:"updated_at"`
	Stats        *SessionStats `json:"stats,omitempty"`
}

type Hello struct {
	WorkerID                    string    `json:"worker_id"`
	WorkerName                  string    `json:"worker_name"`
	Hostname                    string    `json:"hostname"`
	OS                          string    `json:"os"`
	Arch                        string    `json:"arch"`
	WorkerVersion               string    `json:"worker_version"`
	SupportsImageInput          bool      `json:"supports_image_input,omitempty"`
	SupportsSessionWorkspaces   bool      `json:"supports_session_workspaces,omitempty"`
	SupportsSessionDeletion     bool      `json:"supports_session_deletion,omitempty"`
	SupportsConversationHistory bool      `json:"supports_conversation_history,omitempty"`
	ProtocolMin                 int       `json:"protocol_min"`
	ProtocolMax                 int       `json:"protocol_max"`
	LastAckedEventSeq           uint64    `json:"last_acked_event_seq"`
	Runtimes                    []Runtime `json:"runtimes"`
}

type HelloAck struct {
	ConnectionID             string `json:"connection_id"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	ResumeFromEventSeq       uint64 `json:"resume_from_event_seq"`
}

type Heartbeat struct {
	WorkerID                    string    `json:"worker_id"`
	SupportsImageInput          bool      `json:"supports_image_input,omitempty"`
	SupportsSessionWorkspaces   bool      `json:"supports_session_workspaces,omitempty"`
	SupportsSessionDeletion     bool      `json:"supports_session_deletion,omitempty"`
	SupportsConversationHistory bool      `json:"supports_conversation_history,omitempty"`
	UptimeSeconds               int64     `json:"uptime_seconds"`
	Runtimes                    []Runtime `json:"runtimes"`
}

type Operation string

const (
	StartTurn        Operation = "start_turn"
	NewSession       Operation = "new_session"
	Steer            Operation = "steer"
	Interrupt        Operation = "interrupt"
	ApprovalResponse Operation = "approval_response"
	InputResponse    Operation = "input_response"
	CodexCommand     Operation = "codex_command"
	ReadHistory      Operation = "read_history"
	BrowseWorkspace  Operation = "browse_workspace"
	DeleteSession    Operation = "delete_session"
)

// Command contains an immutable execution target. No dispatch path may consult a
// mutable selection to replace any of these fields.
type Command struct {
	ID                string    `json:"command_id"`
	WorkerID          string    `json:"worker_id"`
	RuntimeID         string    `json:"runtime_id"`
	RuntimeGeneration uint64    `json:"runtime_generation"`
	SessionID         string    `json:"session_id,omitempty"`
	ThreadID          string    `json:"codex_thread_id,omitempty"`
	Operation         Operation `json:"operation"`
	ExpectedTurnID    string    `json:"expected_turn_id,omitempty"`
	Arguments         Arguments `json:"arguments"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type Arguments struct {
	Workspace         *WorkspaceRequest    `json:"workspace,omitempty"`
	SessionName       string               `json:"session_name,omitempty"`
	CreateDirectory   bool                 `json:"create_directory,omitempty"`
	History           *HistoryRequest      `json:"history,omitempty"`
	SelectionRevision *uint64              `json:"selection_revision,omitempty"`
	Codex             *CodexCommandPayload `json:"codex,omitempty"`
	CWD               string               `json:"cwd,omitempty"`
	Text              string               `json:"text,omitempty"`
	Images            []Image              `json:"images,omitempty"`
	ApprovalID        string               `json:"approval_id,omitempty"`
	RequestID         string               `json:"request_id,omitempty"`
	Decision          string               `json:"decision,omitempty"`
	Answers           map[string][]string  `json:"answers,omitempty"`
}

// CodexCommandPayload names a client command, never an arbitrary RPC method.
type CodexCommandPayload struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

func (c Command) Validate() error {
	for _, id := range []string{c.ID, c.WorkerID, c.RuntimeID} {
		if _, err := uuid.Parse(id); err != nil {
			return errors.New("invalid command identity")
		}
	}
	if c.RuntimeGeneration == 0 {
		return errors.New("missing runtime generation")
	}
	if c.CreatedAt.IsZero() || c.ExpiresAt.IsZero() || !c.ExpiresAt.After(c.CreatedAt) {
		return errors.New("invalid command lifetime")
	}
	switch c.Operation {
	case NewSession, BrowseWorkspace:
		if c.SessionID != "" || c.ThreadID != "" {
			return errors.New("new session cannot target an existing thread")
		}
	case StartTurn, Steer, Interrupt, ApprovalResponse, InputResponse, CodexCommand, ReadHistory, DeleteSession:
		if _, err := uuid.Parse(c.SessionID); err != nil || c.ThreadID == "" {
			return errors.New("missing session target")
		}
	default:
		return &Error{Code: UnsupportedOperation, Message: "The worker does not support this operation. Update the worker and try again."}
	}
	if (c.Operation == Steer || c.Operation == Interrupt) && c.ExpectedTurnID == "" {
		return errors.New("missing expected turn")
	}
	if err := ValidateImages(c.Arguments.Images); err != nil {
		return err
	}
	if len(c.Arguments.Images) > 0 && c.Operation != StartTurn && c.Operation != Steer {
		return errors.New("images require a turn input operation")
	}
	if (c.Operation == StartTurn || c.Operation == Steer) && c.Arguments.Text == "" && len(c.Arguments.Images) == 0 {
		return errors.New("missing input")
	}
	if (c.Operation == ApprovalResponse || c.Operation == InputResponse) && (c.Arguments.RequestID == "" || c.Arguments.ApprovalID == "") {
		return errors.New("missing request target")
	}
	if c.Operation == CodexCommand {
		if c.Arguments.Codex == nil || len(c.Arguments.Codex.Name) == 0 || len(c.Arguments.Codex.Name) > 64 || len(c.Arguments.Codex.Args) > 16384 {
			return errors.New("invalid Codex command")
		}
		for _, ch := range c.Arguments.Codex.Name {
			if (ch < 'a' || ch > 'z') && ch != '-' {
				return errors.New("invalid Codex command name")
			}
		}
	}
	if c.Operation == ReadHistory {
		if err := c.Arguments.History.Validate(); err != nil {
			return err
		}
	}
	if c.Operation == BrowseWorkspace {
		if err := c.Arguments.Workspace.Validate(); err != nil {
			return err
		}
	} else if c.Arguments.Workspace != nil {
		return errors.New("workspace browser arguments require browse_workspace")
	}
	if c.Arguments.CreateDirectory || c.Arguments.SessionName != "" {
		if c.Operation != NewSession {
			return errors.New("session creation arguments require new_session")
		}
		if _, err := NormalizeSessionName(c.Arguments.SessionName); err != nil {
			return err
		}
		if c.Arguments.CreateDirectory && strings.TrimSpace(c.Arguments.CWD) == "" {
			return errors.New("directory creation requires a selected parent folder")
		}
	}
	return nil
}

type CommandAck struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
	Error     *Error `json:"error,omitempty"`
}

type Event struct {
	Seq               uint64          `json:"event_seq"`
	ID                string          `json:"event_id"`
	WorkerID          string          `json:"worker_id"`
	RuntimeID         string          `json:"runtime_id,omitempty"`
	RuntimeGeneration uint64          `json:"runtime_generation,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	Kind              string          `json:"kind"`
	OccurredAt        time.Time       `json:"occurred_at"`
	Data              json.RawMessage `json:"data"`
}

func (e Event) Durable() bool {
	switch e.Kind {
	case "agent_message_delta", "command_output_delta", "tool_progress", "token_usage_partial":
		return false
	default:
		return true // Unknown events fail toward preserving data.
	}
}

type EventAck struct {
	Seq uint64 `json:"event_seq"`
}

type Result struct {
	Workspace *WorkspacePage `json:"workspace,omitempty"`
	History   *HistoryPage   `json:"history,omitempty"`
	CommandID string         `json:"command_id,omitempty"`
	TurnID    string         `json:"turn_id,omitempty"`
	Text      string         `json:"text,omitempty"`
	State     string         `json:"state,omitempty"`
	Error     *Error         `json:"error,omitempty"`
	Session   *Session       `json:"session,omitempty"`
}

const DefaultHistoryLimit = 10
const DefaultLastMessagesLimit = 1
const MaxHistoryLimit = 50

// HistoryCursor identifies an item within its turn. History reads never submit
// these saved prompts as input or change the thread's execution state.
type HistoryCursor struct {
	TurnID string `json:"turn_id"`
	ItemID string `json:"item_id"`
}

type HistoryRequest struct {
	Limit    int            `json:"limit"`
	Before   *HistoryCursor `json:"before,omitempty"`
	Messages bool           `json:"messages,omitempty"`
}

func (h *HistoryRequest) Validate() error {
	if h == nil || h.Limit < 1 || h.Limit > MaxHistoryLimit {
		return errors.New("invalid history page size")
	}
	if h.Before != nil && (strings.TrimSpace(h.Before.TurnID) == "" || strings.TrimSpace(h.Before.ItemID) == "" || len(h.Before.TurnID) > 512 || len(h.Before.ItemID) > 512) {
		return errors.New("invalid history cursor")
	}
	return nil
}

type HistoryPrompt struct {
	TurnID    string `json:"turn_id"`
	ItemID    string `json:"item_id"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

type HistoryPage struct {
	Prompts      []HistoryPrompt  `json:"prompts"`
	Messages     []HistoryMessage `json:"messages,omitempty"`
	Conversation bool             `json:"conversation,omitempty"`
	Next         *HistoryCursor   `json:"next,omitempty"`
	Limit        int              `json:"limit"`
}

// HistoryMessage is user-visible conversation text. Reasoning and tool items
// are never part of this projection. Role is either user or assistant.
type HistoryMessage struct {
	TurnID    string `json:"turn_id"`
	ItemID    string `json:"item_id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

type Approval struct {
	ID        string     `json:"approval_id"`
	RequestID string     `json:"request_id"`
	ThreadID  string     `json:"thread_id"`
	TurnID    string     `json:"turn_id,omitempty"`
	ItemID    string     `json:"item_id,omitempty"`
	Type      string     `json:"approval_type"`
	Summary   string     `json:"summary"`
	Decisions []string   `json:"decisions,omitempty"`
	Questions []Question `json:"questions,omitempty"`
	State     string     `json:"state,omitempty"`
}

type Question struct {
	ID      string   `json:"id"`
	Header  string   `json:"header,omitempty"`
	Prompt  string   `json:"prompt"`
	Options []string `json:"options,omitempty"`
	Secret  bool     `json:"secret,omitempty"`
}
