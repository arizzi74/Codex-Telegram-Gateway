package worker

import (
	"context"
	"errors"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Both history commands include Telegram and CLI input plus final replies.
// /tghistory walks newest first; /tglastmessages keeps its chronological view.
func (s *sessionActor) readLastMessages(client *codexadapter.Client, request *protocol.HistoryRequest) (*protocol.HistoryPage, error) {
	command := "/tghistory"
	if request != nil && !request.NewestFirst {
		command = "/tglastmessages"
	}
	if request.Validate() != nil {
		return nil, historyError(protocol.CodexCommandInvalid, "Use "+command+" with a message count from 1 to 50.")
	}
	if _, err := auth.CanonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots); err != nil {
		return nil, historyError(protocol.InvalidWorkspace, "Session workspace is not allowed.")
	}
	ctx, cancel := context.WithTimeout(s.agent.ctx, historyReadTimeout)
	defer cancel()
	messages, older, err := client.ConversationMessages(ctx, s.session.ThreadID, request.Limit, request.Before)
	if err != nil {
		if errors.Is(err, codexadapter.ErrHistoryCursorUnavailable) {
			return nil, historyError(protocol.CodexCommandInvalid, "This message page is no longer available. Run "+command+" to start again.")
		}
		if errors.Is(err, codexadapter.ErrMethodUnavailable) {
			return nil, historyError(protocol.CodexMethodUnsupported, "This Codex app-server does not support reading saved messages. Update Codex and retry.")
		}
		return nil, &protocol.Error{Code: protocol.CodexUnavailable, Message: "Could not read saved messages. Please retry " + command + ".", Retryable: true}
	}
	s.enrichHistoryTimestamps(client, messages)
	return lastMessagesPage(messages, older, request.Limit, s.agent.redactor, request.NewestFirst), nil
}

// Keep each message and the full reply bounded using the same budgets as
// /tghistory. If the byte budget shortens a page, its cursor still covers all
// omitted older messages without repeating newer ones.
func lastMessagesPage(messages []protocol.HistoryMessage, older bool, limit int, redactor *auth.Redactor, newestFirst bool) *protocol.HistoryPage {
	page := &protocol.HistoryPage{Limit: limit, Conversation: true, NewestFirst: newestFirst, Messages: make([]protocol.HistoryMessage, 0, len(messages))}
	remaining := historyPageRunes
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if redactor != nil {
			message.Text = redactor.Redact(message.Text)
		}
		runes := []rune(message.Text)
		if len(runes) > historyPromptRunes {
			message.Truncated = true
			runes = runes[:historyPromptRunes]
		}
		if len(runes) > remaining {
			older = true
			break
		}
		remaining -= len(runes)
		message.Text = string(runes)
		page.Messages = append(page.Messages, message)
	}
	if older && len(page.Messages) > 0 {
		oldest := page.Messages[len(page.Messages)-1]
		page.Next = &protocol.HistoryCursor{TurnID: oldest.TurnID, ItemID: oldest.ItemID}
	}
	if !newestFirst {
		for left, right := 0, len(page.Messages)-1; left < right; left, right = left+1, right-1 {
			page.Messages[left], page.Messages[right] = page.Messages[right], page.Messages[left]
		}
	}
	return page
}
