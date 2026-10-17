package worker

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestInboundFileHistoryMatchesSavedPromptByTurnAndOccurrence(t *testing.T) {
	for _, operation := range []protocol.Operation{protocol.StartTurn, protocol.Steer} {
		t.Run(string(operation), func(t *testing.T) {
			a, runtime, _, cleanup := testAgent(t)
			defer cleanup()
			session := installSession(a, runtime, "inbound-history", "turn")
			command := agentCommand(runtime, session, operation)
			command.Arguments = protocol.Arguments{CWD: session.CWD, Text: "Read this report", Files: []protocol.File{{Name: "report.pdf", Data: []byte("report bytes")}}}
			if _, err := a.store.Receive(command); err != nil {
				t.Fatal(err)
			}
			path, prompt, err := storeInboundFile(a.store, command, session.CWD, a.cfg.AllowedWorkspaceRoots)
			if err != nil || !filepath.IsAbs(path) || !strings.Contains(prompt, path) {
				t.Fatalf("persisted upload prompt=%q path=%q error=%v", prompt, path, err)
			}
			result := &protocol.Result{TurnID: "turn"}
			if operation == protocol.Steer {
				result.TurnID = "" // Accepted steers can use ExpectedTurnID.
			}
			if err := a.store.SetCommandState(command.ID, CommandCompleted, result); err != nil {
				t.Fatal(err)
			}
			record, _, err := a.store.LoadCommand(command.ID)
			if err != nil || record.Command.Arguments.Text != command.Arguments.Text {
				t.Fatalf("saving upload rewrote immutable command: %#v %v", record, err)
			}
			prompts := []codexadapter.UserPrompt{
				{TurnID: "turn", ItemID: "telegram", Text: prompt},
				{TurnID: "turn", ItemID: "cli-caption", Text: command.Arguments.Text},
				{TurnID: "turn", ItemID: "cli-repeat", Text: prompt},
				{TurnID: "other", ItemID: "other-turn", Text: prompt},
			}
			got, err := a.store.externalHistoryPrompts(runtime.ID, session.ThreadID, prompts)
			if err != nil || !reflect.DeepEqual(got, prompts[1:]) {
				t.Fatalf("file prompt filtering = %#v, error=%v", got, err)
			}
			actor := &sessionActor{agent: a, runtime: runtime, session: session}
			actor.observeUserMessage(codexadapter.Event{TurnID: "turn", ItemID: "telegram", Text: prompt})
			events, err := a.store.OutboxAfter(0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == "user_message" {
					t.Fatal("saved file prompt was echoed as external CLI input")
				}
			}
		})
	}
}

func TestInboundFileNativeHistoryShowsActualPathOnceWithoutFileOnlyPrompt(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	client := mustClient(t, a, runtime)
	useWebUIHistoryFixture(a, client)
	session := installSession(a, runtime, "inbound-native-history", "")
	caption := agentCommand(runtime, session, protocol.StartTurn)
	caption.Arguments = protocol.Arguments{CWD: session.CWD, Text: "Summarize", Files: []protocol.File{{Name: "report.pdf", Data: []byte("report")}}}
	if _, err := a.store.Receive(caption); err != nil {
		t.Fatal(err)
	}
	path, prompt, err := storeInboundFile(a.store, caption, session.CWD, a.cfg.AllowedWorkspaceRoots)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetCommandState(caption.ID, CommandCompleted, &protocol.Result{TurnID: "turn"}); err != nil {
		t.Fatal(err)
	}
	fileOnly := agentCommand(runtime, session, protocol.StoreFiles)
	fileOnly.Arguments = protocol.Arguments{CWD: session.CWD, Files: []protocol.File{{Name: "data.csv", Data: []byte("a,b")}}}
	if _, err := a.store.Receive(fileOnly); err != nil {
		t.Fatal(err)
	}
	_, fileOnlyPrompt, err := storeInboundFile(a.store, fileOnly, session.CWD, a.cfg.AllowedWorkspaceRoots)
	if err != nil || fileOnlyPrompt != "" {
		t.Fatalf("file-only upload became a prompt: %q %v", fileOnlyPrompt, err)
	}
	if err := a.store.SetCommandState(fileOnly.ID, CommandCompleted, &protocol.Result{Text: "File saved", State: "completed"}); err != nil {
		t.Fatal(err)
	}
	thread := historyThread(session.ThreadID, prompt)
	thread["cwd"], thread["source"] = session.CWD, "cli"
	thread["turns"].([]map[string]any)[0]["status"] = "completed"
	server.SetThreads([]map[string]any{thread}, nil)
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	page, err := actor.readHistory(client, &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: 10})
	if err != nil || page == nil || len(page.Messages) != 1 || page.Messages[0].Text != prompt || strings.Count(page.Messages[0].Text, path) != 1 {
		t.Fatalf("Telegram history lost or duplicated saved path: %#v %v", page, err)
	}
	raw, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 20, Direction: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	web := webUITestPage(t, raw)
	if len(web.Data) != 1 {
		t.Fatalf("WebUI history synthesized file-only input: %s", raw)
	}
	var item struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(web.Data[0].Item, &item); err != nil || len(item.Content) != 1 || item.Content[0].Text != prompt || strings.Count(item.Content[0].Text, path) != 1 {
		t.Fatalf("WebUI history lost or duplicated saved path: %s %v", raw, err)
	}
}
