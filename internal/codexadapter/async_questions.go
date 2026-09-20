package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// AsyncQuestion is a saved request_user_input_async item. Unlike a blocking
// request, it has no JSON-RPC response ID and remains useful after its turn
// finishes. Its item ID is stable across notifications and history reads.
type AsyncQuestion struct {
	TurnID    string
	ItemID    string
	Text      string
	Questions []Question
	// AnsweredIDs records matching later native answer markers without
	// removing questions, so a caller can reconcile persisted pending state.
	AnsweredIDs []string
}

type asyncUserInputQuestion struct {
	Title   string   `json:"title"`
	Options []string `json:"options"`
}

func normalizeAsyncQuestions(questions []asyncUserInputQuestion) []Question {
	result := make([]Question, 0, len(questions))
	for index, question := range questions {
		if strings.TrimSpace(question.Title) == "" {
			continue
		}
		q := Question{ID: fmt.Sprintf("q%d", index+1), Prompt: question.Title, IsOther: true}
		for _, label := range question.Options {
			if strings.TrimSpace(label) != "" {
				q.Choices = append(q.Choices, Choice{Label: label})
			}
		}
		result = append(result, q)
	}
	return result
}

// FormatAsyncQuestionAnswer matches Codex 0.155's native answer input. Async
// answers are ordinary user messages quoting the question, not RPC replies.
func FormatAsyncQuestionAnswer(title, answer string) string {
	return asyncQuestionAnswerPrefix(title) + strings.TrimSpace(answer)
}

func asyncQuestionAnswerPrefix(title string) string {
	if len(title) > 512 {
		end := 512
		for end > 0 && !utf8.RuneStart(title[end]) {
			end--
		}
		title = title[:end]
	}
	title = strings.NewReplacer("\r", " ", "\n", " ").Replace(title)
	return "> " + title + "\n\n"
}

// AsyncQuestionAnswerMatches recognizes the explicit native answer marker.
// An unrelated prompt or an answer that predates a question does not resolve
// it. Multiple quoted answers may be sent together in a Telegram response.
func AsyncQuestionAnswerMatches(title, text string) bool {
	prefix := asyncQuestionAnswerPrefix(title)
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], prefix)
		if index < 0 {
			return false
		}
		index += offset
		if index == 0 || strings.HasSuffix(text[:index], "\n\n") {
			answer := text[index+len(prefix):]
			if next := strings.Index(answer, "\n\n> "); next >= 0 {
				answer = answer[:next]
			}
			if strings.TrimSpace(answer) != "" {
				return true
			}
		}
		offset = index + len(prefix)
	}
	return false
}

// AsyncQuestions reads structured asynchronous questions in chronological
// order without resuming or changing a thread. AnsweredIDs identifies fields
// followed by native quoted-title answers, including responses entered through
// a CLI. The protocol has no authoritative answered flag; callers must also
// apply their own durable response records. An unrelated user prompt is not
// treated as an answer. Tools, reasoning and attachment bytes are not returned.
func (c *Client) AsyncQuestions(ctx context.Context, threadID string) ([]AsyncQuestion, error) {
	return c.asyncQuestions(ctx, threadID, maxHistoryTurnPages)
}

func (c *Client) asyncQuestions(ctx context.Context, threadID string, maxPages int) ([]AsyncQuestion, error) {
	if strings.TrimSpace(threadID) == "" {
		return nil, errors.New("thread id is required")
	}
	result := make([]AsyncQuestion, 0)
	seenTurns, seenCursors := make(map[string]bool), make(map[string]bool)
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params := map[string]any{"threadId": threadID, "limit": 1, "sortDirection": "asc", "itemsView": "full"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var reply struct {
			Data       json.RawMessage `json:"data"`
			NextCursor string          `json:"nextCursor"`
		}
		if err := c.request(ctx, "thread/turns/list", params, &reply, false); err != nil {
			return nil, err
		}
		turns, err := historyArray(reply.Data)
		if err != nil || len(turns) > 1 || (len(turns) == 0 && reply.NextCursor != "") {
			return nil, fmt.Errorf("invalid async question turn page: %w", ErrHistoryUnavailable)
		}
		for _, raw := range turns {
			var turn struct {
				ID        string `json:"id"`
				ItemsView string `json:"itemsView"`
			}
			if json.Unmarshal(raw, &turn) != nil || strings.TrimSpace(turn.ID) == "" || seenTurns[turn.ID] || (turn.ItemsView != "" && turn.ItemsView != "full") {
				return nil, fmt.Errorf("invalid async question turn: %w", ErrHistoryUnavailable)
			}
			seenTurns[turn.ID] = true
			questions, err := decodeAsyncQuestionTurn(raw)
			if err != nil {
				return nil, err
			}
			result = append(result, questions...)
			result, err = resolveAsyncQuestionHistory(result, raw)
			if err != nil {
				return nil, err
			}
		}
		if reply.NextCursor == "" {
			return result, nil
		}
		if seenCursors[reply.NextCursor] {
			return nil, fmt.Errorf("repeated async question cursor: %w", ErrHistoryUnavailable)
		}
		seenCursors[reply.NextCursor] = true
		cursor = reply.NextCursor
	}
	return nil, fmt.Errorf("async question page limit exceeded: %w", ErrHistoryUnavailable)
}

// resolveAsyncQuestionHistory applies answers only to questions preceding
// them. Earlier-turn questions precede every item in the current turn; a
// current-turn question becomes eligible only after its item is encountered.
func resolveAsyncQuestionHistory(questions []AsyncQuestion, raw json.RawMessage) ([]AsyncQuestion, error) {
	var turn struct {
		ID    string            `json:"id"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &turn); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, rawItem := range turn.Items {
		var item struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, fmt.Errorf("invalid async question history item: %w", ErrHistoryUnavailable)
		}
		seen[item.ID] = true
		if item.Type != "userMessage" {
			continue
		}
		text, err := historyUserText(item.Content)
		if err != nil {
			return nil, err
		}
		for index := range questions {
			question := &questions[index]
			if question.TurnID == turn.ID && !seen[question.ItemID] {
				continue
			}
			answered := make(map[string]bool, len(question.AnsweredIDs))
			for _, id := range question.AnsweredIDs {
				answered[id] = true
			}
			for _, field := range question.Questions {
				if !answered[field.ID] && AsyncQuestionAnswerMatches(field.Prompt, text) {
					question.AnsweredIDs = append(question.AnsweredIDs, field.ID)
				}
			}
		}
	}
	return questions, nil
}

func decodeAsyncQuestionTurn(raw json.RawMessage) ([]AsyncQuestion, error) {
	var turn struct {
		ID    string          `json:"id"`
		Items json.RawMessage `json:"items"`
	}
	if !historyObject(raw) || json.Unmarshal(raw, &turn) != nil || strings.TrimSpace(turn.ID) == "" || len(turn.ID) > 512 {
		return nil, fmt.Errorf("invalid async question turn: %w", ErrHistoryUnavailable)
	}
	items, err := historyArray(turn.Items)
	if err != nil {
		return nil, err
	}
	result := make([]AsyncQuestion, 0)
	seen := make(map[string]bool)
	for _, rawItem := range items {
		var kind struct {
			Type     string `json:"type"`
			Delivery string `json:"delivery"`
		}
		if !historyObject(rawItem) || json.Unmarshal(rawItem, &kind) != nil {
			return nil, fmt.Errorf("invalid async question item: %w", ErrHistoryUnavailable)
		}
		if kind.Type != "agentMessage" || kind.Delivery != "async" {
			continue
		}
		var item struct {
			ID        string                   `json:"id"`
			Type      string                   `json:"type"`
			Delivery  string                   `json:"delivery"`
			Text      string                   `json:"text"`
			Questions []asyncUserInputQuestion `json:"questions"`
		}
		if json.Unmarshal(rawItem, &item) != nil {
			return nil, fmt.Errorf("invalid async question item: %w", ErrHistoryUnavailable)
		}
		if item.Type != "agentMessage" || item.Delivery != "async" || len(item.Questions) == 0 {
			continue
		}
		if strings.TrimSpace(item.ID) == "" || len(item.ID) > 512 || seen[item.ID] {
			return nil, fmt.Errorf("invalid async question identity: %w", ErrHistoryUnavailable)
		}
		seen[item.ID] = true
		questions := normalizeAsyncQuestions(item.Questions)
		if len(questions) != len(item.Questions) {
			return nil, fmt.Errorf("empty async question title: %w", ErrHistoryUnavailable)
		}
		result = append(result, AsyncQuestion{TurnID: turn.ID, ItemID: item.ID, Text: item.Text, Questions: questions})
	}
	return result, nil
}
