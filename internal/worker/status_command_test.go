package worker

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestStatusCommandWeeklyRemainingAndResetAcrossClients(t *testing.T) {
	weeklyReset := time.Date(2026, time.October, 13, 19, 42, 0, 0, time.UTC)
	shortReset := time.Date(2026, time.October, 9, 10, 17, 0, 0, time.UTC)
	for _, clientName := range []string{"telegram", "webui"} {
		for _, weeklySlot := range []string{"primary", "secondary"} {
			t.Run(clientName+"/weekly-"+weeklySlot, func(t *testing.T) {
				a, runtime, server, cleanup := testAgent(t)
				defer cleanup()
				session := installActiveStatusSession(t, a, runtime, server)
				before, err := a.store.ListSessions(runtime.ID)
				if err != nil {
					t.Fatal(err)
				}
				limits := map[string]any{
					"planType": "pro",
					weeklySlot: map[string]any{"usedPercent": 27, "windowDurationMins": 10080, "resetsAt": weeklyReset.Unix()},
				}
				if weeklySlot == "secondary" {
					limits["primary"] = map[string]any{"usedPercent": 11, "windowDurationMins": 300, "resetsAt": shortReset.Unix()}
				}
				if err := server.SetMethodResult("account/rateLimits/read", map[string]any{"rateLimits": limits}); err != nil {
					t.Fatal(err)
				}

				result := runStatusCommand(t, a, runtime, session, clientName)
				for _, want := range []string{"Plan: pro", "Weekly limit remaining: 73%", "Weekly resets: 2026-10-13 19:42 UTC"} {
					if !strings.Contains(result.Text, want) {
						t.Fatalf("missing %q in status:\n%s", want, result.Text)
					}
				}
				if weeklySlot == "secondary" {
					for _, want := range []string{"5-hour limit remaining: 89%", "5-hour resets: 2026-10-09 10:17 UTC"} {
						if !strings.Contains(result.Text, want) {
							t.Fatalf("missing %q in status:\n%s", want, result.Text)
						}
					}
				}
				if strings.Contains(result.Text, "limit used:") || strings.Contains(result.Text, "Weekly limit remaining: 27%") {
					t.Fatalf("status displayed usage instead of remaining allowance:\n%s", result.Text)
				}
				after, err := a.store.ListSessions(runtime.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("status changed the active session:\nbefore=%#v\nafter=%#v", before, after)
				}
				assertStatusReadOnlyRPCs(t, server)
			})
		}
	}
}

func TestStatusCommandUsesSelectedRunningRuntimeVersion(t *testing.T) {
	for _, clientName := range []string{"telegram", "webui"} {
		for _, test := range []struct {
			name      string
			userAgent string
			want      string
		}{
			{"running-handshake", "telegramgw/0.158.0 (Linux 6.8; x86_64) terminal (telegramgw; dev)", "0.158.0"},
			{"missing-handshake", "", "unavailable"},
			{"invalid-handshake", "telegramgw/dev (Linux 6.8; x86_64) terminal/8.8.8", "unavailable"},
		} {
			t.Run(clientName+"/"+test.name, func(t *testing.T) {
				a, runtime, server, cleanup := testAgent(t)
				defer cleanup()
				client := mustClient(t, a, runtime)
				if test.userAgent != "" {
					var err error
					client, server, err = codextest.New(a.ctx, codexadapter.InitializeInfo{UserAgent: test.userAgent})
					if err != nil {
						t.Fatal(err)
					}
					defer server.Close()
				}
				// Neither actor nor manager executable metadata proves which
				// version is serving this connection; only its handshake does.
				runtime.CodexVersion = "codex-cli 0.155.0"
				session := installActiveStatusSession(t, a, runtime, server)
				current := runtime
				current.CodexVersion = "codex-cli 0.157.0"
				a.manager.install(current, client)
				other := runtime
				other.ID, other.ProfileID, other.CodexVersion = "other-runtime", "other-profile", "codex-cli 9.9.9"
				a.manager.install(other, nil)

				result := runStatusCommand(t, a, runtime, session, clientName)
				var versionLine string
				for _, line := range strings.Split(result.Text, "\n") {
					if strings.HasPrefix(line, "Codex runtime: ") {
						versionLine = line
					}
				}
				if versionLine != "Codex runtime: "+test.want {
					t.Fatalf("status did not identify the selected running runtime (%s):\n%s", test.want, result.Text)
				}
				for _, stale := range []string{"0.155.0", "0.157.0", "9.9.9", "8.8.8"} {
					if strings.Contains(result.Text, stale) {
						t.Fatalf("status trusted stale or unrelated version %q:\n%s", stale, result.Text)
					}
				}
				assertStatusReadOnlyRPCs(t, server)
			})
		}
	}
}

func TestStatusCommandMissingAndUnknownWindowMetadata(t *testing.T) {
	for _, test := range []struct {
		name        string
		limits      map[string]any
		unavailable bool
		want        []string
		absent      []string
	}{
		{
			name:        "unsupported-rate-limits",
			unavailable: true,
			want:        []string{"Rate limits: unavailable (", "Model: gpt-5.4"},
			absent:      []string{"limit remaining:", "1970-01-01"},
		},
		{
			name:   "no-windows",
			limits: map[string]any{"planType": "pro"},
			want:   []string{"Rate limits: unavailable", "Weekly limit: unavailable (not reported by Codex)"},
			absent: []string{"limit remaining:", "1970-01-01"},
		},
		{
			name:   "weekly-without-reset",
			limits: map[string]any{"primary": map[string]any{"usedPercent": 27, "windowDurationMins": 10080, "resetsAt": nil}},
			want:   []string{"Weekly limit remaining: 73%", "Weekly resets: unavailable"},
			absent: []string{"1970-01-01", "Weekly limit: unavailable"},
		},
		{
			name: "unknown-durations",
			limits: map[string]any{
				"primary":   map[string]any{"usedPercent": 27, "windowDurationMins": nil, "resetsAt": nil},
				"secondary": map[string]any{"usedPercent": 54, "windowDurationMins": 0, "resetsAt": nil},
			},
			want:   []string{"Primary limit remaining: 73%", "Secondary limit remaining: 46%", "Primary resets: unavailable", "Secondary resets: unavailable", "Weekly limit: unavailable (not reported by Codex)"},
			absent: []string{"Weekly limit remaining:", "5-hour limit remaining:", "1970-01-01"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installActiveStatusSession(t, a, runtime, server)
			if test.unavailable {
				server.SetMethodUnavailable("account/rateLimits/read", true)
			} else if err := server.SetMethodResult("account/rateLimits/read", map[string]any{"rateLimits": test.limits}); err != nil {
				t.Fatal(err)
			}
			result := runStatusCommand(t, a, runtime, session, "webui")
			for _, want := range append(test.want, "Codex runtime: unavailable") {
				if !strings.Contains(result.Text, want) {
					t.Fatalf("missing %q in status:\n%s", want, result.Text)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(result.Text, absent) {
					t.Fatalf("invented metadata %q in status:\n%s", absent, result.Text)
				}
			}
			assertStatusReadOnlyRPCs(t, server)
		})
	}
}

func installActiveStatusSession(t *testing.T, a *Agent, runtime protocol.Runtime, server *codextest.Server) protocol.Session {
	t.Helper()
	session, err := a.store.UpsertSession(protocol.Session{
		RuntimeID: runtime.ID, ThreadID: "thread-status-read-only", CWD: runtime.DefaultCWD,
		State: "running", Loaded: true, ActiveTurnID: "existing-turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "active", "model": "gpt-5.4"}}, []string{session.ThreadID})
	a.onSession(runtime, session)
	return session
}

func runStatusCommand(t *testing.T, a *Agent, runtime protocol.Runtime, session protocol.Session, clientName string) protocol.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if clientName == "webui" {
		result, err := a.executeWebUICommand(ctx, runtime, session, "status", "")
		if err != nil || result.State != "completed" || result.CommandID != "" || result.TurnID != "" {
			t.Fatalf("Web UI status = %#v, %v", result, err)
		}
		return result
	}
	command := agentCommand(runtime, session, protocol.CodexCommand)
	command.Arguments = protocol.Arguments{Codex: &protocol.CodexCommandPayload{Name: "status"}}
	if ack, err := a.HandleCommand(ctx, command); err != nil || ack.Status != "accepted" {
		t.Fatalf("Telegram status ack = %#v, %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && (record.State == CommandCompleted || record.State == CommandFailed)
	})
	record, found, err := a.store.LoadCommand(command.ID)
	if err != nil || !found || record.State != CommandCompleted || record.Result == nil || record.Result.State != "completed" || record.Result.Error != nil || record.Result.TurnID != "" {
		t.Fatalf("Telegram status result = %#v, found=%v, err=%v", record, found, err)
	}
	return *record.Result
}

func assertStatusReadOnlyRPCs(t *testing.T, server *codextest.Server) {
	t.Helper()
	calls := server.Calls()
	for _, method := range []string{"thread/read", "config/read", "account/usage/read", "account/rateLimits/read"} {
		if !hasCall(calls, method) {
			t.Errorf("status omitted %s", method)
		}
	}
	for _, call := range calls {
		switch call.Method {
		case "initialize", "initialized", "config/read", "account/usage/read":
		case "thread/read":
			var params map[string]any
			if err := json.Unmarshal(call.Params, &params); err != nil || params["includeTurns"] != false {
				t.Errorf("status expanded thread history: %s (%v)", call.Params, err)
			}
		case "account/rateLimits/read":
			var params map[string]any
			if err := json.Unmarshal(call.Params, &params); err != nil || params["excludeResetCreditDetails"] != true {
				t.Errorf("status requested reset credit details: %s (%v)", call.Params, err)
			}
		default:
			t.Errorf("status issued unexpected RPC %s: %s", call.Method, call.Params)
		}
	}
}
