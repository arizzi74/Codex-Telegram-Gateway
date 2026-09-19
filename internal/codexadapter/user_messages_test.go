package codexadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompletedUserMessageProjectsTextAndAttachmentPlaceholders(t *testing.T) {
	raw := json.RawMessage(`{"threadId":"thread","turnId":"turn","text":"private top-level field","item":{"id":"input","type":"userMessage","content":[{"type":"text","text":"Review this "},{"type":"localImage","path":"/private/image.png"},{"type":"mention","path":"/private/secret"}]}}`)
	event := newEvent("item/completed", raw)
	if event.Kind != "user_message_completed" || event.Text != "Review this [Image][Attachment]" || event.ItemID != "input" || strings.Contains(event.Text, "private") {
		t.Fatalf("unsafe projection: %#v", event)
	}
	if event := newEvent("item/started", raw); event.Kind == "user_message_completed" {
		t.Fatal("one input echoed before completion")
	}
}
