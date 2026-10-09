package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWebUIGoalReadRequiresResumeAndPinsSelectedThread(t *testing.T) {
	r := testWebUIRelay()
	request := []byte(`{"id":1,"method":"thread/goal/get","params":{}}`)
	if _, err := r.clientMessage(request); err == nil {
		t.Fatal("goal read admitted before selected session was validated")
	}
	r.resumed = true
	for _, params := range []string{
		`{"threadId":"other"}`, `{"threadId":null}`, `{"threadId":1}`,
		`{"thread_id":"thread"}`, `{"objective":"replace goal"}`, `{"status":"active"}`,
		`{"tokenBudget":1}`, `{"includeHistory":true}`, `{"path":"/private/goal"}`,
	} {
		if _, err := r.clientMessage([]byte(`{"id":1,"method":"thread/goal/get","params":` + params + `}`)); err == nil {
			t.Fatalf("unsafe goal read options admitted: %s", params)
		}
	}
	for _, method := range []string{"thread/goal/set", "thread/goal/clear", "thread/goal/start", "thread/goal/resume"} {
		if _, err := r.clientMessage([]byte(`{"id":1,"method":"` + method + `","params":{}}`)); err == nil {
			t.Fatalf("native goal mutation admitted: %s", method)
		}
	}
	data, err := r.clientMessage(request)
	want := []byte(`{"id":1,"method":"thread/goal/get","params":{"threadId":"thread"}}`)
	if err != nil || !webUIEqualJSON(data, want) || !r.resumed {
		t.Fatalf("goal read changed validated selection: %s %v", data, err)
	}
	if r.pending["n:1"] != "thread/goal/get" {
		t.Fatal("goal read was not reserved for its native reply")
	}
}

func TestWebUIGoalReadProjectsNativeSnapshots(t *testing.T) {
	for _, status := range []string{"active", "paused", "blocked", "usageLimited", "budgetLimited", "complete"} {
		t.Run(status, func(t *testing.T) {
			r := testWebUIRelay()
			r.resumed = true
			r.pending["n:1"] = "thread/goal/get"
			response := []byte(`{"jsonrpc":"2.0","id":1,"params":{"secret":"secret"},"result":{"secret":"secret","goal":{"threadId":"thread","objective":"Ship the goal","status":"` + status + `","tokenBudget":200,"tokensUsed":12,"timeUsedSeconds":30,"createdAt":1,"updatedAt":2,"futureField":"secret"}}}`)
			data, err := r.serverMessage(response)
			want := []byte(`{"jsonrpc":"2.0","id":1,"result":{"goal":{"objective":"Ship the goal","status":"` + status + `"}}}`)
			if err != nil || !webUIEqualJSON(data, want) {
				t.Fatalf("goal projection leaked fields or lost native state: %s %v", data, err)
			}
			if len(r.pending) != 0 {
				t.Fatal("completed goal read remained pending")
			}
		})
	}
	for _, result := range []string{`{"goal":null,"secret":"secret"}`, `{}`} {
		r := testWebUIRelay()
		r.resumed = true
		r.pending["n:1"] = "thread/goal/get"
		data, err := r.serverMessage([]byte(`{"id":1,"result":` + result + `}`))
		if err != nil || !webUIEqualJSON(data, []byte(`{"id":1,"result":{"goal":null}}`)) {
			t.Fatalf("missing goal did not remain a harmless empty snapshot: %s %v", data, err)
		}
	}
}

func TestWebUIGoalReadErrorsAreNonfatalAndDoNotExposeNativeDetails(t *testing.T) {
	for _, response := range []string{
		`{"id":1,"error":{"code":-32601,"message":"secret unsupported API","data":{"path":"/secret/goal"}}}`,
		`{"id":1,"error":{"code":-32602,"message":"secret disabled feature"}}`,
		`{"id":1,"result":{"goal":{"threadId":"other","objective":"secret foreign objective","status":"active"}}}`,
		`{"id":1,"result":{"goal":{"objective":"secret unscoped objective","status":"active"}}}`,
		`{"id":1,"result":{"goal":{"threadId":"thread","objective":null,"status":"active"}}}`,
		`{"id":1,"result":{"goal":{"threadId":"thread","objective":"secret objective","status":"secret unknown status"}}}`,
		`{"id":1,"result":{"goal":[]}}`, `{"id":1,"result":null}`, `{"id":1}`,
	} {
		r := testWebUIRelay()
		r.resumed = true
		r.pending["n:1"] = "thread/goal/get"
		data, err := r.serverMessage([]byte(response))
		if err != nil || strings.Contains(string(data), "secret") || !strings.Contains(string(data), "Codex goal is unavailable.") {
			t.Fatalf("goal failure leaked native details or ended relay: %s %v", data, err)
		}
		if len(r.pending) != 0 || !r.resumed {
			t.Fatal("unavailable goal damaged the validated connection")
		}
		if _, err := r.clientMessage([]byte(`{"id":2,"method":"model/list","params":{}}`)); err != nil {
			t.Fatalf("unavailable goal blocked later requests: %v", err)
		}
	}
}

func TestWebUIGoalNotificationsRemainSelectedThreadNotifications(t *testing.T) {
	r := testWebUIRelay()
	update := []byte(`{"method":"thread/goal/updated","params":{"threadId":"thread","turnId":"turn","goal":{"threadId":"thread","objective":"Ship the goal","status":"active","tokensUsed":12,"futureField":"secret"},"futureField":"secret"}}`)
	if data, err := r.serverMessage(update); err != nil || len(data) != 0 {
		t.Fatalf("goal exposed before validated resume: %s %v", data, err)
	}
	r.resumed = true
	data, err := r.serverMessage(update)
	want := []byte(`{"method":"thread/goal/updated","params":{"threadId":"thread","goal":{"objective":"Ship the goal","status":"active"}}}`)
	if err != nil || !webUIEqualJSON(data, want) {
		t.Fatalf("live goal projection incorrect: %s %v", data, err)
	}
	data, err = r.serverMessage([]byte(`{"method":"thread/goal/cleared","params":{"threadId":"thread","futureField":"secret"}}`))
	if err != nil || !webUIEqualJSON(data, []byte(`{"method":"thread/goal/cleared","params":{"threadId":"thread"}}`)) {
		t.Fatalf("clear projection incorrect: %s %v", data, err)
	}
	for _, raw := range []string{
		`{"method":"thread/goal/updated","params":{"threadId":"other","goal":{"threadId":"other","objective":"secret","status":"active"}}}`,
		`{"method":"thread/goal/updated","params":{"threadId":"thread","goal":{"threadId":"other","objective":"secret","status":"active"}}}`,
		`{"method":"thread/goal/updated","params":{"thread_id":"thread","goal":{"threadId":"thread","objective":"secret","status":"active"}}}`,
		`{"method":"thread/goal/updated","params":{"threadId":"thread","goal":{"threadId":"other","objective":"secret","status":{}}}}`,
		`{"method":"thread/goal/cleared","params":{"threadId":"other"}}`,
		`{"method":"thread/goal/cleared","params":{}}`,
		`{"id":1,"method":"thread/goal/updated","params":{"threadId":"thread","goal":{"threadId":"thread","objective":"secret","status":"active"}}}`,
		`{"id":null,"method":"thread/goal/cleared","params":{"threadId":"thread"}}`,
	} {
		if data, err := r.serverMessage([]byte(raw)); err != nil || len(data) != 0 {
			t.Fatalf("unsafe goal notification forwarded: %s %v", data, err)
		}
	}
	if len(r.requests) != 0 {
		t.Fatal("goal notifications became actionable server requests")
	}
	for _, goal := range []string{
		`{"objective":"secret","status":"active"}`,
		`{"threadId":"thread","objective":"secret","status":"unknown"}`,
		`{"threadId":"thread","objective":null,"status":"active"}`, `null`, `[]`,
	} {
		data, err := r.serverMessage([]byte(`{"method":"thread/goal/updated","params":{"threadId":"thread","goal":` + goal + `}}`))
		want := []byte(`{"method":"thread/goal/updated","params":{"threadId":"thread","goal":null}}`)
		if err != nil || !webUIEqualJSON(data, want) {
			t.Fatalf("malformed selected-thread goal retained stale state: %s %v", data, err)
		}
	}
}

func TestWebUIGoalReadReplyCannotCrossResumeInvalidation(t *testing.T) {
	r := testWebUIRelay()
	r.resumed = true
	if _, err := r.clientMessage([]byte(`{"id":1,"method":"thread/goal/get","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.clientMessage([]byte(`{"id":2,"method":"thread/resume","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	data, err := r.serverMessage([]byte(`{"id":1,"result":{"goal":{"threadId":"thread","objective":"secret stale objective","status":"active"}}}`))
	if err != nil || strings.Contains(string(data), "secret") || !strings.Contains(string(data), "Codex goal is unavailable.") {
		t.Fatalf("in-flight goal read crossed resume validation: %s %v", data, err)
	}
	if _, err := r.clientMessage([]byte(`{"id":3,"method":"thread/goal/get","params":{}}`)); err == nil {
		t.Fatal("pending resume permitted another goal read")
	}
	for _, method := range []string{"thread/goal/updated", "thread/goal/cleared"} {
		data, err := r.serverMessage([]byte(`{"method":"` + method + `","params":{"threadId":"thread","goal":{"threadId":"thread","objective":"secret stale objective","status":"active"}}}`))
		if err != nil || len(data) != 0 {
			t.Fatalf("goal notification crossed resume validation: %s %v", data, err)
		}
	}
}

func TestWebUIGoalObjectiveUsesExistingRedactionBoundary(t *testing.T) {
	redactor, err := newWorkerRedactor([]string{`private-value`})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"id":1,"result":{"goal":{"threadId":"thread","objective":"Ship private-value","status":"active"}}}`,
		`{"method":"thread/goal/updated","params":{"threadId":"thread","goal":{"threadId":"thread","objective":"Ship private-value","status":"active"}}}`,
	} {
		r := testWebUIRelay()
		r.resumed = true
		r.pending["n:1"] = "thread/goal/get"
		projected, err := r.serverMessage([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		data, err := redactWebUIJSON(redactor, projected)
		if err != nil || strings.Contains(string(data), "private-value") || !strings.Contains(string(data), "Ship [REDACTED]") {
			t.Fatalf("goal objective bypassed redaction: %s %v", data, err)
		}
		var rpc webUIRPC
		if json.Unmarshal(data, &rpc) != nil || !strings.Contains(string(data), `"status":"active"`) {
			t.Fatalf("goal redaction changed protocol state: %s", data)
		}
	}
}
