package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf16"

	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Sender) renderQuestionAnswer(row registry.Delivery) ([]json.RawMessage, error) {
	var edit registry.QuestionAnswerEdit
	if err := json.Unmarshal(row.Payload, &edit); err != nil || edit.MessageID <= 0 {
		return nil, errors.New("invalid answered question edit")
	}
	question := s.options.Redactor.Redact(edit.Question)
	answer := s.options.Redactor.Redact(edit.Answer)
	message := SendMessage{
		ChatID: row.ChatID, TopicID: row.TopicID,
		Text:     compactQuestionAnswer(question, answer),
		Keyboard: &TelegramKeyboard{},
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{raw}, nil
}

// An answer replaces one existing message. Reserve room for both fields and
// spend unused space from a shorter field on the longer one; never split an
// oversized answer into new messages or cut an astral Unicode character.
func compactQuestionAnswer(question, answer string) string {
	const questionPrefix = "Question: "
	const answerPrefix = "\n\nAnswer: "
	const budget = 4096 - len(questionPrefix) - len(answerPrefix)
	questionLength, answerLength := telegramTextLength(question), telegramTextLength(answer)
	if questionLength+answerLength > budget {
		questionBudget := min(questionLength, max(budget/2, budget-answerLength))
		question = truncateQuestionAnswerField(question, questionBudget)
		answer = truncateQuestionAnswerField(answer, budget-telegramTextLength(question))
	}
	return questionPrefix + question + answerPrefix + answer
}

func truncateQuestionAnswerField(text string, limit int) string {
	if telegramTextLength(text) <= limit {
		return text
	}
	remaining := limit - 1 // Keep a visible truncation marker.
	for index, r := range text {
		remaining -= utf16.RuneLen(r)
		if remaining < 0 {
			return text[:index] + "…"
		}
	}
	return text
}

func (s *Sender) sendQuestionAnswer(ctx context.Context, row registry.Delivery, message SendMessage) (int64, error) {
	var edit registry.QuestionAnswerEdit
	if err := json.Unmarshal(row.Payload, &edit); err != nil || edit.MessageID <= 0 {
		return 0, errors.New("invalid answered question edit")
	}
	// Force removal of all question controls and formatting, including when
	// retrying a previously prepared checkpoint.
	message.Keyboard, message.Entities = &TelegramKeyboard{}, []TelegramEntity{}
	if err := s.authorizeDeliveryDestination(ctx, row, message.ChatID); err != nil {
		return 0, err
	}
	var err error
	if api, ok := s.api.(TelegramFormattedEditAPI); ok {
		err = api.EditFormatted(ctx, edit.MessageID, message)
	} else {
		err = s.api.Edit(ctx, message.ChatID, edit.MessageID, message.Text, message.Keyboard)
	}
	if questionAnswerEditSettled(err) {
		return edit.MessageID, nil
	}
	return edit.MessageID, err
}

func questionAnswerEditSettled(err error) bool {
	var telegram *TelegramError
	if !errors.As(err, &telegram) || telegram.Code != 400 || telegram.RetryAfter > 0 {
		return false
	}
	description := strings.ToLower(strings.TrimSpace(telegram.Description))
	// A checkpoint may fail after Telegram accepted the edit. Missing or
	// permanently uneditable originals also finish without reposting them.
	return description == "bad request: message is not modified" ||
		strings.HasPrefix(description, "bad request: message is not modified:") ||
		description == "bad request: message to edit not found" ||
		description == "bad request: message can't be edited" ||
		description == "bad request: message cannot be edited"
}
