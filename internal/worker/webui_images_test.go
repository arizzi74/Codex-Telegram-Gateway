package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

func webUITestImage(t *testing.T) ([]byte, string) {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes(), "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
}
func webUIImagePrompt(t *testing.T, method string, input []map[string]string) []byte {
	t.Helper()
	params := map[string]any{"input": input}
	if method == "turn/steer" {
		params["expectedTurnId"] = "active"
	}
	data, err := json.Marshal(map[string]any{"id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWebUIImageInputsRemainScopedAndPreserveBytes(t *testing.T) {
	_, url := webUITestImage(t)
	for _, method := range []string{"turn/start", "turn/steer"} {
		for _, input := range [][]map[string]string{{{"type": "image", "url": url}}, {{"type": "text", "text": "Caption"}, {"type": "image", "url": url}}} {
			r := testWebUIRelay()
			raw := webUIImagePrompt(t, method, input)
			if _, err := r.clientMessage(raw); err == nil {
				t.Fatal("image admitted before validated resume")
			}
			r.resumed = true
			forward, err := r.clientMessage(raw)
			if err != nil || !strings.Contains(string(forward), url) || !strings.Contains(string(forward), `"threadId":"thread"`) {
				t.Fatalf("image bytes or target changed: %s %v", forward, err)
			}
		}
	}
	for _, input := range [][]map[string]string{
		{{"type": "image", "url": "https://example.invalid/picture"}}, {{"type": "localImage", "path": "/private/picture"}}, {{"type": "image", "url": url, "path": "/private"}},
		{{"type": "image", "url": url}, {"type": "image", "url": url}}, {{"type": "image", "url": "data:image/png;base64,aW52YWxpZA=="}},
		{{"type": "text", "text": strings.Repeat("x", protocol.MaxWebUITextBytes/2+1)}, {"type": "text", "text": strings.Repeat("y", protocol.MaxWebUITextBytes/2+1)}},
	} {
		r := testWebUIRelay()
		r.resumed = true
		if _, err := r.clientMessage(webUIImagePrompt(t, "turn/start", input)); err == nil {
			t.Fatal("unsafe image/prompt accepted")
		}
	}
}

func TestWebUIImageEchoAndHistoryUsePlaceholders(t *testing.T) {
	_, url := webUITestImage(t)
	redactor, err := newWorkerRedactor([]string{"private-caption", "iVBOR"})
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{"type": "userMessage", "id": "prompt", "content": []map[string]string{{"type": "text", "text": "private-caption "}, {"type": "image", "url": url}, {"type": "localImage", "path": "/private/image.png"}}}
	for _, payload := range []any{map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread", "item": item}}, map[string]any{"data": []any{map[string]any{"turnId": "turn", "item": item}}}} {
		raw, _ := json.Marshal(payload)
		visible, err := redactWebUIJSON(redactor, raw)
		if err != nil || strings.Contains(string(visible), "base64") || strings.Contains(string(visible), "/private/image") || strings.Contains(string(visible), "private-caption") || strings.Count(string(visible), "[Image]") != 2 {
			t.Fatalf("unsafe echoed image: %s %v", visible, err)
		}
	}
}

func TestWebUIInputByteBudgetIncludesInFlightImages(t *testing.T) {
	pool := newWebUIRelays(nil, context.Background(), nil)
	clients := make([]*webUIRelay, 5)
	for i := range clients {
		client := &webUIRelay{id: string(rune('a' + i)), pool: pool, ctx: context.Background()}
		clients[i] = client
		pool.clients[client.id] = client
	}
	if !clients[0].reserveInput(15<<20) || clients[0].reserveInput(6<<20) {
		t.Fatal("per-connection image budget ineffective")
	}
	for i := 1; i < 4; i++ {
		if !clients[i].reserveInput(15 << 20) {
			t.Fatal("valid global image budget rejected")
		}
	}
	if clients[4].reserveInput(5 << 20) {
		t.Fatal("global input byte ceiling ineffective")
	}
	clients[0].releaseInput(15 << 20)
	if !clients[4].reserveInput(5 << 20) {
		t.Fatal("consumed input capacity not released")
	}
	for _, client := range clients {
		client.releaseInput(20 << 20)
	}
	if pool.inputBytes != 0 {
		t.Fatal("input byte accounting leaked")
	}
}
