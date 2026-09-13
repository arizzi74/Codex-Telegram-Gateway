package codextest

import (
	"context"
	"testing"
	"time"
)

func TestFixtureEmitsMalformedJSONAndDelaysResponses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, fixture, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	fixture.SetThreads([]map[string]any{{"id": "thread-a", "cwd": t.TempDir(), "status": "idle"}}, nil)
	fixture.SetResponseDelay("thread/list", 25*time.Millisecond)
	started := time.Now()
	if _, err = client.ListThreads(ctx, "", 10); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("delayed fixture response returned in %s", elapsed)
	}
	if err := fixture.EmitRaw("{not-json"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-client.Events():
		if event.Method != "adapter/malformed" || event.Err == nil {
			t.Fatalf("malformed event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed fixture record was not surfaced")
	}
	if _, err = client.ListThreads(ctx, "", 10); err != nil {
		t.Fatalf("fixture did not recover after malformed record: %v", err)
	}
}
