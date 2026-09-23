package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUIHubFencesWorkerAndConnectionAndBoundsSlowBrowser(t *testing.T) {
	hub := NewHub(nil, nil, time.Second, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := &peer{workerID: uuid.New(), connectionID: uuid.New(), ctx: ctx}
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	s := &WebUIStream{hub: hub, peer: owner, id: uuid.NewString(), ctx: streamCtx, cancel: stop, frames: make(chan protocol.WebUIFrame, 1)}
	hub.webuis[s.id] = s
	hub.peers[owner.workerID.String()] = owner
	frame := protocol.WebUIFrame{ID: s.id, Action: "output", Data: json.RawMessage(`{"method":"turn/started","params":{"threadId":"test"}}`)}
	e, _ := protocol.NewEnvelope("webui", frame)
	for _, attacker := range []*peer{
		{workerID: uuid.New(), connectionID: uuid.New(), ctx: ctx},
		{workerID: owner.workerID, connectionID: uuid.New(), ctx: ctx},
	} {
		if err := hub.handleWebUI(attacker, e); err == nil {
			t.Fatal("foreign connection injected output")
		}
	}
	if len(s.frames) != 0 {
		t.Fatal("foreign output reached browser")
	}
	if err := hub.handleWebUI(owner, e); err != nil {
		t.Fatal(err)
	}
	// Buffer exhaustion ends only the slow viewer; the worker remains healthy.
	if err := hub.handleWebUI(owner, e); err != nil {
		t.Fatal(err)
	}
	if s.ctx.Err() == nil || owner.ctx.Err() != nil || s.Err() == nil {
		t.Fatal("slow viewer was not isolated")
	}
	delete(hub.webuis, s.id)
	if err := hub.handleWebUI(owner, e); err != nil {
		t.Fatal("late detached output rejected worker:", err)
	}
}

func TestWebUIHubCapabilitiesAndLimits(t *testing.T) {
	hub := NewHub(nil, nil, time.Second, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := uuid.NewString()
	session := registry.AdminSession{Session: protocol.Session{WorkerID: id}}
	if _, err := hub.OpenWebUI(ctx, session); !errors.Is(err, ErrWebUIUnavailable) {
		t.Fatalf("offline: %v", err)
	}
	p := &peer{workerID: uuid.MustParse(id), ctx: ctx}
	hub.peers[id] = p
	if _, err := hub.OpenWebUI(ctx, session); !errors.Is(err, ErrWebUIUnsupported) {
		t.Fatalf("old worker: %v", err)
	}
	p.supportsWebUI = true
	for range 16 {
		hub.webuis[uuid.NewString()] = &WebUIStream{peer: p}
	}
	if _, err := hub.OpenWebUI(ctx, session); !errors.Is(err, ErrWebUILimit) {
		t.Fatalf("connection cap: %v", err)
	}
}

func TestWebUIHubBoundsQueuedBytesAndReleasesConsumedFrames(t *testing.T) {
	hub := NewHub(nil, nil, time.Second, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &peer{ctx: ctx}
	s := &WebUIStream{hub: hub, peer: p, id: uuid.NewString(), ctx: ctx, cancel: cancel, frames: make(chan protocol.WebUIFrame, 64)}
	hub.webuis[s.id] = s
	data := json.RawMessage(`{"result":"` + strings.Repeat("x", 11<<20) + `"}`)
	frame := protocol.WebUIFrame{ID: s.id, Action: "output", Data: data}
	e, err := protocol.NewEnvelope("webui", frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.handleWebUI(p, e); err != nil {
		t.Fatal(err)
	}
	s.Consumed(<-s.Frames())
	if hub.webuiQueuedBytes != 0 || s.queuedBytes != 0 {
		t.Fatal("consumed output retained byte budget")
	}
	if err := hub.handleWebUI(p, e); err != nil {
		t.Fatal(err)
	}
	if err := hub.handleWebUI(p, e); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil || len(s.frames) != 1 {
		t.Fatal("byte cap did not disconnect before frame-count cap")
	}
	inFlight := <-s.Frames()
	s.Close()
	if hub.webuiQueuedBytes == 0 {
		t.Fatal("closed stream released an in-flight browser write too early")
	}
	s.Consumed(inFlight)
	if hub.webuiQueuedBytes != 0 {
		t.Fatal("failed connection leaked byte accounting")
	}
}

func TestGatewayLandingUnderTGWOnly(t *testing.T) {
	mux := NewMux(nil, NewHub(nil, nil, time.Second, time.Second))
	for _, test := range []struct {
		path, location string
		code           int
	}{
		{"/tgw/", "/tgw/webui/", http.StatusTemporaryRedirect},
		{"/", "", http.StatusNotFound},
		{"/tgw/unknown", "", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.code || response.Header().Get("Location") != test.location {
			t.Fatalf("%s: %d %s", test.path, response.Code, response.Header().Get("Location"))
		}
	}
}
