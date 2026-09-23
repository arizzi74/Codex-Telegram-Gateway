package codexadapter

import (
	"encoding/json"
	"testing"
)

func TestTranscriptTurnPageBoundsColdReadAndPreservesNativeItems(t *testing.T) {
	for _, full := range []bool{false, true} {
		client, fake := newFake(t)
		initialize(t, client, fake)
		type result struct {
			page TranscriptTurnPage
			err  error
		}
		done := make(chan result, 1)
		go func() {
			page, err := client.ReadTranscriptTurnPage(t.Context(), "thread", "anchor", "desc", full)
			done <- result{page, err}
		}()
		request := fake.next(t)
		fields := params(t, request)
		view := "notLoaded"
		items := `[]`
		if full {
			view = "full"
			items = `[{"id":"reason","type":"reasoning","summary":["Public summary"]},{"id":"tool","type":"commandExecution","aggregatedOutput":"output"}]`
		}
		if method(t, request) != "thread/turns/list" || string(fields["limit"]) != "1" || string(fields["cursor"]) != `"anchor"` || string(fields["itemsView"]) != `"`+view+`"` {
			t.Fatalf("unbounded source read: %#v", fields)
		}
		fake.respond(t, request, json.RawMessage(`{"data":[{"id":"turn","status":"completed","startedAt":1700000000,"completedAt":1700000100,"itemsView":"`+view+`","items":`+items+`}],"nextCursor":"next"}`))
		got := <-done
		if got.err != nil || len(got.page.Data) != 1 || got.page.NextCursor != "next" || got.page.Data[0].StartedAt == nil || *got.page.Data[0].StartedAt != 1700000000 || (full && len(got.page.Data[0].Items) != 2) {
			t.Fatalf("source page lost data: %#v %v", got.page, got.err)
		}
	}
}

func TestTranscriptTurnPageRejectsMalformedOrRepeatedSource(t *testing.T) {
	for _, response := range []string{
		`{}`,
		`{"data":[],"nextCursor":"next"}`,
		`{"data":[{"id":"turn","status":"unknown","items":[]}]}`,
		`{"data":[{"id":"turn","status":"completed","itemsView":"notLoaded","items":[]}]}`,
		`{"data":[{"id":"turn","status":"completed","items":[]},{"id":"second","status":"completed","items":[]}]}`,
		`{"data":[{"id":"turn","status":"completed","items":[]}],"nextCursor":"same"}`,
	} {
		client, fake := newFake(t)
		initialize(t, client, fake)
		done := make(chan error, 1)
		go func() {
			_, err := client.ReadTranscriptTurnPage(t.Context(), "thread", "same", "desc", true)
			done <- err
		}()
		request := fake.next(t)
		fake.respond(t, request, json.RawMessage(response))
		if err := <-done; err == nil {
			t.Fatalf("accepted invalid source: %s", response)
		}
	}
}
