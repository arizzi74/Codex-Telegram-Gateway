package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIRestrictsRPCToSelectedThread(t *testing.T) {
	for _, data := range []string{
		`{"id":1,"method":"config/read","params":{}}`,
		`{"id":1,"method":"command/exec","params":{"command":["sh"]}}`,
		`{"id":1,"method":"thread/start","params":{}}`,
		`{"id":1,"method":"thread/read","params":{"threadId":"another"}}`,
		`{"id":1,"method":"thread/read","params":{"includeTurns":true}}`,
		`{"id":1,"method":"thread/resume","params":{"path":"/tmp/other.jsonl"}}`,
		`{"id":1,"method":"thread/resume","params":{"approvalPolicy":"never"}}`,
		`{"id":1,"method":"turn/start","params":{"input":[{"type":"text","text":"hello"}],"sandboxPolicy":{"type":"dangerFullAccess"}}}`,
		`{"id":1,"method":"turn/start","params":{"input":[{"type":"localImage","path":"/etc/passwd"}]}}`,
		`{"id":1,"method":"turn/start","params":{"input":[{"type":"text","text":"hello","path":"/etc/passwd"}]}}`,
		`{"id":1,"method":"thread/turns/list","params":{"limit":101}}`,
		`{"id":1,"method":"turn/interrupt","params":{}}`,
		`{"id":1,"method":"turn/steer","params":{"input":[{"type":"text","text":"hello"}]}}`,
		`{"id":1,"result":{"decision":"accept"}}`,
		`{"id":{},"method":"model/list"}`,
	} {
		r := testWebUIRelay()
		if _, err := r.clientMessage([]byte(data)); err == nil {
			t.Errorf("unsafe message accepted: %s", data)
		}
	}
	r := testWebUIRelay()
	data, err := r.clientMessage([]byte(`{"id":1,"method":"thread/resume","params":{}}`))
	if err != nil || !strings.Contains(string(data), `"threadId":"thread"`) || !strings.Contains(string(data), `"excludeTurns":true`) {
		t.Fatalf("resume scope/pagination = %s, %v", data, err)
	}
	if _, err := r.clientMessage([]byte(`{"id":1,"method":"model/list"}`)); err == nil {
		t.Fatal("duplicate outstanding request accepted")
	}
	r.resumed = true
	data, err = r.clientMessage([]byte(`{"id":2,"method":"thread/turns/list","params":{}}`))
	if err != nil || !strings.Contains(string(data), `"limit":20`) || !strings.Contains(string(data), `"sortDirection":"desc"`) {
		t.Fatalf("history scope/pagination = %s,%v", data, err)
	}
}

func TestWebUIResumeValidatesLiveWorkingDirectory(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	for _, tc := range []struct {
		name, stored, live string
		allowed            bool
	}{
		{"live directory allowed", allowed, allowed, true},
		{"stored directory trails allowed live directory", outside, allowed, true},
		{"stored directory hides disallowed live directory", allowed, outside, false},
		{"explicit empty live directory", allowed, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testWebUIRelay()
			r.pool = &webUIRelays{c: &Connection{}}
			r.pool.c.cfg.AllowedWorkspaceRoots = []string{allowed}
			if _, err := r.clientMessage([]byte(`{"id":1,"method":"thread/resume","params":{}}`)); err != nil {
				t.Fatal(err)
			}
			response, _ := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"cwd": tc.live, "thread": map[string]any{"id": "thread", "cwd": tc.stored}}})
			_, err := r.serverMessage(response)
			if (err == nil) != tc.allowed || r.resumed != tc.allowed {
				t.Fatalf("live cwd boundary: resumed=%t, error=%v", r.resumed, err)
			}
		})
	}
}

func testWebUIRelay() *webUIRelay {
	return &webUIRelay{session: protocol.Session{ThreadID: "thread"}, pending: make(map[string]string), requests: make(map[string]webUIRequest)}
}

func TestWebUIRateLimitsReadRequiresResumeAndExcludesAccountDetails(t *testing.T) {
	r := testWebUIRelay()
	request := []byte(`{"id":1,"method":"account/rateLimits/read","params":{}}`)
	if _, err := r.clientMessage(request); err == nil {
		t.Fatal("account usage allowed before selected session was validated")
	}
	r.resumed = true
	for _, params := range []string{
		`{"threadId":"thread"}`, `{"accountId":"other"}`, `{"excludeResetCreditDetails":false}`,
		`{"excludeResetCreditDetails":null}`, `{"excludeResetCreditDetails":"true"}`, `{"includeAuth":true}`,
	} {
		if _, err := r.clientMessage([]byte(`{"id":1,"method":"account/rateLimits/read","params":` + params + `}`)); err == nil {
			t.Fatalf("unsafe account option accepted: %s", params)
		}
	}
	for _, method := range []string{"account/read", "account/login/start", "account/logout", "account/rateLimits/reset", "account/usage/read"} {
		if _, err := r.clientMessage([]byte(`{"id":1,"method":"` + method + `","params":{}}`)); err == nil {
			t.Fatalf("unrelated account method allowed: %s", method)
		}
	}
	data, err := r.clientMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	var forwarded webUIRPC
	if err := json.Unmarshal(data, &forwarded); err != nil || len(forwarded.Params) != 1 || string(forwarded.Params["excludeResetCreditDetails"]) != "true" {
		t.Fatalf("rate-limit request has unexpected account options: %s (%v)", data, err)
	}
	data, err = r.serverMessage([]byte(`{"id":1,"params":{"accessToken":"secret"},"result":{
		"accessToken":"secret","rateLimitResetCredits":{"records":["secret"]},"ordinaryUsageAllowed":false,
		"rateLimits":{"limitId":"codex","limitName":"Codex","planType":"pro","credits":{"balance":"secret"},"primary":{"usedPercent":23,"windowDurationMins":300,"resetsAt":1800000000,"secret":"secret"},"secondary":{"usedPercent":84,"windowDurationMins":10080,"resetsAt":1800200000}},
		"rateLimitsByLimitId":{"codex_other":{"limitId":"codex_other","limitName":"Other","planType":null,"primary":{"usedPercent":0,"windowDurationMins":1440,"resetsAt":null},"secondary":null,"individualLimit":{"memberId":"secret"}}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"id":1,"result":{"ordinaryUsageAllowed":false,"rateLimits":{"limitId":"codex","limitName":"Codex","planType":"pro","primary":{"usedPercent":23,"windowDurationMins":300,"resetsAt":1800000000},"secondary":{"usedPercent":84,"windowDurationMins":10080,"resetsAt":1800200000}},"rateLimitsByLimitId":{"codex_other":{"limitId":"codex_other","limitName":"Other","primary":{"usedPercent":0,"windowDurationMins":1440}}}}}`)
	if !webUIEqualJSON(data, want) {
		t.Fatalf("rate-limit projection leaked fields or lost quota data:\ngot  %s\nwant %s", data, want)
	}
	if len(r.pending) != 0 {
		t.Fatal("completed rate-limit request still pending")
	}
}

func TestWebUIRateLimitErrorsDoNotExposeAccountDetails(t *testing.T) {
	for _, response := range []string{
		`{"id":1,"error":{"code":-32000,"message":"secret account data","data":{"accessToken":"secret"}}}`,
		`{"id":1,"result":{"rateLimits":{"primary":{"usedPercent":"invalid"}},"secret":"secret"}}`,
		`{"id":1,"result":null}`,
	} {
		r := testWebUIRelay()
		r.resumed = true
		r.pending["n:1"] = "account/rateLimits/read"
		data, err := r.serverMessage([]byte(response))
		if err != nil || strings.Contains(string(data), "secret") || !strings.Contains(string(data), "Codex usage limits are unavailable.") {
			t.Fatalf("unsafe or disruptive quota error: %s %v", data, err)
		}
	}
	r := testWebUIRelay()
	r.pending["n:1"] = "account/rateLimits/read"
	data, err := r.serverMessage([]byte(`{"id":1,"result":{"rateLimits":{"primary":{"usedPercent":10}}}}`))
	if err != nil || !strings.Contains(string(data), "Codex usage limits are unavailable.") {
		t.Fatalf("response exposed after resume was invalidated: %s %v", data, err)
	}
}

func TestWebUIRateLimitNotificationsRemainScopedAndSparse(t *testing.T) {
	r := testWebUIRelay()
	update := []byte(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":0,"windowDurationMins":300,"resetsAt":null,"secret":"secret"},"secondary":null,"planType":null,"credits":{"balance":"secret"}},"account":{"token":"secret"}}}`)
	if data, err := r.serverMessage(update); err != nil || len(data) != 0 {
		t.Fatalf("account notification exposed before validated resume: %s %v", data, err)
	}
	r.resumed = true
	data, err := r.serverMessage(update)
	want := []byte(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":0,"windowDurationMins":300}}}}`)
	if err != nil || !webUIEqualJSON(data, want) {
		t.Fatalf("sparse rolling quota projection incorrect: %s %v", data, err)
	}
	for _, raw := range []string{
		`{"method":"account/rateLimits/updated","params":{"threadId":"other","rateLimits":{"primary":{"usedPercent":40}}}}`,
		`{"id":1,"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":40}}}}`,
		`{"id":null,"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":40}}}}`,
		`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":"invalid"}}}}`,
		`{"method":"account/updated","params":{"accessToken":"secret"}}`,
		`{"id":2,"method":"account/chatgptAuthTokens/refresh","params":{"accessToken":"secret"}}`,
	} {
		if data, err := r.serverMessage([]byte(raw)); err != nil || len(data) != 0 {
			t.Fatalf("unexpected account event forwarded: %s %v", data, err)
		}
	}
	if len(r.requests) != 0 {
		t.Fatal("rate-limit updates became actionable server requests")
	}
}

func TestWebUIMutationsWaitForValidatedResume(t *testing.T) {
	r := testWebUIRelay()
	for _, raw := range []string{
		`{"id":1,"method":"turn/start","params":{"input":[{"type":"text","text":"hello"}]}}`,
		`{"id":2,"method":"turn/steer","params":{"input":[{"type":"text","text":"hello"}],"expectedTurnId":"turn"}}`,
		`{"id":3,"method":"turn/interrupt","params":{"turnId":"turn"}}`,
		`{"id":4,"method":"thread/turns/list","params":{}}`,
	} {
		if _, err := r.clientMessage([]byte(raw)); err == nil {
			t.Fatalf("request admitted before metadata validation: %s", raw)
		}
	}
	if output, err := r.serverMessage([]byte(`{"method":"item/completed","params":{"threadId":"thread","item":{"type":"agentMessage","text":"private history"}}}`)); err != nil || len(output) != 0 {
		t.Fatalf("content forwarded before resume validated: %s %v", output, err)
	}
	if _, err := r.clientMessage([]byte(`{"id":5,"method":"thread/resume","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.clientMessage([]byte(`{"id":6,"method":"turn/interrupt","params":{"turnId":"turn"}}`)); err == nil {
		t.Fatal("in-flight resume enabled mutation")
	}
	r.resumed = true
	if _, err := r.clientMessage([]byte(`{"id":7,"method":"turn/interrupt","params":{"turnId":"turn"}}`)); err != nil {
		t.Fatal(err)
	}
	delete(r.pending, "n:5") // The first resume completed before this retry.
	if _, err := r.clientMessage([]byte(`{"id":8,"method":"thread/resume","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if r.resumed {
		t.Fatal("second resume did not invalidate prior workspace validation")
	}
	if _, err := r.serverMessage([]byte(`{"id":8,"result":{"thread":{"id":"thread"}}}`)); err == nil {
		t.Fatal("missing actual CWD accepted")
	}
	r.pool = &webUIRelays{c: &Connection{}}
	r.pool.c.cfg.AllowedWorkspaceRoots = []string{t.TempDir()}
	r.pending["n:9"] = "thread/resume"
	outside, _ := json.Marshal(map[string]any{"id": 9, "result": map[string]any{"thread": map[string]any{"id": "thread", "cwd": t.TempDir()}}})
	if _, err := r.serverMessage(outside); err == nil || r.resumed {
		t.Fatal("resume outside current allowed roots enabled a browser mutation")
	}
}

func TestWebUIQuestionsAreScopedAndCannotBeAnsweredTwice(t *testing.T) {
	r := testWebUIRelay()
	r.resumed = true
	data, err := r.serverMessage([]byte(`{"id":12,"method":"item/tool/requestUserInput","params":{"threadId":"other","questions":[{"id":"question","question":"Question?"}]}}`))
	if err != nil || data != nil || len(r.requests) != 0 {
		t.Fatalf("other session leaked: %s %v", data, err)
	}
	request := []byte(`{"id":12,"method":"item/tool/requestUserInput","params":{"threadId":"thread","questions":[{"id":"question","question":"Question?"}]}}`)
	if data, err := r.serverMessage(request); err != nil || len(data) == 0 {
		t.Fatalf("question missing: %s %v", data, err)
	}
	answer := []byte(`{"id":12,"result":{"answers":{"question":{"answers":["hello"]}}}}`)
	if _, err := r.clientMessage(answer); err != nil {
		t.Fatal(err)
	}
	if _, err := r.clientMessage(answer); err == nil {
		t.Fatal("duplicate answer accepted")
	}
	if _, err := r.serverMessage(request); err != nil {
		t.Fatal(err)
	}
	if _, err := r.serverMessage([]byte(`{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":12}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.clientMessage(answer); err == nil {
		t.Fatal("answer after external resolution accepted")
	}
	if data, err := r.serverMessage([]byte(`{"id":13,"method":"account/chatgptAuthTokens/refresh","params":{"threadId":"thread"}}`)); err != nil || data != nil {
		t.Fatalf("authentication callback leaked: %s %v", data, err)
	}
}

func TestWebUIRejectsSubagentResumeResponse(t *testing.T) {
	for _, thread := range []string{
		`{"id":"other"}`,
		`{"id":"thread","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent"}}}}`,
		`{"id":"thread","parentThreadId":"parent"}`,
		`{"id":"thread","threadSource":"subagent"}`,
		`{"id":"thread","ephemeral":true}`,
	} {
		r := testWebUIRelay()
		r.pending["n:1"] = "thread/resume"
		if _, err := r.serverMessage([]byte(`{"id":1,"result":{"thread":` + thread + `}}`)); err == nil {
			t.Errorf("internal/other thread accepted: %s", thread)
		}
	}
}

func TestWebUIOutputRedactsNativeContentFields(t *testing.T) {
	redactor, err := newWorkerRedactor([]string{`private-value`})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"id":"private-value","method":"item/commandExecution/outputDelta","params":{"threadId":"private-value","delta":"private-value","aggregatedOutput":"private-value","command":["print","private-value"],"nextCursor":"private-value","path":"/private/rollout.jsonl","rolloutPath":"/private/file","other":{"unknownContent":"private-value"}}}`)
	data, err := redactWebUIJSON(redactor, raw)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ID     string
		Params map[string]json.RawMessage
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"delta", "aggregatedOutput", "command", "other"} {
		if strings.Contains(string(payload.Params[key]), "private-value") {
			t.Errorf("content leaked: %s", data)
		}
	}
	if payload.ID != "private-value" || string(payload.Params["threadId"]) != `"private-value"` || string(payload.Params["nextCursor"]) != `"private-value"` {
		t.Fatalf("structural IDs changed: %s", data)
	}
	if payload.Params["rolloutPath"] != nil {
		t.Fatalf("rollout path leaked: %s", data)
	}
}

func TestWebUIQuestionRestoresRedactedOptionsAndLimitsPermissions(t *testing.T) {
	redactor, err := newWorkerRedactor([]string{`private-[ab]`})
	if err != nil {
		t.Fatal(err)
	}
	question := webUIRequest{method: "item/tool/requestUserInput", params: map[string]json.RawMessage{"questions": json.RawMessage(`[{"id":"q","options":[{"label":"private-a"},{"label":"private-b"}]}]`)}}
	visible, err := redactWebUIJSON(redactor, question.params["questions"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(visible), "[REDACTED] (option 1)") || !strings.Contains(string(visible), "[REDACTED] (option 2)") {
		t.Fatalf("redacted choices collide: %s", visible)
	}
	answer, err := webUIAnswer(question, json.RawMessage(`{"answers":{"q":{"answers":["[REDACTED] (option 2)"]}}}`), redactor)
	if err != nil || !strings.Contains(string(answer), "private-b") {
		t.Fatalf("selected option was not restored locally: %s %v", answer, err)
	}
	permissions := webUIRequest{method: "item/permissions/requestApproval", params: map[string]json.RawMessage{"permissions": json.RawMessage(`{"fileSystem":{"read":["/tmp/private-a"]}}`)}}
	for _, raw := range []string{`{"permissions":{"fileSystem":{"read":["/"]}},"scope":"turn"}`, `{"permissions":{},"scope":"session"}`} {
		if _, err := webUIAnswer(permissions, json.RawMessage(raw), redactor); err == nil {
			t.Fatalf("overbroad grant accepted: %s", raw)
		}
	}
	answer, err = webUIAnswer(permissions, json.RawMessage(`{"permissions":{"fileSystem":{"read":["/tmp/[REDACTED]"]}},"scope":"turn"}`), redactor)
	if err != nil || !strings.Contains(string(answer), "private-a") {
		t.Fatalf("requested permissions not restored: %s %v", answer, err)
	}
}

func TestWebUIRelayUsesGuardedConnectionAndDetachPreservesTurn(t *testing.T) {
	proxy, socket, requests := newNativeProxyTest(t)
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	t.Cleanup(func() { _ = store.Close() })
	cfg.AllowedWorkspaceRoots = []string{t.TempDir()}
	runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1, State: "running", LocalSocket: socket}
	session, err := store.UpsertSession(protocol.Session{WorkerID: workerID, RuntimeID: runtime.ID, ThreadID: "thread", Name: "Test", CWD: cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	redactor, err := newWorkerRedactor([]string{`private-value`})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.configureRedactor(redactor); err != nil {
		t.Fatal(err)
	}
	c := &Connection{cfg: cfg, store: store, snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }}
	historyEntered := make(chan struct{})
	historyRelease := make(chan struct{})
	c.historyWebUI = func(ctx context.Context, _ protocol.Runtime, _ protocol.Session, _ webUIHistoryRequest) (json.RawMessage, error) {
		close(historyEntered)
		select {
		case <-historyRelease:
			return json.RawMessage(`{"data":[]}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	commandCalls := 0
	c.commandWebUI = func(_ context.Context, gotRuntime protocol.Runtime, gotSession protocol.Session, name, args string) (protocol.Result, error) {
		commandCalls++
		if gotRuntime.ID != runtime.ID || gotRuntime.Generation != runtime.Generation || gotSession.ID != session.ID || name != "status" || args != "" {
			return protocol.Result{}, validationError("Incorrect command target.")
		}
		return protocol.Result{Text: "status private-value", State: "completed"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := make(chan outbound, 32)
	observed := make(chan protocol.WebUIFrame, 32)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case output := <-writes:
				frame, err := protocol.Payload[protocol.WebUIFrame](output.envelope)
				output.done <- err
				if err == nil {
					observed <- frame
				}
			}
		}
	}()
	pool := newWebUIRelays(c, ctx, writes)
	defer pool.close()
	id := uuid.NewString()
	pool.receive(protocol.WebUIFrame{ID: id, Action: "open", RuntimeID: runtime.ID, RuntimeGeneration: 1, SessionID: session.ID})
	initialize := nextNativeRequest(t, requests)
	if !strings.Contains(string(initialize.payload), `"method":"initialize"`) {
		t.Fatalf("handshake = %s", initialize.payload)
	}
	nativeWrite(t, initialize.connection, `{"id":"webui-initialize","result":{}}`)
	if got := nextNativeRequest(t, requests); !strings.Contains(string(got.payload), `"method":"initialized"`) {
		t.Fatalf("handshake notification = %s", got.payload)
	}
	if frame := nextWebUIFrame(t, observed); frame.Action != "ready" {
		t.Fatalf("ready = %#v", frame)
	}
	pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: json.RawMessage(`{"id":99,"method":"thread/resume","params":{}}`)})
	resume := nextNativeRequest(t, requests)
	response, _ := json.Marshal(map[string]any{"id": 99, "result": map[string]any{"thread": map[string]any{"id": "thread", "cwd": session.CWD}}})
	nativeWrite(t, resume.connection, string(response))
	if frame := nextWebUIFrame(t, observed); frame.Action != "output" {
		t.Fatalf("resume = %#v", frame)
	}
	pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: json.RawMessage(`{"id":98,"method":"thread/items/list","params":{}}`)})
	history := nextNativeRequest(t, requests)
	nativeWrite(t, history.connection, `{"id":98,"error":{"code":-32601,"message":"unavailable"}}`)
	select {
	case <-historyEntered:
	case <-time.After(time.Second):
		t.Fatal("history fallback did not start")
	}
	nativeWrite(t, history.connection, `{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"idle"}}}`)
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), `"thread/status/changed"`) {
		t.Fatalf("slow history blocked native events: %s", frame.Data)
	}
	close(historyRelease)
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), `"id":98`) || !strings.Contains(string(frame.Data), `"data":[]`) {
		t.Fatalf("history fallback result missing: %s", frame.Data)
	}
	command := json.RawMessage(`{"id":101,"method":"gateway/command","params":{"name":"status"}}`)
	pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: command})
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), `"text":"status [REDACTED]"`) || strings.Contains(string(frame.Data), "private-value") {
		t.Fatalf("local command result missing or unredacted: %s", frame.Data)
	}
	pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: command})
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), "already been used") || commandCalls != 1 {
		t.Fatalf("command replay was not rejected: %s, calls=%d", frame.Data, commandCalls)
	}
	pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: json.RawMessage(`{"id":100,"method":"account/rateLimits/read","params":{}}`)})
	limits := nextNativeRequest(t, requests)
	if !strings.Contains(string(limits.payload), `"excludeResetCreditDetails":true`) || strings.Contains(string(limits.payload), `"threadId"`) {
		t.Fatalf("native quota request was not restricted: %s", limits.payload)
	}
	nativeWrite(t, limits.connection, `{"id":100,"result":{"rateLimits":{"primary":{"usedPercent":10,"windowDurationMins":300},"credits":{"balance":"secret"}}}}`)
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), `"usedPercent":10`) || strings.Contains(string(frame.Data), "secret") {
		t.Fatalf("native quota response incorrect: %s", frame.Data)
	}
	nativeWrite(t, limits.connection, `{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":11}}}}`)
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), `"usedPercent":11`) {
		t.Fatalf("native quota update missing: %s", frame.Data)
	}
	// Prompts accepted on the Telegram observer's separate native connection
	// arrive as ordinary lifecycle notifications, without a browser request ID.
	nativeWrite(t, resume.connection, `{"method":"item/completed","params":{"threadId":"other","turnId":"other-turn","item":{"id":"hidden-prompt","type":"userMessage","content":[{"type":"text","text":"do not forward"}]}}}`)
	nativeWrite(t, resume.connection, `{"method":"item/completed","params":{"threadId":"thread","turnId":"telegram-turn","item":{"id":"telegram-prompt","type":"userMessage","content":[{"type":"text","text":"hello private-value"}]}}}`)
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), `"id":"telegram-prompt"`) || !strings.Contains(string(frame.Data), "[REDACTED]") || strings.Contains(string(frame.Data), "private-value") {
		t.Fatalf("external user prompt missing, unscoped or unredacted = %#v", frame)
	}
	_, imageURL := webUITestImage(t)
	pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: webUIImagePrompt(t, "turn/start", []map[string]string{{"type": "text", "text": "hello"}, {"type": "image", "url": imageURL}})})
	start := nextNativeRequest(t, requests)
	if !strings.Contains(string(start.payload), `"threadId":"thread"`) || !strings.Contains(string(start.payload), imageURL) {
		t.Fatalf("thread not bound: %s", start.payload)
	}
	nativeWrite(t, start.connection, `{"id":1,"result":{"turn":{"id":"turn","status":"inProgress"}}}`)
	if frame := nextWebUIFrame(t, observed); frame.Action != "output" {
		t.Fatalf("turn response = %#v", frame)
	}
	imageEcho, _ := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": map[string]any{"id": "image-prompt", "type": "userMessage", "content": []map[string]string{{"type": "image", "url": imageURL}, {"type": "text", "text": "private-value"}}}}})
	nativeWrite(t, start.connection, string(imageEcho))
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), "[Image]") || strings.Contains(string(frame.Data), "base64") || strings.Contains(string(frame.Data), "private-value") {
		t.Fatalf("image echo leaked content instead of placeholder: %s", frame.Data)
	}
	nativeWrite(t, start.connection, `{"method":"item/agentMessage/delta","params":{"threadId":"other","turnId":"other-turn","delta":"do not forward"}}`)
	nativeWrite(t, start.connection, `{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","delta":"private-"}}`)
	nativeWrite(t, start.connection, `{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","delta":"value"}}`)
	nativeWrite(t, start.connection, `{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"id":"message","type":"agentMessage","text":"private-value"}}}`)
	if frame := nextWebUIFrame(t, observed); strings.Contains(string(frame.Data), "private-value") || strings.Contains(string(frame.Data), "do not forward") {
		t.Fatalf("unredacted or unscoped event = %#v", frame)
	}
	nativeWrite(t, start.connection, `{"id":42,"method":"item/tool/requestUserInput","params":{"threadId":"thread","turnId":"turn","questions":[{"id":"question","question":"Question?"}]}}`)
	if frame := nextWebUIFrame(t, observed); !strings.Contains(string(frame.Data), "requestUserInput") {
		t.Fatalf("question = %#v", frame)
	}
	pool.receive(protocol.WebUIFrame{ID: id, Action: "close"})
	done := make(chan struct{})
	go func() { pool.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not detach promptly")
	}
	if err := proxy.checkIdle(); err == nil {
		t.Fatal("pending turn/question no longer blocks updates after browser detach")
	}
	select {
	case request := <-requests:
		t.Fatalf("detach sent a backend mutation: %s", request.payload)
	default:
	}
	nativeWrite(t, start.connection, `{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":42}}`)
	nativeWrite(t, start.connection, `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`)
	waitFor(t, func() bool { return proxy.checkIdle() == nil })
}

func nextWebUIFrame(t *testing.T, frames <-chan protocol.WebUIFrame) protocol.WebUIFrame {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(3 * time.Second):
		t.Fatal("web UI output missing")
		return protocol.WebUIFrame{}
	}
}

func TestWebUIRejectsStaleAndDisallowedTargets(t *testing.T) {
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	defer store.Close()
	cfg.AllowedWorkspaceRoots = []string{t.TempDir()}
	runtime := protocol.Runtime{ID: uuid.NewString(), Generation: 2, State: "running", LocalSocket: "/tmp/app.sock"}
	session, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "thread", CWD: cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	c := &Connection{cfg: cfg, store: store, snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }}
	p := newWebUIRelays(c, context.Background(), nil)
	frame := protocol.WebUIFrame{RuntimeID: runtime.ID, RuntimeGeneration: 1, SessionID: session.ID}
	if _, _, err := p.target(frame); err == nil {
		t.Fatal("stale runtime accepted")
	}
	frame.RuntimeGeneration = 2
	if _, _, err := p.target(frame); err != nil {
		t.Fatal(err)
	}
	session.CWD = t.TempDir()
	if _, err := store.UpsertSession(session); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.target(frame); err == nil {
		t.Fatal("outside workspace accepted")
	}
	session.CWD = cfg.AllowedWorkspaceRoots[0]
	session.Archived = true
	if _, err := store.UpsertSession(session); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.target(frame); err == nil {
		t.Fatal("archived session accepted")
	}
}
