package worker

import (
	"fmt"
	"testing"
)

func TestNativeActivityTracksTypedIDsAndSuccessfulOrFailedReplies(t *testing.T) {
	var activity nativeActivity
	assertNativeIdle(t, &activity)
	activity.clientMessage([]byte(`{"method":"initialized"}`))
	assertNativeIdle(t, &activity)
	activity.clientMessage([]byte(`{"id":7,"method":"thread/read","params":{"threadId":"thread"}}`))
	activity.clientMessage([]byte(`{"id":"7","method":"model/list","params":{}}`))
	assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
	activity.serverMessage([]byte(`{"id":"7","result":{}}`))
	assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
	activity.serverMessage([]byte(`{"id":7,"error":{"code":-32601,"message":"unavailable"}}`))
	assertNativeIdle(t, &activity)
	// Completed request IDs may be reused, including integers above float64's
	// exact range. Their identity must not be rounded by JSON decoding.
	activity.clientMessage([]byte(`{"id":9007199254740993,"method":"account/read"}`))
	activity.clientMessage([]byte(`{"id":9007199254740992,"method":"account/read"}`))
	activity.serverMessage([]byte(`{"id":9007199254740992,"result":{}}`))
	assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
	activity.serverMessage([]byte(`{"id":9007199254740993,"result":{}}`))
	activity.clientMessage([]byte(`{"id":7,"method":"config/read"}`))
	activity.serverMessage([]byte(`{"id":7,"result":{}}`))
	assertNativeIdle(t, &activity)
}

func TestNativeActivityApprovalAndResolutionRaces(t *testing.T) {
	for _, resolveFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(resolveFirst), func(t *testing.T) {
			var activity nativeActivity
			activity.serverMessage([]byte(`{"id":"approval","method":"item/tool/requestUserInput","params":{"threadId":"thread"}}`))
			assertNativeBusy(t, &activity, "worker update: native CLI approvals or input are pending")
			if resolveFirst {
				activity.serverMessage([]byte(`{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":"approval"}}`))
				assertNativeIdle(t, &activity)
			}
			activity.clientMessage([]byte(`{"id":"approval","result":{"answers":{}}}`))
			if !resolveFirst {
				assertNativeBusy(t, &activity, "worker update: native CLI approvals or input are pending")
			}
			activity.serverMessage([]byte(`{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":"approval"}}`))
			assertNativeIdle(t, &activity)
		})
	}
}

func TestNativeActivityAnsweredApprovalWaitsForItsParentTurn(t *testing.T) {
	for _, answerFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(answerFirst), func(t *testing.T) {
			var activity nativeActivity
			activity.serverMessage([]byte(`{"id":1,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread","turnId":"turn"}}`))
			if answerFirst {
				activity.clientMessage([]byte(`{"id":1,"result":{"decision":"accept"}}`))
			}
			assertNativeBusy(t, &activity, "worker update: native CLI approvals or input are pending")
			activity.serverMessage(nativeTurnMessage("turn/completed", "other-thread", "turn"))
			activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "other-turn"))
			assertNativeBusy(t, &activity, "worker update: native CLI approvals or input are pending")
			activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
			assertNativeIdle(t, &activity)
			if !answerFirst {
				activity.clientMessage([]byte(`{"id":1,"result":{"decision":"accept"}}`))
				assertNativeIdle(t, &activity)
			}
		})
	}
}

func TestNativeActivityUnknownParentApprovalNeedsExplicitResolution(t *testing.T) {
	for _, params := range []string{`{}`, `{"threadId":"thread"}`, `{"turnId":"turn"}`} {
		t.Run(params, func(t *testing.T) {
			var activity nativeActivity
			activity.serverMessage([]byte(`{"id":1,"method":"item/tool/requestUserInput","params":` + params + `}`))
			activity.clientMessage([]byte(`{"id":1,"result":{"answers":{}}}`))
			activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
			assertNativeBusy(t, &activity, "worker update: native CLI approvals or input are pending")
			activity.serverMessage([]byte(`{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":1}}`))
			assertNativeIdle(t, &activity)
		})
	}
}

func TestNativeActivityAcceptedTurnsAwaitMatchingCompletion(t *testing.T) {
	for _, method := range []string{"turn/start", "turn/steer", "review/start"} {
		for _, completionFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", method, completionFirst), func(t *testing.T) {
				var activity nativeActivity
				activity.clientMessage([]byte(fmt.Sprintf(`{"id":1,"method":%q,"params":{"threadId":"thread"}}`, method)))
				assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
				if completionFirst {
					activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
				}
				result := `{"id":1,"result":{"turn":{"id":"turn","status":"inProgress"}}}`
				if method == "turn/steer" {
					result = `{"id":1,"result":{"turnId":"turn"}}`
				}
				activity.serverMessage([]byte(result))
				if completionFirst {
					assertNativeIdle(t, &activity)
				} else {
					assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
					activity.serverMessage(nativeTurnMessage("turn/completed", "other-thread", "turn"))
					assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
					activity.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"idle"}}}`))
					assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
					activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
					assertNativeIdle(t, &activity)
				}
			})
		}
	}
}

func TestNativeActivityDetachedReviewTracksReviewThread(t *testing.T) {
	var activity nativeActivity
	activity.clientMessage([]byte(`{"id":1,"method":"review/start","params":{"threadId":"source","delivery":"detached"}}`))
	activity.serverMessage([]byte(`{"id":1,"result":{"reviewThreadId":"review","turn":{"id":"turn","status":"inProgress"}}}`))
	activity.serverMessage(nativeTurnMessage("turn/completed", "source", "turn"))
	assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
	activity.serverMessage(nativeTurnMessage("turn/completed", "review", "turn"))
	assertNativeIdle(t, &activity)
}

func TestNativeActivityCompactTracksEmptyAckAndEarlyNotifications(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprint(early), func(t *testing.T) {
			var activity nativeActivity
			activity.clientMessage([]byte(`{"id":1,"method":"thread/compact/start","params":{"threadId":"thread"}}`))
			if early {
				activity.serverMessage(nativeTurnMessage("turn/started", "thread", "compact-turn"))
				activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "compact-turn"))
			}
			activity.serverMessage([]byte(`{"id":1,"result":{}}`))
			if !early {
				assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
				activity.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread","status":"idle"}}`))
				assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
				activity.serverMessage(nativeTurnMessage("turn/started", "thread", "compact-turn"))
				activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "compact-turn"))
			}
			assertNativeIdle(t, &activity)
		})
	}
}

func TestNativeActivityProcessSpawnWaitsForExitAndSupportsReuse(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprint(early), func(t *testing.T) {
			var activity nativeActivity
			for range 2 {
				activity.clientMessage([]byte(`{"id":1,"method":"process/spawn","params":{"processHandle":"process"}}`))
				if early {
					activity.serverMessage([]byte(`{"method":"process/exited","params":{"processHandle":"process","exitCode":0}}`))
				}
				activity.serverMessage([]byte(`{"id":1,"result":{}}`))
				if !early {
					assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
					activity.serverMessage([]byte(`{"method":"process/exited","params":{"processHandle":"unrelated","exitCode":0}}`))
					assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
					activity.serverMessage([]byte(`{"method":"process/exited","params":{"processHandle":"process","exitCode":0}}`))
				}
				assertNativeIdle(t, &activity)
			}
		})
	}
}

func TestNativeActivityLegacyCompactedSettlesWithoutTurnStarted(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprint(early), func(t *testing.T) {
			var activity nativeActivity
			activity.clientMessage([]byte(`{"id":1,"method":"thread/compact/start","params":{"threadId":"thread"}}`))
			completed := []byte(`{"method":"thread/compacted","params":{"threadId":"thread","turnId":"compact"}}`)
			if early {
				activity.serverMessage(completed)
			}
			activity.serverMessage([]byte(`{"id":1,"result":{}}`))
			if !early {
				assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
				activity.serverMessage(completed)
			}
			assertNativeIdle(t, &activity)
		})
	}
}

func TestNativeActivityStandaloneExecWaitsForFinalReply(t *testing.T) {
	var activity nativeActivity
	activity.clientMessage([]byte(`{"id":1,"method":"command/exec","params":{"processId":"command","command":["true"]}}`))
	activity.serverMessage([]byte(`{"method":"command/exec/outputDelta","params":{"processId":"command","delta":""}}`))
	assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
	activity.serverMessage([]byte(`{"id":1,"result":{"exitCode":0,"stdout":"","stderr":""}}`))
	assertNativeIdle(t, &activity)
}

func TestNativeActivityStartupHookWaitsForMatchingCompletion(t *testing.T) {
	var activity nativeActivity
	activity.serverMessage([]byte(`{"method":"hook/started","params":{"threadId":"thread","turnId":null,"run":{"id":"hook"}}}`))
	assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
	activity.serverMessage([]byte(`{"method":"hook/completed","params":{"threadId":"other-thread","run":{"id":"hook"}}}`))
	assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
	activity.serverMessage([]byte(`{"method":"hook/completed","params":{"threadId":"thread","run":{"id":"hook"}}}`))
	assertNativeIdle(t, &activity)
}

func TestNativeActivityOverlappingCompactionsAreUncertain(t *testing.T) {
	var activity nativeActivity
	activity.clientMessage([]byte(`{"id":1,"method":"thread/compact/start","params":{"threadId":"thread"}}`))
	activity.clientMessage([]byte(`{"id":2,"method":"thread/compact/start","params":{"threadId":"thread"}}`))
	activity.serverMessage([]byte(`{"id":1,"result":{}}`))
	activity.serverMessage([]byte(`{"id":2,"result":{}}`))
	activity.serverMessage([]byte(`{"method":"thread/compacted","params":{"threadId":"thread","turnId":"compact"}}`))
	assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
}

func TestNativeActivityObservesRunningThreadSnapshotAndNotifications(t *testing.T) {
	var activity nativeActivity
	activity.clientMessage([]byte(`{"id":1,"method":"thread/resume","params":{"threadId":"thread"}}`))
	activity.serverMessage([]byte(`{"id":1,"result":{"thread":{"id":"thread","status":{"type":"active"},"turns":[{"id":"turn","status":"inProgress"}]}}}`))
	assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
	activity.serverMessage(nativeTurnMessage("turn/completed", "thread", "turn"))
	activity.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"idle"}}}`))
	assertNativeIdle(t, &activity)
	activity.serverMessage(nativeTurnMessage("turn/started", "other-thread", "other-turn"))
	assertNativeBusy(t, &activity, "worker update: a native CLI thread is active or its idle state cannot be verified")
	activity.serverMessage(nativeTurnMessage("turn/completed", "other-thread", "other-turn"))
	assertNativeIdle(t, &activity)
}

func TestNativeActivityMalformedOrAmbiguousMessagesRemainUncertain(t *testing.T) {
	for _, raw := range []string{
		`not json`, `null`, `[]`, `{}`, `{"method":"initialized"} {}`,
		`{"id":null,"method":"config/read"}`, `{"id":true,"method":"config/read"}`, `{"id":1.5,"method":"config/read"}`,
		`{"id":1,"id":2,"method":"config/read"}`, `{"id":1,"method":"config/read","params":[]}`,
		`{"id":1,"method":"config/read","result":{}}`, `{"method":"native/customNotification"}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"one","threadId":"two"}}`,
		`{"jsonrpc":"1.0","method":"initialized"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var activity nativeActivity
			activity.clientMessage([]byte(raw))
			assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
			activity.clientMessage([]byte(`{"method":"initialized"}`))
			assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
		})
	}
	for _, raw := range []string{
		`{"id":1,"result":{},"error":{}}`, `{"id":1,"result":{}}`,
		`{"method":"serverRequest/resolved","params":{"requestId":null}}`,
		`{"method":"turn/started","params":{"threadId":"thread"}}`,
		`{"method":"unknown/notification"}`, `{"id":1,"method":"unknown/request"}`,
	} {
		t.Run("server/"+raw, func(t *testing.T) {
			var activity nativeActivity
			activity.serverMessage([]byte(raw))
			assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
		})
	}
}

func TestNativeActivityDuplicateInFlightRequestsCannotBeCleared(t *testing.T) {
	for _, server := range []bool{false, true} {
		t.Run(fmt.Sprint(server), func(t *testing.T) {
			var activity nativeActivity
			request := []byte(`{"id":1,"method":"model/list"}`)
			send := activity.clientMessage
			if server {
				request = []byte(`{"id":1,"method":"item/tool/requestUserInput"}`)
				send = activity.serverMessage
			}
			send(request)
			send(request)
			if server {
				activity.clientMessage([]byte(`{"id":1,"result":{}}`))
			} else {
				activity.serverMessage([]byte(`{"id":1,"result":{}}`))
			}
			assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
		})
	}
}

func TestNativeActivityUnknownAsyncAcknowledgementsStayUncertain(t *testing.T) {
	for _, method := range []string{"future/start", "thread/shellCommand", "thread/queue/start", "thread/realtime/start"} {
		t.Run(method, func(t *testing.T) {
			var activity nativeActivity
			activity.clientMessage([]byte(fmt.Sprintf(`{"id":1,"method":%q,"params":{"threadId":"thread"}}`, method)))
			assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
			activity.serverMessage([]byte(`{"id":1,"result":{}}`))
			assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
		})
	}
}

func assertNativeIdle(t *testing.T, activity *nativeActivity) {
	t.Helper()
	if err := activity.idleError(); err != nil || !activity.idle() {
		t.Fatalf("activity should be idle: %v", err)
	}
}

func assertNativeBusy(t *testing.T, activity *nativeActivity, reason string) {
	t.Helper()
	err := activity.idleError()
	if err == nil || err.Error() != reason || activity.idle() {
		t.Fatalf("activity refusal = %v; want %s", err, reason)
	}
}

func nativeTurnMessage(method, thread, turn string) []byte {
	return []byte(fmt.Sprintf(`{"method":%q,"params":{"threadId":%q,"turn":{"id":%q}}}`, method, thread, turn))
}
