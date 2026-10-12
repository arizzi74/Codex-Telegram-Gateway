package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestVerifyThreadBackgroundTerminalsIdleRequiresExplicitEmptyPage(t *testing.T) {
	for _, tc := range []struct {
		name, result, want string
	}{
		{"empty", `{"data":[]}`, ""},
		{"null cursor", `{"data":[],"nextCursor":null}`, ""},
		{"empty cursor", `{"data":[],"nextCursor":""}`, ""},
		{"terminal", `{"data":[{"processId":"process","command":"PRIVATE_COMMAND","cwd":"PRIVATE_CWD","itemId":"item"}]}`, "still present"},
		{"more pages", `{"data":[],"nextCursor":"more"}`, "still present"},
		{"missing data", `{}`, "invalid state page"},
		{"null data", `{"data":null}`, "invalid state page"},
		{"wrong data type", `{"data":{}}`, "invalid state page"},
		{"wrong cursor type", `{"data":[],"nextCursor":1}`, "invalid state page"},
		{"duplicate data", `{"data":[{"command":"PRIVATE_COMMAND"}],"data":[]}`, "invalid state page"},
		{"duplicate cursor", `{"data":[],"nextCursor":"more","nextCursor":null}`, "invalid state page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() { done <- client.VerifyThreadBackgroundTerminalsIdle(context.Background(), "thread") }()
			request := fake.next(t)
			p := params(t, request)
			if method(t, request) != "thread/backgroundTerminals/list" || string(p["threadId"]) != `"thread"` || string(p["limit"]) != "1" || len(p) != 2 {
				t.Fatalf("terminal verification did not request bounded first page: %s", request["params"])
			}
			fake.respond(t, request, json.RawMessage(tc.result))
			err := <-done
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatalf("terminal verification = %v; want %s without command details", err, tc.want)
			}
		})
	}
}

func TestVerifyThreadBackgroundTerminalsIdleFailsClosedOnRPCError(t *testing.T) {
	for _, code := range []int{-32601, -32000} {
		client, fake := newFake(t)
		initialize(t, client, fake)
		done := make(chan error, 1)
		go func() { done <- client.VerifyThreadBackgroundTerminalsIdle(context.Background(), "thread") }()
		request := fake.next(t)
		fake.write(t, map[string]any{"id": json.RawMessage(request["id"]), "error": map[string]any{"code": code, "message": "PRIVATE_COMMAND"}})
		if err := <-done; err == nil || err.Error() != "worker update: background terminals could not be verified" {
			t.Fatalf("unverifiable terminal error = %v", err)
		}
	}
}

func TestVerifyThreadBackgroundTerminalsIdlePreservesCancellation(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.VerifyThreadBackgroundTerminalsIdle(ctx, "thread"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verification = %v", err)
	}
	if err := client.VerifyThreadBackgroundTerminalsIdle(context.Background(), " "); err == nil {
		t.Fatal("blank thread identity was accepted")
	}
}
