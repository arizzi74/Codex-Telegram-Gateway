package worker

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/iaia/telegramgw/internal/codexadapter"
)

// Exercise the actual protected-socket client and its 32 MiB transport ceiling,
// rather than injecting a synthetic oversized result after JSON decoding.
func TestWebUIHistoryOversizedColdTurnCannotCloseObserver(t *testing.T) {
	a, runtime, observerServer, cleanup := testAgent(t)
	defer cleanup()
	observer := mustClient(t, a, runtime)
	socket := filepath.Join(attachmentTestDir(t), "history.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	methods := make(chan string, 4)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ws, err := websocket.Accept(w, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		defer close(closed)
		for {
			_, data, err := ws.Read(t.Context())
			if err != nil {
				return
			}
			var rpc webUIRPC
			if json.Unmarshal(data, &rpc) != nil {
				return
			}
			methods <- rpc.Method
			switch rpc.Method {
			case "initialize":
				reply, _ := json.Marshal(webUIRPC{ID: rpc.ID, Result: json.RawMessage(`{}`)})
				if err := ws.Write(t.Context(), websocket.MessageText, reply); err != nil {
					return
				}
			case "initialized":
			case "thread/turns/list":
				if string(rpc.Params["limit"]) != "1" || string(rpc.Params["itemsView"]) != `"full"` {
					return
				}
				writer, err := ws.Writer(t.Context(), websocket.MessageText)
				if err != nil {
					return
				}
				// Deliberately exceed the native frame cap, before the worker
				// can decode, redact, or cap any single display item.
				_, _ = io.WriteString(writer, `{"id":`+string(rpc.ID)+`,"result":{"data":[{"id":"turn","status":"completed","items":[{"id":"giant","type":"agentMessage","text":"`)
				_, _ = io.Copy(writer, strings.NewReader(strings.Repeat("x", 33<<20)))
				_, _ = io.WriteString(writer, `"}]}]}}`)
				_ = writer.Close()
				return
			default:
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	runtime.LocalSocket = socket
	a.manager.install(runtime, observer)
	session := installSession(a, runtime, "oversized-thread", "turn")
	observerServer.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "source": "cli", "turns": []map[string]any{webUITestTurn("turn", "completed", 1)}}}, nil)
	if err := observerServer.Request("item/tool/requestUserInput", 101, map[string]any{"threadId": session.ThreadID, "turnId": "turn", "questions": []map[string]any{{"id": "q", "question": "Still waiting?"}}}); err != nil {
		t.Fatal(err)
	}
	var pending codexadapter.Request
	select {
	case pending = <-observer.Requests():
	case <-time.After(time.Second):
		t.Fatal("missing observer request")
	}
	if _, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 20, Direction: "desc"}); err == nil {
		t.Fatal("oversized source turn accepted")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("temporary history connection was not closed")
	}
	select {
	case <-observer.Done():
		t.Fatalf("cold history killed durable observer: %v", observer.Err())
	default:
	}
	if !observer.RequestPending(pending.RequestID) {
		t.Fatal("cold history lost the observer's pending question")
	}
	if fullWebUIHistoryReads(observerServer) != 0 {
		t.Fatal("full turn was requested on durable observer")
	}
	if thread, err := observer.ReadThread(t.Context(), session.ThreadID, false); err != nil || thread.ID != session.ThreadID {
		t.Fatalf("observer no longer answers metadata: %#v %v", thread, err)
	}
	if err := observer.ReplyAnswers(t.Context(), pending.RequestID, map[string][]string{"q": {"Yes"}}); err != nil {
		t.Fatalf("observer question cannot be answered: %v", err)
	}
	current, state, ok := a.manager.Client(runtime.ID)
	if !ok || current != observer || state.State != "running" || state.Generation != runtime.Generation {
		t.Fatal("source failure changed runtime ownership or generation")
	}
	close(methods)
	for method := range methods {
		if method != "initialize" && method != "initialized" && method != "thread/turns/list" {
			t.Fatalf("read-only source invoked %s", method)
		}
	}
}
