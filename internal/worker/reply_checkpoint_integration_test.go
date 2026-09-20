package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/iaia/telegramgw/internal/gateway"
)

type gatedQuestionRecorder struct {
	*telegramRecorder
	title   string
	sent    chan int64
	release chan struct{}
}

func (g *gatedQuestionRecorder) Send(ctx context.Context, message gateway.SendMessage) (int64, error) {
	id, err := g.telegramRecorder.Send(ctx, message)
	if err == nil && strings.Contains(message.Text, g.title) {
		g.sent <- id
		select {
		case <-g.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return id, err
}

func TestControlPlaneReplyWaitsForQuestionCheckpoint(t *testing.T) {
	const title = "A gated question from Beta"
	var gate *gatedQuestionRecorder
	e := newSessionModeControl(t, func(recorder *telegramRecorder) gateway.TelegramAPI {
		gate = &gatedQuestionRecorder{telegramRecorder: recorder, title: title, sent: make(chan int64, 1), release: make(chan struct{})}
		return gate
	})
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	a, b := e.sessions["thread-alpha"], e.sessions["thread-beta"]
	selectControlSession(t, e.gw, e.telegram, 100, e.runtime, 2)
	e.prompt(t, 102, "Start Beta")
	var turn string
	waitControl(t, func() bool { turn = currentActive(t, e.local, e.runtime, b.ThreadID); return turn != "" })
	selectControlSession(t, e.gw, e.telegram, 103, e.runtime, 1)
	if err := e.codex.Emit("item/completed", map[string]any{"threadId": b.ThreadID, "turnId": turn, "item": map[string]any{"id": "gated-question", "type": "agentMessage", "delivery": "async", "questions": []map[string]any{{"title": title}}}}); err != nil {
		t.Fatal(err)
	}
	var messageID int64
	waitControl(t, func() bool {
		select {
		case messageID = <-gate.sent:
			return true
		default:
			return false
		}
	})
	var routes int
	if err := e.probe.QueryRowContext(e.ctx, "SELECT count(*) FROM bot_message_routes WHERE bot_id='bot' AND chat_id=9 AND message_id=?", messageID).Scan(&routes); err != nil || routes != 0 {
		t.Fatalf("gate did not expose uncommitted route: %d %v", routes, err)
	}
	// Telegram can deliver the reply before sendMessage's response reaches the
	// sender. Keep that response gated through the webhook's bounded wait: it
	// must return a retryable failure without accepting or rerouting the input.
	body, _ := json.Marshal(map[string]any{"update_id": 105, "message": map[string]any{
		"message_id": 105, "from": map[string]any{"id": 7}, "chat": map[string]any{"id": 9}, "text": "Linux",
		"reply_to_message": map[string]any{"message_id": messageID, "chat": map[string]any{"id": 9}},
	}})
	req, err := http.NewRequest(http.MethodPost, e.gw.server.URL+"/tgapi/v1/telegram/webhook", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
	response, err := e.gw.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("reply before question checkpoint returned %d; want retryable 503", response.StatusCode)
	}
	var premature int
	if err := e.probe.QueryRowContext(e.ctx, `SELECT count(*) FROM commands WHERE telegram_update_id=105`).Scan(&premature); err != nil || premature != 0 {
		t.Fatalf("unroutable reply queued a command: %d %v", premature, err)
	}
	release.Do(func() { close(gate.release) })
	postTelegram(t, e.gw.server.Client(), e.gw.server.URL, "secret", 105, "Linux", messageID)
	waitControl(t, func() bool {
		var count int
		err := e.probe.QueryRowContext(e.ctx, `SELECT count(*) FROM commands WHERE operation='input_response' AND session_id=? AND telegram_update_id=105`, b.ID).Scan(&count)
		return err == nil && count == 1
	})
	var wrong int
	if err := e.probe.QueryRowContext(e.ctx, `SELECT count(*) FROM commands WHERE session_id=? AND telegram_update_id=105`, a.ID).Scan(&wrong); err != nil || wrong != 0 {
		t.Fatalf("Beta answer escaped into Alpha: %d %v", wrong, err)
	}
	e.assertBinding(t, a.ThreadID)
}
