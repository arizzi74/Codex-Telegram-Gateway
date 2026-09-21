package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

// These are informational notifications and read-only requests in the locally
// generated official Codex 0.155.1 app-server protocol. None grants permission
// to ignore a turn, approval, or process already tracked by the proxy.
func TestNativeActivityCurrentProtocolInformationalNotifications(t *testing.T) {
	for _, method := range []string{
		"thread/attachment/updated", "rawResponseItem/completed", "rawResponse/completed",
		"fuzzyFileSearch/sessionUpdated", "fuzzyFileSearch/sessionCompleted",
		"mcpServer/oauthLogin/completed", "mcpServer/event/stream/notification",
		"account/login/completed", "externalAgentConfig/import/progress",
		"externalAgentConfig/import/completed", "windowsSandbox/setupCompleted",
	} {
		t.Run(method, func(t *testing.T) {
			var activity nativeActivity
			activity.serverMessage(nativeTurnMessage("turn/started", "thread", "turn"))
			message, err := json.Marshal(map[string]any{"method": method, "params": map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			activity.serverMessage(message)
			assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
			activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
			assertNativeIdle(t, &activity)
		})
	}
}

func TestNativeActivityCurrentProtocolReadOnlyRequests(t *testing.T) {
	for _, method := range []string{
		"thread/attachment/list", "memory/status", "plugin/share/list", "remoteControl/pairing/status",
		"remoteControl/client/list", "userVerification/status", "externalAgentConfig/detect",
		"externalAgentConfig/import/readHistories", "getConversationSummary", "gitDiffToRemote",
		"getAuthStatus", "thread/realtime/listVoices", "windowsSandbox/readiness",
	} {
		t.Run(method, func(t *testing.T) {
			var activity nativeActivity
			message, err := json.Marshal(map[string]any{"id": 1, "method": method, "params": map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			activity.clientMessage(message)
			assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
			activity.serverMessage([]byte(`{"id":1,"result":{}}`))
			assertNativeIdle(t, &activity)
		})
	}
}

func TestNativeActivityObservationGapNeedsFencedVerification(t *testing.T) {
	var activity nativeActivity
	activity.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"systemError"}}}`))
	assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
	if activity.uncertain || activity.observationGap == "" || activity.settledError() != nil {
		t.Fatalf("systemError should need fresh observation, not permanent uncertainty: %+v", activity)
	}
	activity.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"idle"}}}`))
	assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
	if activity.settledError() != nil {
		t.Fatal("an observation gap must remain eligible for fenced verification")
	}
}

func TestNativeActivityObservationGapCannotHideAcceptedWork(t *testing.T) {
	var activity nativeActivity
	activity.serverMessage(nativeTurnMessage("turn/started", "thread", "turn"))
	activity.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"systemError"}}}`))
	activity.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"idle"}}}`))
	if activity.settledError() == nil {
		t.Fatal("fresh verification must not bypass an accepted turn")
	}
	activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
	if activity.settledError() != nil {
		t.Fatal("completed work should become eligible for fenced verification")
	}
	assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
	activity.serverMessage([]byte(`{"method":"future/activity","params":{"private":"do not log"}}`))
	if activity.settledError() == nil || !activity.uncertain {
		t.Fatal("unknown activity must remain a hard blocker even with an observation gap")
	}
	if activity.uncertaintyReason != "unrecognized native server notification: future/activity" {
		t.Fatalf("diagnostic = %q", activity.uncertaintyReason)
	}
}

func TestNativeActivityHardUncertaintyRetainsFirstSafeDiagnostic(t *testing.T) {
	for _, test := range []struct {
		name     string
		messages []string
		server   bool
		want     string
	}{
		{"malformed", []string{`{"password":"private",`}, false, "malformed or ambiguous JSON-RPC object"},
		{"duplicate", []string{`{"id":1,"method":"model/list"}`, `{"id":1,"method":"model/list"}`}, false, "duplicate native CLI request ID"},
		{"notification", []string{`{"method":"future/activity","params":{"token":"private"}}`}, true, "unrecognized native server notification: future/activity"},
		{"unsafe method", []string{`{"method":"/private/file or\nsecret"}`}, true, "unrecognized native server notification"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var activity nativeActivity
			for _, message := range test.messages {
				if test.server {
					activity.serverMessage([]byte(message))
				} else {
					activity.clientMessage([]byte(message))
				}
			}
			activity.requireVerification("subsequent observation gap")
			activity.serverMessage([]byte(`{"method":"another/unknown"}`))
			if activity.uncertaintyReason != test.want || strings.Contains(activity.uncertaintyReason, "private") {
				t.Fatalf("diagnostic = %q; want %q", activity.uncertaintyReason, test.want)
			}
			if activity.settledError() == nil {
				t.Fatal("ambiguous or unknown work must not be eligible for recovery")
			}
		})
	}
}

func TestNativeActivityDisconnectedReadOnlyRequestCanBeReverified(t *testing.T) {
	for _, method := range []string{"config/read", "thread/read", "thread/list", "thread/queue/list", "model/list"} {
		t.Run(method, func(t *testing.T) {
			var activity nativeActivity
			message, _ := json.Marshal(map[string]any{"id": 1, "method": method, "params": map[string]any{"threadId": "thread"}})
			activity.clientMessage(message)
			if activity.settledError() == nil {
				t.Fatal("a connected request must wait for its reply")
			}
			if err := activity.disconnectedError(); err != nil {
				t.Fatalf("lost read-only request should permit independent verification: %v", err)
			}
			activity.serverMessage(nativeTurnMessage("turn/started", "thread", "turn"))
			if activity.disconnectedError() == nil {
				t.Fatal("lost read-only request cannot hide an active turn")
			}
		})
	}
}

func TestNativeActivityDisconnectedMutationsStillRequireAcknowledgement(t *testing.T) {
	for _, method := range []string{"turn/start", "thread/queue/add", "thread/name/set", "config/value/write", "fs/writeFile", "command/exec", "process/spawn", "future/start", "account/read", "getAuthStatus"} {
		t.Run(method, func(t *testing.T) {
			var activity nativeActivity
			message, _ := json.Marshal(map[string]any{"id": 1, "method": method, "params": map[string]any{"threadId": "thread", "processHandle": "process"}})
			activity.clientMessage(message)
			if activity.disconnectedError() == nil {
				t.Fatal("loss of a mutating or unknown request's response must remain blocked")
			}
		})
	}
}

func TestNativeActivityQueuesNeedFreshVerificationAfterAcknowledgement(t *testing.T) {
	for _, method := range []string{"thread/queue/add", "thread/queue/update", "thread/queue/start", "thread/queue/list", "thread/queue/delete", "thread/queue/reorder"} {
		for _, early := range []bool{false, true} {
			t.Run(method+"/early="+map[bool]string{false: "false", true: "true"}[early], func(t *testing.T) {
				var activity nativeActivity
				message, _ := json.Marshal(map[string]any{"id": 1, "method": method, "params": map[string]any{"threadId": "thread"}})
				activity.clientMessage(message)
				if early {
					activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
				}
				if method == "thread/queue/start" {
					activity.serverMessage([]byte(`{"id":1,"result":{"turn":{"id":"turn","status":"inProgress"}}}`))
					if !early {
						if activity.settledError() == nil {
							t.Fatal("accepted queued turn must complete before verification")
						}
						activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
					}
				} else {
					activity.serverMessage([]byte(`{"id":1,"result":{"queuedSubmission":{"id":"queued"}}}`))
				}
				assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
				if activity.uncertain || activity.settledError() != nil || activity.observationGap == "" {
					t.Fatalf("acknowledged queue should require recoverable verification: %+v", activity)
				}
				if _, ok := activity.queueThreads["thread"]; !ok {
					t.Fatal("queue verification must include the changed thread")
				}
			})
		}
	}
}

func TestNativeActivityQueueNotificationsDoNotLoseDirtyThreads(t *testing.T) {
	var activity nativeActivity
	activity.serverMessage([]byte(`{"method":"thread/queue/changed","params":{"threadId":"one"}}`))
	activity.serverMessage([]byte(`{"method":"thread/queue/changed","params":{"threadId":"two"}}`))
	activity.serverMessage(nativeTurnMessage("turn/completed", "one", "turn"))
	if len(activity.queueThreads) != 2 || activity.settledError() != nil {
		t.Fatalf("queue notifications must preserve both dirty threads for verification: %+v", activity)
	}
	activity.serverMessage([]byte(`{"method":"thread/queue/changed","params":{}}`))
	if !activity.uncertain || activity.settledError() == nil {
		t.Fatal("an unidentifiable queue cannot be cleared by thread snapshots")
	}
}

func TestNativeActivityDeletionSignalRequiresSuccessfulMatchingRequest(t *testing.T) {
	for _, test := range []struct {
		name, request, response, want string
	}{
		{"success", `{"id":1,"method":"thread/delete","params":{"threadId":"thread"}}`, `{"id":1,"result":{}}`, "thread"},
		{"RPC error", `{"id":1,"method":"thread/delete","params":{"threadId":"thread"}}`, `{"id":1,"error":{"code":-32600,"message":"unavailable"}}`, ""},
		{"missing thread", `{"id":1,"method":"thread/delete","params":{}}`, `{"id":1,"result":{}}`, ""},
		{"unmatched reply", `{"id":1,"method":"thread/delete","params":{"threadId":"thread"}}`, `{"id":2,"result":{}}`, ""},
		{"invalid result", `{"id":1,"method":"thread/delete","params":{"threadId":"thread"}}`, `{"id":1,"result":[]}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var activity nativeActivity
			activity.serverMessage(nativeTurnMessage("turn/started", "thread", "turn"))
			activity.queueChanged("thread")
			activity.clientMessage([]byte(test.request))
			if activity.deletedThread != "" {
				t.Fatal("a deletion request alone is not evidence of deletion")
			}
			activity.serverMessage([]byte(test.response))
			if activity.deletedThread != test.want {
				t.Fatalf("deleted thread = %q; want %q", activity.deletedThread, test.want)
			}
			if len(activity.turns) != 1 || len(activity.queueThreads) != 1 || activity.settledError() == nil {
				t.Fatal("deletion signal must not erase independent accepted work or pending verification")
			}
			activity.clientMessage([]byte(`{"method":"initialized"}`))
			if activity.deletedThread != "" {
				t.Fatal("deletion signal must be consumed only for its original message")
			}
		})
	}
}

func TestNativeActivityDeletionNotificationRequiresValidThread(t *testing.T) {
	for _, test := range []struct {
		name, message, want string
	}{
		{"valid", `{"method":"thread/deleted","params":{"threadId":"thread"}}`, "thread"},
		{"missing thread", `{"method":"thread/deleted","params":{}}`, ""},
		{"wrong thread type", `{"method":"thread/deleted","params":{"threadId":1}}`, ""},
		{"duplicate field", `{"method":"thread/deleted","params":{"threadId":"one","threadId":"two"}}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var activity nativeActivity
			activity.markUncertain("previous unresolved ambiguity")
			activity.serverMessage([]byte(test.message))
			if activity.deletedThread != test.want {
				t.Fatalf("deleted thread = %q; want %q", activity.deletedThread, test.want)
			}
			if !activity.uncertain || activity.uncertaintyReason != "previous unresolved ambiguity" || activity.settledError() == nil {
				t.Fatal("deletion notification must not clear existing hard uncertainty")
			}
			activity.serverMessage([]byte(`{"method":"warning","params":{}}`))
			if activity.deletedThread != "" {
				t.Fatal("deletion signal must not survive a subsequent notification")
			}
		})
	}
}
