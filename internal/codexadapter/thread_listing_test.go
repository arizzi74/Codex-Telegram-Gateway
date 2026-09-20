package codexadapter

import (
	"context"
	"testing"
)

func TestThreadListingUsesStateDatabaseForInventoryAndLatest(t *testing.T) {
	for _, latest := range []bool{false, true} {
		client, fake := newFake(t)
		initialize(t, client, fake)
		done := make(chan error, 1)
		go func() {
			var err error
			if latest {
				_, _, err = client.LatestThread(context.Background(), "/workspace/project")
			} else {
				_, err = client.ListThreads(context.Background(), "next", 100)
			}
			done <- err
		}()
		request := fake.next(t)
		fields := params(t, request)
		if method(t, request) != "thread/list" || string(fields["useStateDbOnly"]) != "true" || string(fields["sortKey"]) != `"updated_at"` || string(fields["sortDirection"]) != `"desc"` {
			t.Fatalf("history-scanning discovery parameters: %v", fields)
		}
		if latest && string(fields["cwd"]) != `"/workspace/project"` {
			t.Fatalf("latest listing lost exact workspace: %v", fields)
		}
		fake.respond(t, request, map[string]any{"data": []any{}})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestThreadListingCachesOnlyExplicitUnsupportedDatabaseFlag(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	for attempt := 0; attempt < 2; attempt++ {
		done := make(chan error, 1)
		go func() { _, err := client.ListThreads(context.Background(), "cursor", 20); done <- err }()
		request := fake.next(t)
		if attempt == 0 {
			if string(params(t, request)["useStateDbOnly"]) != "true" {
				t.Fatal("first request did not try database-only mode")
			}
			fake.write(t, map[string]any{"id": request["id"], "error": RPCError{Code: -32602, Message: "unknown field `useStateDbOnly`"}})
			request = fake.next(t)
		}
		fields := params(t, request)
		if _, present := fields["useStateDbOnly"]; present || string(fields["cursor"]) != `"cursor"` || string(fields["limit"]) != "20" || len(fields["sourceKinds"]) == 0 {
			t.Fatalf("invalid compatibility request: %v", fields)
		}
		fake.respond(t, request, map[string]any{"data": []any{}})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, rpc := range []*RPCError{
		{Code: -32602, Message: "invalid sortKey"},
		{Code: -32602, Message: "useStateDbOnly must be boolean"},
		{Code: -32000, Message: "unsupported useStateDbOnly"},
		{Code: -32601, Message: "unknown useStateDbOnly"},
	} {
		if unsupportedStateDBListField(rpc) {
			t.Fatalf("unrelated error permits expensive fallback: %v", rpc)
		}
	}
}
