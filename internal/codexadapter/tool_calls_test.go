package codexadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStartedToolCallsProjectOnlyCallInputs(t *testing.T) {
	for _, tc := range []struct {
		name, item, want string
	}{
		{"command", `{"type":"commandExecution","command":"go test ./...\necho '<done>'","cwd":"/private-output","aggregatedOutput":"private-output","commandActions":[{"type":"unknown","command":"private-output"}]}`, "commandExecution\ngo test ./...\necho '<done>'"},
		{"mcp", `{"type":"mcpToolCall","server":"docs","tool":"search","arguments":{"q":"test"},"result":{"text":"private-output"},"error":"private-output"}`, "docs.search\n{\n  \"q\": \"test\"\n}"},
		{"dynamic source", `{"type":"dynamicToolCall","namespace":"functions","tool":"exec","arguments":"const value = await tools.exec_command({cmd: 'ls'});\ntext(value);","contentItems":[{"text":"private-output"}]}`, "functions.exec\nconst value = await tools.exec_command({cmd: 'ls'});\ntext(value);"},
		{"dynamic structured", `{"type":"dynamicToolCall","namespace":null,"tool":"lookup","arguments":{"id":9007199254740993},"success":true}`, "lookup\n{\n  \"id\": 9007199254740993\n}"},
		{"file changes", `{"type":"fileChange","changes":[{"path":"main.go","kind":{"type":"update","move_path":"private-output"},"diff":"private-output"},{"path":"new.go","kind":{"type":"add"},"diff":"private-output"}]}`, "fileChange\nupdate main.go\nadd new.go"},
		{"web fallback", `{"type":"webSearch","query":"example query","results":[{"text":"private-output"}]}`, "webSearch\nexample query"},
		{"web queries", `{"type":"webSearch","query":"private-output","action":{"type":"search","queries":["first","second"]}}`, "webSearch\nfirst\nsecond"},
		{"web open", `{"type":"webSearch","query":"private-output","action":{"type":"openPage","url":"https://example.com"}}`, "webSearch.openPage\nhttps://example.com"},
		{"web find", `{"type":"webSearch","action":{"type":"findInPage","url":"https://example.com","pattern":"needle"}}`, "webSearch.findInPage\nhttps://example.com\nneedle"},
		{"image view", `{"type":"imageView","path":"image.png"}`, "imageView\nimage.png"},
		{"image generation", `{"type":"imageGeneration","result":"private-output","revisedPrompt":"private-output","savedPath":"private-output"}`, "imageGeneration"},
		{"collaboration", `{"type":"collabAgentToolCall","tool":"spawnAgent","prompt":"private-output","receiverThreadIds":["agent-1"],"agentsStates":{"agent-1":{"message":"private-output"}}}`, "collaboration.spawnAgent\nagent-1"},
		{"sleep", `{"type":"sleep","durationMs":1200}`, "sleep\n1200 ms"},
		{"missing arguments", `{"type":"dynamicToolCall","tool":"clock","arguments":null}`, "clock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var item map[string]any
			// Preserve integer arguments exactly while adding protocol IDs.
			decoder := json.NewDecoder(strings.NewReader(tc.item))
			decoder.UseNumber()
			if err := decoder.Decode(&item); err != nil {
				t.Fatal(err)
			}
			item["id"] = "item-1"
			payload, err := json.Marshal(map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": item, "text": "private-output"})
			if err != nil {
				t.Fatal(err)
			}
			event := newEvent("item/started", payload)
			if event.Kind != "tool_call_started" || event.ThreadID != "thread-1" || event.TurnID != "turn-1" || event.ItemID != "item-1" || event.Text != tc.want || event.Phase != "" {
				t.Fatalf("projection = %#v; want text %q", event, tc.want)
			}
			if strings.Contains(event.Text, "private-output") {
				t.Fatal("tool output entered the visible projection")
			}
			if completed := newEvent("item/completed", payload); completed.Kind != "item_completed" {
				t.Fatalf("completed tool projected a new call: %#v", completed)
			}
		})
	}
}

func TestToolCallProjectionExcludesNonToolsAndUnknownItems(t *testing.T) {
	for _, kind := range []string{"reasoning", "agentMessage", "userMessage", "plan", "functionCallOutput", "subAgentActivity", "futureTool"} {
		payload, err := json.Marshal(map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"id": "item-1", "type": kind, "text": "private", "command": "private", "arguments": "private"}})
		if err != nil {
			t.Fatal(err)
		}
		if event := newEvent("item/started", payload); event.Kind != "item_started" {
			t.Fatalf("non-tool %s projected a call: %#v", kind, event)
		}
	}
}

func TestToolArgumentsRedactCredentialFieldsAndBoundNesting(t *testing.T) {
	raw := json.RawMessage(`{"headers":{"Authorization":"private-1","Cookie":"private-2"},"api_key":"private-3","arguments":[{"client-secret":"private-4","accessToken":"private-5","password":"private-6","input":"keep"}],"code":"text(await tools.clock__curr_time({}));"}`)
	text := toolArguments(raw)
	if strings.Contains(text, "private-") || strings.Count(text, "[REDACTED]") != 6 || !strings.Contains(text, "keep") || !strings.Contains(text, "tools.clock__curr_time") {
		t.Fatalf("unexpected redacted arguments: %s", text)
	}
	deep := strings.Repeat(`{"nested":`, 30) + `"private-deep"` + strings.Repeat("}", 30)
	text = toolArguments(json.RawMessage(deep))
	if strings.Contains(text, "private-deep") || !strings.Contains(text, "[omitted]") {
		t.Fatalf("deep arguments were not omitted: %s", text)
	}
}
