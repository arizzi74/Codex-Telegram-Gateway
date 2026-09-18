//go:build linux

package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	"golang.org/x/sys/unix"
)

// TestNativeCLIReconnect is opt-in because it runs the installed Codex binary
// and needs Linux PTYs. Everything uses a temporary home and an offline provider;
// no credentials, existing sessions, model requests or running workers are used.
//
// CODEX_NATIVE_RECONNECT_TEST=1 go test ./internal/worker -run '^TestNativeCLIReconnect$' -count=1
func TestNativeCLIReconnect(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_RECONNECT_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_RECONNECT_TEST=1 to test the installed Codex TUI")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	dir := attachmentTestDir(t)
	configText := fmt.Sprintf("model_provider = \"isolated\"\nmodel = \"reconnect-test\"\ncheck_for_update_on_startup = false\n[model_providers.isolated]\nname = \"Isolated reconnect test\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[projects.%q]\ntrust_level = \"trusted\"\n", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	threadID := nativeSavedFixture(t, dir)
	environment := []string{}
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "CODEX_HOME=") || strings.HasPrefix(value, "TERM=") || strings.HasPrefix(value, "OPENAI_API_KEY=") || strings.HasPrefix(value, "CODEX_API_KEY=") {
			continue
		}
		environment = append(environment, value)
	}
	environment = append(environment, "CODEX_HOME="+dir, "TERM=xterm-256color")
	startServer := func(name string) (string, func()) {
		path := filepath.Join(dir, name+".sock")
		command := exec.Command(codex, "app-server", "--listen", "unix://"+path)
		command.Dir, command.Env = dir, environment
		command.Stdout, command.Stderr = io.Discard, io.Discard
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		stop := nativeStartProcess(t, command)
		nativeWait(t, 10*time.Second, "app-server socket", func() bool { _, err := os.Stat(path); return err == nil })
		return path, stop
	}
	backend, stopServer := startServer("first")
	m := &RuntimeManager{cfg: config.WorkerConfig{StateFile: filepath.Join(dir, "worker.db")}, attachments: make(map[string]*attachmentProxy)}
	runtime := protocol.Runtime{ID: uuid.NewString(), ProfileID: "main", Generation: 1}
	first, path, err := m.openAttachment(runtime, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.close)
	master, slave := nativePTY(t)
	command := exec.Command(codex, "--remote", "unix://"+path, "--no-alt-screen", "resume", threadID)
	command.Dir, command.Env = dir, environment
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	stopCLI := nativeStartProcess(t, command)
	_ = slave.Close()
	var outputMu sync.Mutex
	var output bytes.Buffer
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buffer := make([]byte, 64<<10)
		for {
			n, err := master.Read(buffer)
			if n > 0 {
				data := buffer[:n]
				outputMu.Lock()
				_, _ = output.Write(data)
				outputMu.Unlock()
				if bytes.Contains(data, []byte("\x1b[6n")) {
					_, _ = master.Write([]byte("\x1b[1;1R"))
				}
				if bytes.Contains(data, []byte("\x1b]11;?")) {
					_, _ = master.Write([]byte("\x1b]11;rgb:0000/0000/0000\x1b\\"))
				}
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { stopCLI(); _ = master.Close(); <-readDone })
	text := func() string { outputMu.Lock(); defer outputMu.Unlock(); return output.String() }
	nativeWait(t, 20*time.Second, "native TUI startup", func() bool { return strings.Contains(text(), "reconnect-test") })
	attachedIdle := func(proxy *attachmentProxy) bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		return len(proxy.sessions) > 0 && proxy.idleLocked() == nil
	}
	nativeWait(t, 10*time.Second, "idle native attachment", func() bool { return attachedIdle(first) })
	const draft = "unsent-reconnect-draft"
	if _, err := master.Write([]byte(draft)); err != nil {
		t.Fatal(err)
	}
	nativeWait(t, 5*time.Second, "unsent draft", func() bool { return strings.Contains(text(), draft) })
	if err := first.pause(); err != nil {
		t.Fatalf("idle real native CLI blocked update: %v", err)
	}
	first.close()
	stopServer()
	backend, _ = startServer("second")
	runtime.Generation++
	second, newPath, err := m.openAttachment(runtime, backend)
	if err != nil || newPath != path {
		t.Fatalf("stable proxy restart failed: %v", err)
	}
	t.Cleanup(second.close)
	nativeWait(t, 30*time.Second, "native reconnect", func() bool {
		rendered := text()
		reconnected := strings.LastIndex(rendered, "Reconnected. No input was resent.")
		return reconnected >= 0 && strings.Contains(rendered[reconnected:], draft) && attachedIdle(second)
	})
	if strings.Contains(text(), "This conversation is unavailable") || strings.Contains(text(), "Automatic reconnect could not restore") {
		t.Fatal("native reconnect did not restore the saved test thread")
	}
	if err := syscall.Kill(command.Process.Pid, 0); err != nil {
		t.Fatal("original native TUI process did not survive reconnect")
	}
	if err := second.pause(); err != nil {
		t.Fatalf("reconnected idle native CLI blocked update: %v", err)
	}
	if err := filepath.WalkDir(filepath.Join(dir, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
			data, err := os.ReadFile(path)
			if err != nil || bytes.Contains(data, []byte(draft)) {
				t.Errorf("unsent draft was persisted as a prompt, or history was unreadable: %v", err)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("Native TUI resumed its saved thread through the restarted worker proxy; process and unsent draft survived without replay.")
}

func nativeSavedFixture(t *testing.T, home string) string {
	t.Helper()
	id := uuid.NewString()
	now := time.Now().UTC()
	dir := filepath.Join(home, "sessions", now.Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, record := range []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": id, "timestamp": now.Format(time.RFC3339Nano), "cwd": home, "originator": "codex_cli_rs", "cli_version": "0.154.0", "source": "cli", "model_provider": "isolated", "base_instructions": map[string]any{"text": "Offline reconnect test fixture.", "version": 1}}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Saved offline reconnect-test fixture."}}}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Recorded offline."}}, "phase": "final"}},
	} {
		record["timestamp"] = now.Format(time.RFC3339Nano)
		if err := json.NewEncoder(file).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func nativeStartProcess(t *testing.T, command *exec.Cmd) func() {
	t.Helper()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			select {
			case <-done:
				return
			default:
			}
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				<-done
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func nativePTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 35, Col: 120}); err != nil {
		t.Fatal(err)
	}
	return master, slave
}

func nativeWait(t *testing.T, duration time.Duration, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
