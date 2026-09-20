package worker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

const (
	historyPromptRunes = 16000
	historyPageRunes   = 64000
	historyReadTimeout = 20 * time.Second
)

// readHistory only projects saved input. In particular, it never resumes a
// cold thread or changes the actor's active command, turn, or session state.
func (s *sessionActor) readHistory(client *codexadapter.Client, request *protocol.HistoryRequest) (*protocol.HistoryPage, error) {
	if request != nil && request.Messages {
		return s.readLastMessages(client, request)
	}
	if err := request.Validate(); err != nil {
		return nil, historyError(protocol.CodexCommandInvalid, "Use /tghistory with a page size from 1 to 50.")
	}
	if _, err := auth.CanonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots); err != nil {
		return nil, historyError(protocol.InvalidWorkspace, "Session workspace is not allowed.")
	}
	ctx, cancel := context.WithTimeout(s.agent.ctx, historyReadTimeout)
	defer cancel()
	prompts, err := s.recentExternalPrompts(ctx, client, request)
	if err != nil {
		if errors.Is(err, codexadapter.ErrHistoryCursorUnavailable) {
			return nil, historyError(protocol.CodexCommandInvalid, "This history page is no longer available. Run /tghistory to start again.")
		}
		if errors.Is(err, codexadapter.ErrHistoryUnavailable) {
			return nil, historyError(protocol.CodexUnavailable, "Codex did not return saved prompt history for this thread. Try again when it is available.")
		}
		if errors.Is(err, codexadapter.ErrMethodUnavailable) {
			return nil, historyError(protocol.CodexMethodUnsupported, "This Codex app-server does not support reading saved history. Update Codex and retry.")
		}
		// A read failure never has an unknown execution outcome, and its raw
		// message can contain saved input or local file locations.
		return nil, &protocol.Error{Code: protocol.CodexUnavailable, Message: "Could not read saved prompt history. Please retry /tghistory.", Retryable: true}
	}
	// The cursor has already been applied while reading newest turns first.
	return historyPage(prompts, &protocol.HistoryRequest{Limit: request.Limit}, s.agent.redactor)
}

// Read only enough newest turns to fill the requested page and establish
// whether an older eligible prompt exists. Large saved conversations should
// not require replaying every historical turn just to display two prompts.
func (s *sessionActor) recentExternalPrompts(ctx context.Context, client *codexadapter.Client, request *protocol.HistoryRequest) ([]codexadapter.UserPrompt, error) {
	filter, err := s.agent.store.externalPromptFilter(s.runtime.ID, s.session.ThreadID)
	if err != nil {
		return nil, err
	}
	newest := make([]codexadapter.UserPrompt, 0, request.Limit+1)
	cursor := ""
	seenCursors, seenTurns := make(map[string]bool), make(map[string]bool)
	foundBefore := request.Before == nil
	for pages := 0; pages < 10000; pages++ {
		page, err := client.ReadHistoryPage(ctx, s.session.ThreadID, cursor, "desc")
		if err != nil {
			return nil, err
		}
		for _, turn := range page.Turns {
			if seenTurns[turn.ID] {
				return nil, codexadapter.ErrHistoryUnavailable
			}
			seenTurns[turn.ID] = true
			prompts := filter(turn.UserPrompts)
			end := len(prompts)
			if !foundBefore {
				for i, prompt := range prompts {
					if prompt.TurnID == request.Before.TurnID && prompt.ItemID == request.Before.ItemID {
						end, foundBefore = i, true
						break
					}
				}
				if !foundBefore {
					continue
				}
			}
			for i := end - 1; i >= 0 && len(newest) < request.Limit+1; i-- {
				newest = append(newest, prompts[i])
			}
		}
		if len(newest) > request.Limit || page.NextCursor == "" {
			if !foundBefore {
				return nil, codexadapter.ErrHistoryCursorUnavailable
			}
			for left, right := 0, len(newest)-1; left < right; left, right = left+1, right-1 {
				newest[left], newest[right] = newest[right], newest[left]
			}
			return newest, nil
		}
		if seenCursors[page.NextCursor] {
			return nil, codexadapter.ErrHistoryUnavailable
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return nil, codexadapter.ErrHistoryUnavailable
}

func historyError(code, message string) *protocol.Error {
	return &protocol.Error{Code: code, Message: message}
}

// externalHistoryPrompts omits only input for which this worker's ledger has an
// accepted, correlated gateway submission. Exact text and turn identity are
// matched before redaction; each submission consumes just one chronological
// occurrence, so repeated CLI text remains visible. Runtime generations may
// change while the saved thread and its original command records remain valid.
func (s *Store) externalHistoryPrompts(runtimeID, threadID string, prompts []codexadapter.UserPrompt) ([]codexadapter.UserPrompt, error) {
	filter, err := s.externalPromptFilter(runtimeID, threadID)
	if err != nil {
		return nil, err
	}
	return filter(prompts), nil
}

// Build the accepted-input ledger once for a paginated history read. Each
// chronological occurrence is consumed only once even across turn pages.
func (s *Store) externalPromptFilter(runtimeID, threadID string) (func([]codexadapter.UserPrompt) []codexadapter.UserPrompt, error) {
	type key struct{ turn, text string }
	counts := make(map[key]int)
	err := s.db.View(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketCommands).ForEach(func(_, value []byte) error {
			record, err := decodeCommand(value)
			if err != nil {
				return err
			}
			c := record.Command
			if c.WorkerID != s.workerID || c.RuntimeID != runtimeID || c.ThreadID != threadID || record.Result == nil {
				return nil
			}
			// A terminal turn failure still has saved input, whereas a rejected
			// submission never acquired its own turn identity. Only successful
			// steers may use ExpectedTurnID as their correlation fallback.
			if record.State != CommandCompleted && (record.State != CommandFailed || record.Result.TurnID == "") {
				return nil
			}
			if record.Result.Error != nil && record.Result.TurnID == "" {
				return nil
			}
			text, turn := c.Arguments.Text, record.Result.TurnID
			switch c.Operation {
			case protocol.StartTurn:
				text += strings.Repeat("[Image]", len(c.Arguments.Images))
			case protocol.Steer:
				text += strings.Repeat("[Image]", len(c.Arguments.Images))
				if turn == "" {
					turn = c.ExpectedTurnID
				}
			case protocol.CodexCommand:
				if c.Arguments.Codex == nil || strings.ToLower(strings.TrimSpace(c.Arguments.Codex.Name)) != "init" || strings.TrimSpace(c.Arguments.Codex.Args) != "" {
					return nil
				}
				text = initPrompt
			default:
				return nil
			}
			if turn != "" && text != "" {
				counts[key{turn, text}]++
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return func(prompts []codexadapter.UserPrompt) []codexadapter.UserPrompt {
		eligible := make([]codexadapter.UserPrompt, 0, len(prompts))
		for _, prompt := range prompts {
			k := key{prompt.TurnID, prompt.Text}
			if counts[k] > 0 {
				counts[k]--
				continue
			}
			eligible = append(eligible, prompt)
		}
		return eligible
	}, nil
}

// historyPage chooses a contiguous suffix before an exclusive cursor, and
// returns it newest first. The size budget can shorten a page, but
// Next always points to the oldest returned item, leaving every older eligible
// prompt available on the following page.
func historyPage(prompts []codexadapter.UserPrompt, request *protocol.HistoryRequest, redactor *auth.Redactor) (*protocol.HistoryPage, error) {
	for _, prompt := range prompts {
		if strings.TrimSpace(prompt.TurnID) == "" || strings.TrimSpace(prompt.ItemID) == "" || len(prompt.TurnID) > 512 || len(prompt.ItemID) > 512 {
			return nil, historyError(protocol.CodexUnavailable, "Codex returned history without usable message identities. Please retry /tghistory.")
		}
	}
	end := len(prompts)
	if request.Before != nil {
		end = -1
		for i, prompt := range prompts {
			if prompt.TurnID == request.Before.TurnID && prompt.ItemID == request.Before.ItemID {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, historyError(protocol.CodexCommandInvalid, "This history page is no longer available. Run /tghistory to start again.")
		}
	}
	page := &protocol.HistoryPage{Limit: request.Limit, NewestFirst: true, Prompts: make([]protocol.HistoryPrompt, 0, request.Limit)}
	remaining, start := historyPageRunes, end
	for start > 0 && len(page.Prompts) < request.Limit {
		prompt := prompts[start-1]
		text := prompt.Text
		if redactor != nil {
			text = redactor.Redact(text)
		}
		runes := []rune(text)
		truncated := len(runes) > historyPromptRunes
		if truncated {
			runes = runes[:historyPromptRunes]
		}
		if len(runes) > remaining {
			break
		}
		remaining -= len(runes)
		page.Prompts = append(page.Prompts, protocol.HistoryPrompt{TurnID: prompt.TurnID, ItemID: prompt.ItemID, Text: string(runes), Truncated: truncated, Timestamp: prompt.Timestamp})
		start--
	}
	if start > 0 && len(page.Prompts) > 0 {
		oldest := page.Prompts[len(page.Prompts)-1]
		page.Next = &protocol.HistoryCursor{TurnID: oldest.TurnID, ItemID: oldest.ItemID}
	}
	return page, nil
}
