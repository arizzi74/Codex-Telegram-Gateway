package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

// The native proxy and the worker's management connection observe the same
// runtime independently. Keep both real transports in recovery tests so a
// settled proxy alone cannot stand in for live runtime verification.
func newUpdateRecoveryTest(t *testing.T) (*Agent, *codextest.Server, *attachmentProxy, *websocket.Conn, *websocket.Conn, <-chan nativeProxyRequest) {
	t.Helper()
	a, runtime, server, cleanup := testAgent(t)
	t.Cleanup(cleanup)
	proxy, path, requests := newNativeProxyTest(t)
	a.manager.mu.Lock()
	a.manager.attachments[runtime.ID] = proxy
	a.manager.runtimes[runtime.ID].runtime.LocalSocket = path
	a.manager.mu.Unlock()
	client := dialNativeProxy(t, path)
	nativeWrite(t, client, `{"id":1,"method":"config/read","params":{}}`)
	request := nextNativeRequest(t, requests)
	nativeWrite(t, request.connection, `{"id":1,"result":{}}`)
	nativeRecoveryRead(t, proxy, client)
	waitFor(t, func() bool { return proxy.checkIdle() == nil })
	return a, server, proxy, client, request.connection, requests
}

// Receiving a frame at the frontend can precede the proxy finishing its write
// bookkeeping. Wait for that transport boundary without treating the activity
// (which may intentionally remain busy) as idle.
func nativeRecoveryRead(t *testing.T, proxy *attachmentProxy, client *websocket.Conn) {
	t.Helper()
	_ = nativeRead(t, client)
	waitFor(t, func() bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		for session := range proxy.sessions {
			if session.forwarding != 0 {
				return false
			}
		}
		return true
	})
}

func nativeRecoveryGap(t *testing.T, proxy *attachmentProxy, client, backend *websocket.Conn) {
	t.Helper()
	nativeWrite(t, backend, `{"method":"thread/status/changed","params":{"threadId":"native-thread","status":{"type":"systemError"}}}`)
	nativeRecoveryRead(t, proxy, client)
	waitFor(t, func() bool { return nativeRecoveryHasGap(proxy) })
}

func nativeRecoveryHasGap(proxy *attachmentProxy) bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.observationGap != "" {
		return true
	}
	for session := range proxy.sessions {
		if session.activity.observationGap != "" {
			return true
		}
	}
	return false
}

func prepareRecoveredUpdate(t *testing.T, a *Agent, proxy *attachmentProxy) {
	t.Helper()
	lease, err := a.prepareUpdate(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("fresh idle verification did not recover update: %v", err)
	}
	if nativeRecoveryHasGap(proxy) {
		t.Error("successful verification retained observation gap")
	}
	if err := a.abortUpdate(lease.Token); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRecoveryRechecksRuntimeForRepeatedObservationGaps(t *testing.T) {
	a, server, proxy, client, backend, _ := newUpdateRecoveryTest(t)
	server.SetThreads([]map[string]any{{"id": "native-thread", "status": "idle"}}, []string{"native-thread"})
	for range 2 {
		nativeRecoveryGap(t, proxy, client, backend)
		if err := proxy.checkIdle(); err == nil {
			t.Fatal("observation gap was cleared without live verification")
		}
		before := countCall(server.Calls(), "thread/loaded/list")
		prepareRecoveredUpdate(t, a, proxy)
		if countCall(server.Calls(), "thread/loaded/list") <= before {
			t.Fatal("recovery reused a previous runtime snapshot")
		}
	}
}

func TestUpdateRecoveryFailedRuntimeCheckRetainsGapAndReopensAdmission(t *testing.T) {
	for _, failure := range []string{"active_thread", "rpc_failure"} {
		t.Run(failure, func(t *testing.T) {
			a, server, proxy, client, backend, requests := newUpdateRecoveryTest(t)
			nativeRecoveryGap(t, proxy, client, backend)
			if failure == "active_thread" {
				server.SetThreads([]map[string]any{{"id": "native-thread", "status": "active", "turns": []map[string]any{{"id": "running", "status": "inProgress"}}}}, []string{"native-thread"})
			} else {
				server.SetRPCError("thread/loaded/list", -32000, "runtime unavailable")
			}
			if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
				_ = a.abortUpdate(lease.Token)
				t.Fatal("failed live runtime verification allowed update")
			}
			if !nativeRecoveryHasGap(proxy) {
				t.Fatal("failed live runtime verification erased observation gap")
			}
			proxy.mu.Lock()
			paused := proxy.paused
			proxy.mu.Unlock()
			if paused {
				t.Fatal("failed preparation left native command admission fenced")
			}
			nativeWrite(t, client, `{"id":2,"method":"config/read"}`)
			request := nextNativeRequest(t, requests)
			nativeWrite(t, request.connection, `{"id":2,"result":{}}`)
			nativeRecoveryRead(t, proxy, client)
			server.SetRPCError("thread/loaded/list", 0, "")
			server.SetThreads([]map[string]any{{"id": "native-thread", "status": "idle"}}, []string{"native-thread"})
			prepareRecoveredUpdate(t, a, proxy)
		})
	}
}

func TestUpdateRecoveryDoesNotBypassNativeWork(t *testing.T) {
	cases := []struct {
		name, request, accepted, settle, blocker string
	}{
		{"rpc", `{"id":2,"method":"config/read"}`, "", `{"id":2,"result":{}}`, "in flight"},
		{"turn", "", `{"method":"turn/started","params":{"threadId":"native-thread","turn":{"id":"turn"}}}`, `{"method":"turn/completed","params":{"threadId":"native-thread","turn":{"id":"turn"}}}`, "native CLI thread"},
		{"approval", "", `{"id":"approval","method":"item/tool/requestUserInput","params":{"threadId":"native-thread"}}`, `{"method":"serverRequest/resolved","params":{"threadId":"native-thread","requestId":"approval"}}`, "approvals or input"},
		{"process", `{"id":2,"method":"process/spawn","params":{"processHandle":"process"}}`, `{"id":2,"result":{}}`, `{"method":"process/exited","params":{"processHandle":"process","exitCode":0}}`, "native CLI thread"},
		{"hook", "", `{"method":"hook/started","params":{"threadId":"native-thread","run":{"id":"hook"}}}`, `{"method":"hook/completed","params":{"threadId":"native-thread","run":{"id":"hook"}}}`, "native CLI thread"},
		{"compaction", `{"id":2,"method":"thread/compact/start","params":{"threadId":"native-thread"}}`, `{"id":2,"result":{}}`, `{"method":"thread/compacted","params":{"threadId":"native-thread","turnId":"compact"}}`, "native CLI thread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, proxy, client, backend, requests := newUpdateRecoveryTest(t)
			nativeRecoveryGap(t, proxy, client, backend)
			if tc.request != "" {
				nativeWrite(t, client, tc.request)
				_ = nextNativeRequest(t, requests)
			}
			if tc.accepted != "" {
				nativeWrite(t, backend, tc.accepted)
				nativeRecoveryRead(t, proxy, client)
			}
			if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
				_ = a.abortUpdate(lease.Token)
				t.Fatal("recovery bypassed pending native work")
			} else if !strings.Contains(err.Error(), tc.blocker) {
				t.Fatalf("unexpected refusal: %v; want %s", err, tc.blocker)
			}
			if !nativeRecoveryHasGap(proxy) {
				t.Fatal("pending native work incorrectly cleared observation gap")
			}
			nativeWrite(t, backend, tc.settle)
			nativeRecoveryRead(t, proxy, client)
			prepareRecoveredUpdate(t, a, proxy)
		})
	}
}

func TestUpdateRecoveryDistinguishesSettledDisconnectFromUnconfirmedRPC(t *testing.T) {
	for _, state := range []string{"settled", "unconfirmed_read", "unconfirmed_turn"} {
		t.Run(state, func(t *testing.T) {
			a, _, proxy, client, backend, requests := newUpdateRecoveryTest(t)
			if state == "unconfirmed_read" {
				nativeWrite(t, client, `{"id":2,"method":"config/read"}`)
				_ = nextNativeRequest(t, requests)
			} else if state == "unconfirmed_turn" {
				nativeWrite(t, client, `{"id":2,"method":"turn/start","params":{"threadId":"native-thread"}}`)
				_ = nextNativeRequest(t, requests)
			}
			_ = backend.CloseNow()
			waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.sessions) == 0 })
			if state == "unconfirmed_turn" {
				if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
					_ = a.abortUpdate(lease.Token)
					t.Fatal("idle runtime snapshot erased unconfirmed accepted RPC")
				} else if !strings.Contains(err.Error(), "could not be verified") {
					t.Fatalf("unconfirmed RPC refusal = %v", err)
				}
			} else {
				if !nativeRecoveryHasGap(proxy) {
					t.Fatal("abnormal backend disconnect did not require fresh verification")
				}
				prepareRecoveredUpdate(t, a, proxy)
			}
		})
	}
}

func TestUpdateRecoveryRejectsEventsRacingLiveVerification(t *testing.T) {
	for _, event := range []string{
		`{"method":"thread/status/changed","params":{"threadId":"native-thread","status":{"type":"systemError"}}}`,
		`{"method":"thread/name/updated","params":{"threadId":"native-thread","name":"renamed"}}`,
	} {
		t.Run(event, func(t *testing.T) {
			a, server, proxy, client, backend, _ := newUpdateRecoveryTest(t)
			nativeRecoveryGap(t, proxy, client, backend)
			before := countCall(server.Calls(), "thread/loaded/list")
			server.SetResponseDelay("thread/loaded/list", 150*time.Millisecond)
			type result struct {
				lease UpdateLease
				err   error
			}
			done := make(chan result, 1)
			go func() { lease, err := a.prepareUpdate(context.Background(), time.Minute); done <- result{lease, err} }()
			waitFor(t, func() bool { return countCall(server.Calls(), "thread/loaded/list") > before })
			nativeWrite(t, backend, event)
			nativeRecoveryRead(t, proxy, client)
			got := <-done
			if got.err == nil {
				_ = a.abortUpdate(got.lease.Token)
				t.Fatal("server event crossing live verification did not invalidate evidence")
			} else if !strings.Contains(got.err.Error(), "changed during idle verification") {
				t.Fatalf("racing event refusal = %v", got.err)
			}
			if !nativeRecoveryHasGap(proxy) {
				t.Fatal("racing verification erased observation gap")
			}
			server.SetResponseDelay("thread/loaded/list", 0)
			prepareRecoveredUpdate(t, a, proxy)
		})
	}
}

func TestUpdateRecoveryClosesDetachedSettledSessions(t *testing.T) {
	a, _, proxy, client, backend, _ := newUpdateRecoveryTest(t)
	nativeRecoveryGap(t, proxy, client, backend)
	_ = client.CloseNow()
	waitFor(t, func() bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		for session := range proxy.sessions {
			if session.frontGone {
				return true
			}
		}
		return false
	})
	prepareRecoveredUpdate(t, a, proxy)
	waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.sessions) == 0 })
	if err := proxy.checkIdle(); err != nil {
		t.Fatalf("cleaning a recovered detached session created a new blocker: %v", err)
	}
}

func TestUpdateRecoveryVerifiesQueuedNativeSubmissions(t *testing.T) {
	for _, state := range []string{"queued", "unavailable", "disconnected_with_queue", "lost_pending_queue_read", "consumed_into_active_turn"} {
		t.Run(state, func(t *testing.T) {
			a, server, proxy, client, backend, requests := newUpdateRecoveryTest(t)
			server.SetThreads([]map[string]any{{"id": "queued-thread", "status": "idle"}}, nil)
			// Queued native work can exist without an active or loaded thread.
			// Empty loaded-thread discovery is not evidence that this queue is safe.
			if state == "lost_pending_queue_read" {
				nativeWrite(t, client, `{"id":2,"method":"thread/queue/list","params":{"threadId":"queued-thread"}}`)
				_ = nextNativeRequest(t, requests)
			} else {
				nativeWrite(t, backend, `{"method":"thread/queue/changed","params":{"threadId":"queued-thread"}}`)
				nativeRecoveryRead(t, proxy, client)
				waitFor(t, func() bool { return nativeRecoveryHasGap(proxy) })
			}
			if state == "unavailable" {
				server.SetMethodUnavailable("thread/queue/list", true)
			} else if state == "consumed_into_active_turn" {
				server.SetThreads([]map[string]any{{"id": "queued-thread", "status": "active", "turns": []map[string]any{{"id": "queued-turn", "status": "inProgress"}}}}, nil)
				if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{}, "nextCursor": nil}); err != nil {
					t.Fatal(err)
				}
			} else if err := server.SetMethodResult("thread/queue/list", map[string]any{
				"data":       []map[string]any{{"id": "submission", "clientUserMessageId": "message", "input": []any{}}},
				"nextCursor": nil,
			}); err != nil {
				t.Fatal(err)
			}
			if state == "disconnected_with_queue" || state == "lost_pending_queue_read" {
				_ = backend.CloseNow()
				waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.sessions) == 0 })
			}
			if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
				_ = a.abortUpdate(lease.Token)
				t.Fatal("idle loaded-thread list bypassed unverified native queue")
			}
			if !hasCall(server.Calls(), "thread/queue/list") {
				t.Fatal("update refusal did not inspect native queue")
			}
			if !nativeRecoveryHasGap(proxy) {
				t.Fatal("unverified native queue lost its recovery marker")
			}
			server.SetMethodUnavailable("thread/queue/list", false)
			server.SetThreads([]map[string]any{{"id": "queued-thread", "status": "idle"}}, nil)
			if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{}, "nextCursor": nil}); err != nil {
				t.Fatal(err)
			}
			prepareRecoveredUpdate(t, a, proxy)
		})
	}
}

func TestUpdateRecoveryForgetsQueueOnlyAfterConfirmedNativeDeletion(t *testing.T) {
	for _, state := range []string{"notification", "successful_reply", "failed_reply", "late_queue_notification"} {
		t.Run(state, func(t *testing.T) {
			a, server, proxy, client, backend, requests := newUpdateRecoveryTest(t)
			nativeWrite(t, backend, `{"method":"thread/queue/changed","params":{"threadId":"deleted-thread"}}`)
			nativeRecoveryRead(t, proxy, client)
			server.SetRPCError("thread/queue/list", -32600, "thread does not exist")
			server.SetRPCError("thread/read", -32600, "thread does not exist")
			if state == "successful_reply" || state == "failed_reply" {
				nativeWrite(t, client, `{"id":2,"method":"thread/delete","params":{"threadId":"deleted-thread"}}`)
				_ = nextNativeRequest(t, requests)
				if state == "failed_reply" {
					nativeWrite(t, backend, `{"id":2,"error":{"code":-32600,"message":"deletion refused"}}`)
				} else {
					nativeWrite(t, backend, `{"id":2,"result":{}}`)
				}
			} else {
				nativeWrite(t, backend, `{"method":"thread/deleted","params":{"threadId":"deleted-thread"}}`)
			}
			nativeRecoveryRead(t, proxy, client)
			if state == "late_queue_notification" {
				nativeWrite(t, backend, `{"method":"thread/queue/changed","params":{"threadId":"deleted-thread"}}`)
				nativeRecoveryRead(t, proxy, client)
			}
			if state == "failed_reply" {
				if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
					_ = a.abortUpdate(lease.Token)
					t.Fatal("failed deletion removed the queue safety requirement")
				}
				if !hasCall(server.Calls(), "thread/queue/list") {
					t.Fatal("failed deletion suppressed queue verification")
				}
				return
			}
			prepareRecoveredUpdate(t, a, proxy)
			if hasCall(server.Calls(), "thread/queue/list") || hasCall(server.Calls(), "thread/read") {
				t.Fatal("confirmed deletion attempted to verify nonexistent thread")
			}
		})
	}
}

func TestUpdateRecoveryManagementDeletionCleansDisconnectedQueueWithMatchingGeneration(t *testing.T) {
	a, server, proxy, client, backend, _ := newUpdateRecoveryTest(t)
	nativeWrite(t, backend, `{"method":"thread/queue/changed","params":{"threadId":"deleted-thread"}}`)
	nativeRecoveryRead(t, proxy, client)
	_ = backend.CloseNow()
	waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.sessions) == 0 })
	server.SetRPCError("thread/queue/list", -32600, "thread does not exist")
	server.SetRPCError("thread/read", -32600, "thread does not exist")
	var runtime protocol.Runtime
	a.manager.mu.RLock()
	for _, managed := range a.manager.runtimes {
		runtime = managed.runtime
	}
	a.manager.mu.RUnlock()
	stale := runtime
	stale.Generation--
	event := codexadapter.Event{Kind: "thread_deleted", ThreadID: "deleted-thread"}
	a.onEvent(stale, event)
	if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("stale runtime generation erased live queue obligation")
	}
	before := countCall(server.Calls(), "thread/queue/list")
	if before == 0 {
		t.Fatal("stale deletion prevented queue verification")
	}
	a.onEvent(runtime, event)
	prepareRecoveredUpdate(t, a, proxy)
	if countCall(server.Calls(), "thread/queue/list") != before {
		t.Fatal("confirmed management deletion retained disconnected queue obligation")
	}
}

func TestUpdateRecoveryQueueDeletionDoesNotEraseOtherNativeWork(t *testing.T) {
	for _, tc := range []struct {
		name, start, finish, blocker string
	}{
		{"turn", `{"method":"turn/started","params":{"threadId":"deleted-thread","turn":{"id":"turn"}}}`, `{"method":"turn/completed","params":{"threadId":"deleted-thread","turn":{"id":"turn"}}}`, "native CLI thread"},
		{"approval", `{"id":"approval","method":"item/tool/requestUserInput","params":{"threadId":"deleted-thread"}}`, `{"method":"serverRequest/resolved","params":{"threadId":"deleted-thread","requestId":"approval"}}`, "approvals or input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, server, proxy, client, backend, _ := newUpdateRecoveryTest(t)
			nativeWrite(t, backend, `{"method":"thread/queue/changed","params":{"threadId":"deleted-thread"}}`)
			nativeRecoveryRead(t, proxy, client)
			nativeWrite(t, backend, tc.start)
			nativeRecoveryRead(t, proxy, client)
			nativeWrite(t, backend, `{"method":"thread/deleted","params":{"threadId":"deleted-thread"}}`)
			nativeRecoveryRead(t, proxy, client)
			server.SetRPCError("thread/queue/list", -32600, "thread does not exist")
			if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
				_ = a.abortUpdate(lease.Token)
				t.Fatal("queue deletion erased separately tracked native work")
			} else if !strings.Contains(err.Error(), tc.blocker) {
				t.Fatalf("unexpected active-work refusal: %v", err)
			}
			nativeWrite(t, backend, tc.finish)
			nativeRecoveryRead(t, proxy, client)
			prepareRecoveredUpdate(t, a, proxy)
			if hasCall(server.Calls(), "thread/queue/list") {
				t.Fatal("settling native work restored deleted queue obligation")
			}
		})
	}
}
