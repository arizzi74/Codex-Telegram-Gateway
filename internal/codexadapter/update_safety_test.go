package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func awaitUpdateSafety(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("update safety did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func abandonUpdateRPC(t *testing.T, client *Client, fake *fakeServer, method string) int64 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.request(ctx, method, map[string]any{"private": "SECRET_PAYLOAD"}, nil, false) }()
	request := fake.next(t)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned call = %v", err)
	}
	var id int64
	if err := json.Unmarshal(request["id"], &id); err != nil {
		t.Fatal(err)
	}
	awaitUpdateSafety(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.updateWriting[id] == nil
	})
	return id
}

func TestUpdateSafetyRetainsMutationsUnknownsAndFirstResponse(t *testing.T) {
	for _, method := range []string{"turn/start", "thread/compact/start", "thread/name/set", "future/read", "SECRET_METHOD\n/private"} {
		t.Run(method, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			id := abandonUpdateRPC(t, client, fake, method)
			status := client.UpdateSafety()
			if status.UnconfirmedRequests != 1 || status.PendingRequests != 0 || client.UpdateQuiescent() {
				t.Fatalf("missing mutation blocker: %+v", status)
			}
			client.handleLine([]byte(fmt.Sprintf(`{"id":%d,"result":{"private":"SECRET_RESPONSE"}}`, id)))
			// A contradictory duplicate error cannot erase an accepted success.
			client.handleLine([]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"no method"}}`, id)))
			if client.UpdateSafety().UnconfirmedRequests != 1 {
				t.Fatal("late success/duplicate erased mutation uncertainty")
			}
			raw, _ := json.Marshal(client.UpdateSafety())
			if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "private") || strings.Contains(string(raw), "future/read") {
				t.Fatalf("private diagnostic: %s", raw)
			}
			client.mu.Lock()
			retained := client.updateUnconfirmed[id]
			hasPayload := retained.reply != nil
			client.mu.Unlock()
			if hasPayload {
				t.Fatal("abandoned record retained response payload")
			}
		})
	}
}

func TestUpdateSafetyOnlyDefinitiveLateRejectionsRecover(t *testing.T) {
	cases := []struct {
		name, response string
		recover        bool
	}{
		{"invalid_request", `{"id":%d,"error":{"code":-32600,"message":"rejected"}}`, true},
		{"unknown_method", `{"id":%d,"error":{"code":-32601,"message":"rejected"}}`, true},
		{"invalid_params", `{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"rejected"}}`, true},
		{"server_error", `{"id":%d,"error":{"code":-32000,"message":"uncertain"}}`, false},
		{"internal_error", `{"id":%d,"error":{"code":-32603,"message":"uncertain"}}`, false},
		{"missing_code", `{"id":%d,"error":{"message":"rejected"}}`, false},
		{"missing_message", `{"id":%d,"error":{"code":-32601}}`, false},
		{"null_code", `{"id":%d,"error":{"code":null,"message":"rejected"}}`, false},
		{"null_message", `{"id":%d,"error":{"code":-32601,"message":null}}`, false},
		{"null_error", `{"id":%d,"error":null}`, false},
		{"ambiguous", `{"id":%d,"result":null,"error":{"code":-32601,"message":"rejected"}}`, false},
		{"duplicate_code", `{"id":%d,"error":{"code":0,"code":-32601,"message":"rejected"}}`, false},
		{"duplicate_id", `{"id":999,"id":%d,"error":{"code":-32601,"message":"rejected"}}`, false},
		{"string_id", `{"id":"%d","error":{"code":-32601,"message":"rejected"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			id := abandonUpdateRPC(t, client, fake, "turn/start")
			client.handleLine([]byte(fmt.Sprintf(tc.response, id)))
			if client.UpdateQuiescent() != tc.recover {
				t.Fatalf("recovery=%v safety=%+v", tc.recover, client.UpdateSafety())
			}
		})
	}
}

type updateGateWriter struct {
	io.WriteCloser
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *updateGateWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.WriteCloser.Write(data)
}

func TestUpdateSafetyWaitsForAbandonedReadWriteAndCancelsUnsentMutation(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	gate := &updateGateWriter{WriteCloser: client.t.In, entered: make(chan struct{}), release: make(chan struct{})}
	client.t.In = gate
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	readDone := make(chan error, 1)
	go func() { readDone <- client.request(readCtx, "thread/read", nil, nil, false) }()
	<-gate.entered
	mutationCtx, cancelMutation := context.WithCancel(context.Background())
	defer cancelMutation()
	mutationDone := make(chan error, 1)
	go func() { mutationDone <- client.request(mutationCtx, "turn/start", nil, nil, false) }()
	awaitUpdateSafety(t, func() bool { return client.UpdateSafety().PendingRequests == 2 })
	cancelMutation()
	if err := <-mutationDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cancelRead()
	if err := <-readDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	status := client.UpdateSafety()
	if status.PendingRequests != 1 || status.UnconfirmedRequests != 0 || len(status.Blockers) != 1 || status.Blockers[0].Phase != "writing" || client.UpdateQuiescent() {
		t.Fatalf("unfinished write not fenced: %+v", status)
	}
	close(gate.release)
	request := fake.next(t)
	awaitUpdateSafety(t, client.UpdateQuiescent)
	fake.respond(t, request, nil)
}

func TestUpdateSafetyBoundsUnsafeRetentionButNotReadRecovery(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		t.Run(fmt.Sprint(readOnly), func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			method := "turn/start"
			if readOnly {
				method = "thread/read"
			}
			for i := 0; i < maxUpdateUnconfirmed+5; i++ {
				abandonUpdateRPC(t, client, fake, method)
			}
			status := client.UpdateSafety()
			if len(status.Blockers) > MaxUpdateSafetyBlockers {
				t.Fatal("diagnostics are unbounded")
			}
			client.mu.Lock()
			unsafeCount, readCount := len(client.updateUnconfirmed), len(client.updateReadTombstones)
			client.mu.Unlock()
			if unsafeCount > maxUpdateUnconfirmed || readCount > maxUpdateReadTombstones {
				t.Fatal("retained tracking is unbounded")
			}
			if readOnly {
				if status.Overflow || !client.UpdateQuiescent() {
					t.Fatalf("read tombstones poisoned update: %+v", status)
				}
			} else {
				if !status.Overflow || status.UnconfirmedRequests != maxUpdateUnconfirmed {
					t.Fatalf("unsafe overflow not fenced: %+v", status)
				}
				client.mu.Lock()
				ids := make([]int64, 0, len(client.updateUnconfirmed))
				for id := range client.updateUnconfirmed {
					ids = append(ids, id)
				}
				client.mu.Unlock()
				for _, id := range ids {
					client.handleLine([]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"rejected"}}`, id)))
				}
				if client.UpdateQuiescent() || !client.UpdateSafety().Overflow {
					t.Fatal("lost unsafe identities were forgotten after retained ones settled")
				}
			}
		})
	}
}

func TestUpdateSafetyResponseCancellationRace(t *testing.T) {
	for _, method := range []string{"thread/read", "turn/start"} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reject=%v", method, reject), func(t *testing.T) {
				for i := 0; i < 100; i++ {
					client := &Client{ready: true, pending: make(map[int64]*pendingRPC), updateUnconfirmed: make(map[int64]*pendingRPC), updateWriting: make(map[int64]*pendingRPC), updateReadTombstones: make(map[int64]struct{})}
					request := &pendingRPC{id: 1, method: method, readOnly: updateReadOnlyMethod(method), writeDone: true, startedAt: time.Now(), done: make(chan struct{})}
					client.pending[1] = request
					var group sync.WaitGroup
					group.Add(2)
					go func() { defer group.Done(); client.acceptRPCResponse(1, rpcResponse{}, reject) }()
					go func() { defer group.Done(); client.finishRPCCall(request, true, false) }()
					group.Wait()
					if client.UpdateQuiescent() != (request.readOnly || reject) {
						t.Fatalf("response/cancel race: %+v", client.UpdateSafety())
					}
				}
			})
		}
	}
}

func TestUpdateSafetyCloseAndDuplicateResponseCannotDoubleClose(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { done <- client.request(context.Background(), "thread/read", nil, nil, false) }()
	request := fake.next(t)
	var id int64
	_ = json.Unmarshal(request["id"], &id)
	client.fail(ErrClosed)
	client.acceptRPCResponse(id, rpcResponse{}, false)
	client.acceptRPCResponse(id, rpcResponse{}, false)
	if err := <-done; err == nil {
		t.Fatal("closed request succeeded")
	}
	if !client.UpdateSafety().Closed || client.UpdateQuiescent() {
		t.Fatal("closed client appeared safe")
	}
}

func TestUpdateSafetyMalformedLiveResponseAndDecodeFailureRetainMutation(t *testing.T) {
	for _, malformed := range []bool{true, false} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var result struct {
					Value int `json:"value"`
				}
				done <- client.request(ctx, "turn/start", nil, &result, false)
			}()
			request := fake.next(t)
			if malformed {
				client.handleLine([]byte(fmt.Sprintf(`{"id":%s,"error":{"code":null,"message":"bad"}}`, request["id"])))
				if client.UpdateSafety().PendingRequests != 1 {
					t.Fatal("malformed error settled a live mutation")
				}
				cancel()
			} else {
				fake.respond(t, request, map[string]any{"value": "bad type"})
			}
			if err := <-done; err == nil {
				t.Fatal("invalid response succeeded")
			}
			if client.UpdateSafety().UnconfirmedRequests != 1 {
				t.Fatal("invalid response erased mutation uncertainty")
			}
		})
	}
}

func TestUpdateSafetyBoundsLiveRequestsAndPreservesApprovals(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := make(chan error, maxPendingRPCs)
	for i := 0; i < maxPendingRPCs; i++ {
		go func() { done <- client.request(ctx, "thread/read", nil, nil, false) }()
		_ = fake.next(t)
	}
	if err := client.request(ctx, "turn/start", nil, nil, false); err == nil || !strings.Contains(err.Error(), "request limit") {
		t.Fatalf("new mutation was not rejected before admission at capacity: %v", err)
	}
	status := client.UpdateSafety()
	if status.PendingRequests != maxPendingRPCs || status.Overflow || len(status.Blockers) != MaxUpdateSafetyBlockers {
		t.Fatalf("live request bound = %+v", status)
	}
	cancel()
	for i := 0; i < maxPendingRPCs; i++ {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	awaitUpdateSafety(t, client.UpdateQuiescent)
	fake.write(t, map[string]any{"id": "private-approval", "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "private-thread"}})
	<-client.Requests()
	status = client.UpdateSafety()
	if status.PendingApprovals != 1 || client.UpdateQuiescent() {
		t.Fatalf("approval guard was weakened: %+v", status)
	}
}
