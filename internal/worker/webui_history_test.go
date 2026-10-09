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
		items[i] = map[string]any{"turnId": "same-enormous-turn", "startedAtMs": int64(1800000000123 + i*1000), "completedAtMs": int64(1800000000456 + i*1000), "item": map[string]any{"id": fmt.Sprintf("message-%d", i), "type": "agentMessage", "text": "message"}}
	}
	response, _ := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"data": items, "nextCursor": "older-items", "backwardsCursor": "reverse", "unexpected": "do not forward"}})
	result, err := r.serverMessage(response)
	if err != nil || strings.Contains(string(result), "unexpected") || !strings.Contains(string(result), `"nextCursor":"older-items"`) || !strings.Contains(string(result), `"startedAtMs":1800000019123`) || !strings.Contains(string(result), `"completedAtMs":1800000019456`) {
		t.Fatalf("item page lost cursor/time or kept extra fields: %s %v", result, err)
	}
}

func TestWebUIItemPagesPreserveTimesThroughRedactionAndDisplayLimits(t *testing.T) {
	redactor, err := newWorkerRedactor([]string{`private-secret`})
	if err != nil {
		t.Fatal(err)
	}
	items := []map[string]any{
		{"turnId": "same-turn", "startedAtMs": int64(1800000000123), "completedAtMs": int64(1800000000999), "item": map[string]any{"id": "reply", "type": "agentMessage", "text": "reply private-secret"}},
		{"turnId": "same-turn", "startedAtMs": int64(1800000010456), "completedAtMs": int64(1800000010999), "item": map[string]any{"id": "reasoning", "type": "reasoning", "summary": []string{"public private-secret"}, "content": []string{"RAW_REASONING"}, "encryptedContent": "RAW_REASONING"}},
		{"turnId": "same-turn", "startedAtMs": int64(1800000020789), "completedAtMs": int64(1800000020999), "item": map[string]any{"id": "tool", "type": "commandExecution", "status": "completed", "aggregatedOutput": strings.Repeat("private-secret ", 30000)}},
		{"turnId": "same-turn", "turnStartedAt": int64(1800000000), "turnCompletedAt": int64(1800000030), "item": map[string]any{"id": "undated", "type": "userMessage", "content": []any{}}},
	}
	raw, err := json.Marshal(map[string]any{"data": items})
	if err != nil {
		t.Fatal(err)
	}
	result, err := webUIItemPage(raw, redactor)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), "private-secret") || strings.Contains(string(result), "RAW_REASONING") {
		t.Fatalf("private content survived page sanitization: %.200s", result)
	}
	page := webUITestPage(t, result)
	for i := 0; i < 3; i++ {
		entry := page.Data[i]
		if entry.TurnID != "same-turn" || entry.StartedAtMS == nil || *entry.StartedAtMS != items[i]["startedAtMs"] || entry.CompletedAtMS == nil || *entry.CompletedAtMS != items[i]["completedAtMs"] {
			t.Fatalf("entry %d lost native times: %#v", i, entry)
		}
	}
	if !strings.Contains(string(page.Data[1].Item), "public [REDACTED]") || !strings.Contains(string(page.Data[2].Item), "displayNotice") || len(page.Data[2].Item) > webUIHistoryItemBytes {
		t.Fatal("reasoning/oversized item projections changed")
	}
	undated := page.Data[3]
	if undated.StartedAtMS != nil || undated.CompletedAtMS != nil || undated.TurnStartedAt == nil || *undated.TurnStartedAt != 1800000000 || undated.TurnCompletedAt == nil || *undated.TurnCompletedAt != 1800000030 {
		t.Fatalf("turn seconds became item milliseconds: %#v", undated)
	}
}

func TestWebUIItemPagesOmitInvalidLifecycleTimes(t *testing.T) {
	invalid := []string{"", "null", "0", "-1", "253402300800000", "9223372036854775807", "9223372036854775808", "-9223372036854775809", "1.5", `"1800000000123"`, "true", "{}", "[]"}
	for _, value := range invalid {
		name := value
		if name == "" {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			for _, field := range []string{"startedAtMs", "completedAtMs"} {
				other := "completedAtMs"
				if field == other {
					other = "startedAtMs"
				}
				invalidField := ""
				if value != "" {
					invalidField = fmt.Sprintf(`,"%s":%s`, field, value)
				}
				raw := fmt.Sprintf(`{"data":[{"turnId":"turn","item":{"id":"item","type":"agentMessage","text":"reply"},"turnStartedAt":1800000000,"%s":1800000000123%s}]}`, other, invalidField)
				result, err := webUIItemPage([]byte(raw))
				if err != nil {
					t.Fatalf("invalid optional %s prevented loading the item: %v", field, err)
				}
				var page struct {
					Data []map[string]json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(result, &page); err != nil {
					t.Fatal(err)
				}
				entry := page.Data[0]
				if _, exists := entry[field]; exists || string(entry[other]) != "1800000000123" || string(entry["turnStartedAt"]) != "1800000000" {
					t.Fatalf("invalid time replaced or corrupted an independent date: %s", result)
				}
			}
		})
	}
	for _, value := range []int64{1, 253402300799999} {
		raw := fmt.Sprintf(`{"data":[{"turnId":"turn","item":{"id":"item","type":"agentMessage","text":"reply"},"startedAtMs":%d,"completedAtMs":%d}]}`, value, value)
		result, err := webUIItemPage([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		entry := webUITestPage(t, result).Data[0]
		if entry.StartedAtMS == nil || *entry.StartedAtMS != value || entry.CompletedAtMS == nil || *entry.CompletedAtMS != value {
			t.Fatalf("valid millisecond boundary %d was lost", value)
		}
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
