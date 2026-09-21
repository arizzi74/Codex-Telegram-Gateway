package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
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
	// Answers preserves the text following each native quoted-title marker.
	// Superseded fields never receive invented answers.
	Answers map[string][]string
	// SupersededIDs records fields cleared by a later ordinary user prompt.
	// This follows the native input lifecycle without claiming they were answered.
	SupersededIDs []string
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
	_, ok := extractAsyncQuestionAnswer(title, text, nil, true)
	return ok
}

// AsyncQuestionAnswer returns the explicit native answer, preserving its
// multiline body. Only the quoted-title framing and inter-answer separator
// are removed. Callers supply known titles when a submission can contain
// several answers. Other quoted paragraphs belong to the answer itself.
// An ordinary subsequent prompt is not evidence of an answer.
func AsyncQuestionAnswer(title, text string, knownTitles ...string) (string, bool) {
	return extractAsyncQuestionAnswer(title, text, knownTitles, false)
}

func extractAsyncQuestionAnswer(title, text string, knownTitles []string, matchOnly bool) (string, bool) {
	text = asyncQuestionPromptText(text)
	prefix := asyncQuestionAnswerPrefix(title)
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], prefix)
		if index < 0 {
			return "", false
		}
		index += offset
		if index == 0 || strings.HasSuffix(text[:index], "\n\n") {
			answer := text[index+len(prefix):]
			end := len(answer)
			for _, known := range knownTitles {
				if next := strings.Index(answer, "\n\n"+asyncQuestionAnswerPrefix(known)); next >= 0 && next < end {
					end = next
				}
			}
			// Keep the legacy boolean marker recognizer conservative for an
			// empty answer followed by a separate, unknown quoted question.
			if matchOnly {
				if next := strings.Index(answer, "\n\n> "); next >= 0 && next < end {
					end = next
				}
			}
			answer = answer[:end]
			if strings.TrimSpace(answer) != "" {
				return answer, true
			}
		}
		offset = index + len(prefix)
	}
	return "", false
}

// AsyncQuestionInputSupersedes follows the native TUI's ordinary-prompt
// lifecycle: submitting a new prompt clears its queued async questions. A
// native quoted-title answer only resolves matching fields instead. Context
// fragments injected into user-message history are not user submissions.
func AsyncQuestionInputSupersedes(text string) bool {
	text = asyncQuestionPromptText(text)
	if text == "" {
		return false
	}
	// Preserve other questions even when an explicit answer's title does not
	// match any locally known request, or its answer is empty. Neither is an
	// ordinary prompt from which we can infer that all questions were cleared.
	if strings.HasPrefix(text, "> ") && strings.Contains(text, "\n\n") {
		return false
	}
	return true
}

var asyncExternalContext = regexp.MustCompile(`^<external_([A-Za-z0-9_]+)>`)
var asyncInternalContext = regexp.MustCompile(`^<codex_internal_context source="[a-z][a-z0-9_]*">`)

// asyncQuestionPromptText removes only complete, recognized context wrappers
// at the start of input. History may concatenate several text parts, so keep
// walking those wrappers and retain any actual prompt that follows them.
func asyncQuestionPromptText(text string) string {
	for {
		// Keep trailing newlines: even an unanswered native quote remains an
		// explicit answer-shaped input, never a request to clear every question.
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
		lower := strings.ToLower(text)
		closing := ""
		for _, pair := range [][2]string{
			{"# agents.md instructions", "</instructions>"},
			{"<user_instructions>", "</user_instructions>"},
			{"<environment_context>", "</environment_context>"},
			{"<skill>", "</skill>"},
			{"<user_shell_command>", "</user_shell_command>"},
			{"<turn_aborted>", "</turn_aborted>"},
			{"<subagent_notification>", "</subagent_notification>"},
			{"<recommended_plugins>", "</recommended_plugins>"},
			{"<goal_context>", "</goal_context>"},
		} {
			if strings.HasPrefix(lower, pair[0]) {
				closing = pair[1]
				break
			}
		}
		if match := asyncExternalContext.FindStringSubmatch(lower); match != nil {
			closing = "</external_" + match[1] + ">"
		} else if asyncInternalContext.MatchString(lower) {
			closing = "</codex_internal_context>"
		} else if strings.HasPrefix(lower, "<hook_prompt ") && strings.Contains(strings.SplitN(lower, ">", 2)[0], `hook_run_id="`) {
			closing = "</hook_prompt>"
		}
		if closing != "" {
			if index := asyncContextClosingIndex(text, closing); index >= 0 {
				text = text[index+len(closing):]
				continue
			}
		}
		if strings.HasPrefix(text, "Warning: The maximum number of unified exec processes you can keep open is") ||
			strings.HasPrefix(text, "Warning: Your account was flagged for potentially high-risk cyber activity") ||
			(strings.HasPrefix(text, "Warning: apply_patch was requested via ") && strings.HasSuffix(strings.TrimSpace(text), "Use the apply_patch tool instead of exec_command.")) {
			return ""
		}
		return text
	}
}

func asyncContextClosingIndex(text, closing string) int {
	for offset := 0; offset < len(text); {
		index := strings.IndexByte(text[offset:], '<')
		if index < 0 {
			break
		}
		index += offset
		if len(text)-index >= len(closing) && strings.EqualFold(text[index:index+len(closing)], closing) {
			return index
		}
		offset = index + 1
	}
	return -1
}

// AsyncQuestions reads structured asynchronous questions in chronological
// order without resuming or changing a thread. AnsweredIDs identifies fields
// followed by native quoted-title answers, including responses entered through
// a CLI. SupersededIDs identifies fields cleared by a later ordinary prompt,
// matching the native TUI lifecycle. The protocol has no authoritative pending
// flag; callers must also apply their own durable response records. Tools,
// reasoning and attachment bytes are not returned.
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

// resolveAsyncQuestionHistory applies user input only to questions preceding
// it. Earlier-turn questions precede every item in the current turn; a
// current-turn question becomes eligible only after its item is encountered.
func resolveAsyncQuestionHistory(questions []AsyncQuestion, raw json.RawMessage) ([]AsyncQuestion, error) {
	var titles []string
	for _, question := range questions {
		for _, field := range question.Questions {
			titles = append(titles, field.Prompt)
		}
	}
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
		supersedes := AsyncQuestionInputSupersedes(text)
		for index := range questions {
			question := &questions[index]
			if question.TurnID == turn.ID && !seen[question.ItemID] {
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
				if supersedes {
					question.SupersededIDs = append(question.SupersededIDs, field.ID)
				} else if answer, ok := AsyncQuestionAnswer(field.Prompt, text, titles...); ok {
					question.AnsweredIDs = append(question.AnsweredIDs, field.ID)
					if question.Answers == nil {
						question.Answers = make(map[string][]string)
					}
					question.Answers[field.ID] = []string{answer}
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
