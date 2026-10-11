package releasemanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	for _, entry := range []string{
		"WHISPER_MODEL_COMMIT=" + voiceModelRevision,
		"WHISPER_MODEL_SHA256=" + voiceModelSHA256,
		"WHISPER_MODEL_SIZE=" + strconv.FormatInt(voiceModelSize, 10),
		"WHISPER_SMALL_MODEL_COMMIT=" + voiceSmallModelRevision,
		"WHISPER_SMALL_MODEL_SHA256=" + voiceSmallModelSHA256,
		"WHISPER_SMALL_MODEL_SIZE=" + strconv.FormatInt(voiceSmallModelSize, 10),
	} {
		if !strings.Contains(string(data), entry+"\n") {
			t.Fatalf("native/manager pin mismatch for %s", entry)
		}
	}
}

func TestVoiceSetupOptions(t *testing.T) {
	for _, test := range []struct {
		args []string
		want VoiceSetupOptions
	}{
		{nil, VoiceSetupOptions{}},
		{[]string{"--model", "small", "--threads", "3"}, VoiceSetupOptions{Model: "small", Threads: 3}},
		{[]string{"--model=base", "--threads=1"}, VoiceSetupOptions{Model: "base", Threads: 1}},
		{[]string{"--threads", "32"}, VoiceSetupOptions{Threads: 32}},
	} {
		got, err := parseVoiceSetupOptions(test.args)
		if err != nil || got != test.want {
			t.Fatalf("parseVoiceSetupOptions(%v) = %+v, %v; want %+v", test.args, got, err, test.want)
		}
	}
	for _, args := range [][]string{
		{"--model", "small.en"}, {"--model", "../../ggml-small.bin"}, {"--model="},
		{"--threads", "0"}, {"--threads", "-1"}, {"--threads", "33"},
		{"--threads", "three"}, {"--other"}, {"small"}, {"--model"},
	} {
		if _, err := parseVoiceSetupOptions(args); err == nil {
			t.Fatalf("accepted invalid voice setup arguments %v", args)
		}
	}
}

func TestVoiceSetupDefaultsAndPreservesSupportedConfiguration(t *testing.T) {
	for _, test := range []struct {
		name    string
		voice   map[string]any
		opts    VoiceSetupOptions
		model   string
		threads int
	}{
		{name: "fresh", model: "base", threads: 2},
		{name: "legacy zero", voice: map[string]any{"threads": float64(0)}, model: "base", threads: 2},
		{name: "small", voice: map[string]any{"model_path": voiceSmallModelPath, "threads": float64(3)}, model: "small", threads: 3},
		{name: "base", voice: map[string]any{"model_path": voiceModelPath, "threads": float64(1)}, model: "base", threads: 1},
		{name: "model override", voice: map[string]any{"model_path": voiceModelPath, "threads": float64(3)}, opts: VoiceSetupOptions{Model: "small"}, model: "small", threads: 3},
		{name: "thread override", voice: map[string]any{"model_path": voiceSmallModelPath, "threads": float64(2)}, opts: VoiceSetupOptions{Threads: 32}, model: "small", threads: 32},
		{name: "replace custom", voice: map[string]any{"model_path": "/custom/model", "threads": "bad"}, opts: VoiceSetupOptions{Model: "small", Threads: 3}, model: "small", threads: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := map[string]any{"voice": test.voice}
			model, threads, err := resolveVoiceSetupOptions(cfg, test.opts)
			if err != nil || model.Name != test.model || threads != test.threads {
				t.Fatalf("model %q, threads %d, error %v", model.Name, threads, err)
			}
		})
	}
}

func TestVoiceSetupRejectsUnsupportedSavedSettings(t *testing.T) {
	for _, voice := range []map[string]any{
		{"model_path": "/custom/model"}, {"model_path": "/usr/local/share/codex-whisper/../codex-whisper/ggml-small.bin"},
		{"model_path": float64(1)}, {"threads": float64(33)}, {"threads": float64(-1)},
		{"threads": float64(1.5)}, {"threads": "3"},
	} {
		if _, _, err := resolveVoiceSetupOptions(map[string]any{"voice": voice}, VoiceSetupOptions{}); err == nil {
			t.Fatalf("accepted unsupported saved voice configuration %v", voice)
		}
	}
}

func TestVoiceModelInstallationRejectsUnsafeModelAndAncestors(t *testing.T) {
	for _, test := range []struct {
		name, badPath string
		mode          os.FileMode
		uid           uint32
		missing       bool
		valid         bool
	}{
		{name: "root owned", valid: true},
		{name: "model absent", badPath: voiceSmallModelPath, missing: true, valid: true},
		{name: "model symlink", badPath: voiceSmallModelPath, mode: os.ModeSymlink | 0644},
		{name: "model directory", badPath: voiceSmallModelPath, mode: os.ModeDir | 0755},
		{name: "model service owned", badPath: voiceSmallModelPath, mode: 0644, uid: 1001},
		{name: "ancestor symlink", badPath: "/usr/local", mode: os.ModeSymlink | 0755},
		{name: "ancestor service owned", badPath: "/usr/local/share", mode: os.ModeDir | 0755, uid: 1001},
		{name: "ancestor writable", badPath: "/usr/local/share/codex-whisper", mode: os.ModeDir | 0775},
		{name: "ancestor regular file", badPath: "/usr/local/share", mode: 0644},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkedRoot := false
			err := validateVoiceModelInstallation(voiceSmallModelPath, func(path string) (os.FileInfo, error) {
				checkedRoot = checkedRoot || path == "/"
				if path == test.badPath {
					if test.missing {
						return nil, os.ErrNotExist
					}
					return installerFileInfo{mode: test.mode, uid: test.uid}, nil
				}
				mode := os.ModeDir | 0755
				if path == voiceSmallModelPath {
					mode = 0644
				}
				return installerFileInfo{mode: mode}, nil
			})
			if (err == nil) != test.valid || (test.valid && !checkedRoot) {
				t.Fatalf("valid=%v checked root=%v error=%v", test.valid, checkedRoot, err)
			}
		})
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

func TestVoiceModelDownloadEnforcesSelectedModelSizeAndChecksum(t *testing.T) {
	data := []byte("verified model")
	digest := sha256.Sum256(data)
	spec, _ := pinnedVoiceModel("small")
	spec.Size, spec.SHA256 = int64(len(data)), hex.EncodeToString(digest[:])
	for _, test := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{"verified", data, true},
		{"wrong checksum", []byte("corrupted data"), false},
		{"truncated", data[:len(data)-1], false},
		{"oversized", append(append([]byte(nil), data...), bytes.Repeat([]byte("x"), 100)...), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := bytes.NewReader(test.body)
			m := New(nil)
			m.HTTP.Transport = gatewaySetupRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != voiceSmallModelURL {
					t.Fatal("small model download used an unexpected source")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Header: make(http.Header)}, nil
			})
			target := filepath.Join(t.TempDir(), "ggml-small.bin")
			temporary, err := m.downloadPinnedVoiceModel(t.Context(), target, spec)
			if (err == nil) != test.ok {
				t.Fatalf("download = %q, %v; expected success %v", temporary, err, test.ok)
			}
			if test.ok {
				if updateRead(t, temporary) != string(data) {
					t.Fatal("verified download changed")
				}
			} else if entries, err := os.ReadDir(filepath.Dir(target)); err != nil || len(entries) != 0 {
				t.Fatalf("rejected model left temporary artifacts: %v, %v", entries, err)
			}
			if int64(len(test.body)-body.Len()) > spec.Size+1 {
				t.Fatal("download read beyond the pinned model size limit")
			}
		})
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

func TestVoiceSmallInstallValidatesSelectedThreadsAndPreservesBaseModel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("gateway readiness uses Linux process identity")
	}
	m, l, helper, baseModel, notices := voiceApplyFixture(t)
	l.Binary, _ = os.Executable()
	m.ReadyTimeout = 2 * time.Second
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ready\n")) }))
	defer server.Close()
	if err := os.WriteFile(baseModel, []byte("keep original base model"), 0644); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(filepath.Dir(baseModel), "ggml-small.bin")
	cfg, _ := ReadJSON(l.Config)
	cfg["listen"] = strings.TrimPrefix(server.URL, "http://")
	cfg["voice"] = map[string]any{"enabled": true, "model_path": baseModel, "threads": 2, "timeout_seconds": 91}
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	data := []byte("verified test model")
	digest := sha256.Sum256(data)
	spec, _ := pinnedVoiceModel("small")
	spec.Size, spec.SHA256 = int64(len(data)), hex.EncodeToString(digest[:])
	m.HTTP.Transport = gatewaySetupRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != voiceSmallModelURL {
			t.Fatal("installer requested an unexpected model")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
	})
	preflight, restarts := 0, 0
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] == helper {
			if !reflect.DeepEqual(args, []string{helper, "--model", model, "--threads", "3"}) || updateRead(t, model) != string(data) {
				t.Fatalf("helper preflight did not use the verified selected model and threads: %v", args)
			}
			preflight++
			return CommandResult{Output: []byte(`{"type":"ready","protocol":1}`)}, nil
		}
		if args[0] != "systemctl" || !strings.Contains(strings.Join(args, " "), "codex-gateway.service") {
			t.Fatalf("voice setup touched an unrelated service: %v", args)
		}
		if args[1] == "restart" {
			restarts++
		}
		return CommandResult{Output: []byte("ActiveState=active\nMainPID=" + strconv.Itoa(os.Getpid()) + "\n")}, nil
	}
	if err := m.installVoiceModel(t.Context(), l, helper, model, notices, spec, 3); err != nil {
		t.Fatal(err)
	}
	cfg, _ = ReadJSON(l.Config)
	voice := cfg["voice"].(map[string]any)
	if voice["enabled"] != true || voice["model_path"] != model || voice["threads"] != float64(3) || voice["timeout_seconds"] != float64(91) {
		t.Fatalf("small setup changed configuration unexpectedly: %v", voice)
	}
	if preflight != 1 || restarts != 1 || updateRead(t, baseModel) != "keep original base model" {
		t.Fatal("setup did not validate once, restart only the gateway, and retain the base model")
	}
}

func TestVoiceInstallRejectsCorruptExistingSmallModelBeforeCommands(t *testing.T) {
	m, l, helper, model, notices := voiceApplyFixture(t)
	before := updateRead(t, l.Config)
	if err := os.WriteFile(model, []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		t.Fatalf("corrupt model reached helper or service startup: %v", args)
		return CommandResult{}, nil
	}
	spec, _ := pinnedVoiceModel("small")
	if err := m.installVoiceModel(t.Context(), l, helper, model, notices, spec, 3); err == nil {
		t.Fatal("accepted a corrupt existing small model")
	}
	if updateRead(t, l.Config) != before || updateRead(t, filepath.Join(l.Bin, "codex-whisper")) != "old helper" {
		t.Fatal("corrupt model changed gateway configuration or helper")
	}
}

func TestVoiceApplyFailureRestoresConfigAndHelper(t *testing.T) {
	for _, failure := range []string{"restart", "notices", "readiness"} {
		t.Run(failure, func(t *testing.T) {
			m, l, helper, model, notices := voiceApplyFixture(t)
			cfg, _ := ReadJSON(l.Config)
			cfg["voice"] = map[string]any{"enabled": true, "model_path": voiceModelPath, "threads": 2, "timeout_seconds": 91}
			if err := WriteJSON(l.Config, cfg); err != nil {
				t.Fatal(err)
			}
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
			if err := m.applyVoiceModel(t.Context(), l, helper, model, notices, "small", 3); err == nil {
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
