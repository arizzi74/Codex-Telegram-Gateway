package worker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Preserve cursor, identity, and size guarantees for requests from older gateways.
func TestHistoryLegacyPagesAreNewestFirstExclusiveAndStable(t *testing.T) {
	prompts := make([]codexadapter.UserPrompt, 6)
	for i := range prompts {
		stamp := time.Date(2026, 9, 20, 10, i, 0, 0, time.UTC)
		prompts[i] = codexadapter.UserPrompt{TurnID: "turn", ItemID: fmt.Sprintf("item-%d", i), Text: fmt.Sprintf("prompt %d", i), Timestamp: &stamp}
	}
	first, err := historyPage(prompts[:5], &protocol.HistoryRequest{Limit: 2}, nil)
	if err != nil || len(first.Prompts) != 2 || first.Prompts[0].ItemID != "item-4" || first.Prompts[1].ItemID != "item-3" || first.Next == nil || first.Next.ItemID != "item-3" {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	if first.Prompts[0].Timestamp == nil || !first.Prompts[0].Timestamp.Equal(*prompts[4].Timestamp) {
		t.Fatalf("history lost original turn time: %#v", first.Prompts[0])
	}
	// A new prompt arriving does not shift the boundary of older pages.
	second, err := historyPage(prompts, &protocol.HistoryRequest{Limit: 2, Before: first.Next}, nil)
	if err != nil || len(second.Prompts) != 2 || second.Prompts[0].ItemID != "item-2" || second.Prompts[1].ItemID != "item-1" || second.Next == nil || second.Next.ItemID != "item-1" {
		t.Fatalf("second page = %#v, %v", second, err)
	}
	last, err := historyPage(prompts, &protocol.HistoryRequest{Limit: 2, Before: second.Next}, nil)
	if err != nil || len(last.Prompts) != 1 || last.Prompts[0].ItemID != "item-0" || last.Next != nil {
		t.Fatalf("last page = %#v, %v", last, err)
	}
	_, err = historyPage(prompts, &protocol.HistoryRequest{Limit: 2, Before: &protocol.HistoryCursor{TurnID: "unrelated", ItemID: "item-3"}}, nil)
	if err == nil {
		t.Fatal("unknown cursor restarted history instead of rejecting it")
	}
}

func TestHistoryLegacyRejectsUnusableMessageIdentities(t *testing.T) {
	for _, prompt := range []codexadapter.UserPrompt{
		{TurnID: "", ItemID: "item", Text: "saved"},
		{TurnID: "turn", ItemID: " ", Text: "saved"},
		{TurnID: strings.Repeat("t", 513), ItemID: "item", Text: "saved"},
		{TurnID: "turn", ItemID: strings.Repeat("i", 513), Text: "saved"},
	} {
		if _, err := historyPage([]codexadapter.UserPrompt{prompt}, &protocol.HistoryRequest{Limit: 10}, nil); err == nil {
			t.Fatal("accepted a prompt that cannot have a valid pagination cursor")
		}
	}
}

func TestHistoryLegacyRedactsAndBoundsPagesWithoutSkippingOlderPrompts(t *testing.T) {
	redactor, err := auth.NewRedactor([]string{`secret-value`}, "[hidden]")
	if err != nil {
		t.Fatal(err)
	}
	prompts := make([]codexadapter.UserPrompt, 7)
	for i := range prompts {
		prompts[i] = codexadapter.UserPrompt{TurnID: "turn", ItemID: fmt.Sprint(i), Text: "secret-value" + strings.Repeat("界", historyPromptRunes)}
	}
	request := &protocol.HistoryRequest{Limit: 50}
	var visited []string
	for {
		page, err := historyPage(prompts, request, redactor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Prompts) == 0 {
			t.Fatal("history failed to make progress")
		}
		total := 0
		for _, prompt := range page.Prompts {
			if !prompt.Truncated || !utf8.ValidString(prompt.Text) || !strings.HasPrefix(prompt.Text, "[hidden]") || strings.Contains(prompt.Text, "secret-value") {
				t.Fatalf("invalid redacted/truncated prompt %q", prompt.ItemID)
			}
			n := utf8.RuneCountInString(prompt.Text)
			if n > historyPromptRunes {
				t.Fatalf("prompt size = %d", n)
			}
			total += n
			visited = append(visited, prompt.ItemID)
		}
		if total > historyPageRunes {
			t.Fatalf("page size = %d", total)
		}
		if page.Next == nil {
			break
		}
		request.Before = page.Next
	}
	if want := []string{"6", "5", "4", "3", "2", "1", "0"}; !reflect.DeepEqual(visited, want) {
		t.Fatalf("pagination skipped or repeated input: %#v", visited)
	}
}
