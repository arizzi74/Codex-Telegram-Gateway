package worker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type nativeProxyRequest struct {
	connection *websocket.Conn
	payload    []byte
}

func newNativeProxyTest(t *testing.T) (*attachmentProxy, string, <-chan nativeProxyRequest) {
	t.Helper()
	dir, err := os.MkdirTemp("", "worker-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "server.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	requests := make(chan nativeProxyRequest, 32)
	var group sync.WaitGroup
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group.Add(1)
		defer group.Done()
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		connection.SetReadLimit(nativeMessageLimit)
		for {
			kind, payload, err := connection.Read(ctx)
			if err != nil || kind != websocket.MessageText {
				return
			}
			select {
			case requests <- nativeProxyRequest{connection, payload}:
			case <-ctx.Done():
				return
			}
		}
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { cancel(); _ = server.Close(); <-done; group.Wait() })
	path := filepath.Join(dir, "app.sock")
	proxy, err := newAttachmentProxy(path, filepath.Join(dir, "server.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.close)
	return proxy, path, requests
}

func dialNativeProxy(t *testing.T, path string) *websocket.Conn {
	t.Helper()
	connection, _, err := tryDialNativeProxy(path)
	if err != nil {
		t.Fatal(err)
	}
	connection.SetReadLimit(nativeMessageLimit)
	t.Cleanup(func() { _ = connection.CloseNow() })
	return connection
}

func tryDialNativeProxy(path string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	return websocket.Dial(ctx, "ws://codex.local/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}, CompressionMode: websocket.CompressionContextTakeover})
}

func nativeWrite(t *testing.T, connection *websocket.Conn, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatal(err)
	}
}

func nativeRead(t *testing.T, connection *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func nextNativeRequest(t *testing.T, requests <-chan nativeProxyRequest) nativeProxyRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("native request did not reach backend")
		return nativeProxyRequest{}
	}
}

func TestIdleNativeCLIAllowsUpdateAndFencesNewRequests(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	proxy, path, requests := newNativeProxyTest(t)
	a.manager.mu.Lock()
	a.manager.attachments[runtime.ID] = proxy
	a.manager.runtimes[runtime.ID].runtime.LocalSocket = path
	a.manager.mu.Unlock()
	client := dialNativeProxy(t, path)
	nativeWrite(t, client, `{"id":1,"method":"config/read","params":{}}`)
	request := nextNativeRequest(t, requests)
	if _, err := a.prepareUpdate(context.Background(), time.Minute); err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("pending request allowed update: %v", err)
	}
	nativeWrite(t, request.connection, `{"id":1,"result":{}}`)
	_ = nativeRead(t, client)
	waitFor(t, func() bool { return proxy.checkIdle() == nil })
	lease, err := a.prepareUpdate(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("idle attached CLI prevented update: %v", err)
	}
	nativeWrite(t, client, `{"id":2,"method":"turn/start","params":{"threadId":"thread-1"}}`)
	var rejected struct {
		ID    int `json:"id"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(nativeRead(t, client)), &rejected); err != nil || rejected.ID != 2 || rejected.Error == nil || rejected.Error.Code != -32002 {
		t.Fatalf("request was not explicitly refused during update: %+v %v", rejected, err)
	}
	select {
	case <-requests:
		t.Fatal("request crossed prepared update fence")
	default:
	}
	blocked, response, err := tryDialNativeProxy(path)
	if err == nil {
		_ = blocked.CloseNow()
		t.Fatal("new CLI connected during prepared update")
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new CLI did not receive retryable rejection: %v", response)
	}
	if err := a.abortUpdate(lease.Token); err != nil {
		t.Fatal(err)
	}
	nativeWrite(t, client, `{"id":3,"method":"config/read","params":{}}`)
	request = nextNativeRequest(t, requests)
	nativeWrite(t, request.connection, `{"id":3,"result":{}}`)
	_ = nativeRead(t, client)
	_ = client.CloseNow()
	waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.sessions) == 0 })
	lease, err = a.prepareUpdate(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("past native use still blocked update: %v", err)
	}
	if err := a.abortUpdate(lease.Token); err != nil {
		t.Fatal(err)
	}
}

func TestNativeDisconnectDrainsAcceptedRPC(t *testing.T) {
	proxy, path, requests := newNativeProxyTest(t)
	client := dialNativeProxy(t, path)
	nativeWrite(t, client, `{"id":"pending","method":"config/read","params":{}}`)
	request := nextNativeRequest(t, requests)
	_ = client.CloseNow()
	if err := proxy.pause(); err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("disconnect was mistaken for completed RPC: %v", err)
	}
	nativeWrite(t, request.connection, `{"id":"pending","result":{}}`)
	waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.sessions) == 0 })
	if err := proxy.pause(); err != nil {
		t.Fatalf("completed disconnected request left a permanent blocker: %v", err)
	}
	proxy.resume()
}

func TestNativeApprovalAndLateNotificationPreventUpdate(t *testing.T) {
	proxy, path, requests := newNativeProxyTest(t)
	client := dialNativeProxy(t, path)
	nativeWrite(t, client, `{"id":1,"method":"config/read","params":{}}`)
	request := nextNativeRequest(t, requests)
	nativeWrite(t, request.connection, `{"id":1,"result":{}}`)
	_ = nativeRead(t, client)
	waitFor(t, func() bool { return proxy.checkIdle() == nil })
	if err := proxy.pause(); err != nil {
		t.Fatal(err)
	}
	nativeWrite(t, request.connection, `{"id":"approval","method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1"}}`)
	_ = nativeRead(t, client)
	if err := proxy.checkIdle(); err == nil || !strings.Contains(err.Error(), "approvals or input") {
		t.Fatalf("late server request did not invalidate idle check: %v", err)
	}
	nativeWrite(t, client, `{"id":"approval","result":{"decision":"accept"}}`)
	select {
	case <-requests:
		t.Fatal("approval response crossed update fence")
	case <-time.After(25 * time.Millisecond):
	}
	proxy.resume()
	_ = nextNativeRequest(t, requests)
	if err := proxy.checkIdle(); err == nil {
		t.Fatal("approval response write was mistaken for backend confirmation")
	}
	nativeWrite(t, request.connection, `{"method":"serverRequest/resolved","params":{"requestId":"approval"}}`)
	_ = nativeRead(t, client)
	waitFor(t, func() bool { return proxy.checkIdle() == nil })
	if err := proxy.pause(); err != nil {
		t.Fatalf("resolved approval kept update blocked: %v", err)
	}
	proxy.resume()
}

func TestNativeUnconfirmedRPCPreventsUpdateAfterServerDisconnect(t *testing.T) {
	proxy, path, requests := newNativeProxyTest(t)
	client := dialNativeProxy(t, path)
	nativeWrite(t, client, `{"id":1,"method":"turn/start","params":{"threadId":"thread-pending"}}`)
	request := nextNativeRequest(t, requests)
	_ = request.connection.CloseNow()
	waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.sessions) == 0 })
	if err := proxy.pause(); err == nil || !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("unknown RPC outcome allowed restart: %v", err)
	}
}

func TestNativeProxyLargeFramesAndConcurrentShutdown(t *testing.T) {
	proxy, path, requests := newNativeProxyTest(t)
	client := dialNativeProxy(t, path)
	payload := `{"id":1,"method":"config/read","params":{"large":"` + strings.Repeat("x", 1<<20) + `"}}`
	nativeWrite(t, client, payload)
	request := nextNativeRequest(t, requests)
	if string(request.payload) != payload {
		t.Fatal("proxy changed or truncated the native message")
	}
	response := `{"id":1,"result":{"large":"` + strings.Repeat("y", 1<<20) + `"}}`
	nativeWrite(t, request.connection, response)
	if nativeRead(t, client) != response {
		t.Fatal("proxy changed or truncated the native response")
	}
	_ = dialNativeProxy(t, path)
	done := make(chan struct{})
	go func() {
		var group sync.WaitGroup
		group.Go(proxy.close)
		group.Go(proxy.close)
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy shutdown stranded an idle CLI")
	}
}
