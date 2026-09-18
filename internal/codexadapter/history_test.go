package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUserPromptsReadsOnlyRequestedThreadHistory(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type outcome struct {
		prompts []UserPrompt
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		prompts, err := client.UserPrompts(context.Background(), "thread-target")
		done <- outcome{prompts, err}
	}()
	request := fake.next(t)
	if got := method(t, request); got != "thread/read" {
		t.Fatalf("history made mutating or unexpected RPC %q", got)
	}
	fields := params(t, request)
	if len(fields) != 2 || string(fields["threadId"]) != `"thread-target"` || string(fields["includeTurns"]) != "true" {
		t.Fatalf("thread/read parameters = %v", fields)
	}
	fake.respond(t, request, json.RawMessage(`{"thread":{"id":"thread-target","turns":[
		{"id":"turn-first","items":[
			{"type":"developerMessage","content":[{"type":"text","text":"private developer content"}]},
			{"type":"userMessage","id":"user-first","content":[{"type":"text","text":"  Check "},{"type":"text","text":"the build.\n"}]},
			{"type":"agentMessage","id":"assistant","text":"private assistant content"},
			{"type":"reasoning","id":"reasoning","content":["private reasoning"]},
			{"type":"commandExecution","id":"command","aggregatedOutput":"private tool content"}
		]},
		{"id":"turn-second","items":[
			{"type":"userMessage","id":"user-second","content":[{"type":"text","text":"And the tests."}]}
		]}
	]}}`))
	select {
	case result := <-done:
		want := []UserPrompt{{TurnID: "turn-first", ItemID: "user-first", Text: "  Check the build.\n"}, {TurnID: "turn-second", ItemID: "user-second", Text: "And the tests."}}
		if result.err != nil || !reflect.DeepEqual(result.prompts, want) {
			t.Fatalf("UserPrompts = %#v, %v; want %#v", result.prompts, result.err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("history did not finish after thread/read; an additional RPC may have been attempted")
	}
	// Closing the client unblocks the fake's input. No subscribe, resume,
	// start, or steer message may have followed the one history read.
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if fake.scan.Scan() {
		t.Fatalf("unexpected additional RPC: %s", fake.scan.Text())
	}
}

func TestUserPromptsRejectsDifferentThread(t *testing.T) {
	for _, returnedID := range []string{"thread-other", ""} {
		t.Run("returned="+returnedID, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() {
				prompts, err := client.UserPrompts(context.Background(), "thread-target")
				if len(prompts) != 0 {
					done <- errors.New("returned other thread's prompts")
					return
				}
				done <- err
			}()
			request := fake.next(t)
			fake.respond(t, request, map[string]any{"thread": map[string]any{"id": returnedID, "turns": []any{}}})
			if err := <-done; err == nil || !strings.Contains(err.Error(), "different history thread") {
				t.Fatalf("wrong thread error = %v", err)
			}
		})
	}
}

func TestUserPromptsReadFailureDoesNotResume(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { _, err := client.UserPrompts(context.Background(), "thread-target"); done <- err }()
	request := fake.next(t)
	fake.write(t, map[string]any{"id": request["id"], "error": map[string]any{"code": -32601, "message": "unsupported"}})
	if err := <-done; !errors.Is(err, ErrMethodUnavailable) {
		t.Fatalf("history error = %v, want ErrMethodUnavailable", err)
	}
	_ = client.Close()
	if fake.scan.Scan() {
		t.Fatalf("unexpected fallback RPC: %s", fake.scan.Text())
	}
}

func TestUserPromptAttachmentsDoNotExposeLocationsOrMetadata(t *testing.T) {
	got, err := decodeUserPrompts(json.RawMessage(`{"turns":[{"id":"turn","items":[
		{"type":"userMessage","id":"mixed","content":[
			{"type":"text","text":"Look: ","text_elements":[{"path":"/private/metadata-secret"}]},
			{"type":"localImage","path":"/private/image-secret.png"},
			{"type":"image","url":"https://example.invalid/image-secret?token=secret"},
			{"type":"skill","name":"secret-skill","path":"/private/skill-secret"},
			{"type":"mention","name":"secret-name","path":"app://secret-app"},
			{"type":"futureAttachment","text":"secret-text","path":"/private/future-secret"}
		]},
		{"type":"userMessage","id":"image-only","content":[{"type":"localImage","path":"/private/secret.png"}]},
		{"type":"userMessage","id":"empty","content":[]},
		{"type":"userMessage","id":"whitespace","content":[{"type":"text","text":" \n\t"}]},
		{"type":"userMessage","id":"unknown-only","content":[{"type":"futureAttachment","text":"secret-text"}]},
		{"type":"futureItem","id":"ignored","content":"secret-text"}
	]}]}`))
	want := []UserPrompt{{TurnID: "turn", ItemID: "mixed", Text: "Look: [Image][Image][Attachment][Attachment]"}, {TurnID: "turn", ItemID: "image-only", Text: "[Image]"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %#v, %v; want %#v", got, err, want)
	}
}

func TestUserPromptsHistoryAvailabilityAndValidation(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		unavailable bool
		valid       bool
	}{
		{name: "empty history", raw: `{"turns":[]}`, valid: true},
		{name: "empty turn", raw: `{"turns":[{"id":"turn","items":[]}]}`, valid: true},
		{name: "missing turns", raw: `{}`, unavailable: true},
		{name: "null turns", raw: `{"turns":null}`, unavailable: true},
		{name: "missing items", raw: `{"turns":[{"id":"turn"}]}`, unavailable: true},
		{name: "null items", raw: `{"turns":[{"id":"turn","items":null}]}`, unavailable: true},
		{name: "malformed JSON", raw: `{"turns":[`},
		{name: "object turns", raw: `{"turns":{}}`},
		{name: "null turn", raw: `{"turns":[null]}`},
		{name: "object items", raw: `{"turns":[{"id":"turn","items":{}}]}`},
		{name: "string item", raw: `{"turns":[{"id":"turn","items":["userMessage"]}]}`},
		{name: "missing turn id", raw: `{"turns":[{"items":[{"id":"item","type":"userMessage","content":[{"type":"text","text":"hi"}]}]}]}`},
		{name: "missing item id", raw: `{"turns":[{"id":"turn","items":[{"type":"userMessage","content":[{"type":"text","text":"hi"}]}]}]}`},
		{name: "missing content", raw: `{"turns":[{"id":"turn","items":[{"id":"item","type":"userMessage"}]}]}`, unavailable: true},
		{name: "object content", raw: `{"turns":[{"id":"turn","items":[{"id":"item","type":"userMessage","content":{}}]}]}`},
		{name: "null content part", raw: `{"turns":[{"id":"turn","items":[{"id":"item","type":"userMessage","content":[null]}]}]}`},
		{name: "null text", raw: `{"turns":[{"id":"turn","items":[{"id":"item","type":"userMessage","content":[{"type":"text","text":null}]}]}]}`},
		{name: "object text", raw: `{"turns":[{"id":"turn","items":[{"id":"item","type":"userMessage","content":[{"type":"text","text":{}}]}]}]}`},
		{name: "missing text", raw: `{"turns":[{"id":"turn","items":[{"id":"item","type":"userMessage","content":[{"type":"text"}]}]}]}`},
		{name: "duplicate prompt id", raw: `{"turns":[{"id":"turn","items":[{"id":"item","type":"userMessage","content":[{"type":"text","text":"one"}]},{"id":"item","type":"userMessage","content":[{"type":"text","text":"two"}]}]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompts, err := decodeUserPrompts(json.RawMessage(tt.raw))
			if tt.valid {
				if err != nil || len(prompts) != 0 {
					t.Fatalf("valid empty history = %#v, %v", prompts, err)
				}
				return
			}
			if err == nil || prompts != nil {
				t.Fatalf("invalid history = %#v, %v", prompts, err)
			}
			if errors.Is(err, ErrHistoryUnavailable) != tt.unavailable {
				t.Fatalf("history unavailable = %v, want %v: %v", errors.Is(err, ErrHistoryUnavailable), tt.unavailable, err)
			}
		})
	}
}
