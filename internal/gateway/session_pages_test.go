package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func sessionPagesFixture(count int) *renderStoreFake {
	store := renderFixture()
	store.sessions = store.sessions[1:]
	for index := count - 1; index >= 0; index-- {
		store.sessions = append(store.sessions, protocol.Session{
			ID: uuid.NewString(), RuntimeID: testRuntimeID.String(), WorkerID: testWorkerID.String(),
			ThreadID: fmt.Sprintf("thread-%03d", index), Name: fmt.Sprintf("session-%03d", index),
			State: "idle", CWD: "/work/project", UpdatedAt: time.Unix(int64(index), 0),
		})
	}
	return store
}

func TestSessionPagesReachEverySessionWithBoundedControls(t *testing.T) {
	store := sessionPagesFixture(49)
	store.sessions = append(store.sessions, protocol.Session{ID: uuid.NewString(), RuntimeID: testRuntimeID.String(), Archived: true, Name: "archived"})
	seen := map[uuid.UUID]bool{}
	for page := 0; page < 5; page++ {
		store.callbacks = nil
		text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String(), SessionPage: page}))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, fmt.Sprintf("Page %d of 5 · 49 sessions", page+1)) || strings.Contains(text, "other-session") || strings.Contains(text, "archived") {
			t.Fatalf("incorrect page summary: %q", text)
		}
		selected, previous, next := 0, false, false
		for _, callback := range store.callbacks {
			if callback.RuntimeID != testRuntimeID || callback.Generation != 7 || callback.UserID != 42 || callback.ChatID != 99 || callback.TopicID != 4 {
				t.Fatalf("page callback lost its target: %#v", callback)
			}
			switch callback.Action {
			case "select":
				if seen[callback.SessionID] {
					t.Fatal("session repeated across pages")
				}
				seen[callback.SessionID] = true
				if !strings.Contains(text, fmt.Sprintf("session-%03d", page*sessionPageSize+selected)) {
					t.Fatal("session order changed with activity timestamps")
				}
				selected++
			case "sessions":
				switch callback.SessionPage {
				case page - 1:
					previous = true
				case page + 1:
					next = true
				default:
					t.Fatal("navigation targets an unrelated page")
				}
			}
		}
		if selected != min(sessionPageSize, 49-page*sessionPageSize) || previous != (page > 0) || next != (page < 4) {
			t.Fatalf("page %d: selected=%d previous=%v next=%v", page, selected, previous, next)
		}
		assertSessionPageBudget(t, text, keyboard)
	}
	if len(seen) != 49 {
		t.Fatalf("only %d sessions accessible", len(seen))
	}
}

type fullTokenRenderStore struct{ *renderStoreFake }

func (f fullTokenRenderStore) CreateCallback(ctx context.Context, callback registry.Callback) (string, error) {
	if _, err := f.renderStoreFake.CreateCallback(ctx, callback); err != nil {
		return "", err
	}
	// Exercise the maximum callback size accepted by the renderer, including
	// its cb: prefix, rather than the shorter test fake's usual tokens.
	return strings.Repeat("x", 61), nil
}

func TestSessionPageBoundsLongEscapedAndUnicodeLabels(t *testing.T) {
	for _, label := range []string{strings.Repeat("<>&", 500), strings.Repeat("😀\"\\\n", 500)} {
		store := sessionPagesFixture(100)
		store.workers[0].Name = label
		store.runtimes[0].Name = label
		for i := range store.sessions {
			store.sessions[i].Name, store.sessions[i].CWD, store.sessions[i].State = label, "/"+label, label
		}
		sender := NewSender(fullTokenRenderStore{store}, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
		text, keyboard, err := sender.render(context.Background(), uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String(), SessionPage: 2}))
		if err != nil {
			t.Fatal(err)
		}
		assertSessionPageBudget(t, text, keyboard)
	}
}

func assertSessionPageBudget(t *testing.T, text string, keyboard *TelegramKeyboard) {
	t.Helper()
	if !utf8.ValidString(text) || telegramTextLength(text) > 4000 {
		t.Fatalf("page exceeds a single valid Telegram message: %d", telegramTextLength(text))
	}
	markup, err := json.Marshal(keyboard)
	if err != nil || len(markup) > sessionKeyboardMaxBytes {
		t.Fatalf("keyboard exceeds budget: bytes=%d error=%v", len(markup), err)
	}
	buttons := 0
	for _, row := range keyboard.Rows {
		buttons += len(row)
		for _, button := range row {
			if len(button.Data) > 64 || len(button.Text) > 36 || !utf8.ValidString(button.Text) || strings.Contains(button.Text, "\n") {
				t.Fatal("invalid or unbounded page button")
			}
		}
	}
	if buttons > 2*sessionPageSize+3 {
		t.Fatalf("page has too many buttons: %d", buttons)
	}
}

func TestSessionPagesHandleEmptyAndRemovedSessions(t *testing.T) {
	for _, count := range []int{0, 1, 10, 11} {
		store := sessionPagesFixture(count)
		text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String(), SessionPage: 1000000}))
		if err != nil {
			t.Fatal(err)
		}
		pages := max(1, (count+9)/10)
		if !strings.Contains(text, fmt.Sprintf("Page %d of %d", pages, pages)) || strings.Contains(text, "No sessions found") != (count == 0) {
			t.Fatalf("invalid clamped page: %q", text)
		}
		assertSessionPageBudget(t, text, keyboard)
	}
	store := sessionPagesFixture(10)
	if _, _, err := testSender(store, nil).render(context.Background(), uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String(), SessionPage: -1})); err == nil {
		t.Fatal("negative page accepted")
	}
}

func TestSessionPageRedactsBeforeTruncating(t *testing.T) {
	store := sessionPagesFixture(1)
	secret := "token=" + strings.Repeat("sensitive", 100)
	store.sessions[1].Name, store.sessions[1].CWD = secret, "/work/"+secret
	redactor, err := auth.NewRedactor([]string{`token=[a-z]+`}, "")
	if err != nil {
		t.Fatal(err)
	}
	text, keyboard, err := testSender(store, redactor).render(context.Background(), uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String()}))
	if err != nil {
		t.Fatal(err)
	}
	markup, _ := json.Marshal(keyboard)
	if strings.Contains(text+string(markup), "sensitive") || !strings.Contains(text, "[REDACTED]") {
		t.Fatal("truncation exposed a partial secret")
	}
	assertSessionPageBudget(t, text, keyboard)
	store.sessions[1].Name, store.sessions[1].Preview = "", secret
	text, keyboard, err = testSender(store, redactor).render(context.Background(), uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String()}))
	if err != nil {
		t.Fatal(err)
	}
	markup, _ = json.Marshal(keyboard)
	if strings.Contains(text+string(markup), "sensitive") {
		t.Fatal("fallback preview truncation exposed a partial secret")
	}
}
