package codexadapter

import (
	"bytes"
	"encoding/json"
	"strings"
)

// toolCallText projects only call inputs from the app-server's ThreadItem
// variants. Results, diffs, reasoning and unrecognized future items must never
// become visible progress merely because they contain a text-like field.
func toolCallText(params json.RawMessage) string {
	var value struct {
		Item struct {
			Type      string          `json:"type"`
			Command   string          `json:"command"`
			Server    string          `json:"server"`
			Namespace string          `json:"namespace"`
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
			Path      string          `json:"path"`
			Query     string          `json:"query"`
			Action    struct {
				Type    string   `json:"type"`
				Query   string   `json:"query"`
				Queries []string `json:"queries"`
				URL     string   `json:"url"`
				Pattern string   `json:"pattern"`
			} `json:"action"`
			Changes []struct {
				Path string `json:"path"`
				Kind struct {
					Type string `json:"type"`
				} `json:"kind"`
			} `json:"changes"`
			ReceiverThreadIDs []string    `json:"receiverThreadIds"`
			DurationMS        json.Number `json:"durationMs"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &value) != nil {
		return ""
	}
	item := value.Item
	switch item.Type {
	case "commandExecution":
		return callDetails("commandExecution", item.Command)
	case "mcpToolCall":
		return callDetails(qualifiedTool(item.Server, item.Tool, "mcpToolCall"), toolArguments(item.Arguments))
	case "dynamicToolCall":
		return callDetails(qualifiedTool(item.Namespace, item.Tool, "dynamicToolCall"), toolArguments(item.Arguments))
	case "fileChange":
		lines := make([]string, 0, len(item.Changes))
		for _, change := range item.Changes {
			lines = append(lines, strings.TrimSpace(change.Kind.Type+" "+change.Path))
		}
		return callDetails("fileChange", strings.Join(lines, "\n"))
	case "webSearch":
		name, details := "webSearch", item.Query
		switch item.Action.Type {
		case "search":
			details = first(strings.Join(item.Action.Queries, "\n"), item.Action.Query, item.Query)
		case "openPage":
			name, details = "webSearch.openPage", item.Action.URL
		case "findInPage":
			name, details = "webSearch.findInPage", callDetails(item.Action.URL, item.Action.Pattern)
		}
		return callDetails(name, details)
	case "imageView":
		return callDetails("imageView", item.Path)
	case "imageGeneration":
		// revisedPrompt and result are generated output, not call arguments.
		return "imageGeneration"
	case "collabAgentToolCall":
		// Keep the operation and targets without reproducing private agent
		// prompts or their evolving internal state.
		return callDetails(qualifiedTool("collaboration", item.Tool, "collabAgentToolCall"), strings.Join(item.ReceiverThreadIDs, "\n"))
	case "sleep":
		if item.DurationMS != "" {
			return callDetails("sleep", item.DurationMS.String()+" ms")
		}
		return "sleep"
	default:
		return ""
	}
}

func qualifiedTool(namespace, tool, fallback string) string {
	if tool == "" {
		return fallback
	}
	if namespace != "" {
		return namespace + "." + tool
	}
	return tool
}

func callDetails(name, details string) string {
	if strings.TrimSpace(details) == "" {
		return name
	}
	return name + "\n" + details
}

func toolArguments(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	value = redactArgumentFields(value, 0)
	if text, ok := value.(string); ok {
		// Freeform tools such as functions.exec carry source as a string.
		return text
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ""
	}
	return string(encoded)
}

// Argument names provide an additional credential boundary before the worker
// applies configured value redaction. Arbitrary tool result fields are never
// passed to this function.
func redactArgumentFields(value any, depth int) any {
	if depth >= 16 {
		return "[omitted]"
	}
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			name := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(key))
			switch name {
			case "authorization", "proxyauthorization", "cookie", "setcookie", "password", "passwd", "secret", "clientsecret", "token", "accesstoken", "refreshtoken", "apikey", "privatekey", "credential", "credentials":
				value[key] = "[REDACTED]"
			default:
				value[key] = redactArgumentFields(child, depth+1)
			}
		}
	case []any:
		for i := range value {
			value[i] = redactArgumentFields(value[i], depth+1)
		}
	}
	return value
}
