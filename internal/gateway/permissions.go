package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Sender) renderPermissions(ctx context.Context, row registry.Delivery, identity string, event protocol.Event, result protocol.Result) (string, *TelegramKeyboard, error) {
	menu := result.Permissions
	// Validate the entire menu before writing any callbacks.
	if err := menu.Validate(); err != nil {
		return "", nil, err
	}
	sessionID, err := requiredUUID("session", event.SessionID)
	if err != nil {
		return "", nil, err
	}
	runtimeID, err := requiredUUID("runtime", event.RuntimeID)
	if err != nil {
		return "", nil, err
	}
	commandID, err := requiredUUID("command", result.CommandID)
	if err != nil {
		return "", nil, err
	}
	if event.RuntimeGeneration == 0 {
		return "", nil, errors.New("render permissions: missing runtime generation")
	}
	store, ok := s.store.(interface {
		PermissionsRequester(context.Context, uuid.UUID) (int64, error)
	})
	if !ok {
		return "", nil, errors.New("render permissions: requester lookup unavailable")
	}
	requester, err := store.PermissionsRequester(ctx, commandID)
	if err != nil || requester == 0 {
		return "", nil, errors.New("render permissions: requester unavailable")
	}
	// Reserve the built-in choices and Cancel before fitting custom profiles.
	// Use Telegram's maximum callback-token size so generated tokens cannot
	// push the final keyboard past the same budget used by session pickers.
	labels := make([]string, len(menu.Options))
	shown := make([]bool, len(menu.Options))
	budget := &TelegramKeyboard{}
	placeholder := strings.Repeat("x", 64)
	for i, option := range menu.Options {
		labels[i] = s.sessionListField(option.Label, 80, 160)
		if !strings.HasPrefix(option.ID, "profile:") {
			shown[i] = true
			budget.Rows = append(budget.Rows, []TelegramButton{{Text: labels[i], Data: placeholder}})
		}
	}
	markup, err := json.Marshal(budget)
	if err != nil || len(markup) > sessionKeyboardMaxBytes {
		return "", nil, errors.New("render permissions: required controls exceed keyboard budget")
	}
	omitted := false
	for i, option := range menu.Options {
		if !strings.HasPrefix(option.ID, "profile:") {
			continue
		}
		budget.Rows = append(budget.Rows, []TelegramButton{{Text: labels[i], Data: placeholder}})
		markup, err = json.Marshal(budget)
		if err != nil {
			return "", nil, err
		}
		if len(markup) > sessionKeyboardMaxBytes {
			budget.Rows = budget.Rows[:len(budget.Rows)-1]
			omitted = true
			continue
		}
		shown[i] = true
	}
	text := identity + "\n\n" + result.Text
	keyboard := &TelegramKeyboard{}
	for i, option := range menu.Options {
		if option.Description != "" {
			text += "\n\n" + option.Label + " — " + option.Description
		} else if !shown[i] {
			text += "\n\n" + option.Label
		}
		if !shown[i] {
			continue
		}
		token, err := s.callback(ctx, row, registry.Callback{
			Action: "permissions", UserID: requester, SessionID: sessionID, RuntimeID: runtimeID,
			Generation: int64(event.RuntimeGeneration), Decision: option.ID, QuestionID: result.CommandID,
		})
		if err != nil {
			return "", nil, err
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: labels[i], Data: token}})
	}
	if omitted {
		text += "\n\nSome configured profiles are listed without buttons to fit Telegram's menu limit. Select them with /permissions profile:NAME, replacing NAME with the profile name."
	}
	return text, keyboard, nil
}
