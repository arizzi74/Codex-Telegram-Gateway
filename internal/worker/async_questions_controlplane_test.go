package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

// Exercise the actual native event -> durable outbox -> gateway sender ->
// Telegram callback/reply -> frozen worker command path. Beta's pending async
// input is independent of both the selected Alpha session and Beta's turn.
func TestControlPlaneAsyncQuestionsAcrossSessions(t *testing.T) {
	for _, idle := range []bool{false, true} {
		name := "active_callback_steers"
		if idle {
			name = "completed_turn_text_starts"
		}
		t.Run(name, func(t *testing.T) {
			e := newSessionModeControl(t)
			a, b := e.sessions["thread-alpha"], e.sessions["thread-beta"]
			selectControlSession(t, e.gw, e.telegram, 100, e.runtime, 2)
			e.prompt(t, 102, "Start Beta work")
			var turn string
			waitControl(t, func() bool {
				turn = currentActive(t, e.local, e.runtime, b.ThreadID)
				return turn != ""
			})
			selectControlSession(t, e.gw, e.telegram, 103, e.runtime, 1)
			e.assertBinding(t, a.ThreadID)
			const title = "Which platform should Beta support?"
			if err := e.codex.Emit("item/completed", map[string]any{
				"threadId": b.ThreadID, "turnId": turn,
				"item": map[string]any{"id": "async-beta", "type": "agentMessage", "status": "completed", "phase": "final_answer", "delivery": "async", "text": "Beta needs platform details", "questions": []map[string]any{{"title": title, "options": []string{"Linux", "Mac"}}}},
			}); err != nil {
				t.Fatal(err)
			}
			waitControl(t, func() bool { return e.telegram.hasText("❓", b.Name, title) })
			original := e.telegram.latestMessageID(title)
			var progress int
			if err := e.probe.QueryRowContext(e.ctx, `SELECT count(*) FROM events WHERE session_id=? AND kind='agent_progress_message'`, b.ID).Scan(&progress); err != nil || progress != 0 {
				t.Fatalf("async request became temporary progress: %d %v", progress, err)
			}
			if idle {
				if err := e.codex.Emit("turn/completed", map[string]any{"threadId": b.ThreadID, "turnId": turn, "status": "completed"}); err != nil {
					t.Fatal(err)
				}
				waitControl(t, func() bool {
					var active string
					err := e.probe.QueryRowContext(e.ctx, `SELECT COALESCE(active_turn_id,'') FROM sessions WHERE session_id=?`, b.ID).Scan(&active)
					return err == nil && active == "" && currentActive(t, e.local, e.runtime, b.ThreadID) == ""
				})
			}
			e.prompt(t, 105, "/tgquestions")
			var open string
			waitControl(t, func() bool {
				open = wizardButton(e.telegram, "Pending questions and approvals", "Open 1 · Question")
				return open != "" && e.telegram.hasText("Pending questions and approvals", b.Name)
			})
			postCallback(t, e.gw.server.Client(), e.gw.server.URL, "secret", 106, open)
			waitControl(t, func() bool { return e.telegram.countText(title) == 2 })
			fresh := e.telegram.latestMessageID(title)
			if fresh == original || e.telegram.wasDeleted(original) {
				t.Fatalf("pending question was temporary or failed to replay: original=%d fresh=%d", original, fresh)
			}
			e.assertBinding(t, a.ThreadID)
			if idle {
				postTelegram(t, e.gw.server.Client(), e.gw.server.URL, "secret", 107, "Linux", fresh)
			} else {
				answer := wizardButton(e.telegram, title, "Linux")
				if answer == "" {
					t.Fatal("replayed question omitted answer options")
				}
				postCallback(t, e.gw.server.Client(), e.gw.server.URL, "secret", 107, answer)
			}
			method := "turn/steer"
			if idle {
				method = "turn/start"
			}
			waitControl(t, func() bool {
				for _, call := range e.codex.Calls() {
					if call.Method != method {
						continue
					}
					var params struct {
						ThreadID       string `json:"threadId"`
						ExpectedTurnID string `json:"expectedTurnId"`
						Input          []struct {
							Type, Text string
						} `json:"input"`
					}
					if json.Unmarshal(call.Params, &params) == nil && params.ThreadID == b.ThreadID && len(params.Input) == 1 && params.Input[0].Type == "text" && params.Input[0].Text == "> "+title+"\n\nLinux" && (idle || params.ExpectedTurnID == turn) {
						return true
					}
				}
				return false
			})
			waitControl(t, func() bool {
				var state string
				err := e.probe.QueryRowContext(e.ctx, `SELECT state FROM approvals WHERE session_id=? AND codex_request_id='async:async-beta'`, b.ID).Scan(&state)
				return err == nil && state == "cleared"
			})
			var raw []byte
			if err := e.probe.QueryRowContext(e.ctx, `SELECT payload FROM commands WHERE operation='input_response' AND session_id=?`, b.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var command protocol.Command
			if err := json.Unmarshal(raw, &command); err != nil || command.SessionID != b.ID || command.ThreadID != b.ThreadID || command.ExpectedTurnID != "" {
				t.Fatalf("wrong frozen async response route: %#v %v", command, err)
			}
			e.assertBinding(t, a.ThreadID)
			e.prompt(t, 108, "/tgquestions")
			waitControl(t, func() bool { return e.telegram.hasText("No pending requests") })
			for _, response := range e.codex.Responses() {
				if strings.Contains(string(response.Result), "Linux") {
					t.Fatal("async answer incorrectly sent as JSON-RPC request response")
				}
			}
		})
	}
}
