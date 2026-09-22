package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

func (c *Connection) sendRedactedEnvelope(ctx context.Context, writes chan<- outbound, kind string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data, err = redactOutboundJSON(c.store.redactor.Load(), data)
	if err != nil {
		return err
	}
	return sendEnvelope(ctx, writes, kind, json.RawMessage(data))
}

func newWorkerRedactor(patterns []string) (*auth.Redactor, error) {
	return auth.NewRedactor(append([]string{`cwk_[a-fA-F0-9]{64}`, `sk-[A-Za-z0-9_-]{20,}`, `[0-9]{6,12}:[A-Za-z0-9_-]{30,}`}, patterns...), "")
}

// Configure the outbound boundary before starting producers or the transport.
// Upgrade/reconnect replay also sanitizes already queued events, preserving all
// event identities, sequence numbers, and local execution state.
func (s *Store) configureRedactor(redactor *auth.Redactor) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		s.redactor.Store(redactor)
		bucket := tx.Bucket(bucketOutbox)
		return bucket.ForEach(func(key, value []byte) error {
			var event protocol.Event
			if err := json.Unmarshal(value, &event); err != nil {
				return err
			}
			data, err := redactOutboundJSON(redactor, event.Data)
			if err != nil {
				return err
			}
			if bytes.Equal(data, event.Data) {
				return nil
			}
			event.Data = data
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			return bucket.Put(key, encoded)
		})
	})
}

func (s *Store) appendEvent(tx *workerdb.Tx, event protocol.Event) (protocol.Event, error) {
	var err error
	event.Data, err = redactOutboundJSON(s.redactor.Load(), event.Data)
	if err != nil {
		return protocol.Event{}, err
	}
	return appendEvent(tx, event)
}

// Work on a separate wire projection: IDs, enums and executable arguments must
// not change, and raw session paths/question choices remain available locally.
// The same policy covers nested results, discovery, question recovery, hello,
// heartbeat, and command acknowledgement payloads.
func redactOutboundJSON(redactor *auth.Redactor, data json.RawMessage) (json.RawMessage, error) {
	if redactor == nil || len(data) == 0 {
		return data, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("worker outbound payload: %w", err)
	}
	redactOutboundValue(redactor, value)
	return json.Marshal(value)
}

func redactOutboundValue(redactor *auth.Redactor, value any) {
	switch value := value.(type) {
	case []any:
		for _, entry := range value {
			redactOutboundValue(redactor, entry)
		}
	case map[string]any:
		for key, field := range value {
			switch key {
			case "name", "preview", "git_branch", "git_root", "text", "message", "summary", "header", "prompt", "label", "description", "last_message", "model", "reasoning_effort", "worker_name", "hostname", "codex_version", "local_socket":
				if text, ok := field.(string); ok {
					value[key] = redactor.Redact(text)
				}
			case "options":
				if entries, ok := field.([]any); ok {
					labels := make([]string, len(entries))
					allLabels := true
					for i, entry := range entries {
						var ok bool
						labels[i], ok = entry.(string)
						allLabels = allLabels && ok
					}
					if allLabels {
						for i, label := range redactedQuestionOptions(redactor, labels) {
							entries[i] = label
						}
					}
				}
			case "answers":
				if answers, ok := field.(map[string]any); ok {
					secret := make(map[string]bool)
					if questions, ok := value["questions"].([]any); ok {
						for _, question := range questions {
							if question, ok := question.(map[string]any); ok {
								id, _ := question["id"].(string)
								secret[id], _ = question["secret"].(bool)
							}
						}
					}
					for id, answers := range answers {
						if answers, ok := answers.([]any); ok {
							for i, answer := range answers {
								if text, ok := answer.(string); ok {
									answers[i] = redactor.Redact(text)
									if secret[id] {
										answers[i] = "[REDACTED]"
									}
								}
							}
						}
					}
				}
			}
			redactOutboundValue(redactor, field)
		}
	}
}

// Number colliding labels so two different raw options remain selectable even
// when custom rules redact both to the same visible text. The raw values never
// enter the gateway; input resolves these labels against the local approval.
func redactedQuestionOptions(redactor *auth.Redactor, options []string) []string {
	labels := make([]string, len(options))
	seen := make(map[string]bool, len(options))
	collision := false
	for i, option := range options {
		labels[i] = redactor.Redact(option)
		collision = collision || seen[labels[i]]
		seen[labels[i]] = true
	}
	if collision {
		for i := range labels {
			labels[i] = fmt.Sprintf("%s (option %d)", labels[i], i+1)
		}
	}
	return labels
}

func originalQuestionAnswers(redactor *auth.Redactor, approval protocol.Approval, answers map[string][]string) map[string][]string {
	resolved := make(map[string][]string, len(answers))
	for id, values := range answers {
		resolved[id] = append([]string(nil), values...)
	}
	for _, question := range approval.Questions {
		labels := redactedQuestionOptions(redactor, question.Options)
		for i, answer := range resolved[question.ID] {
			for j, label := range labels {
				if answer == label {
					resolved[question.ID][i] = question.Options[j]
					break
				}
			}
		}
	}
	return resolved
}
