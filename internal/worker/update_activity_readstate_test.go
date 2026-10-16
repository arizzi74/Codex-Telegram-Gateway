package worker

import (
	"fmt"
	"testing"
)

// Codex 0.163.0 read-state messages describe durable read/unread metadata.
// They neither start work nor settle a turn or an approval.
func TestNativeActivityReadStateNotificationsRemainIdle(t *testing.T) {
	for _, readState := range []string{
		`{"revision":"1","firstUnread":null}`,
		`{"revision":"2","firstUnread":{"type":"threadStart"}}`,
		`{"revision":"3","firstUnread":{"type":"turn","turnId":"completed-turn"}}`,
	} {
		t.Run(readState, func(t *testing.T) {
			var activity nativeActivity
			activity.serverMessage([]byte(`{"method":"thread/readState/changed","params":{"threadId":"thread","readState":` + readState + `}}`))
			assertNativeIdle(t, &activity)
			if activity.settledError() != nil || activity.disconnectedError() != nil {
				t.Fatal("read-state metadata must not latch uncertainty or background work")
			}
		})
	}
}

func TestNativeActivityReadStateUpdateWaitsForReply(t *testing.T) {
	for _, test := range []struct {
		name, operation, reply string
	}{
		{"mark read", "read", `{"id":7,"result":{"readState":{"revision":"2","firstUnread":null}}}`},
		{"mark unread", "unread", `{"id":7,"result":{"readState":{"revision":"2","firstUnread":{"type":"threadStart"}}}}`},
		{"revision conflict", "read", `{"id":7,"error":{"code":-32000,"message":"revision conflict"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var activity nativeActivity
			activity.clientMessage([]byte(fmt.Sprintf(`{"id":7,"method":"thread/readState/update","params":{"threadId":"thread","expectedRevision":"1","operation":{"type":%q}}}`, test.operation)))
			assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
			activity.serverMessage([]byte(`{"method":"thread/readState/changed","params":{"threadId":"thread","readState":{"revision":"2","firstUnread":null}}}`))
			assertNativeBusy(t, &activity, "worker update: native CLI requests are still in flight")
			// A metadata mutation may still be executing after disconnection.
			// Its notification is not a substitute for the matching RPC reply.
			if err := activity.disconnectedError(); err == nil || err.Error() != "worker update: native CLI requests are still in flight" {
				t.Fatalf("lost read-state mutation reply must block recovery; got %v", err)
			}
			activity.serverMessage([]byte(test.reply))
			assertNativeIdle(t, &activity)
		})
	}
}

func TestNativeActivityReadStateCannotSettleAcceptedWork(t *testing.T) {
	for _, test := range []struct {
		name, reason, completion string
		start                    func(*nativeActivity)
	}{
		{
			name:       "accepted turn",
			reason:     "worker update: a native CLI thread is active or its idle state cannot be verified",
			completion: string(nativeTurnMessage("turn/completed", "thread", "turn")),
			start: func(activity *nativeActivity) {
				activity.clientMessage([]byte(`{"id":1,"method":"turn/start","params":{"threadId":"thread"}}`))
				activity.serverMessage([]byte(`{"id":1,"result":{"turn":{"id":"turn","status":"inProgress"}}}`))
			},
		},
		{
			name:       "answered approval",
			reason:     "worker update: native CLI approvals or input are pending",
			completion: `{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":"approval"}}`,
			start: func(activity *nativeActivity) {
				activity.serverMessage([]byte(`{"id":"approval","method":"item/tool/requestUserInput","params":{"threadId":"thread","turnId":"turn"}}`))
				activity.clientMessage([]byte(`{"id":"approval","result":{"answers":{}}}`))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var activity nativeActivity
			test.start(&activity)
			assertNativeBusy(t, &activity, test.reason)
			activity.serverMessage([]byte(`{"method":"thread/readState/changed","params":{"threadId":"thread","readState":{"revision":"2","firstUnread":null}}}`))
			assertNativeBusy(t, &activity, test.reason)
			activity.clientMessage([]byte(`{"id":7,"method":"thread/readState/update","params":{"threadId":"thread","expectedRevision":"2","operation":{"type":"read"}}}`))
			activity.serverMessage([]byte(`{"id":7,"result":{"readState":{"revision":"3","firstUnread":null}}}`))
			assertNativeBusy(t, &activity, test.reason)
			if activity.disconnectedError() == nil {
				t.Fatal("read-state acknowledgements must not hide accepted work after disconnection")
			}
			activity.serverMessage([]byte(test.completion))
			assertNativeIdle(t, &activity)
		})
	}
}

func TestNativeActivityReadStateDoesNotPermitUnknownOperations(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		start        func(*nativeActivity)
	}{
		{
			name:   "unknown notification",
			reason: "unrecognized native server notification: thread/readState/futureChanged",
			start: func(activity *nativeActivity) {
				activity.serverMessage([]byte(`{"method":"thread/readState/futureChanged","params":{"threadId":"thread"}}`))
			},
		},
		{
			name:   "unknown request",
			reason: "unrecognized asynchronous native CLI request: thread/readState/futureUpdate",
			start: func(activity *nativeActivity) {
				activity.clientMessage([]byte(`{"id":1,"method":"thread/readState/futureUpdate","params":{"threadId":"thread"}}`))
				activity.serverMessage([]byte(`{"id":1,"result":{}}`))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var activity nativeActivity
			test.start(&activity)
			activity.serverMessage([]byte(`{"method":"thread/readState/changed","params":{"threadId":"thread","readState":{"revision":"2","firstUnread":null}}}`))
			activity.clientMessage([]byte(`{"id":7,"method":"thread/readState/update","params":{"threadId":"thread","expectedRevision":"2","operation":{"type":"read"}}}`))
			activity.serverMessage([]byte(`{"id":7,"result":{"readState":{"revision":"3","firstUnread":null}}}`))
			assertNativeBusy(t, &activity, "worker update: native CLI activity could not be verified")
			if !activity.uncertain || activity.uncertaintyReason != test.reason || activity.disconnectedError() == nil {
				t.Fatalf("read-state metadata must preserve unknown-operation uncertainty: %+v", activity)
			}
		})
	}
}
