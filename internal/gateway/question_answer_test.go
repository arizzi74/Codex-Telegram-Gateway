package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/registry"
)

type questionAnswerStore struct {
	*renderStoreFake
	row          registry.Delivery
	chunks       []registry.DeliveryChunk
	marked       []int64
	markErr      error
	retryDelay   time.Duration
	presentCalls int
}

func (s *questionAnswerStore) ClaimDeliveries(context.Context, int) ([]registry.Delivery, error) {
	return []registry.Delivery{s.row}, nil
}

func (s *questionAnswerStore) DeliveryChunks(context.Context, string) ([]registry.DeliveryChunk, error) {
	return s.chunks, nil
}

func (s *questionAnswerStore) PrepareDeliveryChunks(_ context.Context, _ string, messages []json.RawMessage) ([]registry.DeliveryChunk, error) {
	for index, raw := range messages {
		s.chunks = append(s.chunks, registry.DeliveryChunk{Index: index, Payload: raw})
	}
	return s.chunks, nil
}

func (s *questionAnswerStore) MarkDeliveryChunkSent(_ context.Context, _ string, index int, messageID int64, session, turn, approval string, questions ...string) error {
	if session != "" || turn != "" || approval != "" || (len(questions) != 0 && questions[0] != "") {
		return errors.New("question edit attempted to change reply routing")
	}
	if s.markErr != nil {
		err := s.markErr
		s.markErr = nil
		return err
	}
	s.chunks[index].Sent = true
	s.marked = append(s.marked, messageID)
	return nil
}

func (s *questionAnswerStore) RetryDelivery(_ context.Context, _ string, delay time.Duration, _ string) error {
	s.retryDelay = delay
	return nil
}

func (s *questionAnswerStore) TelegramSessionPresentation(context.Context, string, int64, int64, string) (registry.TelegramSessionPresentation, error) {
	s.presentCalls++
	return registry.TelegramSessionPresentation{Name: "Other session", Marker: "🔵", MultiSession: true}, nil
}

func questionAnswerFixture(t *testing.T, question, answer string) *questionAnswerStore {
	t.Helper()
	raw, err := json.Marshal(registry.QuestionAnswerEdit{MessageID: 83, Question: question, Answer: answer})
	if err != nil {
		t.Fatal(err)
	}
	return &questionAnswerStore{renderStoreFake: renderFixture(), row: registry.Delivery{
		ID: "answered-question", Kind: "question_answered", BotID: "bot", ChatID: 42, TopicID: 7, Payload: raw,
	}}
}

func TestQuestionAnswerEditsOriginalAsPlainRedactedTextWithNoKeyboard(t *testing.T) {
	store := questionAnswerFixture(t, "Which 🖥️ system?\nToken: PRIVATE_VALUE", "Linux & <literal>\n秘密 🙂")
	redactor, err := auth.NewRedactor([]string{"PRIVATE_VALUE"}, "")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/botfake-token/editMessageText" {
			t.Errorf("question answer sent a new message: %s", r.URL.Path)
		}
		var payload struct {
			ChatID    int64           `json:"chat_id"`
			MessageID int64           `json:"message_id"`
			Text      string          `json:"text"`
			Entities  json.RawMessage `json:"entities"`
			Markup    json.RawMessage `json:"reply_markup"`
			ParseMode string          `json:"parse_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.ChatID != 42 || payload.MessageID != 83 || payload.ParseMode != "" || string(payload.Entities) != "[]" || string(payload.Markup) != `{"inline_keyboard":[]}` {
			t.Errorf("wrong edit destination or controls: %+v", payload)
		}
		if want := "Question: Which 🖥️ system?\nToken: [REDACTED]\n\nAnswer: Linux & <literal>\n秘密 🙂"; payload.Text != want {
			t.Errorf("text = %q, want %q", payload.Text, want)
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":83}}`))
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint, client.http = server.URL, server.Client()
	sender := NewSender(store, client, nil, SenderOptions{Redactor: redactor})
	for range 2 {
		if err := sender.sendDelivery(t.Context(), store.row); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || len(store.marked) != 1 || store.marked[0] != 83 || len(store.chunks) != 1 || store.presentCalls != 0 || len(store.callbacks) != 0 {
		t.Fatalf("answer repeated or acquired session presentation: calls=%d store=%+v", calls, store)
	}
}

func TestQuestionAnswerRetriesDurableEditAfterCheckpointFailure(t *testing.T) {
	store := questionAnswerFixture(t, "Which system?", "Linux")
	store.markErr = errors.New("checkpoint interrupted")
	api := &progressReplacementAPIFake{}
	if err := NewSender(store, api, nil).sendDelivery(t.Context(), store.row); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	api.editErr = &TelegramError{Code: 400, Description: "Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}
	if err := NewSender(store, api, nil).sendDelivery(t.Context(), store.row); err != nil {
		t.Fatal(err)
	}
	if len(api.messages) != 0 || len(api.edits) != 2 || len(store.marked) != 1 || len(store.chunks) != 1 || !store.chunks[0].Sent || api.editIDs[0] != 83 || api.editIDs[1] != 83 {
		t.Fatalf("durable edit retry lost identity or reposted: edits=%v marked=%v", api.editIDs, store.marked)
	}
	if api.edits[0].Text != api.edits[1].Text || api.edits[1].Keyboard == nil || len(api.edits[1].Keyboard.Rows) != 0 {
		t.Fatal("edit retry lost frozen text or keyboard removal")
	}
}

func TestQuestionAnswerMissingOriginalSettlesAndTransientErrorsRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		settle bool
		delay  time.Duration
	}{
		{"deleted", &TelegramError{Code: 400, Description: "Bad Request: message to edit not found"}, true, 0},
		{"uneditable", &TelegramError{Code: 400, Description: "Bad Request: message can't be edited"}, true, 0},
		{"uneditable alternate", &TelegramError{Code: 400, Description: "Bad Request: message cannot be edited"}, true, 0},
		{"unchanged", &TelegramError{Code: 400, Description: "Bad Request: message is not modified"}, true, 0},
		{"rate limited", &TelegramError{Code: 429, RetryAfter: 19 * time.Second}, false, 19 * time.Second},
		{"retry hint", &TelegramError{Code: 400, Description: "Bad Request: message can't be edited", RetryAfter: 9 * time.Second}, false, 9 * time.Second},
		{"server unavailable", &TelegramError{Code: 502}, false, time.Second},
		{"other bad request", &TelegramError{Code: 400, Description: "Bad Request: invalid payload"}, false, time.Second},
		{"timeout", context.DeadlineExceeded, false, time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := questionAnswerFixture(t, "Ready?", "Yes")
			api := &progressReplacementAPIFake{editErr: test.err}
			err := NewSender(store, api, nil).flush(t.Context())
			if (err == nil) != test.settle || (len(store.marked) == 1) != test.settle || store.retryDelay != test.delay {
				t.Fatalf("settled=%v err=%v marked=%v retry=%v", test.settle, err, store.marked, store.retryDelay)
			}
			if len(api.messages) != 0 || len(api.editIDs) != 1 || api.editIDs[0] != 83 {
				t.Fatal("edit failure sent a replacement message")
			}
			if !test.settle {
				api.editErr = nil
				if err := NewSender(store, api, nil).flush(t.Context()); err != nil || len(store.marked) != 1 {
					t.Fatalf("retry did not complete: %v", err)
				}
			}
		})
	}
}

func TestQuestionAnswerLongFieldsKeepBothWithinTelegramUTF16Limit(t *testing.T) {
	for _, test := range []struct{ question, answer string }{
		{"Which system?", "Linux"},
		{strings.Repeat("🧪", 9000), "Linux"},
		{"Which system?", strings.Repeat("🙂", 9000)},
		{strings.Repeat("🧪", 9000), strings.Repeat("🙂", 9000)},
		{strings.Repeat("q", 9000), strings.Repeat("a", 9000)},
	} {
		text := compactQuestionAnswer(test.question, test.answer)
		question, answer, ok := strings.Cut(strings.TrimPrefix(text, "Question: "), "\n\nAnswer: ")
		if !ok || !utf8.ValidString(text) || len(utf16.Encode([]rune(text))) > 4096 || question == "" || answer == "" {
			t.Fatalf("invalid shortened text: length=%d", len(utf16.Encode([]rune(text))))
		}
		for _, field := range [][2]string{{question, test.question}, {answer, test.answer}} {
			if field[0] != field[1] && (!strings.HasSuffix(field[0], "…") || !strings.HasPrefix(field[1], strings.TrimSuffix(field[0], "…"))) {
				t.Fatal("truncation lost original field content or marker")
			}
		}
	}
}

func TestQuestionAnswerRedactsBeforeTruncating(t *testing.T) {
	store := questionAnswerFixture(t, strings.Repeat("🧪", 1020)+"SECRET_BOUNDARY"+strings.Repeat("x", 6000), strings.Repeat("a", 8000))
	redactor, err := auth.NewRedactor([]string{"SECRET_BOUNDARY"}, "safe")
	if err != nil {
		t.Fatal(err)
	}
	api := &progressReplacementAPIFake{}
	if err := NewSender(store, api, nil, SenderOptions{Redactor: redactor}).sendDelivery(t.Context(), store.row); err != nil {
		t.Fatal(err)
	}
	if len(api.edits) != 1 || strings.Contains(api.edits[0].Text, "SECRET") || telegramTextLength(api.edits[0].Text) > 4096 {
		t.Fatal("long answer bypassed redaction or Telegram's text limit")
	}
}

func TestTelegramClientExplicitlyRemovesInlineKeyboard(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if string(payload["reply_markup"]) != `{"inline_keyboard":[]}` {
			t.Errorf("empty keyboard was omitted: %s", payload["reply_markup"])
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	client := NewTelegramClient("fake-token")
	client.endpoint, client.http = server.URL, server.Client()
	if err := client.Edit(t.Context(), 42, 83, "Answered", &TelegramKeyboard{}); err != nil {
		t.Fatal(err)
	}
	if err := client.EditFormatted(t.Context(), 83, SendMessage{ChatID: 42, Text: "Answered", Keyboard: &TelegramKeyboard{}}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("edit calls=%d", calls)
	}
}
