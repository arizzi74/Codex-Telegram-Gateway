package protocol

import "time"

// SessionStats summarizes the saved user conversation, including input from
// native clients. Missing counters mean unavailable; incomplete history makes
// message counts lower bounds. Token totals are the latest recorded usage.
type SessionStats struct {
	CreatedAt             *time.Time `json:"created_at,omitempty"`
	PromptCount           *int64     `json:"prompt_count,omitempty"`
	AssistantMessageCount *int64     `json:"assistant_message_count,omitempty"`
	TotalTokens           *int64     `json:"total_tokens,omitempty"`
	InputTokens           *int64     `json:"input_tokens,omitempty"`
	CachedInputTokens     *int64     `json:"cached_input_tokens,omitempty"`
	OutputTokens          *int64     `json:"output_tokens,omitempty"`
	ReasoningOutputTokens *int64     `json:"reasoning_output_tokens,omitempty"`
	ContextTokens         *int64     `json:"context_tokens,omitempty"`
	ContextWindow         *int64     `json:"context_window,omitempty"`
	ActiveSince           *time.Time `json:"active_since,omitempty"`
	LastMessageAt         *time.Time `json:"last_message_at,omitempty"`
	LastMessageRole       string     `json:"last_message_role,omitempty"`
	LastMessage           string     `json:"last_message,omitempty"`
	Model                 string     `json:"model,omitempty"`
	ReasoningEffort       string     `json:"reasoning_effort,omitempty"`
	ObservedAt            time.Time  `json:"observed_at"`
	HistoryComplete       bool       `json:"history_complete"`
}
