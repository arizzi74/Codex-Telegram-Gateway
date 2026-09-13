package codexadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeServer struct {
	in     *io.PipeReader
	out    *io.PipeWriter
	scan   *bufio.Scanner
	closed sync.Once
}

func newFake(t *testing.T) (*Client, *fakeServer) {
	t.Helper()
	serverIn, clientIn := io.Pipe()
	clientOut, serverOut := io.Pipe()
	fake := &fakeServer{in: serverIn, out: serverOut, scan: bufio.NewScanner(serverIn)}
	client := New(Transport{
		In:  clientIn,
		Out: clientOut,
		Close: func() error {
			fake.closed.Do(func() {
				_ = clientIn.Close()
				_ = clientOut.Close()
				_ = serverIn.Close()
				_ = serverOut.Close()
			})
			return nil
		},
	}, Config{RequestTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })
	return client, fake
}

func (f *fakeServer) next(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	if !f.scan.Scan() {
		t.Fatalf("expected client JSONL message, scan error: %v", f.scan.Err())
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(f.scan.Bytes(), &message); err != nil {
		t.Fatalf("decode client message: %v", err)
	}
	return message
}

func (f *fakeServer) write(t *testing.T, message any) {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal fake message: %v", err)
	}
	data = append(data, '\n')
	if _, err := f.out.Write(data); err != nil {
		t.Fatalf("write fake message: %v", err)
	}
}

func (f *fakeServer) respond(t *testing.T, request map[string]json.RawMessage, result any) {
	t.Helper()
	f.write(t, struct {
		ID     json.RawMessage `json:"id"`
		Result any             `json:"result"`
	}{ID: request["id"], Result: result})
}

func method(t *testing.T, message map[string]json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(message["method"], &value); err != nil {
		t.Fatalf("decode method: %v", err)
	}
	return value
}

func params(t *testing.T, message map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(message["params"], &value); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	return value
}

func initialize(t *testing.T, client *Client, fake *fakeServer) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- client.Initialize(context.Background()) }()
	request := fake.next(t)
	if got := method(t, request); got != "initialize" {
		t.Fatalf("initialize method = %q", got)
	}
	fake.respond(t, request, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-cli/0.154.0"})
	if got := method(t, fake.next(t)); got != "initialized" {
		t.Fatalf("notification = %q", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("initialize: %v", err)
	}
}

func TestInitializeGatesOperationsAndCorrelatesConcurrentResponses(t *testing.T) {
	client, fake := newFake(t)
	if _, err := client.ListThreads(context.Background(), "", 1); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("uninitialized ListThreads error = %v, want ErrNotInitialized", err)
	}
	initialize(t, client, fake)

	type outcome struct {
		page ThreadPage
		err  error
	}
	listed := make(chan outcome, 1)
	loaded := make(chan outcome, 1)
	go func() { page, err := client.ListThreads(context.Background(), "", 2); listed <- outcome{page, err} }()
	go func() { page, err := client.LoadedThreads(context.Background(), "", 2); loaded <- outcome{page, err} }()
	first, second := fake.next(t), fake.next(t)
	if method(t, first) == method(t, second) {
		t.Fatal("expected distinct concurrent methods")
	}
	// Deliberately reply out of order to exercise the response ID map.
	for _, request := range []map[string]json.RawMessage{second, first} {
		if method(t, request) == "thread/list" {
			fake.respond(t, request, map[string]any{"data": []map[string]any{{"id": "stored", "sessionId": "stored", "preview": "saved"}}})
		} else {
			fake.respond(t, request, map[string]any{"data": []string{"loaded"}})
		}
	}
	a, b := <-listed, <-loaded
	if a.err != nil || b.err != nil {
		t.Fatalf("concurrent calls: list=%v loaded=%v", a.err, b.err)
	}
	if len(a.page.Threads) != 1 || a.page.Threads[0].ID != "stored" {
		t.Fatalf("stored page = %#v", a.page)
	}
	if len(b.page.Threads) != 1 || b.page.Threads[0].ID != "loaded" {
		t.Fatalf("loaded page = %#v", b.page)
	}
}

func TestNotificationsServerRequestsAndExactReply(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	fake.write(t, map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thr_1", "turnId": "turn_1", "itemId": "item_1", "delta": "hi"}})
	fake.write(t, map[string]any{"method": "future/event", "params": map[string]any{"threadId": "thr_1", "value": 1}})
	first, second := <-client.Events(), <-client.Events()
	if first.ThreadID != "thr_1" || first.TurnID != "turn_1" || first.ItemID != "item_1" {
		t.Fatalf("event ids = %#v", first)
	}
	if first.Kind != "agent_message_delta" || first.Text != "hi" {
		t.Fatalf("normalized event = %#v", first)
	}
	if second.Method != "future/event" || !second.Unknown || len(second.Params) == 0 {
		t.Fatalf("unknown event not preserved: %#v", second)
	}

	fake.write(t, map[string]any{"method": "item/commandExecution/requestApproval", "id": "approval-7", "params": map[string]any{"threadId": "thr_1", "turnId": "turn_1", "itemId": "item_1", "reason": "test"}})
	request := <-client.Requests()
	if request.Method != "item/commandExecution/requestApproval" || string(request.ID) != "\"approval-7\"" {
		t.Fatalf("request = %#v", request)
	}
	done := make(chan error, 1)
	go func() { done <- client.Reply(context.Background(), request, map[string]any{"decision": "approve"}) }()
	response := fake.next(t)
	if string(response["id"]) != "\"approval-7\"" {
		t.Fatalf("reply id = %s", response["id"])
	}
	var result map[string]string
	if err := json.Unmarshal(response["result"], &result); err != nil || result["decision"] != "approve" {
		t.Fatalf("reply result = %s, %v", response["result"], err)
	}
	if err := <-done; err != nil {
		t.Fatalf("reply: %v", err)
	}
}

func TestApprovalProjectionBuildsOneExactReply(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	fake.write(t, map[string]any{"method": "item/commandExecution/requestApproval", "id": 17, "params": map[string]any{"threadId": "thr_1", "turnId": "turn_1", "itemId": "item_1", "command": "go test ./...", "cwd": "/work", "reason": "verification"}})
	request := <-client.Requests()
	if request.Kind != "approval_requested" || request.ApprovalType != "command_execution" || request.RequestID == "" || request.Command != "go test ./..." || !contains(request.Decisions, "acceptForSession") {
		t.Fatalf("approval projection = %#v", request)
	}
	done := make(chan error, 1)
	go func() {
		done <- client.ReplyApproval(context.Background(), request.RequestID, ApprovalResponse{Decision: "accept"})
	}()
	response := fake.next(t)
	if string(response["id"]) != "17" {
		t.Fatalf("reply id = %s", response["id"])
	}
	var result map[string]string
	if err := json.Unmarshal(response["result"], &result); err != nil || result["decision"] != "accept" {
		t.Fatalf("reply = %s, %v", response["result"], err)
	}
	if err := <-done; err != nil {
		t.Fatalf("approval reply: %v", err)
	}
	if err := client.ReplyApproval(context.Background(), request.RequestID, ApprovalResponse{Decision: "accept"}); err == nil || !strings.Contains(err.Error(), "no longer pending") {
		t.Fatalf("duplicate reply error = %v", err)
	}
}

func TestServerRequestResolvedClearsTrackedApproval(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	fake.write(t, map[string]any{"method": "item/fileChange/requestApproval", "id": 18, "params": map[string]any{"threadId": "thr_1", "turnId": "turn_1", "itemId": "item_1"}})
	request := <-client.Requests()
	fake.write(t, map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"threadId": "thr_1", "requestId": 18}})
	event := <-client.Events()
	if event.Kind != "server_request_resolved" || event.RequestID != request.RequestID {
		t.Fatalf("resolved event = %#v, request = %#v", event, request)
	}
	if err := client.ReplyApproval(context.Background(), request.RequestID, ApprovalResponse{Decision: "accept"}); err == nil || !strings.Contains(err.Error(), "no longer pending") {
		t.Fatalf("cleared approval reply error = %v", err)
	}
}

func TestTimeoutDoesNotPoisonLaterRequests(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.ReadThread(ctx, "thr_1", false); done <- err }()
	timedOut := fake.next(t)
	if got := method(t, timedOut); got != "thread/read" {
		t.Fatalf("method = %q", got)
	}
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	// A late answer must be quarantined as an unmatched response, while later
	// work still receives its own response.
	fake.respond(t, timedOut, map[string]any{"thread": map[string]any{"id": "thr_1"}})
	nextDone := make(chan error, 1)
	go func() { _, err := client.ReadThread(context.Background(), "thr_2", false); nextDone <- err }()
	next := fake.next(t)
	fake.respond(t, next, map[string]any{"thread": map[string]any{"id": "thr_2"}})
	if err := <-nextDone; err != nil {
		t.Fatalf("later request: %v", err)
	}
	select {
	case event := <-client.Events():
		if event.Method != "adapter/unmatchedResponse" {
			t.Fatalf("late reply event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing unmatched late response event")
	}
}

func TestTypedTurnMethodsCarryExpectedTurnID(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	startDone := make(chan struct {
		turn Turn
		err  error
	}, 1)
	go func() {
		turn, err := client.StartTurn(context.Background(), "thr_1", "first")
		startDone <- struct {
			turn Turn
			err  error
		}{turn, err}
	}()
	start := fake.next(t)
	if got := method(t, start); got != "turn/start" {
		t.Fatalf("start method = %q", got)
	}
	fake.respond(t, start, map[string]any{"turn": map[string]any{"id": "turn_1", "threadId": "thr_1", "status": "inProgress"}})
	if result := <-startDone; result.err != nil || result.turn.ID != "turn_1" {
		t.Fatalf("start result = %#v", result)
	}

	steerDone := make(chan error, 1)
	go func() { _, err := client.Steer(context.Background(), "thr_1", "turn_1", "more"); steerDone <- err }()
	steer := fake.next(t)
	if got := method(t, steer); got != "turn/steer" {
		t.Fatalf("steer method = %q", got)
	}
	var expected string
	if err := json.Unmarshal(params(t, steer)["expectedTurnId"], &expected); err != nil || expected != "turn_1" {
		t.Fatalf("expectedTurnId = %q, %v", expected, err)
	}
	fake.respond(t, steer, map[string]any{"turnId": "turn_1"})
	if err := <-steerDone; err != nil {
		t.Fatalf("steer: %v", err)
	}
	if _, err := client.Steer(context.Background(), "thr_1", "turn_other", "stale"); !errors.Is(err, ErrStaleTurn) {
		t.Fatalf("stale steer = %v", err)
	}

	interruptDone := make(chan error, 1)
	go func() { interruptDone <- client.Interrupt(context.Background(), "thr_1", "turn_1") }()
	interrupt := fake.next(t)
	if got := method(t, interrupt); got != "turn/interrupt" {
		t.Fatalf("interrupt method = %q", got)
	}
	fake.respond(t, interrupt, map[string]any{})
	if err := <-interruptDone; err != nil {
		t.Fatalf("interrupt: %v", err)
	}
}

func TestMalformedJSONDoesNotStopReader(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	if _, err := io.WriteString(fake.out, "{bad json}\n"); err != nil {
		t.Fatal(err)
	}
	fake.write(t, map[string]any{"method": "thread/status/changed", "params": map[string]any{"threadId": "thr_1"}})
	malformed, valid := <-client.Events(), <-client.Events()
	if malformed.Method != "adapter/malformed" || malformed.Err == nil {
		t.Fatalf("malformed = %#v", malformed)
	}
	if valid.Method != "thread/status/changed" || valid.ThreadID != "thr_1" {
		t.Fatalf("valid event = %#v", valid)
	}
}

func TestMissingRequiredMethodMarksCapabilityUnavailable(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { _, err := client.ListThreads(context.Background(), "", 1); done <- err }()
	request := fake.next(t)
	fake.write(t, struct {
		ID    json.RawMessage `json:"id"`
		Error RPCError        `json:"error"`
	}{ID: request["id"], Error: RPCError{Code: -32601, Message: "method not found"}})
	if err := <-done; !errors.Is(err, ErrMethodUnavailable) {
		t.Fatalf("missing method error = %v", err)
	}
	if client.Supports("thread/list") || client.Capabilities().Methods["thread/list"] {
		t.Fatal("thread/list capability remained available")
	}
}

func TestProcessExitFailsPendingRequest(t *testing.T) {
	client, fake := newFake(t)
	wait := make(chan error, 1)
	// This client owns a separate fake transport with an explicit Wait signal.
	_ = fake
	client.Close()
	serverIn, clientIn := io.Pipe()
	clientOut, serverOut := io.Pipe()
	client = New(Transport{In: clientIn, Out: clientOut, Wait: wait, Close: func() error {
		_ = clientIn.Close()
		_ = clientOut.Close()
		_ = serverIn.Close()
		_ = serverOut.Close()
		return nil
	}}, Config{RequestTimeout: time.Second})
	defer client.Close()
	server := &fakeServer{in: serverIn, out: serverOut, scan: bufio.NewScanner(serverIn)}
	initialize(t, client, server)
	done := make(chan error, 1)
	go func() { _, err := client.ListThreads(context.Background(), "", 1); done <- err }()
	_ = server.next(t)
	wait <- errors.New("simulated crash")
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("pending error = %v", err)
	}
}
