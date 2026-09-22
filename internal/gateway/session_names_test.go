package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/registry"
)

func renderSessionNameMessages(t *testing.T, sender *Sender, page int) ([]SendMessage, string) {
	t.Helper()
	raw, err := sender.renderDeliveryMessages(context.Background(), uiRow(t, registry.AcceptResult{
		View: "sessions", RuntimeID: testRuntimeID.String(), SessionPage: page,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("session page has no delivery messages")
	}
	messages := make([]SendMessage, len(raw))
	var text strings.Builder
	for index, payload := range raw {
		message := &messages[index]
		if err := json.Unmarshal(payload, message); err != nil {
			t.Fatal(err)
		}
		if message.ChatID != 99 || message.TopicID != 4 {
			t.Fatalf("chunk %d lost its Telegram destination", index)
		}
		if message.Text == "" || !utf8.ValidString(message.Text) || telegramTextLength(message.Text) > 4000 {
			t.Fatalf("chunk %d is not a valid bounded Telegram message: %d UTF-16 units", index, telegramTextLength(message.Text))
		}
		if index < len(raw)-1 && message.Keyboard != nil {
			t.Fatalf("chunk %d exposes controls before the complete session names", index)
		}
		text.WriteString(message.Text)
	}
	keyboard := messages[len(messages)-1].Keyboard
	if keyboard == nil {
		t.Fatal("final session page chunk is missing its controls")
	}
	assertSessionPageBudget(t, "", keyboard)
	return messages, text.String()
}

func TestSessionNamesRemainCompleteAcrossDeliveryChunks(t *testing.T) {
	var label strings.Builder
	for index := 0; index < 900; index++ {
		fmt.Fprintf(&label, "section-%04d😀 ", index)
	}
	fullLabel := strings.TrimSpace(label.String()) + " final-session-name-marker"
	for _, field := range []string{"name", "preview"} {
		t.Run(field, func(t *testing.T) {
			store := sessionPagesFixture(1)
			if field == "name" {
				store.sessions[1].Name = fullLabel
				store.sessions[1].Preview = "preview must not replace an explicit name"
			} else {
				store.sessions[1].Name = ""
				store.sessions[1].Preview = fullLabel
			}
			messages, text := renderSessionNameMessages(t, testSender(store, nil), 0)
			if len(messages) < 2 {
				t.Fatal("long session name was shortened instead of split across messages")
			}
			if !strings.Contains(text, "1. ⚪ "+fullLabel+"\n") {
				t.Fatal("full session name was not preserved when delivery chunks were reassembled")
			}
			if !strings.Contains(text, "Idle · Persisted\n/work/project") {
				t.Fatal("session details should place the workspace on its own line")
			}
			if strings.Contains(text, "preview must not replace") {
				t.Fatal("preview replaced the explicit session name")
			}
			keyboard := messages[len(messages)-1].Keyboard
			if keyboard.Rows[0][0].Text != "1 section-0000😀 sectio…" || keyboard.Rows[0][1].Text != "Status 1" {
				t.Fatalf("long name did not produce a compact numbered session control: %#v", keyboard.Rows[0])
			}
		})
	}
}

func TestSessionNamesAreRedactedBeforeDeliverySplits(t *testing.T) {
	redactor, err := auth.NewRedactor([]string{`token=[a-z]+`}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"name", "preview"} {
		t.Run(field, func(t *testing.T) {
			store := sessionPagesFixture(1)
			prefix := strings.Repeat("😀 readable ", 500)
			secret := "token=" + strings.Repeat("sensitive", 1000)
			label := prefix + secret + " readable tail"
			if field == "name" {
				store.sessions[1].Name = label
			} else {
				store.sessions[1].Name, store.sessions[1].Preview = "", label
			}
			messages, text := renderSessionNameMessages(t, testSender(store, redactor), 0)
			if len(messages) < 2 || !strings.Contains(text, prefix+"[REDACTED] readable tail") {
				t.Fatal("redacted name lost readable content during splitting")
			}
			if strings.Contains(text, "sensitive") || strings.Contains(text, "token=") {
				t.Fatal("session name leaked a secret across message chunks")
			}
		})
	}
}

func TestSessionSelectionLabelsFitWithoutSplittingUnicode(t *testing.T) {
	for _, test := range []struct {
		name string
		want string
	}{
		{"Smart Stage", "1 Smart Stage"},
		{" Smart\n\t Stage ", "1 Smart Stage"},
		{strings.Repeat("x", 22), "1 " + strings.Repeat("x", 22)},
		{strings.Repeat("x", 23), "1 " + strings.Repeat("x", 21) + "…"},
		{strings.Repeat("😀", 11), "1 " + strings.Repeat("😀", 11)},
		{strings.Repeat("😀", 12), "1 " + strings.Repeat("😀", 10) + "…"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := sessionPagesFixture(1)
			store.sessions[1].Name = test.name
			messages, _ := renderSessionNameMessages(t, testSender(store, nil), 0)
			button := messages[len(messages)-1].Keyboard.Rows[0][0]
			if button.Text != test.want {
				t.Fatalf("session label = %q; want %q", button.Text, test.want)
			}
		})
	}
}

func TestSessionNameNumbersMatchCallbacksAcrossPages(t *testing.T) {
	store := sessionPagesFixture(23)
	idsByName := make(map[string]string)
	for _, session := range store.sessions {
		idsByName[session.Name] = session.ID
	}
	messages, text := renderSessionNameMessages(t, testSender(store, nil), 1)
	keyboard := messages[len(messages)-1].Keyboard
	for index := 0; index < sessionPageSize; index++ {
		number := sessionPageSize + index + 1
		name := fmt.Sprintf("session-%03d", number-1)
		if !strings.Contains(text, fmt.Sprintf("\n\n%d. ⚪ %s\n", number, name)) {
			t.Fatalf("session %s is missing its global number %d", name, number)
		}
		for column, action := range []string{"select", "status"} {
			callbackIndex := 2*index + column
			button := keyboard.Rows[index][column]
			callback := store.callbacks[callbackIndex]
			wantLabel := fmt.Sprintf("%d %s", number, name)
			if action == "status" {
				wantLabel = fmt.Sprintf("Status %d", number)
			}
			if button.Text != wantLabel {
				t.Fatalf("session %s control has ambiguous label %q", name, button.Text)
			}
			if button.Data != "cb:cb_opaque_"+string(rune('a'+callbackIndex)) || callback.Action != action || callback.SessionID.String() != idsByName[name] {
				t.Fatalf("%s does not target its displayed session", wantLabel)
			}
		}
	}
	if strings.Contains(text, "\n\n1. ") || strings.Contains(text, "session-020") {
		t.Fatal("page 2 reset session numbers or included a session from another page")
	}
}

func TestSessionNameContentDoesNotIncreaseKeyboardSize(t *testing.T) {
	store := sessionPagesFixture(30)
	label := strings.Repeat("<>&\"\\😀", 900)
	for index := range store.sessions {
		store.sessions[index].Name = label
	}
	sender := NewSender(fullTokenRenderStore{store}, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
	messages, text := renderSessionNameMessages(t, sender, 1)
	if strings.Count(text, label) != sessionPageSize {
		t.Fatal("session names with markup characters were escaped or truncated in plain text")
	}
	keyboard := messages[len(messages)-1].Keyboard
	if len(keyboard.Rows) != sessionPageSize+2 {
		t.Fatal("middle session page lost navigation or new-session controls")
	}
	for index, row := range keyboard.Rows[:sessionPageSize] {
		number := sessionPageSize + index + 1
		if len(row) != 2 || !strings.HasPrefix(row[0].Text, fmt.Sprintf("%d ", number)) || !strings.HasSuffix(row[0].Text, "…") || row[1].Text != fmt.Sprintf("Status %d", number) {
			t.Fatalf("session row %d includes a long or ambiguous control", number)
		}
	}
}
