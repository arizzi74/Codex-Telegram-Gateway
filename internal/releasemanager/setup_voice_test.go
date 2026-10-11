package releasemanager

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestVoicePinsMatchBuildSourceManifest(t *testing.T) {
	data, err := os.ReadFile("../../native/whisper/source.env")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"WHISPER_MODEL_COMMIT=" + voiceModelRevision, "WHISPER_MODEL_SHA256=" + voiceModelSHA256, "WHISPER_MODEL_SIZE=147951465"} {
		if !strings.Contains(string(data), entry+"\n") {
			t.Fatalf("native/manager pin mismatch for %s", entry)
		}
	}
}

func TestVoiceModelRedirectPolicy(t *testing.T) {
	for _, raw := range []string{"https://huggingface.co/file", "https://cas-bridge.xethub.hf.co/file?signature=private", "https://cdn-lfs.huggingface.co/file"} {
		u, _ := url.Parse(raw)
		if !modelRedirectURL(u) {
			t.Fatalf("rejected model CDN %s", u.Host)
		}
	}
	for _, raw := range []string{"http://huggingface.co/file", "https://attacker.hf.co.evil/file", "https://user@huggingface.co/file", "https://huggingface.co:444/file", "https://127.0.0.1/file"} {
		u, _ := url.Parse(raw)
		if modelRedirectURL(u) {
			t.Fatalf("accepted unexpected URL %s", raw)
		}
	}
}

func TestVoiceModelCorruptionLeavesNoInstalledModelOrTemporary(t *testing.T) {
	m := New(nil)
	m.HTTP.Transport = gatewaySetupRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != voiceModelURL {
			t.Fatal("model download used a mutable or unexpected source")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("corrupt")), Header: make(http.Header)}, nil
	})
	model := filepath.Join(t.TempDir(), "ggml-base.bin")
	if _, err := m.downloadVoiceModel(t.Context(), model); err == nil {
		t.Fatal("accepted a corrupt model")
	}
	files, err := os.ReadDir(filepath.Dir(model))
	if err != nil || len(files) != 0 {
		t.Fatalf("corrupt download left artifacts: %v %v", files, err)
	}
}

func TestVoiceModelRedirectErrorsDoNotExposeSignedURL(t *testing.T) {
	m := New(nil)
	m.HTTP.Transport = gatewaySetupRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": []string{"https://evil.example/file?secret=private-token"}}, Request: r}, nil
	})
	_, err := m.downloadVoiceModel(t.Context(), filepath.Join(t.TempDir(), "model"))
	if err == nil || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "evil.example") {
		t.Fatalf("unsafe download diagnostic: %v", err)
	}
}

func voiceApplyFixture(t *testing.T) (*Manager, *Layout, string, string, string) {
	t.Helper()
	l, _, _ := updateFixture(t, "gateway")
	root := t.TempDir()
	helper, model, notices := filepath.Join(root, "new-helper"), filepath.Join(root, "share", "ggml-base.bin"), filepath.Join(root, "notices")
	for _, name := range []string{"NOTICE.md", "source.env", "licenses/whisper.cpp-MIT.txt", "licenses/Whisper-model-MIT.txt"} {
		path := filepath.Join(notices, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("notice"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(model), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte("new helper"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Bin, "codex-whisper"), []byte("old helper"), 0755); err != nil {
		t.Fatal(err)
	}
	m := New(nil)
	m.ReadyTimeout, m.PollInterval = 20*time.Millisecond, time.Millisecond
	return m, l, helper, model, notices
}

func TestVoiceApplyPreservesConfigAndRestartsOnlyGateway(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("gateway readiness uses Linux process identity")
	}
	m, l, helper, model, notices := voiceApplyFixture(t)
	l.Binary, _ = os.Executable()
	m.ReadyTimeout = 2 * time.Second
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ready\n")) }))
	defer server.Close()
	cfg, _ := ReadJSON(l.Config)
	cfg["listen"], cfg["custom"] = strings.TrimPrefix(server.URL, "http://"), map[string]any{"keep": true}
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.Config, 0640); err != nil {
		t.Fatal(err)
	}
	var actions [][]string
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		actions = append(actions, args)
		return CommandResult{Output: []byte("ActiveState=active\nMainPID=" + strconv.Itoa(os.Getpid()) + "\n")}, nil
	}
	if err := m.applyVoice(t.Context(), l, helper, model, notices); err != nil {
		t.Fatal(err)
	}
	cfg, _ = ReadJSON(l.Config)
	voice := cfg["voice"].(map[string]any)
	if voice["enabled"] != true || voice["helper_path"] != filepath.Join(l.Bin, "codex-whisper") || voice["model_path"] != model || voice["threads"] != float64(2) || cfg["custom"].(map[string]any)["keep"] != true {
		t.Fatalf("configuration changed unexpectedly: %v", cfg)
	}
	info, _ := os.Stat(l.Config)
	if info.Mode().Perm() != 0640 {
		t.Fatalf("config permissions changed: %o", info.Mode().Perm())
	}
	if updateRead(t, filepath.Join(l.Bin, "codex-whisper")) != "new helper" {
		t.Fatal("helper not installed")
	}
	for _, args := range actions {
		if args[0] != "systemctl" || !strings.Contains(strings.Join(args, " "), "codex-gateway.service") {
			t.Fatalf("voice setup touched another service: %v", args)
		}
	}
	if len(actions) != 2 || actions[0][1] != "restart" {
		t.Fatalf("unexpected service actions: %v", actions)
	}
}

func TestVoiceApplyFailureRestoresConfigAndHelper(t *testing.T) {
	for _, failure := range []string{"restart", "notices", "readiness"} {
		t.Run(failure, func(t *testing.T) {
			m, l, helper, model, notices := voiceApplyFixture(t)
			before := updateRead(t, l.Config)
			if failure == "notices" {
				notices = filepath.Join(t.TempDir(), "missing")
			}
			var restarts int
			m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] != "systemctl" || !strings.Contains(strings.Join(args, " "), "codex-gateway.service") {
					t.Fatalf("touched unrelated service: %v", args)
				}
				if args[1] == "restart" {
					restarts++
					if failure == "restart" && restarts == 1 {
						return CommandResult{}, errors.New("restart failure")
					}
				}
				return CommandResult{}, nil
			}
			if err := m.applyVoice(t.Context(), l, helper, model, notices); err == nil {
				t.Fatal("expected setup failure")
			}
			if updateRead(t, l.Config) != before || updateRead(t, filepath.Join(l.Bin, "codex-whisper")) != "old helper" {
				t.Fatal("setup failure did not restore original files")
			}
			if failure == "notices" && restarts != 0 {
				t.Fatal("preparation failure restarted gateway")
			}
			if failure != "notices" && restarts != 2 {
				t.Fatalf("gateway not recovered: %d restarts", restarts)
			}
		})
	}
}

func TestGatewayHelperUpdateRollsBackWithGatewayBeforeStart(t *testing.T) {
	l, packages, release := updateFixture(t, "gateway")
	helper := filepath.Join(l.Bin, "codex-whisper")
	if err := os.WriteFile(helper, []byte("old helper"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packages["gateway"], "codex-whisper"), []byte("new helper"), 0755); err != nil {
		t.Fatal(err)
	}
	var actions []string
	m := New(nil)
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] == "runuser" {
			if updateRead(t, helper) != "new helper" {
				t.Fatal("new helper not staged with gateway")
			}
			return CommandResult{}, errors.New("migration failure")
		}
		if args[0] == "systemctl" {
			actions = append(actions, args[1])
			if args[1] == "start" && updateRead(t, helper) != "old helper" {
				t.Fatal("gateway restarted before helper rollback")
			}
			return CommandResult{}, nil
		}
		t.Fatalf("unexpected command: %v", args)
		return CommandResult{}, nil
	}
	if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err == nil {
		t.Fatal("expected migration failure")
	}
	if !reflect.DeepEqual(actions, []string{"stop", "start"}) || updateRead(t, helper) != "old helper" {
		t.Fatal("helper/gateway rollback was incomplete")
	}
	backups, _ := filepath.Glob(filepath.Join(l.Backups, "*", "*-codex-whisper"))
	if len(backups) != 1 || updateRead(t, backups[0]) != "old helper" {
		t.Fatal("helper backup was not preserved")
	}
}

func TestGatewayEnabledVoiceRejectsReleaseWithoutHelper(t *testing.T) {
	l, packages, release := updateFixture(t, "gateway")
	cfg, _ := ReadJSON(l.Config)
	cfg["voice"] = map[string]any{"enabled": true}
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	m := New(nil)
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		t.Fatalf("incomplete voice release touched service: %v", args)
		return CommandResult{}, nil
	}
	if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err == nil {
		t.Fatal("accepted a voice-enabled gateway release without helper")
	}
}
