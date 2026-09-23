package codexadapter

import (
	"encoding/json"
	"testing"
)

func TestThreadSettingsNotificationProjectsOnlyCurrentPreferences(t *testing.T) {
	event := newEvent("thread/settings/updated", json.RawMessage(`{"threadId":"thread-a","threadSettings":{"model":"gpt-test","effort":"high","cwd":"/private","approvalPolicy":"never","private":"hidden"}}`))
	if event.Kind != "thread_settings_updated" || event.ThreadID != "thread-a" || event.Settings == nil || event.Settings.Model != "gpt-test" || event.Settings.ReasoningEffort != "high" {
		t.Fatalf("settings event: %+v", event)
	}
	for _, effort := range []string{`null`, `""`} {
		event = newEvent("thread/settings/updated", json.RawMessage(`{"threadId":"thread-a","threadSettings":{"model":"gpt-test","effort":`+effort+`}}`))
		if event.Settings == nil || event.Settings.ReasoningEffort != "" {
			t.Fatalf("default effort must remain an authoritative empty value: %+v", event.Settings)
		}
	}
	for _, raw := range []string{
		`{"model":"unrelated","effort":"high"}`,
		`{"threadSettings":null}`,
		`{"threadSettings":{"model":"","effort":"high"}}`,
		`{"threadSettings":{"model":"not a model","effort":"high"}}`,
		`{"threadSettings":{"model":"gpt-test","effort":7}}`,
		`{"threadSettings":{"model":"gpt-test","effort":"high\nprivate"}}`,
	} {
		if got := newEvent("thread/settings/updated", json.RawMessage(raw)); got.Settings != nil {
			t.Fatalf("malformed settings projected: %s", raw)
		}
	}
	if got := newEvent("turn/started", json.RawMessage(`{"threadSettings":{"model":"stale-model","effort":"low"}}`)); got.Settings != nil {
		t.Fatal("historical turn fields became current settings")
	}
}
