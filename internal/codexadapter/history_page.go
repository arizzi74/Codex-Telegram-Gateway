package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
)

// HistoryTurnPage is a bounded projection suitable for a local cache. Cursors
// are opaque: NextCursor continues in the current direction; BackwardsCursor
// reverses direction and includes the anchor turn again to capture new items.
type HistoryTurnPage struct {
	Turns           []HistoryTurn
	NextCursor      string
	BackwardsCursor string
}

// HistoryTurn retains display text and structured question lifecycle input.
// It deliberately excludes raw reasoning, tools, and attachment payloads.
type HistoryTurn struct {
	ID             string
	Status         string
	StartedAt      *time.Time
	CompletedAt    *time.Time
	UserPrompts    []UserPrompt
	Messages       []protocol.HistoryMessage
	QuestionEvents []HistoryQuestionEvent
}

// HistoryQuestionEvent preserves question/input ordering within a turn. An
// input can resolve an earlier question but cannot resolve a later question.
type HistoryQuestionEvent struct {
	ItemID    string
	Text      string
	Questions []Question
	IsInput   bool
}

// ReadHistoryPage reads at most one full turn without resuming or subscribing
// to the thread. This bounds image-heavy responses while allowing callers to
// persist each projected page and continue from a cursor after interruption.
func (c *Client) ReadHistoryPage(ctx context.Context, threadID, cursor, sortDirection string) (HistoryTurnPage, error) {
	if strings.TrimSpace(threadID) == "" || len(threadID) > 512 || len(cursor) > 4096 || (sortDirection != "asc" && sortDirection != "desc") {
		return HistoryTurnPage{}, errors.New("invalid history page request")
	}
	params := map[string]any{"threadId": threadID, "limit": 1, "sortDirection": sortDirection, "itemsView": "full"}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var reply struct {
		Data            json.RawMessage `json:"data"`
		NextCursor      string          `json:"nextCursor"`
		BackwardsCursor string          `json:"backwardsCursor"`
	}
	if err := c.request(ctx, "thread/turns/list", params, &reply, false); err != nil {
		return HistoryTurnPage{}, err
	}
	rawTurns, err := historyArray(reply.Data)
	if err != nil || len(rawTurns) > 1 || len(reply.NextCursor) > 4096 || len(reply.BackwardsCursor) > 4096 || (len(rawTurns) == 0 && (reply.NextCursor != "" || reply.BackwardsCursor != "")) || (cursor != "" && cursor == reply.NextCursor) {
		return HistoryTurnPage{}, fmt.Errorf("invalid bounded history page: %w", ErrHistoryUnavailable)
	}
	page := HistoryTurnPage{Turns: make([]HistoryTurn, 0, len(rawTurns)), NextCursor: reply.NextCursor, BackwardsCursor: reply.BackwardsCursor}
	for _, raw := range rawTurns {
		turn, err := decodeHistoryTurn(raw)
		if err != nil {
			return HistoryTurnPage{}, err
		}
		page.Turns = append(page.Turns, turn)
	}
	return page, nil
}

func decodeHistoryTurn(raw json.RawMessage) (HistoryTurn, error) {
	var value struct {
		ID          string            `json:"id"`
		Status      string            `json:"status"`
		ItemsView   string            `json:"itemsView"`
		Items       []json.RawMessage `json:"items"`
		StartedAt   *int64            `json:"startedAt"`
		CompletedAt *int64            `json:"completedAt"`
	}
	if !historyObject(raw) || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value.ID) == "" || len(value.ID) > 512 || (value.ItemsView != "" && value.ItemsView != "full") {
		return HistoryTurn{}, fmt.Errorf("invalid bounded history turn: %w", ErrHistoryUnavailable)
	}
	turn := HistoryTurn{ID: value.ID, Status: value.Status, StartedAt: historyTimestamp(value.StartedAt), CompletedAt: historyTimestamp(value.CompletedAt)}
	var err error
	turn.UserPrompts, err = decodeUserPromptTurns([]json.RawMessage{raw})
	if err != nil {
		return HistoryTurn{}, err
	}
	turn.Messages, err = decodeConversationTurn(raw)
	if err != nil {
		return HistoryTurn{}, err
	}
	questions, err := decodeAsyncQuestionTurn(raw)
	if err != nil {
		return HistoryTurn{}, err
	}
	questionByID := make(map[string]AsyncQuestion, len(questions))
	for _, question := range questions {
		questionByID[question.ItemID] = question
	}
	promptByID := make(map[string]UserPrompt, len(turn.UserPrompts))
	for _, prompt := range turn.UserPrompts {
		promptByID[prompt.ItemID] = prompt
	}
	for _, rawItem := range value.Items {
		var item struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(rawItem, &item) != nil {
			return HistoryTurn{}, fmt.Errorf("invalid bounded history item: %w", ErrHistoryUnavailable)
		}
		if prompt, ok := promptByID[item.ID]; ok {
			turn.QuestionEvents = append(turn.QuestionEvents, HistoryQuestionEvent{ItemID: prompt.ItemID, Text: prompt.Text, IsInput: true})
		} else if question, ok := questionByID[item.ID]; ok {
			turn.QuestionEvents = append(turn.QuestionEvents, HistoryQuestionEvent{ItemID: question.ItemID, Text: question.Text, Questions: question.Questions})
		}
	}
	return turn, nil
}

// ResolveHistoryQuestions applies chronological cached turns to the prior
// question state. Cache callers should replace overlapping turns by ID before
// replaying them; turn order is execution order, not arbitrary UUID order.
func ResolveHistoryQuestions(prior []AsyncQuestion, turns []HistoryTurn) []AsyncQuestion {
	result := make([]AsyncQuestion, len(prior))
	seen := make(map[[2]string]bool, len(prior))
	for i, question := range prior {
		question.AnsweredIDs = append([]string(nil), question.AnsweredIDs...)
		question.SupersededIDs = append([]string(nil), question.SupersededIDs...)
		if question.Answers != nil {
			answers := make(map[string][]string, len(question.Answers))
			for id, values := range question.Answers {
				answers[id] = append([]string(nil), values...)
			}
			question.Answers = answers
		}
		result[i] = question
		seen[[2]string{question.TurnID, question.ItemID}] = true
	}
	for _, turn := range turns {
		seenInTurn := make(map[string]bool)
		for _, event := range turn.QuestionEvents {
			if !event.IsInput {
				seenInTurn[event.ItemID] = true
				key := [2]string{turn.ID, event.ItemID}
				if len(event.Questions) != 0 && !seen[key] {
					seen[key] = true
					result = append(result, AsyncQuestion{TurnID: turn.ID, ItemID: event.ItemID, Text: event.Text, Questions: event.Questions})
				}
				continue
			}
			var titles []string
			for _, question := range result {
				for _, field := range question.Questions {
					titles = append(titles, field.Prompt)
				}
			}
			for index := range result {
				question := &result[index]
				if question.TurnID == turn.ID && !seenInTurn[question.ItemID] {
					continue
				}
				resolved := make(map[string]bool, len(question.AnsweredIDs)+len(question.SupersededIDs))
				for _, id := range question.AnsweredIDs {
					resolved[id] = true
				}
				for _, id := range question.SupersededIDs {
					resolved[id] = true
				}
				for _, field := range question.Questions {
					if resolved[field.ID] {
						continue
					}
					if AsyncQuestionInputSupersedes(event.Text) {
						question.SupersededIDs = append(question.SupersededIDs, field.ID)
					} else if answer, ok := AsyncQuestionAnswer(field.Prompt, event.Text, titles...); ok {
						question.AnsweredIDs = append(question.AnsweredIDs, field.ID)
						if question.Answers == nil {
							question.Answers = make(map[string][]string)
						}
						question.Answers[field.ID] = []string{answer}
					}
				}
			}
		}
	}
	return result
}

func historyTimestamp(seconds *int64) *time.Time {
	// Avoid displaying zero, negative, or non-calendar protocol values as a
	// plausible message date. Missing source dates must remain missing.
	if seconds == nil || *seconds <= 0 || *seconds > 253402300799 {
		return nil
	}
	value := time.Unix(*seconds, 0).UTC()
	return &value
}
