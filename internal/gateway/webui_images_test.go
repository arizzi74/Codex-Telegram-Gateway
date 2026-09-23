package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

type webUIImageRegistry struct{ WorkerRegistry }

func (webUIImageRegistry) CheckConnection(context.Context, uuid.UUID, uuid.UUID) error { return nil }

func TestWebUIHubCarriesLargeInlineImageAndReleasesInputBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hub := NewHub(webUIImageRegistry{}, nil, time.Second, time.Second)
	p := &peer{workerID: uuid.New(), connectionID: uuid.New(), ctx: ctx, writes: make(chan writeRequest)}
	hub.peers[p.workerID.String()] = p
	s := &WebUIStream{hub: hub, peer: p, id: uuid.NewString(), ctx: ctx, cancel: cancel}
	// The gateway preserves opaque input; image decoding belongs to the
	// selected worker. This payload exceeds the previous 1 MiB Web UI cap.
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 2<<20)))
	raw, _ := json.Marshal(map[string]any{"id": 1, "method": "turn/start", "params": map[string]any{"input": []map[string]string{{"type": "image", "url": url}}}})
	for _, fail := range []bool{false, true} {
		done := make(chan error, 1)
		go func() { done <- s.Send(ctx, raw) }()
		var request writeRequest
		select {
		case request = <-p.writes:
		case <-time.After(5 * time.Second):
			t.Fatal("large image was not admitted")
		}
		frame, err := protocol.Payload[protocol.WebUIFrame](request.envelope)
		if err != nil || frame.Action != "input" || !bytes.Equal(frame.Data, raw) {
			t.Fatal("gateway changed inline image content")
		}
		hub.mu.RLock()
		held := hub.webuiInputBytes
		hub.mu.RUnlock()
		if held != len(raw) {
			t.Fatal("in-flight image lost byte reservation")
		}
		var sendErr error
		if fail {
			sendErr = errors.New("write failed")
		}
		request.finish(sendErr)
		if err := <-done; (err != nil) != fail {
			t.Fatalf("send result %v", err)
		}
		hub.mu.RLock()
		remaining, local := hub.webuiInputBytes, s.inputBytes
		hub.mu.RUnlock()
		if remaining != 0 || local != 0 {
			t.Fatal("completed/failed image send leaked input budget")
		}
	}
	hub.mu.Lock()
	hub.webuiInputBytes = 64 << 20
	hub.mu.Unlock()
	if err := s.Send(ctx, raw); err == nil {
		t.Fatal("global input byte ceiling ignored")
	}
	if ctx.Err() != nil {
		t.Fatal("image admission failure stopped durable worker")
	}
}

func TestWebUIQueuedImageRetainsBudgetAfterCallerCancels(t *testing.T) {
	workerCtx, stopWorker := context.WithCancel(t.Context())
	defer stopWorker()
	hub := NewHub(webUIImageRegistry{}, nil, time.Second, time.Second)
	p := &peer{workerID: uuid.New(), connectionID: uuid.New(), ctx: workerCtx, cancel: stopWorker, writes: make(chan writeRequest, 128)}
	hub.peers[p.workerID.String()] = p
	streamCtx, stopStream := context.WithCancel(t.Context())
	defer stopStream()
	s := &WebUIStream{hub: hub, peer: p, id: uuid.NewString(), ctx: streamCtx, cancel: stopStream}
	raw := json.RawMessage(`{"id":1,"method":"turn/start","params":{"input":[{"type":"image","url":"data:image/png;base64,` + strings.Repeat("A", 2<<20) + `"}]}}`)
	for range 2 {
		caller, cancelCaller := context.WithCancel(t.Context())
		done := make(chan error, 1)
		queued := len(p.writes)
		go func() { done <- s.Send(caller, raw) }()
		deadline := time.Now().Add(5 * time.Second)
		for len(p.writes) == queued {
			if time.Now().After(deadline) {
				t.Fatal("image was not queued")
			}
			time.Sleep(time.Millisecond)
		}
		cancelCaller()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation: %v", err)
		}
		hub.mu.RLock()
		held := hub.webuiInputBytes
		hub.mu.RUnlock()
		if held != (queued+1)*len(raw) {
			t.Fatal("caller cancellation released a still-queued image")
		}
	}
	// The writer must complete/drop every admitted request before returning.
	stopWorker()
	writerDone := make(chan struct{})
	go func() { p.writer(); close(writerDone) }()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer shutdown did not drain queued images")
	}
	hub.mu.RLock()
	remaining, local := hub.webuiInputBytes, s.inputBytes
	hub.mu.RUnlock()
	if remaining != 0 || local != 0 || len(p.writes) != 0 {
		t.Fatal("writer shutdown leaked queued image reservation")
	}
}

func TestPeerCompletionOwnsInFlightBytesAndFencesShutdownAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &peer{ctx: ctx, cancel: cancel, writes: make(chan writeRequest, 128)}
	payload := protocol.WebUIFrame{ID: uuid.NewString(), Action: "input", Data: json.RawMessage(`{"id":1}`)}
	var released atomic.Int32
	caller, cancelCaller := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.sendWithCompletion(caller, "webui", payload, func() { released.Add(1) }) }()
	request := <-p.writes // Writer now owns the in-flight envelope.
	cancelCaller()
	if err := <-done; !errors.Is(err, context.Canceled) || released.Load() != 0 {
		t.Fatal("in-flight write released on caller timeout")
	}
	request.finish(nil)
	request.finish(errors.New("duplicate completion"))
	if released.Load() != 1 {
		t.Fatal("completion hook was not once-only")
	}

	const producers = 200
	var waiting sync.WaitGroup
	waiting.Add(producers)
	for range producers {
		go func() {
			defer waiting.Done()
			_ = p.sendWithCompletion(t.Context(), "webui", payload, func() { released.Add(1) })
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(p.writes) < cap(p.writes) {
		if time.Now().After(deadline) {
			t.Fatal("concurrent writers did not fill queue")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	writerDone := make(chan struct{})
	go func() { p.writer(); close(writerDone) }()
	waiting.Wait()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer shutdown stalled")
	}
	if released.Load() != producers+1 || len(p.writes) != 0 {
		t.Fatalf("shutdown raced admission: completed=%d queue=%d", released.Load(), len(p.writes))
	}
	if err := p.sendWithCompletion(t.Context(), "webui", payload, func() { released.Add(1) }); err == nil || released.Load() != producers+2 || len(p.writes) != 0 {
		t.Fatal("late enqueue survived final drain")
	}
}
