package codexadapter

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLastAgentResponseReadsBoundedItemsWithoutResuming(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() {
		text, err := client.LastAgentResponse(context.Background(), "thread")
		if err == nil && text != "saved answer" {
			t.Errorf("last answer=%q", text)
		}
		done <- err
	}()
	for i, reply := range []string{
		`{"data":[{"turnId":"latest","item":{"type":"userMessage","content":[{"type":"image","url":"data:image/png;base64,secret"}]}}],"nextCursor":"older"}`,
		`{"data":[{"turnId":"previous","item":{"type":"agentMessage","text":"saved answer"}}]}`,
	} {
		request := fake.next(t)
		p := params(t, request)
		if method(t, request) != "thread/items/list" || string(p["limit"]) != "1" || string(p["threadId"]) != `"thread"` || string(p["sortDirection"]) != `"desc"` {
			t.Fatalf("unsafe history request: %v", p)
		}
		if i == 1 && string(p["cursor"]) != `"older"` {
			t.Fatal("missing pagination cursor")
		}
		fake.respond(t, request, json.RawMessage(reply))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
