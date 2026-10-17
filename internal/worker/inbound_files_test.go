package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
	"golang.org/x/sys/unix"
)

func inboundTestCommand(workerID, cwd string) protocol.Command {
	c := testCommand(workerID)
	c.Operation = protocol.StoreFiles
	c.SessionID, c.ThreadID = uuid.NewString(), "inbound-thread"
	c.Arguments = protocol.Arguments{CWD: cwd, Files: []protocol.File{{Name: "report 例.pdf", Data: []byte("complete document\x00\xff")}}}
	return c
}

func inboundTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	path, workerID := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(path, workerID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, workerID, path
}

func TestInboundFilesSaveLazilyAndRetryWithoutReplacing(t *testing.T) {
	store, workerID, _ := inboundTestStore(t)
	cwd := canonicalWebUIFileTempDir(t)
	c := inboundTestCommand(workerID, cwd)
	if _, err := os.Lstat(filepath.Join(cwd, "inbound")); !os.IsNotExist(err) {
		t.Fatalf("inbound created before upload: %v", err)
	}
	if _, err := store.Receive(c); err != nil {
		t.Fatal(err)
	}
	path, prompt, err := storeInboundFile(store, c, cwd, []string{cwd})
	if err != nil || prompt != "" || !strings.HasPrefix(path, filepath.Join(cwd, "inbound")+"/") || !strings.HasSuffix(path, "-"+c.ID+".pdf") {
		t.Fatalf("path=%q prompt=%q err=%v", path, prompt, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, c.Arguments.Files[0].Data) {
		t.Fatalf("saved bytes=%q err=%v", data, err)
	}
	before, _ := os.Stat(path)
	directory, _ := os.Stat(filepath.Dir(path))
	if before.Mode().Perm() != 0o600 || directory.Mode().Perm() != 0o700 {
		t.Fatalf("permissions file=%o directory=%o", before.Mode().Perm(), directory.Mode().Perm())
	}
	retryPath, _, err := storeInboundFile(store, c, cwd, []string{cwd})
	after, _ := os.Stat(path)
	if err != nil || retryPath != path || !os.SameFile(before, after) {
		t.Fatalf("retry replaced file path=%q err=%v", retryPath, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary artifacts remained: %#v", entries)
	}
	second := c
	second.ID = uuid.NewString()
	if _, err := store.Receive(second); err != nil {
		t.Fatal(err)
	}
	secondPath, _, err := storeInboundFile(store, second, cwd, []string{cwd})
	if err != nil || secondPath == path {
		t.Fatalf("same-name upload collided: %q %v", secondPath, err)
	}
}

func TestInboundFilesRejectSymlinksSpecialFilesAndUnownedCollisions(t *testing.T) {
	for _, scenario := range []string{"inbound symlink", "inbound regular file", "existing file", "existing symlink", "existing hardlink", "existing fifo"} {
		t.Run(scenario, func(t *testing.T) {
			store, workerID, _ := inboundTestStore(t)
			cwd, outside := canonicalWebUIFileTempDir(t), canonicalWebUIFileTempDir(t)
			c := inboundTestCommand(workerID, cwd)
			inbound := filepath.Join(cwd, "inbound")
			if scenario == "inbound symlink" {
				if err := os.Symlink(outside, inbound); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "inbound regular file" {
				if err := os.WriteFile(inbound, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(inbound, 0o700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(inbound, inboundFilename(c.Arguments.Files[0].Name, c.ID))
				switch scenario {
				case "existing file":
					if err := os.WriteFile(path, c.Arguments.Files[0].Data, 0o600); err != nil {
						t.Fatal(err)
					}
				case "existing symlink":
					if err := os.Symlink(filepath.Join(outside, "target"), path); err != nil {
						t.Fatal(err)
					}
				case "existing hardlink":
					source := filepath.Join(outside, "source")
					if err := os.WriteFile(source, []byte("keep"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(source, path); err != nil {
						t.Fatal(err)
					}
				case "existing fifo":
					if err := unix.Mkfifo(path, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := store.Receive(c); err != nil {
				t.Fatal(err)
			}
			if path, _, err := storeInboundFile(store, c, cwd, []string{cwd}); err == nil {
				t.Fatalf("unsafe destination accepted at %q", path)
			}
			outsideEntries, _ := os.ReadDir(outside)
			if scenario != "existing hardlink" && len(outsideEntries) != 0 {
				t.Fatal("file escaped workspace")
			}
		})
	}
}

func TestInboundFilesRetryRejectsChangedContentIdentityAndHardlinks(t *testing.T) {
	for _, scenario := range []string{"content", "identical replacement", "symlink replacement", "added hardlink"} {
		t.Run(scenario, func(t *testing.T) {
			store, workerID, _ := inboundTestStore(t)
			cwd := canonicalWebUIFileTempDir(t)
			c := inboundTestCommand(workerID, cwd)
			if _, err := store.Receive(c); err != nil {
				t.Fatal(err)
			}
			path, _, err := storeInboundFile(store, c, cwd, []string{cwd})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "content":
				err = os.WriteFile(path, []byte("changed bytes"), 0o600)
			case "identical replacement":
				// Keep the original inode alive so the filesystem cannot reuse it.
				err = os.Rename(path, path+".original")
				if err == nil {
					err = os.WriteFile(path, c.Arguments.Files[0].Data, 0o600)
				}
			case "symlink replacement":
				err = os.Rename(path, path+".original")
				if err == nil {
					err = os.Symlink(path+".original", path)
				}
			case "added hardlink":
				err = os.Link(path, path+".link")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := storeInboundFile(store, c, cwd, []string{cwd}); err == nil {
				t.Fatal("retry accepted a changed upload")
			}
		})
	}
}

func TestInboundFilesRecoverPublishWindowsAfterStoreRestart(t *testing.T) {
	for _, window := range []string{"before publish", "after link", "after unlink"} {
		t.Run(window, func(t *testing.T) {
			store, workerID, database := inboundTestStore(t)
			cwd := canonicalWebUIFileTempDir(t)
			c := inboundTestCommand(workerID, cwd)
			if _, err := store.Receive(c); err != nil {
				t.Fatal(err)
			}
			path, _, err := storeInboundFile(store, c, cwd, []string{cwd})
			if err != nil {
				t.Fatal(err)
			}
			record, _, _ := store.LoadCommand(c.ID)
			if window != "after unlink" {
				record.InboundFile.Published = false
				encoded, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.db.Update(func(tx *workerdb.Tx) error {
					return tx.Bucket(bucketCommands).Put([]byte(c.ID), encoded)
				}); err != nil {
					t.Fatal(err)
				}
			}
			original, _ := os.Stat(path)
			if window != "after unlink" {
				if err := os.Link(path, filepath.Join(cwd, "inbound", record.InboundFile.Temporary)); err != nil {
					t.Fatal(err)
				}
			}
			if window == "before publish" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(database, workerID)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			retryPath, _, err := storeInboundFile(reopened, c, cwd, []string{cwd})
			current, _ := os.Stat(path)
			if err != nil || path != retryPath || !os.SameFile(original, current) || !singleLinkedWebUIFile(current) {
				t.Fatalf("recovered path=%q err=%v", retryPath, err)
			}
		})
	}
}

func TestInboundFileAmbiguousCodexRestartKeepsPathWithoutReplay(t *testing.T) {
	store, workerID, database := inboundTestStore(t)
	cwd := canonicalWebUIFileTempDir(t)
	c := inboundTestCommand(workerID, cwd)
	c.Operation, c.Arguments.Text = protocol.StartTurn, "read document"
	if _, err := store.Receive(c); err != nil {
		t.Fatal(err)
	}
	path, _, err := storeInboundFile(store, c, cwd, []string{cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCommandState(c.ID, CommandExecuting, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(database, workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	record, _, err := reopened.LoadCommand(c.ID)
	if err != nil || record.State != CommandOutcomeUnknown || record.Result == nil || record.Result.Error == nil || !strings.Contains(record.Result.Error.Message, "File saved in "+path) {
		t.Fatalf("ambiguous upload result=%#v err=%v", record, err)
	}
	pending, err := reopened.PendingCommands()
	if err != nil || len(pending) != 0 {
		t.Fatalf("ambiguous Codex command became replayable: %#v %v", pending, err)
	}
	duplicate, err := reopened.Receive(c)
	if err != nil || duplicate.Accepted {
		t.Fatalf("ambiguous command accepted again: %#v %v", duplicate, err)
	}
}

func TestAgentQueuedFileRechecksFrozenWorkspaceBeforeSaving(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "thread-queued-file", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	command := agentCommand(runtime, session, protocol.StartTurn)
	command.Arguments = protocol.Arguments{CWD: session.CWD, Text: "read", Files: []protocol.File{{Name: "report.pdf", Data: []byte("document")}}}
	received := runInboundActorCommand(t, actor, command)
	if received.State != CommandReceived || len(actor.queue) != 1 {
		t.Fatalf("turn not queued: %#v", received)
	}
	actor.session.CWD = filepath.Join(session.CWD, "moved")
	if err := os.Mkdir(actor.session.CWD, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := len(server.Calls())
	actor.start(actor.queue[0])
	record, _, _ := a.store.LoadCommand(command.ID)
	if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != protocol.InvalidWorkspace || record.InboundFile != nil {
		t.Fatalf("queued file followed changed CWD: %#v", record)
	}
	if _, err := os.Lstat(filepath.Join(session.CWD, "inbound")); !os.IsNotExist(err) || len(server.Calls()) != calls {
		t.Fatalf("queued stale command touched disk or Codex: %v", err)
	}
}

func TestInboundFilesPreserveExistingDirectoryModeAndAllowEmptyFile(t *testing.T) {
	store, workerID, _ := inboundTestStore(t)
	cwd := canonicalWebUIFileTempDir(t)
	if err := os.Mkdir(filepath.Join(cwd, "inbound"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(cwd, "inbound"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := inboundTestCommand(workerID, cwd)
	c.Arguments.Files[0].Data = nil
	if _, err := store.Receive(c); err != nil {
		t.Fatal(err)
	}
	path, _, err := storeInboundFile(store, c, cwd, []string{cwd})
	if err != nil {
		t.Fatal(err)
	}
	file, _ := os.Stat(path)
	directory, _ := os.Stat(filepath.Dir(path))
	if file.Size() != 0 || file.Mode().Perm() != 0o600 || directory.Mode().Perm() != 0o755 {
		t.Fatalf("empty upload or directory mode changed: file=%v directory=%v", file, directory)
	}
}

func TestAgentFileOnlyPreservesIdleColdAndActiveTurnsWithoutCodex(t *testing.T) {
	for _, state := range []string{"idle", "not_loaded", "running"} {
		t.Run(state, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "thread-file", CWD: runtime.DefaultCWD, State: state, Loaded: state != "not_loaded"}
			actor := &sessionActor{agent: a, runtime: runtime, session: session, finalText: "keep"}
			if state == "running" {
				actor.session.ActiveTurnID = "live-turn"
				actor.activeCommand = &protocol.Command{ID: "active-command"}
				actor.awaitingTurnStart = true
				actor.queue = []protocol.Command{{ID: "queued-command"}}
			}
			before, active, calls := actor.session, actor.activeCommand, len(server.Calls())
			command := agentCommand(runtime, actor.session, protocol.StoreFiles)
			command.Arguments = protocol.Arguments{CWD: session.CWD, Files: []protocol.File{{Name: "file.bin", Data: []byte("bytes")}}}
			record := runInboundActorCommand(t, actor, command)
			if record.State != CommandCompleted || record.Result == nil || record.Result.Error != nil || !strings.HasPrefix(record.Result.Text, "File saved in "+filepath.Join(session.CWD, "inbound")+"/") || record.Result.TurnID != "" {
				t.Fatalf("file-only result=%#v", record)
			}
			if actor.session != before || actor.activeCommand != active || actor.finalText != "keep" || (state == "running" && (!actor.awaitingTurnStart || len(actor.queue) != 1)) {
				t.Fatal("file-only changed Codex/session state")
			}
			if len(server.Calls()) != calls {
				t.Fatalf("file-only called Codex: %#v", server.Calls()[calls:])
			}
		})
	}
}

func TestAgentFilesCaptionStartAndSteer(t *testing.T) {
	for _, op := range []protocol.Operation{protocol.StartTurn, protocol.Steer} {
		t.Run(string(op), func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installSession(a, runtime, "thread-caption", "")
			if op == protocol.Steer {
				session = installSession(a, runtime, "thread-caption", "live-turn")
			}
			command := agentCommand(runtime, session, op)
			command.Arguments = protocol.Arguments{CWD: session.CWD, Text: "inspect this document", Files: []protocol.File{{Name: "report.pdf", Data: []byte("document")}}}
			if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
				t.Fatalf("ack=%#v err=%v", ack, err)
			}
			method := "turn/start"
			if op == protocol.Steer {
				method = "turn/steer"
			}
			waitFor(t, func() bool { return hasCall(server.Calls(), method) })
			assertInboundRPC(t, server.Calls(), method, command)
			record, _, err := a.store.LoadCommand(command.ID)
			if err != nil || record.InboundFile == nil || record.InboundFile.Prompt != command.Arguments.Text+"\nFile: "+filepath.Join(session.CWD, "inbound", record.InboundFile.Name) || record.Command.Arguments.Text != command.Arguments.Text {
				t.Fatalf("caption ledger=%#v err=%v", record, err)
			}
		})
	}
}

func TestAgentCaptionedFileColdResumeAndFailureKeepSavedPath(t *testing.T) {
	for _, scenario := range []string{"cold", "resume cwd changed", "resume rpc", "start rpc", "steer rpc", "path only steer"} {
		t.Run(scenario, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installColdSession(a, runtime, "thread-cold-file")
			threadCWD := session.CWD
			if scenario == "resume cwd changed" {
				threadCWD = t.TempDir()
			}
			server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": threadCWD}}, nil)
			op := protocol.StartTurn
			if strings.Contains(scenario, "steer") {
				op = protocol.Steer
				session = installSession(a, runtime, session.ThreadID, "active-turn")
			}
			command := agentCommand(runtime, session, op)
			command.Arguments = protocol.Arguments{CWD: session.CWD, Text: "read document", Files: []protocol.File{{Name: "report.pdf", Data: []byte("document")}}}
			if scenario == "path only steer" {
				command.Arguments.Text = ""
			}
			if strings.HasSuffix(scenario, "rpc") {
				method := strings.TrimSuffix(scenario, " rpc")
				if method == "resume" {
					method = "thread/resume"
				} else {
					method = "turn/" + method
				}
				server.SetRPCError(method, -32000, "rejected")
			}
			if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
				t.Fatalf("ack=%#v err=%v", ack, err)
			}
			waitFor(t, func() bool {
				record, _, _ := a.store.LoadCommand(command.ID)
				return record.State == CommandCompleted || record.State == CommandFailed
			})
			record, _, _ := a.store.LoadCommand(command.ID)
			if record.InboundFile == nil || !record.InboundFile.Published {
				t.Fatalf("missing saved file: %#v", record)
			}
			path := filepath.Join(session.CWD, "inbound", record.InboundFile.Name)
			if strings.HasSuffix(scenario, "rpc") || scenario == "resume cwd changed" {
				if record.State != CommandFailed || record.Result.Error == nil || !strings.Contains(record.Result.Error.Message, "File saved in "+path) {
					t.Fatalf("failure hid saved path: %#v", record)
				}
			} else {
				method := "turn/start"
				if op == protocol.Steer {
					method = "turn/steer"
				}
				assertInboundRPC(t, server.Calls(), method, command)
			}
			if scenario == "resume cwd changed" && hasCall(server.Calls(), "turn/start") {
				t.Fatal("file prompt reached a different resumed workspace")
			}
		})
	}
}

func TestAgentFilesRejectChangedFrozenTargetsBeforeSaving(t *testing.T) {
	for _, scenario := range []string{"cwd", "generation", "thread", "turn", "expired", "disallowed"} {
		t.Run(scenario, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "thread-fenced", CWD: runtime.DefaultCWD, State: "running", Loaded: true, ActiveTurnID: "live-turn"}
			actor := &sessionActor{agent: a, runtime: runtime, session: session}
			command := agentCommand(runtime, session, protocol.Steer)
			command.Arguments = protocol.Arguments{CWD: session.CWD, Text: "read", Files: []protocol.File{{Name: "report.pdf", Data: []byte("document")}}}
			switch scenario {
			case "cwd":
				actor.session.CWD = t.TempDir()
			case "generation":
				command.RuntimeGeneration++
			case "thread":
				command.ThreadID = "another-thread"
			case "turn":
				command.ExpectedTurnID = "another-turn"
			case "expired":
				command.ExpiresAt = command.CreatedAt.AddDate(-1, 0, 0)
				command.CreatedAt = command.ExpiresAt.AddDate(-1, 0, 0)
			case "disallowed":
				actor.session.CWD = t.TempDir()
				command.Arguments.CWD = actor.session.CWD
			}
			calls := len(server.Calls())
			record := runInboundActorCommand(t, actor, command)
			if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil || record.InboundFile != nil {
				t.Fatalf("changed target saved file: %#v", record)
			}
			if _, err := os.Lstat(filepath.Join(session.CWD, "inbound")); !os.IsNotExist(err) || len(server.Calls()) != calls {
				t.Fatalf("rejected command touched disk or Codex: %v", err)
			}
		})
	}
}

func runInboundActorCommand(t *testing.T, actor *sessionActor, c protocol.Command) CommandRecord {
	t.Helper()
	if _, err := actor.agent.store.Receive(c); err != nil {
		t.Fatal(err)
	}
	request := actorCommand{command: c, reply: make(chan commandReply, 1)}
	actor.command(request)
	reply := <-request.reply
	if reply.err != nil {
		t.Fatal(reply.err)
	}
	record, _, err := actor.agent.store.LoadCommand(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func assertInboundRPC(t *testing.T, calls []codextest.Call, method string, c protocol.Command) {
	t.Helper()
	for _, call := range calls {
		if call.Method != method {
			continue
		}
		var payload struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.Unmarshal(call.Params, &payload); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(c.Arguments.CWD, "inbound", inboundFilename(c.Arguments.Files[0].Name, c.ID))
		want := []map[string]any{{"type": "text", "text": c.Arguments.Text + "\nFile: " + path, "text_elements": []any{}}}
		if len(payload.Input) != 1 || payload.Input[0]["type"] != "text" || payload.Input[0]["text"] != want[0]["text"] {
			t.Fatalf("input=%#v want=%#v", payload.Input, want)
		}
		data, err := os.ReadFile(path)
		if err != nil || !reflect.DeepEqual(data, c.Arguments.Files[0].Data) {
			t.Fatalf("Codex got a missing/incomplete path: %q err=%v", data, err)
		}
		return
	}
	t.Fatalf("missing %s call", method)
}
