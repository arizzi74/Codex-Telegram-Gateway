package protocol

import (
	"strings"
	"testing"
)

func TestModelMenuRejectsMalformedChoices(t *testing.T) {
	for _, args := range []string{"--menu", "--menu model-a", "--page 0", "--page 3", "--cancel", "model-a", "model-a high", "模型 高"} {
		menu := &ModelMenu{Options: []ModelOption{{Args: args, Label: "Choose"}}}
		if err := menu.Validate(); err != nil {
			t.Fatalf("valid %q: %v", args, err)
		}
	}
	for _, args := range []string{"", " ", "--menu a b", "--cancel a", "--page -1", "--page 1001", "--page bad", "--rpc model", "model\n high", "model  high", "model high extra", "\xff", strings.Repeat("a", 257)} {
		menu := &ModelMenu{Options: []ModelOption{{Args: args, Label: "Choose"}}}
		if err := menu.Validate(); err == nil {
			t.Fatalf("invalid args accepted: %q", args)
		}
	}
	for _, menu := range []*ModelMenu{nil, {}, {Options: make([]ModelOption, 14)}, {Options: []ModelOption{{Args: "model", Label: "First"}, {Args: "model", Label: "Second"}}}, {Options: []ModelOption{{Args: "model", Label: "\n"}}}} {
		if err := menu.Validate(); err == nil {
			t.Fatalf("invalid menu accepted: %#v", menu)
		}
	}
}
