package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type DeliveryStore interface {
	ClaimDeliveries(context.Context, int) ([]registry.Delivery, error)
	DeliveryChunks(context.Context, string) ([]registry.DeliveryChunk, error)
	ExtendDelivery(context.Context, string) error
	PrepareDeliveryChunks(context.Context, string, []json.RawMessage) ([]registry.DeliveryChunk, error)
	MarkDeliveryChunkSent(context.Context, string, int, int64, string, string, string, ...string) error
	RetryDelivery(context.Context, string, time.Duration, string) error
	SessionSnapshot(context.Context) ([]protocol.Session, error)
	RuntimeSnapshot(context.Context) ([]protocol.Runtime, error)
	CreateCallback(context.Context, registry.Callback) (string, error)
	ListWorkers(context.Context) ([]registry.Worker, error)
	TelegramSessionStatus(context.Context, uuid.UUID) (registry.SessionStatus, error)
	PendingApproval(context.Context, uuid.UUID) (protocol.Approval, error)
}
type SenderOptions struct {
	BotID    string
	OwnerID  int64
	Redactor *auth.Redactor
}

// SessionDeliveryRepairStore supports upgrading oversized, previously frozen
// session pickers without replaying messages already accepted by Telegram.
type SessionDeliveryRepairStore interface {
	ReplaceUnsentSessionDeliveryChunks(context.Context, string, int, []json.RawMessage) ([]registry.DeliveryChunk, error)
}
type Sender struct {
	store   DeliveryStore
	api     TelegramAPI
	log     *slog.Logger
	options SenderOptions
}

func NewSender(store DeliveryStore, api TelegramAPI, logger *slog.Logger, options ...SenderOptions) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	var option SenderOptions
	if len(options) > 0 {
		option = options[0]
	}
	return &Sender{store: store, api: api, log: logger, options: option}
}
func (s *Sender) Run(ctx context.Context) error {
	// Deletion retries have their own loop so a slow cleanup request cannot
	// delay delivery of a final answer or an approval request.
	var cleanup sync.WaitGroup
	if store, ok := s.store.(TelegramProgressStore); ok {
		if api, ok := s.api.(TelegramDeleteAPI); ok {
			cleanup.Add(1)
			go func() {
				defer cleanup.Done()
				s.runDeletions(ctx, store, api)
			}()
		}
	}
	defer cleanup.Wait()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := s.flush(ctx); err != nil {
			s.log.Warn("telegram delivery flush", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (s *Sender) flush(ctx context.Context) error {
	// A single short lease avoids a slow Telegram request stranding a batch.
	rows, err := s.store.ClaimDeliveries(ctx, 1)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := s.sendDelivery(ctx, row); err != nil {
			if errors.Is(err, registry.ErrDeliveryLeaseChanged) {
				return err
			}
			delay := telegramRetryDelay(row.Attempt, err)
			if retryErr := s.store.RetryDelivery(ctx, row.ID, delay, "Telegram delivery deferred"); retryErr != nil {
				return retryErr
			}
			return err
		}
	}
	return nil
}

func (s *Sender) sendDelivery(ctx context.Context, row registry.Delivery) error {
	if skip, err := s.skipProgress(ctx, row); err != nil || skip {
		return err
	}
	checkpoints, err := s.store.DeliveryChunks(ctx, row.ID)
	if err != nil {
		return err
	}
	repair := oversizedSessionDelivery(row, checkpoints)
	if len(checkpoints) == 0 || repair {
		messages, err := s.renderDeliveryMessages(ctx, row)
		if err != nil {
			return err
		}
		if repair {
			store, ok := s.store.(SessionDeliveryRepairStore)
			if !ok {
				return errors.New("Telegram session delivery needs keyboard repair")
			}
			checkpoints, err = store.ReplaceUnsentSessionDeliveryChunks(ctx, row.ID, row.Attempt, messages)
		} else {
			checkpoints, err = s.store.PrepareDeliveryChunks(ctx, row.ID, messages)
		}
		if err != nil {
			return err
		}
	}
	sessionID, turnID, approvalID := deliveryRoute(row)
	for _, chunk := range checkpoints {
		if chunk.Sent {
			continue
		}
		if skip, err := s.skipProgress(ctx, row); err != nil || skip {
			return err
		}
		if err := s.store.ExtendDelivery(ctx, row.ID); err != nil {
			return err
		}
		var message SendMessage
		if err := json.Unmarshal(chunk.Payload, &message); err != nil {
			return err
		}
		sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		var id int64
		var err error
		if row.Kind == "tool_progress_message" {
			id, err = s.sendToolProgress(sendCtx, row, message)
		} else {
			id, err = s.api.Send(sendCtx, message)
		}
		cancel()
		if err != nil {
			return err
		}
		if id <= 0 {
			return errors.New("Telegram returned no message identity")
		}
		if err := s.store.MarkDeliveryChunkSent(ctx, row.ID, chunk.Index, id, sessionID, turnID, approvalID, deliveryQuestion(row)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sender) renderDeliveryMessages(ctx context.Context, row registry.Delivery) ([]json.RawMessage, error) {
	renderCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	parts, keyboard, err := s.renderDeliveryParts(renderCtx, row)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, errors.New("empty Telegram delivery")
	}
	messages := make([]json.RawMessage, 0, len(parts))
	for index, part := range parts {
		message := SendMessage{ChatID: row.ChatID, TopicID: row.TopicID, Text: part, DisableNotification: isProgressDelivery(row.Kind)}
		if row.Kind == "tool_progress_message" {
			message.Entities = []TelegramEntity{{Type: "pre", Length: telegramTextLength(part)}}
		}
		// Put controls after their complete explanation.
		if index == len(parts)-1 {
			message.Keyboard = keyboard
		}
		raw, err := json.Marshal(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, raw)
	}
	return messages, nil
}

func oversizedSessionDelivery(row registry.Delivery, chunks []registry.DeliveryChunk) bool {
	if row.Kind != "ui_response" {
		return false
	}
	var response registry.AcceptResult
	if json.Unmarshal(row.Payload, &response) != nil || response.View != "sessions" || response.ErrorCode != "" {
		return false
	}
	for _, chunk := range chunks {
		if chunk.Sent {
			continue
		}
		var message SendMessage
		if json.Unmarshal(chunk.Payload, &message) != nil || message.Keyboard == nil {
			continue
		}
		markup, err := json.Marshal(message.Keyboard)
		if err != nil {
			continue
		}
		buttons := 0
		for _, row := range message.Keyboard.Rows {
			buttons += len(row)
		}
		if len(markup) > sessionKeyboardMaxBytes || buttons > 2*sessionPageSize+3 {
			return true
		}
	}
	return false
}

func telegramRetryDelay(attempt int, err error) time.Duration {
	delay := min(time.Second<<min(max(attempt-1, 0), 6), time.Minute)
	var telegram *TelegramError
	if errors.As(err, &telegram) && telegram.RetryAfter > 0 {
		delay = telegram.RetryAfter
	}
	return delay
}

func deliveryRoute(row registry.Delivery) (sessionID, turnID, approvalID string) {
	if row.Kind == "ui_response" {
		var v struct {
			SessionID  string `json:"session_id"`
			ApprovalID string `json:"approval_id"`
		}
		_ = json.Unmarshal(row.Payload, &v)
		return v.SessionID, "", v.ApprovalID
	}
	var event protocol.Event
	if json.Unmarshal(row.Payload, &event) != nil {
		return "", "", ""
	}
	sessionID = event.SessionID
	var approval protocol.Approval
	if event.Kind == "approval_requested" || event.Kind == "user_input_requested" {
		if json.Unmarshal(event.Data, &approval) == nil {
			return sessionID, approval.TurnID, approval.ID
		}
	}
	var result protocol.Result
	if json.Unmarshal(event.Data, &result) == nil {
		turnID = result.TurnID
		if result.Session != nil {
			sessionID = result.Session.ID
		}
	}
	return
}

func deliveryQuestion(row registry.Delivery) string {
	if row.Kind == "ui_response" {
		var value registry.AcceptResult
		_ = json.Unmarshal(row.Payload, &value)
		return value.QuestionID
	}
	if row.Kind == "user_input_requested" {
		var event protocol.Event
		var approval protocol.Approval
		if json.Unmarshal(row.Payload, &event) == nil && json.Unmarshal(event.Data, &approval) == nil && len(approval.Questions) > 0 {
			return approval.Questions[0].ID
		}
	}
	return ""
}
