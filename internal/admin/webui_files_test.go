package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUIFilesRequireAuthenticationExactOriginAndCSRF(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"session_id":"` + uuid.NewString() + `","path":"/work/report.html","download":false}`
	for _, tc := range []struct {
		name, method, body, token, origin, header, cookie string
		status                                            int
	}{
		{"no authentication", "POST", body, "", passkeyTestOrigin, "csrf", "csrf", 401},
		{"invalid authentication", "POST", body, "bad", passkeyTestOrigin, "csrf", "csrf", 401},
		{"no origin", "POST", body, token, "", "csrf", "csrf", 403},
		{"wrong origin", "POST", body, token, "https://evil.example", "csrf", "csrf", 403},
		{"different port", "POST", body, token, passkeyTestOrigin + ":8443", "csrf", "csrf", 403},
		{"missing CSRF header", "POST", body, token, passkeyTestOrigin, "", "csrf", 403},
		{"missing CSRF cookie", "POST", body, token, passkeyTestOrigin, "csrf", "", 403},
		{"different CSRF", "POST", body, token, passkeyTestOrigin, "wrong", "csrf", 403},
		{"unknown field", "POST", body[:len(body)-1] + `,"worker_id":"` + uuid.NewString() + `"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"invalid session", "POST", `{"session_id":"bad","path":"/work/report.html"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"missing path", "POST", `{"session_id":"` + uuid.NewString() + `"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"wrong method", "GET", "", token, "", "", "", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := webUICommandRequestForTest(server, tc.method, "/tgw/api/v1/webui/files", tc.body, tc.token, tc.origin, tc.header, tc.cookie)
			if response.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("file response lost browser security headers")
			}
		})
	}
}

// This exercises the actual authenticated TLS HTTP -> worker WebSocket path.
// The fake worker intentionally supplies HTML and bytes matched by the normal
// conversation redactor, so downloads cannot silently corrupt arbitrary files.
func TestWebUIFilesMetadataAndExactAttachmentAcrossWorkerTransport(t *testing.T) {
	f := newWebUIFileFixture(t, true)
	path, name := "/work/report.html", `report "quoted".html`
	payload := append([]byte("<!doctype html><script>secret-sentinel</script>\n"), 0, 0xff, 0x80)
	metadata := f.request(path, false)
	first := f.open(path)
	f.ready(first.ID, name, path, int64(len(payload)))
	response := f.response(metadata)
	defer response.Body.Close()
	var info struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&info) != nil || info.Name != name || info.Path != path || info.Size != int64(len(payload)) {
		t.Fatalf("metadata response=%d info=%+v", response.StatusCode, info)
	}
	// Metadata inspection must close the file without requesting even one byte.
	f.expectClose(first.ID)
	download := f.request(path, true)
	second := f.open(path)
	if second.ID == first.ID {
		t.Fatal("download reused the metadata transfer identity")
	}
	f.ready(second.ID, name, path, int64(len(payload)))
	f.credit(second.ID, 0)
	f.output(second.ID, 0, payload, true)
	response = f.response(download)
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("attachment status=%d bytes=%x error=%v", response.StatusCode, got, err)
	}
	kind, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil || kind != "attachment" || parameters["filename"] != name {
		t.Fatalf("unsafe attachment disposition: %q (%v)", response.Header.Get("Content-Disposition"), err)
	}
	if response.Header.Get("Content-Type") != "application/octet-stream" || response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Cache-Control") != "no-store" || response.ContentLength != int64(len(payload)) {
		t.Fatalf("unsafe attachment headers: %+v", response.Header)
	}
	f.expectClose(second.ID)
	commands, err := f.store.PendingCommands(f.ctx, 10)
	if err != nil || len(commands) != 0 {
		t.Fatalf("file reads entered the durable command queue: %+v %v", commands, err)
	}
}

func TestWebUIFilesRejectUnavailableTargetsAndOldWorkers(t *testing.T) {
	for _, scenario := range []string{"missing", "archived", "deleted", "disabled", "old worker"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWebUIFileFixture(t, scenario != "old worker")
			switch scenario {
			case "missing":
				f.session.ID = uuid.NewString()
			case "archived":
				f.session.Archived = true
				f.ingestSession()
			case "deleted":
				requestID := uuid.New()
				if _, err := f.store.QueueWebUISessionDeletion(f.ctx, uuid.MustParse(f.session.ID), requestID); err != nil {
					t.Fatal(err)
				}
				f.session.Archived, f.session.Deleted, f.session.State = true, true, "not_loaded"
				raw, _ := json.Marshal(protocol.Result{CommandID: requestID.String(), State: "completed", Session: &f.session})
				f.sequence++
				if err := f.store.IngestEvent(f.ctx, f.workerID, f.connectionID, protocol.Event{ID: uuid.NewString(), Seq: f.sequence, WorkerID: f.workerID.String(), RuntimeID: f.runtimeID.String(), RuntimeGeneration: 1, SessionID: f.session.ID, Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: raw}); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := f.store.RevokeWorker(f.ctx, f.workerID); err != nil {
					t.Fatal(err)
				}
			}
			response := f.response(f.request("/work/report.txt", false))
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			wantStatus := http.StatusNotFound
			if scenario == "old worker" {
				wantStatus = http.StatusServiceUnavailable
			}
			if err != nil || response.StatusCode != wantStatus {
				t.Fatalf("unavailable target response=%d body=%s error=%v", response.StatusCode, body, err)
			}
			if scenario == "old worker" && !strings.Contains(strings.ToLower(string(body)), "update") {
				t.Fatalf("old worker lacked actionable update advice: %s", body)
			}
		})
	}
}

func TestWebUIFilesRejectUnsafeMetadataBeforeRequestingBytes(t *testing.T) {
	for _, scenario := range []string{"header injection", "directory name", "relative path", "negative size", "oversized file"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWebUIFileFixture(t, true)
			pending := f.request("/work/report.bin", true)
			opened := f.open("/work/report.bin")
			name, path, size := "report.bin", "/work/report.bin", int64(1)
			switch scenario {
			case "header injection":
				name = "report.bin\r\nX-Injected: yes"
			case "directory name":
				name = "../report.bin"
			case "relative path":
				path = "report.bin"
			case "negative size":
				size = -1
			case "oversized file":
				size = (128 << 20) + 1
			}
			f.ready(opened.ID, name, path, size)
			response := f.response(pending)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadGateway || response.Header.Get("X-Injected") != "" || response.Header.Get("Content-Disposition") != "" {
				t.Fatalf("unsafe worker metadata accepted: status=%d headers=%+v", response.StatusCode, response.Header)
			}
			f.expectClose(opened.ID)
		})
	}
}

func TestWebUIFilesPreserveEmptyFilesAndRedactWorkerErrors(t *testing.T) {
	for _, scenario := range []string{"empty file", "worker error"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWebUIFileFixture(t, true)
			pending := f.request("/work/empty.bin", true)
			opened := f.open("/work/empty.bin")
			if scenario == "worker error" {
				f.send("webui", protocol.WebUIFrame{ID: opened.ID, Action: "error", Error: "File secret-sentinel is unavailable."})
			} else {
				f.ready(opened.ID, "empty.bin", "/work/empty.bin", 0)
				f.credit(opened.ID, 0)
				f.output(opened.ID, 0, nil, true)
			}
			response := f.response(pending)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "worker error" {
				if response.StatusCode != http.StatusConflict || strings.Contains(string(body), "secret-sentinel") || !strings.Contains(string(body), "[REDACTED]") {
					t.Fatalf("unsafe worker error: status=%d body=%s", response.StatusCode, body)
				}
			} else if response.StatusCode != http.StatusOK || response.ContentLength != 0 || len(body) != 0 || !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;") {
				t.Fatalf("empty download failed: status=%d headers=%+v bytes=%d", response.StatusCode, response.Header, len(body))
			}
			f.expectClose(opened.ID)
		})
	}
}

func TestWebUIFilesCannotCompleteAfterRevocationOrTargetChange(t *testing.T) {
	for _, scenario := range []string{"browser revoked", "worker revoked", "session archived", "runtime changed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWebUIFileFixture(t, true)
			chunk := bytes.Repeat([]byte("a"), 64<<10)
			pending := f.request("/work/report.bin", true)
			opened := f.open("/work/report.bin")
			f.ready(opened.ID, "report.bin", "/work/report.bin", int64(2*len(chunk)))
			f.credit(opened.ID, 0)
			f.output(opened.ID, 0, chunk, false)
			// Wait for the second credit so the first chunk has passed its checks.
			// Change authorization before returning the next credited chunk.
			f.credit(opened.ID, int64(len(chunk)))
			switch scenario {
			case "browser revoked":
				if err := f.store.RevokeAdminSession(f.ctx, f.adminToken); err != nil {
					t.Fatal(err)
				}
			case "worker revoked":
				if err := f.store.RevokeWorker(f.ctx, f.workerID); err != nil {
					t.Fatal(err)
				}
			case "session archived":
				f.session.Archived = true
				f.ingestSession()
			case "runtime changed":
				if err := f.store.RecordHeartbeat(f.ctx, registry.Heartbeat{WorkerID: f.workerID, ConnectionID: f.connectionID, Runtimes: []registry.Runtime{{ID: f.runtimeID, WorkerID: f.workerID, ProfileID: "main", Name: "Main", Generation: 2, State: "running"}}}); err != nil {
					t.Fatal(err)
				}
			}
			f.output(opened.ID, int64(len(chunk)), chunk, true)
			f.requireIncomplete(pending, int64(2*len(chunk)))
			f.expectClose(opened.ID)
		})
	}
}

func TestWebUIFilesRejectMalformedOffsetsAndTruncatedBodies(t *testing.T) {
	for _, scenario := range []string{"wrong offset", "early EOF", "oversized chunk"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWebUIFileFixture(t, true)
			pending := f.request("/work/report.bin", true)
			opened := f.open("/work/report.bin")
			f.ready(opened.ID, "report.bin", "/work/report.bin", 128<<10)
			f.credit(opened.ID, 0)
			switch scenario {
			case "wrong offset":
				f.output(opened.ID, 1, []byte("wrong"), false)
			case "early EOF":
				f.output(opened.ID, 0, []byte("short"), true)
			case "oversized chunk":
				f.output(opened.ID, 0, bytes.Repeat([]byte("a"), (64<<10)+1), false)
			}
			f.requireIncomplete(pending, 128<<10)
			f.expectClose(opened.ID)
		})
	}
}

type webUIFileHTTPResult struct {
	response *http.Response
	err      error
}

type webUIFileFixture struct {
	t                                 *testing.T
	ctx                               context.Context
	store                             *registry.Store
	client                            *http.Client
	worker                            *websocket.Conn
	adminToken                        string
	workerID, runtimeID, connectionID uuid.UUID
	session                           protocol.Session
	sequence                          uint64
}

func newWebUIFileFixture(t *testing.T, supported bool) *webUIFileFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	store := adminIntegrationStore(t)
	f := &webUIFileFixture{t: t, ctx: ctx, store: store, adminToken: webuiTestLogin(t, store), workerID: uuid.New(), runtimeID: uuid.New()}
	workerToken, err := auth.GenerateWorkerToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: f.workerID, Name: "File test worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken(workerToken)}); err != nil {
		t.Fatal(err)
	}
	hub := gateway.NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, 30*time.Second)
	hub.EventHandler = store.IngestEvent
	redactor, err := auth.NewRedactor([]string{"secret-sentinel"}, "[REDACTED]")
	if err != nil {
		t.Fatal(err)
	}
	console, err := New(store, Config{Origin: passkeyTestOrigin, WebUI: hub, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	mux := gateway.NewMux(store, hub)
	mux.Handle("/tgw/api/v1/webui/", console)
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	f.client = passkeyHTTPClient(t, server)
	f.worker, _, err = websocket.Dial(ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/workers/connect", &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + workerToken}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.worker.CloseNow() })
	f.send("hello", map[string]any{"worker_id": f.workerID.String(), "worker_name": "File test worker", "os": "linux", "arch": "amd64", "protocol_min": 1, "protocol_max": 1, "supports_webui": true, "supports_webui_files": supported,
		"runtimes": []protocol.Runtime{{ID: f.runtimeID.String(), WorkerID: f.workerID.String(), ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}})
	ack, err := protocol.Payload[protocol.HelloAck](f.read("hello_ack"))
	if err != nil {
		t.Fatal(err)
	}
	f.connectionID = uuid.MustParse(ack.ConnectionID)
	f.session = protocol.Session{ID: uuid.NewString(), WorkerID: f.workerID.String(), RuntimeID: f.runtimeID.String(), ThreadID: "file-thread", Name: "File session", CWD: "/work", State: "idle", UpdatedAt: time.Now().UTC()}
	f.ingestSession()
	return f
}

func (f *webUIFileFixture) ingestSession() {
	f.t.Helper()
	f.sequence++
	raw, err := json.Marshal(f.session)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.IngestEvent(f.ctx, f.workerID, f.connectionID, protocol.Event{ID: uuid.NewString(), Seq: f.sequence, WorkerID: f.workerID.String(), RuntimeID: f.runtimeID.String(), RuntimeGeneration: 1, SessionID: f.session.ID, Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: raw}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *webUIFileFixture) send(kind string, payload any) {
	f.t.Helper()
	e, err := protocol.NewEnvelope(kind, payload)
	if err != nil {
		f.t.Fatal(err)
	}
	body, err := json.Marshal(e)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.worker.Write(f.ctx, websocket.MessageText, body); err != nil {
		f.t.Fatal(err)
	}
}

func (f *webUIFileFixture) read(kind string) protocol.Envelope {
	f.t.Helper()
	_, body, err := f.worker.Read(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := protocol.Decode(body)
	if err != nil || e.Type != kind {
		f.t.Fatalf("worker frame type=%q want=%q error=%v", e.Type, kind, err)
	}
	return e
}

func (f *webUIFileFixture) frame() protocol.WebUIFrame {
	f.t.Helper()
	frame, err := protocol.Payload[protocol.WebUIFrame](f.read("webui"))
	if err != nil {
		f.t.Fatal(err)
	}
	return frame
}

func (f *webUIFileFixture) open(path string) protocol.WebUIFrame {
	f.t.Helper()
	frame := f.frame()
	var data struct {
		Path string `json:"path"`
	}
	if frame.Action != "file_open" || frame.RuntimeID != f.runtimeID.String() || frame.RuntimeGeneration != 1 || frame.SessionID != f.session.ID || json.Unmarshal(frame.Data, &data) != nil || data.Path != path {
		f.t.Fatalf("file target or path changed: %+v", frame)
	}
	return frame
}

func (f *webUIFileFixture) ready(id, name, path string, size int64) {
	f.t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": name, "path": path, "size": size})
	f.send("webui", protocol.WebUIFrame{ID: id, Action: "ready", Data: raw})
}

func (f *webUIFileFixture) credit(id string, offset int64) {
	f.t.Helper()
	frame := f.frame()
	var credit struct {
		Offset int64 `json:"offset"`
	}
	if frame.Action != "input" || frame.ID != id || json.Unmarshal(frame.Data, &credit) != nil || credit.Offset != offset {
		f.t.Fatalf("unexpected file credit: %+v want offset=%d", frame, offset)
	}
}

func (f *webUIFileFixture) output(id string, offset int64, body []byte, eof bool) {
	f.t.Helper()
	raw, _ := json.Marshal(map[string]any{"offset": offset, "data": base64.StdEncoding.EncodeToString(body), "eof": eof})
	f.send("webui", protocol.WebUIFrame{ID: id, Action: "output", Data: raw})
}

func (f *webUIFileFixture) expectClose(id string) {
	f.t.Helper()
	frame := f.frame()
	if frame.Action != "close" || frame.ID != id {
		f.t.Fatalf("transfer did not close cleanly: %+v", frame)
	}
}

func (f *webUIFileFixture) request(path string, download bool) <-chan webUIFileHTTPResult {
	f.t.Helper()
	body, err := json.Marshal(map[string]any{"session_id": f.session.ID, "path": path, "download": download})
	if err != nil {
		f.t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(f.ctx, http.MethodPost, passkeyTestOrigin+"/tgw/api/v1/webui/files", bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", passkeyTestOrigin)
	request.Header.Set("X-CSRF-Token", "csrf")
	request.AddCookie(cookie(adminCookie, f.adminToken, true))
	request.AddCookie(cookie(csrfCookie, "csrf", false))
	done := make(chan webUIFileHTTPResult, 1)
	go func() {
		response, err := f.client.Do(request)
		done <- webUIFileHTTPResult{response: response, err: err}
	}()
	return done
}

func (f *webUIFileFixture) result(pending <-chan webUIFileHTTPResult) webUIFileHTTPResult {
	f.t.Helper()
	select {
	case result := <-pending:
		return result
	case <-f.ctx.Done():
		f.t.Fatal(f.ctx.Err())
		return webUIFileHTTPResult{}
	}
}

func (f *webUIFileFixture) response(pending <-chan webUIFileHTTPResult) *http.Response {
	f.t.Helper()
	result := f.result(pending)
	if result.err != nil {
		f.t.Fatal(result.err)
	}
	return result.response
}

func (f *webUIFileFixture) requireIncomplete(pending <-chan webUIFileHTTPResult, declaredSize int64) {
	f.t.Helper()
	result := f.result(pending)
	if result.err != nil {
		return // Aborting before HTTP headers is also a failed download.
	}
	defer result.response.Body.Close()
	body, err := io.ReadAll(result.response.Body)
	if result.response.StatusCode < 300 && (err == nil || int64(len(body)) >= declaredSize) {
		f.t.Fatalf("invalid/unauthorized transfer completed: status=%d bytes=%d error=%v", result.response.StatusCode, len(body), err)
	}
}
