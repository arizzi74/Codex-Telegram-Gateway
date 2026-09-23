package worker

import (
	"strings"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Native Web UI and CLI prompts follow the selected or all-session feed. Exact, accepted
// Telegram submissions are already visible in that chat and are not echoed.
// Keep occurrences within the turn so identical later CLI input is retained.
func (s *sessionActor) observeUserMessage(event codexadapter.Event) {
	if event.TurnID == "" || event.TurnID != s.session.ActiveTurnID || event.ItemID == "" || strings.TrimSpace(event.Text) == "" {
		return
	}
	if _, seen := s.userItems[event.ItemID]; seen {
		return
	}
	if s.userItems == nil {
		s.userItems = map[string]struct{}{}
	}
	s.userItems[event.ItemID] = struct{}{}
	// A replayed or late input must not clear questions asked after that input.
	// Reconcile only once the same event has passed the feed identity guards.
	s.observeAsyncAnswer(event.Text)
	s.userPrompts = append(s.userPrompts, codexadapter.UserPrompt{TurnID: event.TurnID, ItemID: event.ItemID, Text: event.Text})
	external, err := s.agent.store.externalHistoryPrompts(s.runtime.ID, s.session.ThreadID, s.userPrompts)
	if err != nil {
		s.agent.report(err)
		return
	}
	for _, prompt := range external {
		if prompt.ItemID != event.ItemID {
			continue
		}
		text := s.agent.redactor.Redact(prompt.Text)
		if runes := []rune(text); len(runes) > historyPromptRunes {
			text = string(runes[:historyPromptRunes-1]) + "…"
		}
		s.agent.report(s.agent.emit(s.runtime, s.session.ID, "user_message", protocol.Result{TurnID: event.TurnID, Text: text}))
		return
	}
}
