package worker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestWebUIItemPagesCountItemsAndRemainSessionScoped(t *testing.T) {
	r := testWebUIRelay()
	request := []byte(`{"id":1,"method":"thread/items/list","params":{}}`)
	if _, err := r.clientMessage(request); err == nil {
		t.Fatal("items read before validated session resume")
	}
	r.resumed = true
	for _, params := range []string{`{"threadId":"other"}`, `{"limit":21}`, `{"limit":0}`, `{"limit":null}`, `{"turnId":"other-turn"}`, `{"itemsView":"full"}`, `{"path":"/private"}`, `{"cursor":7}`} {
		if _, err := r.clientMessage([]byte(`{"id":1,"method":"thread/items/list","params":` + params + `}`)); err == nil {
			t.Fatalf("unsafe item page accepted: %s", params)
		}
	}
	forward, err := r.clientMessage(request)
	if err != nil || !strings.Contains(string(forward), `"threadId":"thread"`) || !strings.Contains(string(forward), `"limit":20`) || !strings.Contains(string(forward), `"sortDirection":"desc"`) {
		t.Fatalf("item defaults not bounded/scoped: %s %v", forward, err)
	}
	items := make([]map[string]any, 20)
	for i := range items {
		items[i] = map[string]any{"turnId": "same-enormous-turn", "item": map[string]any{"id": fmt.Sprintf("message-%d", i), "type": "agentMessage", "text": "message", "createdAt": 1800000000 + i}}
	}
	response, _ := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"data": items, "nextCursor": "older-items", "backwardsCursor": "reverse", "unexpected": "do not forward"}})
	result, err := r.serverMessage(response)
	if err != nil || strings.Contains(string(result), "unexpected") || !strings.Contains(string(result), `"nextCursor":"older-items"`) || !strings.Contains(string(result), `"createdAt":1800000019`) {
		t.Fatalf("item page lost cursor/time or kept extra fields: %s %v", result, err)
	}
}

func TestWebUIItemPagesRejectInvalidNativePages(t *testing.T) {
	for _, response := range []string{
		`{}`, `{"data":null}`, `{"data":[{"turnId":"turn","item":null}]}`,
		`{"data":[{"turnId":"","item":{"id":"x","type":"userMessage"}}]}`,
		`{"data":[{"turnId":"turn","item":{"id":"x","type":"userMessage"}},{"turnId":"turn","item":{"id":"x","type":"agentMessage"}}]}`,
		`{"data":[],"nextCursor":"` + strings.Repeat("x", 4097) + `"}`,
	} {
		if _, err := webUIItemPage([]byte(response)); err == nil {
			t.Fatalf("invalid native item page accepted: %.200s", response)
		}
	}
	items := make([]map[string]any, 21)
	for i := range items {
		items[i] = map[string]any{"turnId": "turn", "item": map[string]string{"id": fmt.Sprint(i), "type": "agentMessage"}}
	}
	oversized, _ := json.Marshal(map[string]any{"data": items})
	if _, err := webUIItemPage(oversized); err == nil {
		t.Fatal("oversized native item page accepted")
	}
	if _, err := webUIItemPage([]byte(`{"data":[],"nextCursor":null}`)); err != nil {
		t.Fatal(err)
	}
}

func TestWebUIReasoningOnlyExposesCompletedPublicSummary(t *testing.T) {
	redactor, err := newWorkerRedactor([]string{`private-value`})
	if err != nil {
		t.Fatal(err)
	}
	for _, envelope := range []string{
		`{"method":"item/completed","params":{"threadId":"thread","item":%s}}`,
		`{"id":1,"result":{"data":[{"turnId":"turn","item":%s}]}}`,
		`{"id":1,"result":{"data":[{"id":"turn","items":[%s]}]}}`,
	} {
		item := `{"type":"reasoning","id":"summary","summary":["Public private-value summary"],"content":["RAW_REASONING"],"text":"RAW_REASONING","encryptedContent":"RAW_REASONING","futurePrivateField":"RAW_REASONING"}`
		data, err := redactWebUIJSON(redactor, []byte(fmt.Sprintf(envelope, item)))
		if err != nil || strings.Contains(string(data), "RAW_REASONING") || strings.Contains(string(data), "private-value") || !strings.Contains(string(data), "Public [REDACTED] summary") {
			t.Fatalf("summary projection failed: %s %v", data, err)
		}
	}
	r := testWebUIRelay()
	r.resumed = true
	for _, method := range []string{"item/reasoning/summaryTextDelta", "item/reasoning/textDelta"} {
		for _, part := range []string{"private-", "value"} {
			data, err := r.serverMessage([]byte(`{"method":"` + method + `","params":{"threadId":"thread","itemId":"summary","summaryIndex":0,"delta":"` + part + `"}}`))
			if err != nil || len(data) != 0 {
				t.Fatalf("partial reasoning delta escaped: %s %v", data, err)
			}
		}
	}
}
