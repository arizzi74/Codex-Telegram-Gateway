package worker

import (
	"fmt"
	"strings"
	"testing"
)

func terminalRecoveryActivity(thread, turn string) nativeActivity {
	var activity nativeActivity
	activity.serverMessage([]byte(fmt.Sprintf(`{"method":"thread/started","params":{"thread":{"id":%q,"status":{"type":"active"},"turns":[{"id":%q,"status":"inProgress"}]}}}`, thread, turn)))
	activity.serverMessage([]byte(fmt.Sprintf(`{"method":"thread/status/changed","params":{"threadId":%q,"status":{"type":"systemError"}}}`, thread)))
	return activity
}

func terminalRecoveryProxy(activities ...nativeActivity) *attachmentProxy {
	proxy := &attachmentProxy{sessions: make(map[*attachmentSession]struct{})}
	for _, activity := range activities {
		proxy.sessions[&attachmentSession{activity: activity}] = struct{}{}
	}
	return proxy
}

func TestNativeTerminalSubagentRecoveryRequiresExactCompletionProof(t *testing.T) {
	proxy := terminalRecoveryProxy(terminalRecoveryActivity("child", "failed-turn"))
	if err := proxy.pause(); err == nil {
		t.Fatal("ordinary pause must preserve snapshot activity")
	}
	candidates, err := proxy.pauseForTerminalSubagentRecovery()
	if err != nil || len(candidates) != 1 || candidates["child"] != "failed-turn" {
		t.Fatalf("recovery candidates = %v, %v", candidates, err)
	}
	if err := proxy.confirmIdle(); err == nil {
		t.Fatal("idle verification alone settled a snapshot turn")
	}
	for _, proofs := range []map[string]string{nil, {}, {"child": ""}, {"child": "another-turn"}, {"other-child": "failed-turn"}} {
		if err := proxy.confirmIdleWithTerminalSubagents(proofs); err == nil {
			t.Fatalf("incomplete proof allowed recovery: %v", proofs)
		}
		if err := proxy.checkIdle(); err == nil {
			t.Fatal("failed confirmation erased active markers")
		}
		for session := range proxy.sessions {
			if len(session.activity.turns) != 1 || len(session.activity.threads) != 1 || session.activity.observationGap == "" {
				t.Fatal("failed confirmation partially changed activity")
			}
		}
	}
	if err := proxy.confirmIdleWithTerminalSubagents(map[string]string{"child": "failed-turn"}); err != nil {
		t.Fatal(err)
	}
	if err := proxy.checkIdle(); err != nil {
		t.Fatalf("matching terminal proof did not settle snapshot markers: %v", err)
	}
	for session := range proxy.sessions {
		if _, completed := session.activity.completed[nativeTurn{"child", "failed-turn"}]; !completed {
			t.Fatal("reconciled completion was not retained")
		}
		session.activity.serverMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"child","turns":[{"id":"failed-turn","status":"inProgress"}]}}}`))
		if len(session.activity.turns) != 0 {
			t.Fatal("an old snapshot resurrected an independently completed turn")
		}
	}
}

func TestNativeTerminalSubagentRecoveryValidatesAllSessionsAtomically(t *testing.T) {
	proxy := terminalRecoveryProxy(terminalRecoveryActivity("first", "first-turn"), terminalRecoveryActivity("second", "second-turn"))
	if _, err := proxy.pauseForTerminalSubagentRecovery(); err != nil {
		t.Fatal(err)
	}
	if err := proxy.confirmIdleWithTerminalSubagents(map[string]string{"first": "first-turn"}); err == nil {
		t.Fatal("missing second proof allowed partial reconciliation")
	}
	for session := range proxy.sessions {
		if len(session.activity.turns) != 1 || len(session.activity.threads) != 1 || len(session.activity.completed) != 0 {
			t.Fatal("one valid proof erased activity before all proofs were checked")
		}
	}
}

func TestNativeTerminalSubagentRecoveryRejectsConflictingSessions(t *testing.T) {
	proxy := terminalRecoveryProxy(terminalRecoveryActivity("child", "old-turn"), terminalRecoveryActivity("child", "new-turn"))
	if _, err := proxy.pauseForTerminalSubagentRecovery(); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("conflicting native observations allowed verification: %v", err)
	}
	if proxy.paused {
		t.Fatal("failed verification left admission fenced")
	}
}

func TestNativeTerminalSubagentRecoveryPreservesEveryHardBlocker(t *testing.T) {
	for _, test := range []struct {
		name   string
		client []string
		server []string
	}{
		{name: "pending read", client: []string{`{"id":1,"method":"config/read"}`}},
		{name: "pending mutation", client: []string{`{"id":1,"method":"thread/name/set","params":{"threadId":"child","name":"new"}}`}},
		{name: "accepted turn", client: []string{`{"id":1,"method":"turn/start","params":{"threadId":"child"}}`}, server: []string{`{"id":1,"result":{"turn":{"id":"failed-turn","status":"inProgress"}}}`}},
		{name: "turn lifecycle", server: []string{`{"method":"turn/started","params":{"threadId":"child","turn":{"id":"failed-turn"}}}`}},
		{name: "approval", server: []string{`{"id":"approval","method":"item/tool/requestUserInput","params":{"threadId":"child","turnId":"failed-turn"}}`}},
		{name: "process", client: []string{`{"id":1,"method":"process/spawn","params":{"processHandle":"process"}}`}, server: []string{`{"id":1,"result":{}}`}},
		{name: "hook", server: []string{`{"method":"hook/started","params":{"threadId":"child","run":{"id":"hook"}}}`}},
		{name: "compaction", client: []string{`{"id":1,"method":"thread/compact/start","params":{"threadId":"child"}}`}, server: []string{`{"id":1,"result":{}}`}},
		{name: "unknown asynchronous mutation", client: []string{`{"id":1,"method":"future/mutate","params":{"threadId":"child"}}`}, server: []string{`{"id":1,"result":{}}`}},
		{name: "ambiguous message", server: []string{`{"method":"thread/status/changed","params":{"threadId":"child","threadId":"other","status":"idle"}}`}},
		{name: "unrelated active thread", server: []string{`{"method":"thread/started","params":{"thread":{"id":"other","status":"active","turns":[]}}}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			activity := terminalRecoveryActivity("child", "failed-turn")
			for _, raw := range test.client {
				activity.clientMessage([]byte(raw))
			}
			for _, raw := range test.server {
				activity.serverMessage([]byte(raw))
			}
			proxy := terminalRecoveryProxy(activity)
			if _, err := proxy.pauseForTerminalSubagentRecovery(); err == nil {
				t.Fatal("terminal recovery bypassed a hard blocker")
			}
			if proxy.paused {
				t.Fatal("failed recovery fenced admission")
			}
		})
	}
}

func TestNativeTerminalSubagentRecoveryRequiresObservedSystemError(t *testing.T) {
	var activity nativeActivity
	activity.serverMessage([]byte(`{"method":"thread/started","params":{"thread":{"id":"child","status":"active","turns":[{"id":"turn","status":"inProgress"}]}}}`))
	proxy := terminalRecoveryProxy(activity)
	if _, err := proxy.pauseForTerminalSubagentRecovery(); err == nil {
		t.Fatal("an active snapshot without a failure observation allowed recovery")
	}
	for session := range proxy.sessions {
		session.activity.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"unrelated","status":"systemError"}}`))
	}
	if _, err := proxy.pauseForTerminalSubagentRecovery(); err == nil {
		t.Fatal("a failure on an unrelated thread qualified active snapshot recovery")
	}
}

func TestNativeTerminalSubagentRecoveryFreshStatusInvalidatesOldFailure(t *testing.T) {
	for _, status := range []string{"active", "idle", "notLoaded"} {
		t.Run(status, func(t *testing.T) {
			activity := terminalRecoveryActivity("child", "old-turn")
			activity.threadStatus("child", []byte(fmt.Sprintf(`%q`, status)))
			proxy := terminalRecoveryProxy(activity)
			if _, err := proxy.pauseForTerminalSubagentRecovery(); err == nil {
				t.Fatal("an old error qualified recovery after a fresh non-error status")
			}
		})
	}
}

func TestNativeTerminalSubagentRecoveryPreservesProxyHardBlockers(t *testing.T) {
	for _, blocker := range []string{"closed", "uncertain", "initializing", "forwarding"} {
		t.Run(blocker, func(t *testing.T) {
			proxy := terminalRecoveryProxy(terminalRecoveryActivity("child", "turn"))
			for session := range proxy.sessions {
				switch blocker {
				case "closed":
					proxy.closed = true
				case "uncertain":
					proxy.uncertain = true
				case "initializing":
					session.initializing = true
				case "forwarding":
					session.forwarding = 1
				}
			}
			if _, err := proxy.pauseForTerminalSubagentRecovery(); err == nil || proxy.paused {
				t.Fatalf("hard proxy blocker did not preserve admission: %v, paused=%v", err, proxy.paused)
			}
		})
	}
}

func TestNativeTerminalSubagentRecoveryUnionsStatusAndTurnObservations(t *testing.T) {
	var statusOnly nativeActivity
	statusOnly.threadStatus("child", []byte(`"active"`))
	statusOnly.threadStatus("child", []byte(`"systemError"`))
	proxy := terminalRecoveryProxy(statusOnly, terminalRecoveryActivity("child", "failed-turn"))
	candidates, err := proxy.pauseForTerminalSubagentRecovery()
	if err != nil || candidates["child"] != "failed-turn" {
		t.Fatalf("mixed observations lost precise turn identity: %v, %v", candidates, err)
	}
	if err := proxy.confirmIdleWithTerminalSubagents(map[string]string{"child": "other-turn"}); err == nil {
		t.Fatal("status-only evidence hid conflicting snapshot identity")
	}
	if err := proxy.confirmIdleWithTerminalSubagents(map[string]string{"child": "failed-turn"}); err != nil {
		t.Fatal(err)
	}
	if err := proxy.checkIdle(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeTerminalSubagentRecoveryProofsCannotSettleOtherActivity(t *testing.T) {
	var unrelated nativeActivity
	unrelated.completed = map[nativeTurn]struct{}{{"other", "previous"}: {}}
	proxy := terminalRecoveryProxy(terminalRecoveryActivity("child", "failed-turn"), unrelated)
	if _, err := proxy.pauseForTerminalSubagentRecovery(); err != nil {
		t.Fatal(err)
	}
	if err := proxy.confirmIdleWithTerminalSubagents(map[string]string{"child": "failed-turn", "other": "invented"}); err != nil {
		t.Fatal(err)
	}
	for session := range proxy.sessions {
		if _, exists := session.activity.completed[nativeTurn{"other", "previous"}]; exists {
			if len(session.activity.completed) != 1 {
				t.Fatal("an extraneous proof changed an unrelated session")
			}
		}
	}
}

func TestNativeTerminalSubagentRecoveryLateObservationInvalidatesProof(t *testing.T) {
	for _, late := range []string{
		`{"method":"turn/completed","params":{"threadId":"child","turn":{"id":"failed-turn","status":"failed"}}}`,
		`{"method":"thread/name/updated","params":{"threadId":"child","name":"renamed"}}`,
		`{"id":"approval","method":"item/tool/requestUserInput","params":{"threadId":"child","turnId":"failed-turn"}}`,
	} {
		t.Run(late, func(t *testing.T) {
			proxy, path, requests := newNativeProxyTest(t)
			client := dialNativeProxy(t, path)
			nativeWrite(t, client, `{"id":1,"method":"config/read"}`)
			request := nextNativeRequest(t, requests)
			nativeWrite(t, request.connection, `{"id":1,"result":{}}`)
			nativeRecoveryRead(t, proxy, client)
			nativeWrite(t, request.connection, `{"method":"thread/started","params":{"thread":{"id":"child","status":"active","turns":[{"id":"failed-turn","status":"inProgress"}]}}}`)
			nativeRecoveryRead(t, proxy, client)
			nativeWrite(t, request.connection, `{"method":"thread/status/changed","params":{"threadId":"child","status":"systemError"}}`)
			nativeRecoveryRead(t, proxy, client)
			if _, err := proxy.pauseForTerminalSubagentRecovery(); err != nil {
				t.Fatal(err)
			}
			nativeWrite(t, client, `{"id":2,"method":"turn/start","params":{"threadId":"child"}}`)
			if rejection := nativeRead(t, client); !strings.Contains(rejection, `"code":-32002`) {
				t.Fatalf("recovery did not fence new requests: %s", rejection)
			}
			select {
			case <-requests:
				t.Fatal("new work crossed the recovery fence")
			default:
			}
			nativeWrite(t, request.connection, late)
			nativeRecoveryRead(t, proxy, client)
			if err := proxy.confirmIdleWithTerminalSubagents(map[string]string{"child": "failed-turn"}); err == nil || !strings.Contains(err.Error(), "changed during idle verification") {
				t.Fatalf("late observation did not invalidate proof: %v", err)
			}
			for session := range proxy.sessions {
				if len(session.activity.threads) != 1 || session.activity.observationGap == "" {
					t.Fatal("stale proof erased thread activity")
				}
			}
			proxy.resume()
		})
	}
}
