package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type usageRenderStore struct {
	*renderStoreFake
	commandID uuid.UUID
	requester int64
}

func (s *usageRenderStore) UsageRequester(_ context.Context, id uuid.UUID) (int64, error) {
	if id != s.commandID {
		return 0, registry.ErrTelegramTarget
	}
	return s.requester, nil
}

func TestUsageMenuStepsRetainRequesterTargetAndOpaqueChoices(t *testing.T) {
	capability := uuid.NewString()
	steps := map[string][]protocol.UsageOption{
		"overview":     {{Args: "daily", Label: "Daily usage"}, {Args: "resets", Label: "Banked resets"}, {Args: "cancel", Label: "Close"}},
		"resets":       {{Args: "redeem " + capability, Label: "Redeem banked reset", Description: "Expires tomorrow"}, {Args: "--menu", Label: "Back"}},
		"confirmation": {{Args: "confirm " + capability, Label: "Confirm redemption"}, {Args: "cancel", Label: "Cancel"}},
	}
	for step, options := range steps {
		t.Run(step, func(t *testing.T) {
			store := &usageRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
			result := protocol.Result{CommandID: store.commandID.String(), Text: "Usage " + step + ".", UsageMenu: &protocol.UsageMenu{Options: options}}
			row := eventRow(t, "command_completed", result, testSessionID.String())
			row.ChatID, row.TopicID = 101, 3
			sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
			text, keyboard, err := sender.render(t.Context(), row)
			if err != nil || !strings.Contains(text, "auth-fix") || !strings.Contains(text, result.Text) {
				t.Fatalf("menu identity/instructions: %q %v", text, err)
			}
			if keyboard == nil || len(keyboard.Rows) != len(options) || len(store.callbacks) != len(options) {
				t.Fatalf("missing choices: %#v", keyboard)
			}
			for i, cb := range store.callbacks {
				if cb.Action != "usage" || cb.UserID != 77 || cb.BotID != "bot" || cb.ChatID != 101 || cb.TopicID != 3 ||
					cb.SessionID != testSessionID || cb.RuntimeID != testRuntimeID || cb.Generation != 7 || cb.QuestionID != result.CommandID || cb.Decision != options[i].Args || cb.ExpiresAt.IsZero() {
					t.Fatalf("lost requester or frozen choice: %#v", cb)
				}
				button := keyboard.Rows[i][0]
				if button.Text != options[i].Label || !strings.HasPrefix(button.Data, "cb:") || len(button.Data) > 64 || strings.Contains(button.Data, options[i].Args) || strings.Contains(button.Data, capability) {
					t.Fatalf("invalid button/opaque token: %#v", button)
				}
			}
		})
	}
}

func TestUsageMenuRedactsBeforeTruncatingLabels(t *testing.T) {
	store := &usageRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
	secret := "PRIVATE_" + strings.Repeat("s", 95)
	redactor, err := auth.NewRedactor([]string{secret}, "")
	if err != nil {
		t.Fatal(err)
	}
	result := protocol.Result{CommandID: store.commandID.String(), Text: "Usage " + secret + ".", UsageMenu: &protocol.UsageMenu{Options: []protocol.UsageOption{
		{Args: "resets", Label: secret + " resets"}, {Args: "cancel", Label: "Cancel"},
	}}}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42, Redactor: redactor})
	text, keyboard, err := sender.render(t.Context(), eventRow(t, "command_completed", result, testSessionID.String()))
	if err != nil || keyboard == nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(text, "PRIVATE_") || strings.Contains(keyboard.Rows[0][0].Text, "PRIVATE_") || !strings.Contains(keyboard.Rows[0][0].Text, "[REDACTED]") {
		t.Fatalf("secret was exposed before truncation: %q %#v", text, keyboard)
	}
}

func TestUsageMenuPagedEscapedTitlesFitKeyboardBudget(t *testing.T) {
	store := &usageRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
	var options []protocol.UsageOption
	for range 8 {
		options = append(options, protocol.UsageOption{Args: "redeem " + uuid.NewString(), Label: strings.Repeat("<&>", 30)})
	}
	options = append(options,
		protocol.UsageOption{Args: "redeem", Label: "Use next available reset"},
		protocol.UsageOption{Args: "resets 0", Label: "Previous resets"},
		protocol.UsageOption{Args: "resets 2", Label: "Next resets"},
		protocol.UsageOption{Args: "--menu", Label: "Back to usage"},
		protocol.UsageOption{Args: "cancel", Label: "Cancel"})
	result := protocol.Result{CommandID: store.commandID.String(), Text: "Full title: " + options[0].Label, UsageMenu: &protocol.UsageMenu{Options: options}}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
	text, keyboard, err := sender.render(t.Context(), eventRow(t, "command_completed", result, testSessionID.String()))
	if err != nil || keyboard == nil || len(keyboard.Rows) != 13 || len(store.callbacks) != 13 {
		t.Fatalf("escaped title page failed: %#v %v", keyboard, err)
	}
	if !strings.Contains(text, options[0].Label) || !strings.HasSuffix(keyboard.Rows[0][0].Text, "…") {
		t.Fatalf("full title lost or button not shortened: %q %#v", text, keyboard.Rows[0])
	}
	for i, row := range keyboard.Rows {
		if store.callbacks[i].Decision != options[i].Args {
			t.Fatalf("choice changed when shortening title: %#v", store.callbacks[i])
		}
		row[0].Data = strings.Repeat("x", 64)
	}
	markup, err := json.Marshal(keyboard)
	if err != nil || len(markup) > sessionKeyboardMaxBytes {
		t.Fatalf("page exceeds keyboard budget: %d bytes, %v", len(markup), err)
	}
}

func TestUsageMenuRejectsInvalidOrUnownedResultsBeforeCallbacks(t *testing.T) {
	for _, failure := range []string{"empty", "duplicate", "control", "unknown-command", "no-requester", "no-generation", "no-session", "no-runtime", "keyboard-budget"} {
		t.Run(failure, func(t *testing.T) {
			store := &usageRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
			result := protocol.Result{CommandID: store.commandID.String(), UsageMenu: &protocol.UsageMenu{Options: []protocol.UsageOption{{Args: "resets", Label: "Banked resets"}}}}
			switch failure {
			case "empty":
				result.UsageMenu.Options = nil
			case "duplicate":
				result.UsageMenu.Options = append(result.UsageMenu.Options, result.UsageMenu.Options[0])
			case "control":
				result.UsageMenu.Options[0].Args = "resets\n"
			case "unknown-command":
				result.CommandID = uuid.NewString()
			case "no-requester":
				store.requester = 0
			case "keyboard-budget":
				result.UsageMenu.Options = make([]protocol.UsageOption, 50)
				for i := range result.UsageMenu.Options {
					result.UsageMenu.Options[i] = protocol.UsageOption{Args: "redeem " + uuid.NewString(), Label: strings.Repeat("&", 120)}
				}
			}
			row := eventRow(t, "command_completed", result, testSessionID.String())
			var event protocol.Event
			if err := json.Unmarshal(row.Payload, &event); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "no-generation":
				event.RuntimeGeneration = 0
			case "no-session":
				event.SessionID = ""
			case "no-runtime":
				event.RuntimeID = ""
			}
			row.Payload, _ = json.Marshal(event)
			sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
			if _, _, err := sender.render(t.Context(), row); err == nil || len(store.callbacks) != 0 {
				t.Fatalf("invalid menu created callbacks: %v %#v", err, store.callbacks)
			}
		})
	}
}
