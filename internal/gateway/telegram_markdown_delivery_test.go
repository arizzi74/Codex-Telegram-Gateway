package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func markdownDeliveryEntityText(t *testing.T, message SendMessage, entity TelegramEntity) string {
	t.Helper()
	units := utf16.Encode([]rune(message.Text))
	if entity.Offset < 0 || entity.Length <= 0 || entity.Offset+entity.Length > len(units) {
		t.Fatalf("entity outside message: %+v text length=%d", entity, len(units))
	}
	return string(utf16.Decode(units[entity.Offset : entity.Offset+entity.Length]))
}

func TestFinalMarkdownFormatsOnlyBodyWithUTF16Entities(t *testing.T) {
	for _, kind := range []string{"final_agent_message", "turn_completed"} {
		t.Run(kind, func(t *testing.T) {
			store := renderFixture()
			store.workers[0].Name = "**Lab 🧪**"
			store.runtimes[0].Name = "runtime_under_score"
			store.sessions[0].Name = "[session](https://identity.example)"
			body := "**Bold 🧪** with *italic* and `fmt.Println(\"ok\")`.\n\n[Docs](https://example.com/a_b?q=1&x=2)\n\n```go\nfmt.Println(\"🧪\")\n```"
			row := eventRow(t, kind, protocol.Result{Text: body}, testSessionID.String())
			sender := testSender(store, nil)
			chunks, err := sender.renderDeliveryMessages(context.Background(), row)
			if err != nil || len(chunks) != 1 {
				t.Fatalf("chunks=%d err=%v", len(chunks), err)
			}
			message, err := sender.deliveryMessageForSend(context.Background(), row, chunks[0])
			if err != nil {
				t.Fatal(err)
			}
			prefix := "✅ **Lab 🧪** / runtime_under_score / [session](https://identity.example)\n\n"
			if !strings.HasPrefix(message.Text, prefix) || strings.Contains(strings.TrimPrefix(message.Text, prefix), "**Bold") || message.ChatID != row.ChatID || message.TopicID != row.TopicID {
				t.Fatalf("final prefix/body or routing changed: %+v", message)
			}
			found := make(map[string]bool)
			for _, entity := range message.Entities {
				if entity.Offset < telegramTextLength(prefix) {
					t.Fatalf("identity was parsed as Markdown: %+v", entity)
				}
				text := markdownDeliveryEntityText(t, message, entity)
				switch entity.Type {
				case "bold":
					found["bold"] = text == "Bold 🧪"
				case "italic":
					found["italic"] = text == "italic"
				case "code":
					found["code"] = text == "fmt.Println(\"ok\")"
				case "text_link":
					found["link"] = text == "Docs" && entity.URL == "https://example.com/a_b?q=1&x=2"
				case "pre":
					found["pre"] = strings.Contains(text, "fmt.Println(\"🧪\")") && entity.Language == "go"
				}
			}
			for _, kind := range []string{"bold", "italic", "code", "link", "pre"} {
				if !found[kind] {
					t.Fatalf("missing %s entity: %+v", kind, message)
				}
			}
		})
	}
}

func TestFinalMarkdownRedactsRawBodyAndLinkDestinationBeforeParsing(t *testing.T) {
	redactor, err := auth.NewRedactor([]string{"SECRET", "TOKEN"}, "hidden")
	if err != nil {
		t.Fatal(err)
	}
	row := eventRow(t, "turn_completed", protocol.Result{Text: "**SECRET 🧪** and [Docs](https://example.test/TOKEN)"}, testSessionID.String())
	sender := testSender(renderFixture(), redactor)
	chunks, err := sender.renderDeliveryMessages(context.Background(), row)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks=%d err=%v", len(chunks), err)
	}
	if strings.Contains(string(chunks[0]), "SECRET") || strings.Contains(string(chunks[0]), "TOKEN") {
		t.Fatalf("secret persisted in formatted checkpoint: %s", chunks[0])
	}
	message, err := sender.deliveryMessageForSend(context.Background(), row, chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	var bold, link bool
	for _, entity := range message.Entities {
		text := markdownDeliveryEntityText(t, message, entity)
		bold = bold || entity.Type == "bold" && text == "hidden 🧪"
		link = link || entity.Type == "text_link" && text == "Docs" && entity.URL == "https://example.test/hidden"
	}
	if !bold || !link {
		t.Fatalf("redacted text/entity offsets disagree: %+v", message)
	}
}

func TestFinalMarkdownRedactsSecretsIntroducedByMarkdownNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, source, secret string
		visible              bool
	}{
		{"decimal reference", "SEC&#82;ET **public**", "SECRET", true},
		{"hex reference", "SEC&#x52;ET **public**", "SECRET", true},
		{"named reference", "SECRET&amp;VALUE **public**", "SECRET&VALUE", true},
		{"joined styles", "SEC**R**ET **public**", "SECRET", true},
		{"decimal URL", "[docs](https://example.test/SEC&#82;ET) **public**", "SECRET", false},
		{"named URL", "[docs](https://example.test/SECRET&amp;VALUE) **public**", "SECRET&VALUE", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redactor, err := auth.NewRedactor([]string{tc.secret}, "hidden")
			if err != nil {
				t.Fatal(err)
			}
			store := &sessionViewStore{renderStoreFake: renderFixture(), presentation: registry.TelegramSessionPresentation{Name: "Session", Marker: "🟦"}}
			sender := NewSender(store, nil, nil, SenderOptions{Redactor: redactor})
			row := eventRow(t, "turn_completed", protocol.Result{Text: tc.source}, testSessionID.String())
			chunks, err := sender.renderDeliveryMessages(context.Background(), row)
			if err != nil || len(chunks) != 1 {
				t.Fatalf("chunks=%d err=%v", len(chunks), err)
			}
			var checkpoint sessionDeliveryMessage
			if err := json.Unmarshal(chunks[0], &checkpoint); err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{checkpoint.Text, checkpoint.SessionBody, checkpoint.SessionName, checkpoint.SessionMarker} {
				if strings.Contains(value, tc.secret) {
					t.Fatalf("normalized secret persisted: %q", value)
				}
			}
			for _, entities := range [][]TelegramEntity{checkpoint.Entities, checkpoint.SessionBodyEntities} {
				for _, entity := range entities {
					if strings.Contains(entity.URL, tc.secret) || strings.Contains(entity.Language, tc.secret) || entity.Type == "text_link" {
						t.Fatalf("normalized secret retained in entity: %+v", entity)
					}
				}
			}
			if tc.visible {
				if !strings.Contains(checkpoint.Text, "hidden") || len(checkpoint.Entities) != 0 || len(checkpoint.SessionBodyEntities) != 0 {
					t.Fatalf("changed text retained stale entity offsets: %+v", checkpoint)
				}
			} else if len(checkpoint.Entities) != 1 || checkpoint.Entities[0].Type != "bold" {
				t.Fatalf("removing secret URL also lost safe formatting: %+v", checkpoint.Entities)
			}
		})
	}
}

func TestFinalMarkdownRedactionCanSpanIdentityAndNormalizedBody(t *testing.T) {
	redactor, err := auth.NewRedactor([]string{"(?s)auth-fix.*SECRET"}, "hidden")
	if err != nil {
		t.Fatal(err)
	}
	sender := testSender(renderFixture(), redactor)
	row := eventRow(t, "turn_completed", protocol.Result{Text: "SEC**R**ET"}, testSessionID.String())
	chunks, err := sender.renderDeliveryMessages(context.Background(), row)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks=%d err=%v", len(chunks), err)
	}
	message, err := sender.deliveryMessageForSend(context.Background(), row, chunks[0])
	if err != nil || strings.Contains(message.Text, "SECRET") || !strings.Contains(message.Text, "hidden") || len(message.Entities) != 0 {
		t.Fatalf("cross-boundary redaction lost after normalization: %+v err=%v", message, err)
	}
}

func TestFinalMarkdownSplitsWholeDocumentOnceAndReservesSessionHeaders(t *testing.T) {
	store := &sessionViewStore{renderStoreFake: renderFixture(), presentation: registry.TelegramSessionPresentation{Name: "Build **all** 🧪", Marker: "🟦", MultiSession: true}}
	sender := NewSender(store, nil, nil)
	inline := strings.Repeat("go🧪_", 1200)
	pre := strings.Repeat("echo '🧪'\n", 900)
	body := strings.Repeat("plain ", 620) + "`" + inline + "`\n\n```sh\n" + pre + "```"
	row := eventRow(t, "final_agent_message", protocol.Result{Text: body}, testSessionID.String())
	chunks, err := sender.renderDeliveryMessages(context.Background(), row)
	if err != nil || len(chunks) < 3 {
		t.Fatalf("chunks=%d err=%v", len(chunks), err)
	}
	var joined strings.Builder
	var codeLength, preLength int
	header := "🟦 Build **all** 🧪\n\n"
	for _, raw := range chunks {
		var checkpoint sessionDeliveryMessage
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			t.Fatal(err)
		}
		joined.WriteString(checkpoint.SessionBody)
		message, err := sender.deliveryMessageForSend(context.Background(), row, raw)
		if err != nil {
			t.Fatal(err)
		}
		if telegramTextLength(message.Text) > 4000 || !utf8.ValidString(message.Text) || !strings.HasPrefix(message.Text, header) {
			t.Fatalf("invalid final chunk: %+v", message)
		}
		if len(message.Entities) == 0 || message.Entities[0] != (TelegramEntity{Type: "bold", Length: telegramTextLength(strings.TrimSuffix(header, "\n\n"))}) {
			t.Fatalf("session label was parsed or lost: %+v", message.Entities)
		}
		for _, entity := range message.Entities {
			markdownDeliveryEntityText(t, message, entity)
			if entity.Type == "code" {
				codeLength += entity.Length
			}
			if entity.Type == "pre" {
				preLength += entity.Length
				if entity.Language != "sh" {
					t.Fatalf("split code fence lost language: %+v", entity)
				}
			}
		}
		if strings.Contains(string(mustMarshalMarkdownMessage(t, message)), "_session_") {
			t.Fatal("checkpoint metadata leaked into Telegram request")
		}
	}
	rendered, _ := renderTelegramMarkdown(body)
	if joined.String() != "✅ MacBook / Primary / auth-fix\n\n"+rendered || codeLength != telegramTextLength(inline) || preLength != telegramTextLength(pre) {
		t.Fatalf("split document changed text or formatting: code=%d want=%d pre=%d want=%d", codeLength, telegramTextLength(inline), preLength, telegramTextLength(pre))
	}
}

func mustMarshalMarkdownMessage(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSessionMarkdownEntitiesRemainImmutableAcrossModeChanges(t *testing.T) {
	body := "🧪 bold link"
	entities := []TelegramEntity{{Type: "bold", Offset: 3, Length: 4}, {Type: "text_link", Offset: 8, Length: 4, URL: "https://example.test"}}
	checkpoint := sessionDeliveryMessage{SendMessage: SendMessage{Text: body, Entities: append([]TelegramEntity(nil), entities...)}, SessionName: "**Session 🧪**", SessionMarker: "🟩", SessionBody: body, SessionBodyEntities: append([]TelegramEntity(nil), entities...)}
	original := mustMarshalMarkdownMessage(t, checkpoint)
	for _, multi := range []bool{true, false, true, true, false} {
		message := formatSessionDeliveryMessage(checkpoint, multi, "turn_completed")
		headerLength, entityStart := 0, 0
		if multi {
			headerLength = telegramTextLength("🟩 **Session 🧪**\n\n")
			entityStart = 1
		}
		if len(message.Entities) != len(entities)+entityStart {
			t.Fatalf("mode %v entities=%+v", multi, message.Entities)
		}
		for i, entity := range entities {
			entity.Offset += headerLength
			if message.Entities[i+entityStart] != entity {
				t.Fatalf("mode %v accumulated offsets: got=%+v want=%+v", multi, message.Entities[i+entityStart], entity)
			}
		}
		message.Entities[entityStart].Offset = 9999
		if string(mustMarshalMarkdownMessage(t, checkpoint)) != string(original) {
			t.Fatal("formatting mutated or aliased durable body entities")
		}
	}
}

func TestLegacyFinalCheckpointsStayFrozenAndRetainExistingEntities(t *testing.T) {
	store := &sessionViewStore{renderStoreFake: renderFixture(), presentation: registry.TelegramSessionPresentation{MultiSession: false}}
	sender := NewSender(store, nil, nil)
	row := eventRow(t, "turn_completed", protocol.Result{Text: "**new source**"}, testSessionID.String())
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"chat_id":42,"text":"**old source fragment**"}`),
		json.RawMessage(`{"chat_id":42,"text":"🟩 Old\n\n**old source fragment**","entities":[{"type":"bold","offset":0,"length":6}],"_session_name":"Old","_session_marker":"🟩","_session_body":"**old source fragment**"}`),
	} {
		message, err := sender.deliveryMessageForSend(context.Background(), row, raw)
		if err != nil || message.Text != "**old source fragment**" || len(message.Entities) != 0 {
			t.Fatalf("legacy checkpoint reparsed: %+v err=%v", message, err)
		}
	}
	checkpoint := sessionDeliveryMessage{SendMessage: SendMessage{Text: "🟩 Old\n\n🧪 code", Entities: []TelegramEntity{{Type: "bold", Length: 6}, {Type: "code", Offset: 11, Length: 4}}}, SessionName: "Old", SessionMarker: "🟩", SessionBody: "🧪 code"}
	message := formatSessionDeliveryMessage(checkpoint, false, "turn_completed")
	if !reflect.DeepEqual(message.Entities, []TelegramEntity{{Type: "code", Offset: 3, Length: 4}}) {
		t.Fatalf("legacy body entities were dropped: %+v", message.Entities)
	}
}

func TestMarkdownFormattingScopeLeavesOtherDeliveryKindsLiteral(t *testing.T) {
	for _, kind := range []string{"user_message", "agent_progress_message", "tool_progress_message", "command_completed"} {
		t.Run(kind, func(t *testing.T) {
			sender := testSender(renderFixture(), nil)
			row := eventRow(t, kind, protocol.Result{TurnID: "turn-a", Text: "**literal** `value` [link](https://example.test)"}, testSessionID.String())
			chunks, err := sender.renderDeliveryMessages(context.Background(), row)
			if err != nil || len(chunks) != 1 {
				t.Fatalf("chunks=%d err=%v", len(chunks), err)
			}
			message, err := sender.deliveryMessageForSend(context.Background(), row, chunks[0])
			if err != nil || !strings.Contains(message.Text, "**literal** `value` [link](https://example.test)") {
				t.Fatalf("literal output changed: %+v err=%v", message, err)
			}
			if kind == "tool_progress_message" {
				if !reflect.DeepEqual(message.Entities, []TelegramEntity{{Type: "pre", Length: telegramTextLength(message.Text)}}) {
					t.Fatalf("tool progress formatting changed: %+v", message.Entities)
				}
			} else if len(message.Entities) != 0 {
				t.Fatalf("unexpected Markdown entities: %+v", message.Entities)
			}
		})
	}
}

type markdownCheckpointStore struct {
	*sessionViewStore
	chunks       []registry.DeliveryChunk
	preparations int
}

func (s *markdownCheckpointStore) DeliveryChunks(context.Context, string) ([]registry.DeliveryChunk, error) {
	return s.chunks, nil
}

func (s *markdownCheckpointStore) PrepareDeliveryChunks(_ context.Context, _ string, messages []json.RawMessage) ([]registry.DeliveryChunk, error) {
	s.preparations++
	for i, raw := range messages {
		s.chunks = append(s.chunks, registry.DeliveryChunk{Index: i, Payload: append(json.RawMessage(nil), raw...)})
	}
	return s.chunks, nil
}

func (s *markdownCheckpointStore) MarkDeliveryChunkSent(_ context.Context, _ string, index int, _ int64, _, _, _ string, _ ...string) error {
	s.chunks[index].Sent = true
	return nil
}

type markdownRetryAPI struct {
	TelegramAPI
	messages []SendMessage
	failAt   int
}

func (a *markdownRetryAPI) Send(_ context.Context, message SendMessage) (int64, error) {
	a.messages = append(a.messages, message)
	if len(a.messages) == a.failAt {
		return 0, errors.New("temporary Telegram error")
	}
	return int64(100 + len(a.messages)), nil
}

func TestMarkdownDeliveryRetryReusesFrozenEntitiesAfterModeSwitch(t *testing.T) {
	store := &markdownCheckpointStore{sessionViewStore: &sessionViewStore{renderStoreFake: renderFixture(), presentation: registry.TelegramSessionPresentation{Name: "Build 🧪", Marker: "🟦", MultiSession: true}}}
	api := &markdownRetryAPI{failAt: 2}
	sender := NewSender(store, api, nil)
	row := eventRow(t, "final_agent_message", protocol.Result{Text: "`" + strings.Repeat("original🧪 ", 1200) + "`"}, testSessionID.String())
	row.ID = "markdown-final"
	if err := sender.sendDelivery(context.Background(), row); err == nil {
		t.Fatal("expected retryable Telegram error")
	}
	if len(store.chunks) < 3 || !store.chunks[0].Sent || store.chunks[1].Sent || len(api.messages) != 2 {
		t.Fatalf("unexpected checkpoint state: chunks=%+v messages=%d", store.chunks, len(api.messages))
	}
	before := make([]string, len(store.chunks))
	for i, chunk := range store.chunks {
		before[i] = string(chunk.Payload)
	}
	store.presentation.MultiSession = false
	api.failAt = 0
	row.Payload = eventRow(t, "final_agent_message", protocol.Result{Text: "**changed source must not be rendered**"}, testSessionID.String()).Payload
	// A new Sender represents a restarted gateway with the same durable chunks.
	if err := NewSender(store, api, nil).sendDelivery(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if store.preparations != 1 || len(api.messages) != len(store.chunks)+1 {
		t.Fatalf("retry rerendered or replayed accepted chunks: preparations=%d sends=%d chunks=%d", store.preparations, len(api.messages), len(store.chunks))
	}
	for i, chunk := range store.chunks {
		if !chunk.Sent || string(chunk.Payload) != before[i] {
			t.Fatalf("retry changed frozen checkpoint %d", i)
		}
	}
	header := "🟦 Build 🧪\n\n"
	failed, retried := api.messages[1], api.messages[2]
	if !strings.HasPrefix(failed.Text, header) || retried.Text != strings.TrimPrefix(failed.Text, header) || strings.Contains(retried.Text, "changed source") {
		t.Fatalf("retry text changed or session mode ignored: failed=%q retried=%q", failed.Text, retried.Text)
	}
	if len(failed.Entities) != len(retried.Entities)+1 || len(retried.Entities) == 0 {
		t.Fatalf("retry lost frozen formatting: failed=%+v retried=%+v", failed.Entities, retried.Entities)
	}
	for i, entity := range retried.Entities {
		entity.Offset += telegramTextLength(header)
		if failed.Entities[i+1] != entity {
			t.Fatalf("retry entity changed: failed=%+v retried=%+v", failed.Entities[i+1], entity)
		}
	}
}
