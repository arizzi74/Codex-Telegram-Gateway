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

func (s *Sender) renderUsageMenu(ctx context.Context, row registry.Delivery, identity string, event protocol.Event, result protocol.Result) (string, *TelegramKeyboard, error) {
	if err := result.UsageMenu.Validate(); err != nil {
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
		return "", nil, errors.New("render usage menu: missing runtime generation")
	}
	store, ok := s.store.(interface {
		UsageRequester(context.Context, uuid.UUID) (int64, error)
	})
	if !ok {
		return "", nil, errors.New("render usage menu: requester lookup unavailable")
	}
	requester, err := store.UsageRequester(ctx, commandID)
	if err != nil || requester == 0 {
		return "", nil, errors.New("render usage menu: requester unavailable")
	}
	keyboard := &TelegramKeyboard{}
	// JSON expands '<', '>' and '&' to six-byte escapes. Shorten labels before
	// creating callbacks when a valid page would otherwise exceed the budget;
	// full titles remain available in the message body.
	for labelUnits := 80; ; labelUnits /= 2 {
		keyboard.Rows = nil
		for _, option := range result.UsageMenu.Options {
			keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: s.sessionListField(option.Label, labelUnits, 160), Data: strings.Repeat("x", 64)}})
		}
		markup, err := json.Marshal(keyboard)
		if err != nil {
			return "", nil, err
		}
		if len(markup) <= sessionKeyboardMaxBytes {
			break
		}
		if labelUnits <= 20 {
			return "", nil, errors.New("render usage menu: keyboard exceeds budget")
		}
	}
	for i, option := range result.UsageMenu.Options {
		token, err := s.callback(ctx, row, registry.Callback{
			Action: "usage", UserID: requester, SessionID: sessionID, RuntimeID: runtimeID,
			Generation: int64(event.RuntimeGeneration), Decision: option.Args, QuestionID: result.CommandID,
		})
		if err != nil {
			return "", nil, err
		}
		keyboard.Rows[i][0].Data = token
	}
	return identity + "\n\n" + result.Text, keyboard, nil
}
