package protocol

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

const (
	Unauthorized           = "unauthorized"
	UnsupportedProtocol    = "unsupported_protocol"
	UnsupportedOperation   = "unsupported_operation"
	UnknownWorker          = "unknown_worker"
	WorkerDisabled         = "worker_disabled"
	UnknownRuntime         = "unknown_runtime"
	StaleRuntime           = "stale_runtime"
	UnknownSession         = "unknown_session"
	SessionNotAvailable    = "session_not_available"
	SessionBusy            = "session_busy"
	SessionQueueFull       = "session_queue_full"
	StaleTurn              = "stale_turn"
	ApprovalNotPending     = "approval_not_pending"
	CommandExpired         = "command_expired"
	InvalidWorkspace       = "invalid_workspace"
	CodexUnavailable       = "codex_unavailable"
	CodexProtocolError     = "codex_protocol_error"
	CodexMethodUnsupported = "codex_method_unsupported"
	CodexCommandInvalid    = "codex_command_invalid"
	OutcomeUnknown         = "outcome_unknown"
	InternalError          = "internal_error"
)
