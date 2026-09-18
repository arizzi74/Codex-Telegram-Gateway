package telegramcommands

import (
	"regexp"
	"testing"
)

func TestMenuNamesAreValidAndUnique(t *testing.T) {
	commands := Commands()
	if len(commands) > 100 {
		t.Fatal("Telegram accepts at most 100 commands")
	}
	valid := regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	seen := map[string]bool{}
	for _, command := range commands {
		if !valid.MatchString(command.Command) || seen[command.Command] || len(command.Description) == 0 || len(command.Description) > 256 {
			t.Fatalf("invalid menu entry: %#v", command)
		}
		seen[command.Command] = true
	}
	for _, required := range []string{"tgstatus", "status", "tgsessions", "model", "tginput", "tghistory", "compact"} {
		if !seen[required] {
			t.Fatalf("missing menu entry: %s", required)
		}
	}
	for _, spelling := range []string{"debug-config", "debug_config"} {
		if canonical, ok := Canonical(spelling); !ok || canonical != "debug-config" {
			t.Fatal("Telegram alias changed Codex command")
		}
	}
}
