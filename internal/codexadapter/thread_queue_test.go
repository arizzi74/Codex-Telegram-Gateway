package codexadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifyThreadQueueIdleRequiresExplicitEmptyFirstPage(t *testing.T) {
	for _, test := range []struct {
		name, result, want string
	}{
		{"empty", `{"data":[],"nextCursor":null}`, ""},
		{"empty string cursor", `{"data":[],"nextCursor":""}`, ""},
		{"queued", `{"data":[{"id":"queued","input":[{"text":"private prompt"}]}],"nextCursor":null}`, "inputs are still queued"},
		{"more pages", `{"data":[],"nextCursor":"more"}`, "inputs are still queued"},
		{"missing data", `{"nextCursor":null}`, "invalid state page"},
		{"null data", `{"data":null,"nextCursor":null}`, "invalid state page"},
		{"wrong data", `{"data":{},"nextCursor":null}`, "invalid state page"},
		{"omitted optional cursor", `{"data":[]}`, ""},
		{"wrong cursor", `{"data":[],"nextCursor":1}`, "invalid state page"},
		{"duplicate data", `{"data":[{"id":"queued"}],"data":[],"nextCursor":null}`, "invalid state page"},
		{"duplicate cursor", `{"data":[],"nextCursor":"more","nextCursor":null}`, "invalid state page"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() { done <- client.VerifyThreadQueueIdle(context.Background(), "thread") }()
			request := fake.next(t)
			if method(t, request) != "thread/queue/list" {
				t.Fatalf("unexpected method: %s", method(t, request))
			}
			p := params(t, request)
			if string(p["threadId"]) != `"thread"` || string(p["limit"]) != "1" || len(p) != 2 {
				t.Fatalf("expected bounded first page, got %s", request["params"])
			}
			fake.respond(t, request, json.RawMessage(test.result))
			err := <-done
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "private prompt") {
				t.Fatalf("error = %v; want %s without prompt text", err, test.want)
			}
		})
	}
}

func TestVerifyThreadQueueIdleFailsClosedOnUnsupportedRuntime(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { done <- client.VerifyThreadQueueIdle(context.Background(), "thread") }()
	request := fake.next(t)
	fake.write(t, map[string]any{"id": json.RawMessage(request["id"]), "error": map[string]any{"code": -32601, "message": "unsupported method: private prompt"}})
	if err := <-done; err == nil || err.Error() != "worker update: native queued inputs could not be verified" {
		t.Fatalf("unsupported queue error = %v", err)
	}
}
