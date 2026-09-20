// Package telegramcommands defines the Telegram command menu and canonical
// Codex names. Telegram menu names allow underscores, whereas Codex uses hyphens.
package telegramcommands

import "strings"

type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

var gateway = []Command{
	{"tgstart", "Gateway: getting started"},
	{"tghelp", "Gateway: command guide"},
	{"tginstances", "Gateway: workers and runtimes"},
	{"tgsessions", "Gateway: choose a session"},
	{"tgstatus", "Gateway: connection, session and queue state"},
	{"tghistory", "Gateway: show saved Codex prompts"},
	{"tglastmessages", "Gateway: last message or last N user and Codex messages"},
	{"tgmultisession", "Gateway: toggle messages from all sessions"},
	{"tgdisconnect", "Gateway: clear the selected session"},
	{"tgdeletesession", "Gateway: delete a session, keep its folder"},
	{"tgsteer", "Gateway: send guidance to the current turn"},
	{"tginterrupt", "Gateway: interrupt the current turn"},
	{"tgquestions", "Gateway: show pending questions and approvals"},
	{"tginput", "Gateway: answer a pending input request"},
}

var codex = []Command{
	{"status", "Codex: session settings, context and usage"},
	{"model", "Codex: list models or set model and effort"},
	{"reasoning", "Codex: inspect or set reasoning effort"},
	{"permissions", "Codex: choose session permissions"},
	{"approvals", "Codex: inspect or change approval policy"},
	{"fast", "Codex: inspect or change service tier"},
	{"plan", "Codex: change planning mode"},
	{"personality", "Codex: inspect or set response style"},
	{"compact", "Codex: compact this session"},
	{"review", "Codex: review changes"},
	{"rename", "Codex: give this session a name"},
	{"new", "Codex: name a new chat and choose its folder"},
	{"clear", "Codex: start a fresh chat"},
	{"resume", "Codex: choose a saved chat"},
	{"fork", "Codex: branch this chat"},
	{"goal", "Codex: inspect or update the task goal"},
	{"diff", "Codex: inspect workspace changes"},
	{"init", "Codex: ask for project instructions"},
	{"mcp", "Codex: inspect MCP servers"},
	{"apps", "Codex: inspect available apps"},
	{"skills", "Codex: inspect skills"},
	{"plugins", "Codex: inspect plugins"},
	{"hooks", "Codex: inspect lifecycle hooks"},
	{"memories", "Codex: inspect or set memory mode"},
	{"ps", "Codex: inspect background terminals"},
	{"stop", "Codex: stop background terminals"},
	{"clean", "Codex: alias for stopping background terminals"},
	{"copy", "Codex: show the last response"},
	{"usage", "Codex: inspect account usage limits"},
	{"rollout", "Codex: local transcript location instructions"},
	{"agent", "Codex: select an agent session"},
	{"subagents", "Codex: select an agent session"},
	{"archive", "Codex: archive this session"},
	{"delete", "Codex: choose a session to delete"},
	{"approve", "Codex: retry an auto-review denial"},
	{"side", "Codex: side-conversation instructions"},
	{"btw", "Codex: side-conversation instructions"},
	{"mention", "Codex: file context instructions"},
	{"debug_config", "Codex: configuration diagnostics"},
	{"app", "Codex: desktop continuation instructions"},
	{"ide", "Codex: IDE context instructions"},
	{"import", "Codex: local import instructions"},
	{"logout", "Codex: local sign-out instructions"},
	{"feedback", "Codex: local feedback instructions"},
	{"experimental", "Codex: local feature configuration"},
	{"raw", "Terminal: raw scrollback settings"},
	{"keymap", "Terminal: keyboard settings"},
	{"vim", "Terminal: Vim composer settings"},
	{"statusline", "Terminal: status line settings"},
	{"title", "Terminal: window title settings"},
	{"theme", "Terminal: color theme settings"},
	{"pets", "Terminal: pet settings"},
	{"pet", "Terminal: pet settings"},
	{"setup_default_sandbox", "Terminal: Windows sandbox setup"},
	{"sandbox_add_read_dir", "Terminal: Windows sandbox read access"},
	{"quit", "Codex: leave this Telegram session"},
	{"exit", "Codex: leave this Telegram session"},
	{"help", "Codex: command guide"},
}

func Commands() []Command {
	commands := append([]Command(nil), gateway...)
	return append(commands, codex...)
}

func CodexCommands() []Command { return append([]Command(nil), codex...) }

// Canonical accepts both the original CLI spelling and Telegram's menu alias.
func Canonical(name string) (string, bool) {
	name = strings.ReplaceAll(strings.ToLower(name), "-", "_")
	for _, command := range codex {
		if name == command.Command {
			return strings.ReplaceAll(name, "_", "-"), true
		}
	}
	return "", false
}
