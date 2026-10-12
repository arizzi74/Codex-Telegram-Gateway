package audio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testManager(t *testing.T) (*Manager, []byte) {
	t.Helper()
	root := t.TempDir()
	helper := filepath.Join(root, "helper")
	if err := os.WriteFile(helper, []byte("test helper"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("pinned multilingual model fixture")
	digest := sha256.Sum256(data)
	models := []modelSpec{}
	for _, name := range []string{"base", "small"} {
		models = append(models, modelSpec{name: name, systemPath: filepath.Join(root, "system", "ggml-"+name+".bin"), url: "https://huggingface.co/ggerganov/whisper.cpp/resolve/" + modelRevision + "/ggml-" + name + ".bin?download=true", sha256: hex.EncodeToString(digest[:]), size: int64(len(data))})
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, modelRevision) {
			t.Errorf("unpinned model requested: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Header: make(http.Header)}, nil
	})}
	fallback := config.VoiceConfig{Enabled: true, HelperPath: helper, ModelPath: models[1].systemPath, Threads: 3}
	m, err := newManager(filepath.Join(root, "audio"), fallback, dependencies{models: models, http: client, preflight: func(context.Context, config.VoiceConfig) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return m, data
}

func runManager(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("audio manager did not shut down promptly")
		}
	})
}

func waitSettings(t *testing.T, m *Manager, state string) Status {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		status := m.Snapshot()
		if status.Operation.State == state {
			return status
		}
		select {
		case <-deadline.C:
			t.Fatalf("waiting for %s; got %+v", state, status)
		case <-tick.C:
		}
	}
}

func TestSettingsValidation(t *testing.T) {
	valid := Settings{Enabled: true, Model: "small", Threads: 3, TTSVoice: "italian", ReplyWithAudio: true}
	for _, tc := range []struct {
		name   string
		change func(*Settings)
	}{
		{"unknown model", func(s *Settings) { s.Model = "tiny" }},
		{"path model", func(s *Settings) { s.Model = "../../base" }},
		{"zero threads", func(s *Settings) { s.Threads = 0 }},
		{"negative threads", func(s *Settings) { s.Threads = -1 }},
		{"excess threads", func(s *Settings) { s.Threads = 33 }},
		{"voice URL", func(s *Settings) { s.TTSVoice = "https://example.com/model" }},
		{"voice path", func(s *Settings) { s.TTSVoice = "../voice" }},
		{"voice control", func(s *Settings) { s.TTSVoice = "italian\n" }},
		{"voice oversized", func(s *Settings) { s.TTSVoice = strings.Repeat("a", 81) }},
		{"unknown speech engine", func(s *Settings) { s.TTSEngine = "pocket" }},
		{"speech engine URL", func(s *Settings) { s.TTSEngine = "https://example.com/model" }},
		{"speech engine path", func(s *Settings) { s.TTSEngine = "../piper" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.change(&s)
			if s.Validate() == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
	for _, threads := range []int{1, 3, 32} {
		s := valid
		s.Threads = threads
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, engine := range []string{"", "piper", "qwen3"} {
		s := valid
		s.TTSEngine = engine
		if err := s.Validate(); err != nil {
			t.Fatalf("valid engine %q rejected: %v", engine, err)
		}
	}
}

func TestInitialSettingsPreserveSmallAndThreeThreads(t *testing.T) {
	m, _ := testManager(t)
	status := m.Snapshot()
	if status.Settings.Model != "small" || status.Settings.Threads != 3 || !status.Settings.Enabled || status.Settings.TTSVoice != "auto" {
		t.Fatalf("existing settings lost: %+v", status.Settings)
	}
	if m.VoiceConfig().ModelPath != m.deps.models[1].systemPath {
		t.Fatal("existing model path changed")
	}
	if status.Resources.RecommendedModel != "base" || status.Resources.RecommendedThreads != max(1, min(32, status.Resources.AvailableCPUs-1)) {
		t.Fatalf("invalid recommendation: %+v", status.Resources)
	}
	if status.Models[0].Name != "base" || status.Models[0].Advanced || !status.Models[0].Recommended || !status.Models[1].Advanced {
		t.Fatalf("invalid model choices: %+v", status.Models)
	}
	if status.TTS.Available || len(status.TTS.Voices) != 3 {
		t.Fatalf("TTS claimed installed: %+v", status.TTS)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reading settings wrote configuration")
	}
}

func TestVerifiedDownloadCommitsAfterPreflightAndSurvivesRestart(t *testing.T) {
	m, data := testManager(t)
	initial := m.VoiceConfig()
	started, release := make(chan config.VoiceConfig, 1), make(chan struct{})
	m.deps.preflight = func(ctx context.Context, cfg config.VoiceConfig) error {
		started <- cfg
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.SetSpeechSupport(speechFixture{available: true, prepare: func(context.Context, Settings, func(Operation)) error { return nil }})
	runManager(t, m)
	changed := m.Changed()
	requested := Settings{Enabled: true, Model: "base", Threads: 4, ReplyWithAudio: true, TTSVoice: "italian"}
	if err := m.Apply(requested); err != nil {
		t.Fatal(err)
	}
	var checked config.VoiceConfig
	select {
	case checked = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("preflight never started")
	}
	if !reflect.DeepEqual(m.VoiceConfig(), initial) || m.Snapshot().Settings.Model != "small" {
		t.Fatal("effective settings changed before helper acknowledged model")
	}
	if !m.Snapshot().Busy || m.Snapshot().Operation.State != "preflight" {
		t.Fatal("apply progress unavailable")
	}
	if checked.ModelPath != m.managedPath(m.model("base")) || checked.Threads != 4 || checked.HelperPath != m.helper {
		t.Fatalf("wrong preflight: %+v", checked)
	}
	if err := m.Apply(requested); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent apply accepted: %v", err)
	}
	select {
	case <-changed:
		t.Fatal("premature settings notification")
	default:
	}
	close(release)
	status := waitSettings(t, m, "ready")
	if status.Settings != requested || status.Busy || !status.Models[0].Installed {
		t.Fatalf("not committed: %+v", status)
	}
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("missing hot-reload notification")
	}
	path := filepath.Join(m.dir, "settings.json")
	for _, private := range []string{path, checked.ModelPath} {
		info, err := os.Stat(private)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unprotected private file %s: %v", private, err)
		}
	}
	info, err := os.Stat(m.dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("state directory not private")
	}
	got, err := os.ReadFile(checked.ModelPath)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("verified model not installed atomically")
	}
	restarted, err := newManager(m.dir, initial, m.deps)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Snapshot().Settings != requested || restarted.VoiceConfig() != checked {
		t.Fatalf("settings did not survive restart: %+v", restarted.Snapshot())
	}
	// Private state cannot make a modified model effective on the next boot.
	if err := os.WriteFile(checked.ModelPath, bytes.Repeat([]byte("x"), len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err = newManager(m.dir, initial, m.deps)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.VoiceConfig() != initial || restarted.Snapshot().Operation.State != "failed" {
		t.Fatal("tampered persisted model was activated")
	}
}

func TestVerifiedSystemModelAvoidsDownload(t *testing.T) {
	m, data := testManager(t)
	spec := m.model("base")
	if err := os.MkdirAll(filepath.Dir(spec.systemPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.systemPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	m.deps.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("downloaded an already verified system model")
		return nil, errors.New("unexpected download")
	})}
	runManager(t, m)
	if err := m.Apply(Settings{Enabled: true, Model: "base", Threads: 2}); err != nil {
		t.Fatal(err)
	}
	waitSettings(t, m, "ready")
	if m.VoiceConfig().ModelPath != spec.systemPath {
		t.Fatal("system model not reused")
	}
	got, _ := os.ReadFile(spec.systemPath)
	if !bytes.Equal(got, data) {
		t.Fatal("system model was modified")
	}
}

func TestFailedDownloadOrPreflightPreservesCommittedSettings(t *testing.T) {
	for _, mode := range []string{"network", "status", "short", "oversized", "checksum", "preflight", "save"} {
		t.Run(mode, func(t *testing.T) {
			m, data := testManager(t)
			runManager(t, m)
			changed := m.Changed()
			prior := Settings{Enabled: false, Model: "small", Threads: 3, TTSVoice: "auto"}
			if err := m.Apply(prior); err != nil {
				t.Fatal(err)
			}
			waitSettings(t, m, "ready")
			<-changed
			changed = m.Changed()
			priorConfig := m.VoiceConfig()
			settingsPath := filepath.Join(m.dir, "settings.json")
			before, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			m.deps.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if mode == "network" {
					return nil, errors.New("transport secret https://signed.hf.co/?token=SECRET")
				}
				body := data
				status := 200
				switch mode {
				case "status":
					status = 403
				case "short":
					body = data[:len(data)-1]
				case "oversized":
					body = append(append([]byte{}, data...), 1)
				case "checksum":
					body = bytes.Repeat([]byte("x"), len(data))
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Header: make(http.Header)}, nil
			})}
			if mode == "preflight" {
				m.deps.preflight = func(context.Context, config.VoiceConfig) error { return errors.New("helper secret error") }
			}
			if mode == "save" {
				m.deps.preflight = func(context.Context, config.VoiceConfig) error { return os.Rename(m.dir, m.dir+"-moved") }
			}
			if err := m.Apply(Settings{Enabled: true, Model: "base", Threads: 5}); err != nil {
				t.Fatal(err)
			}
			status := waitSettings(t, m, "failed")
			if status.Settings != prior || m.VoiceConfig() != priorConfig || status.Busy {
				t.Fatalf("failure changed active settings: %+v", status)
			}
			if strings.Contains(status.Operation.Message, "SECRET") || strings.Contains(status.Operation.Message, "https:") || strings.Contains(status.Operation.Message, "secret") {
				t.Fatal("failure leaked implementation diagnostics")
			}
			if mode == "save" {
				settingsPath = filepath.Join(m.dir+"-moved", "settings.json")
			}
			after, err := os.ReadFile(settingsPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failure changed persisted settings")
			}
			select {
			case <-changed:
				t.Fatal("failure notified hot reload")
			default:
			}
			if mode != "preflight" && mode != "save" {
				if _, err := os.Stat(m.managedPath(m.model("base"))); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unverified model installed")
				}
			}
			staged, _ := filepath.Glob(filepath.Join(m.dir, "models", ".model-download-*"))
			if len(staged) != 0 {
				t.Fatal("failed model download left staged files")
			}
		})
	}
}

func TestTamperedOrSymlinkModelsAreNotReused(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "checksum", true: "symlink"}[symlink], func(t *testing.T) {
			m, data := testManager(t)
			path := m.model("base").systemPath
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if symlink {
				target := path + "-actual"
				if err := os.WriteFile(target, data, 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(data)), 0644); err != nil {
					t.Fatal(err)
				}
			}
			runManager(t, m)
			if err := m.Apply(Settings{Enabled: true, Model: "base", Threads: 2}); err != nil {
				t.Fatal(err)
			}
			waitSettings(t, m, "ready")
			if m.VoiceConfig().ModelPath != m.managedPath(m.model("base")) {
				t.Fatal("tampered or symlink system model reused")
			}
		})
	}
}

func TestDisableNeedsNoModelOrHelper(t *testing.T) {
	m, _ := testManager(t)
	if err := os.Remove(m.helper); err != nil {
		t.Fatal(err)
	}
	m.deps.preflight = func(context.Context, config.VoiceConfig) error { t.Error("preflight for disabled audio"); return nil }
	m.deps.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("download for disabled audio")
		return nil, errors.New("unexpected")
	})}
	runManager(t, m)
	if err := m.Apply(Settings{Enabled: false, Model: "small", Threads: 3}); err != nil {
		t.Fatal(err)
	}
	waitSettings(t, m, "ready")
	if m.VoiceConfig().Enabled {
		t.Fatal("audio not disabled")
	}
}

type speechFixture struct {
	available bool
	prepare   func(context.Context, Settings, func(Operation)) error
}

func (s speechFixture) Available() bool { return s.available }
func (s speechFixture) Prepare(ctx context.Context, settings Settings, progress func(Operation)) error {
	return s.prepare(ctx, settings, progress)
}

type speechLifecycleFixture struct {
	speechFixture
	initialize func(Settings)
	activate   func(context.Context, Settings) error
	discard    func()
}

func (s speechLifecycleFixture) SetActive(settings Settings) {
	if s.initialize != nil {
		s.initialize(settings)
	}
}
func (s speechLifecycleFixture) Activate(ctx context.Context, settings Settings) error {
	return s.activate(ctx, settings)
}
func (s speechLifecycleFixture) DiscardPrepared() {
	if s.discard != nil {
		s.discard()
	}
}

func TestSpeechEngineCommitAndCleanupLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "prepare failure", "save failure", "cleanup warning"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := testManager(t)
			previous := m.Settings()
			if err := m.save(savedSettings{Version: 1, Settings: previous, ModelLocation: "system"}); err != nil {
				t.Fatal(err)
			}
			settingsPath := filepath.Join(m.dir, "settings.json")
			before, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			requested := previous
			requested.Model, requested.ReplyWithAudio, requested.TTSEngine = "base", true, "qwen3"
			var prepared, activated, discarded atomic.Int32
			discardDone := make(chan struct{})
			active := previous.TTSEngine
			m.SetSpeechSupport(speechLifecycleFixture{
				speechFixture: speechFixture{available: true, prepare: func(context.Context, Settings, func(Operation)) error {
					prepared.Add(1)
					if active != previous.TTSEngine || m.Settings() != previous {
						t.Error("preparation changed the active engine or settings")
					}
					if mode == "prepare failure" {
						return errors.New("private preparation diagnostic")
					}
					if mode == "save failure" {
						return os.Rename(m.dir, m.dir+"-moved")
					}
					return nil
				}},
				discard: func() { discarded.Add(1); close(discardDone) },
				initialize: func(settings Settings) {
					if settings != previous || settings.TTSEngine != "" {
						t.Error("startup silently selected a speech engine")
					}
				},
				activate: func(ctx context.Context, settings Settings) error {
					activated.Add(1)
					data, err := readPrivate(settingsPath)
					var saved savedSettings
					if err != nil || json.Unmarshal(data, &saved) != nil || saved.Settings != requested {
						t.Error("engine activated before settings were saved")
					}
					if m.Settings() != previous {
						t.Error("settings published before engine activation")
					}
					blockedCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
					defer cancel()
					if release, err := m.AcquireInference(blockedCtx); !errors.Is(err, context.DeadlineExceeded) {
						if release != nil {
							release()
						}
						t.Error("inference was admitted while changing the active engine")
					}
					active = settings.TTSEngine
					if mode == "cleanup warning" {
						return errors.New("private cleanup path")
					}
					return nil
				},
			})
			runManager(t, m)
			if err := m.Apply(requested); err != nil {
				t.Fatal(err)
			}
			state := "ready"
			if mode == "prepare failure" || mode == "save failure" {
				state = "failed"
			}
			status := waitSettings(t, m, state)
			if prepared.Load() != 1 {
				t.Fatal("selected engine was not prepared")
			}
			if state == "failed" {
				if activated.Load() != 0 || active != previous.TTSEngine || status.Settings != previous {
					t.Fatal("failed setup activated or cleaned up an engine")
				}
				if mode == "save failure" {
					select {
					case <-discardDone:
					case <-time.After(time.Second):
						t.Fatal("save failure did not discard the candidate")
					}
					if discarded.Load() != 1 {
						t.Fatal("save failure kept the warmed candidate")
					}
					settingsPath = filepath.Join(m.dir+"-moved", "settings.json")
				}
				after, err := os.ReadFile(settingsPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("failed setup changed persisted settings")
				}
			} else if activated.Load() != 1 || discarded.Load() != 0 || active != requested.TTSEngine || status.Settings != requested {
				t.Fatal("saved engine was not activated")
			}
			if mode == "cleanup warning" && (!strings.Contains(status.Operation.Message, "could not all be removed") || strings.Contains(status.Operation.Message, "private") || status.Busy) {
				t.Fatal("cleanup error did not produce a safe applied warning")
			}
		})
	}
}

func TestSpeechSwitchCancellationBeforeAndAfterSettingsCommit(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "before commit", true: "after commit"}[afterCommit], func(t *testing.T) {
			m, _ := testManager(t)
			previous := m.Settings()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			prepared := make(chan struct{})
			var activated, discarded atomic.Int32
			var holdInference func()
			m.SetSpeechSupport(speechLifecycleFixture{
				speechFixture: speechFixture{available: true, prepare: func(ctx context.Context, _ Settings, _ func(Operation)) error {
					if !afterCommit {
						var err error
						holdInference, err = m.AcquireInference(ctx)
						if err != nil {
							return err
						}
					}
					close(prepared)
					return nil
				}},
				discard: func() { discarded.Add(1) },
				activate: func(activationCtx context.Context, _ Settings) error {
					cancel()
					if activationCtx.Err() != nil {
						t.Error("saved activation was canceled")
					}
					activated.Add(1)
					return nil
				},
			})
			done := make(chan struct{})
			go func() { m.Run(ctx); close(done) }()
			requested := previous
			requested.Model, requested.ReplyWithAudio, requested.TTSEngine = "base", true, "piper"
			if err := m.Apply(requested); err != nil {
				t.Fatal(err)
			}
			if !afterCommit {
				select {
				case <-prepared:
				case <-time.After(3 * time.Second):
					t.Fatal("engine was not prepared")
				}
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("canceled switch blocked shutdown")
			}
			if holdInference != nil {
				holdInference()
			}
			if afterCommit {
				if activated.Load() != 1 || discarded.Load() != 0 || m.Settings() != requested {
					t.Fatal("cancellation interrupted a committed switch")
				}
			} else if activated.Load() != 0 || discarded.Load() != 1 || m.Settings() != previous {
				t.Fatal("canceled setup switched the active engine or kept a warmed candidate")
			}
		})
	}
}

func TestSpeechPreparationIsRequiredBeforeCommit(t *testing.T) {
	m, _ := testManager(t)
	initial := m.VoiceConfig()
	var calls atomic.Int32
	m.SetSpeechSupport(speechFixture{available: true, prepare: func(ctx context.Context, settings Settings, progress func(Operation)) error {
		calls.Add(1)
		if settings.TTSVoice != "italian" {
			t.Error("wrong requested language")
		}
		progress(Operation{State: "downloading", Message: "Preparing local speech model.", TotalBytes: 100, DownloadedBytes: 50})
		if m.Snapshot().Settings.Model != "small" {
			t.Error("settings committed before speech setup")
		}
		return errors.New("speech preparation failed with secret URL")
	}})
	runManager(t, m)
	if err := m.Apply(Settings{Enabled: true, Model: "base", Threads: 2, ReplyWithAudio: true, TTSVoice: "italian"}); err != nil {
		t.Fatal(err)
	}
	status := waitSettings(t, m, "failed")
	if calls.Load() != 1 || !status.TTS.Available || m.VoiceConfig() != initial || strings.Contains(status.Operation.Message, "secret") {
		t.Fatal("speech failure did not preserve settings")
	}
	if err := m.Apply(Settings{Enabled: true, Model: "base", Threads: 2}); err != nil {
		t.Fatal(err)
	}
	waitSettings(t, m, "ready")
	if calls.Load() != 1 {
		t.Fatal("speech prepared when replies disabled")
	}
}

func TestDownloadProgressAndShutdownCancellation(t *testing.T) {
	m, data := testManager(t)
	started := make(chan struct{})
	m.deps.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			_, _ = writer.Write(data[:8])
			close(started)
			<-r.Context().Done()
			_ = writer.CloseWithError(r.Context().Err())
		}()
		return &http.Response{StatusCode: 200, Body: reader, ContentLength: int64(len(data)), Header: make(http.Header)}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	if err := m.Apply(Settings{Enabled: true, Model: "base", Threads: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	status := waitSettings(t, m, "downloading")
	if status.Operation.DownloadedBytes != 8 || status.Operation.TotalBytes != int64(len(data)) {
		t.Fatalf("download progress missing: %+v", status.Operation)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled download blocked shutdown")
	}
	if err := m.Apply(Settings{Model: "base", Threads: 2}); !errors.Is(err, ErrClosed) {
		t.Fatalf("accepted apply after shutdown: %v", err)
	}
	if _, err := m.AcquireInference(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("inference accepted after shutdown: %v", err)
	}
	staged, _ := filepath.Glob(filepath.Join(m.dir, "models", ".model-download-*"))
	if len(staged) != 0 {
		t.Fatal("canceled download left staged model")
	}
}

func TestSharedInferenceAdmissionHonorsCancellation(t *testing.T) {
	m, _ := testManager(t)
	release, err := m.AcquireInference(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.AcquireInference(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parallel inference admitted: %v", err)
	}
	release()
	release()
	release, err = m.AcquireInference(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestSettingsChangesBroadcastToEverySubscriber(t *testing.T) {
	m, _ := testManager(t)
	first, second := m.Changed(), m.Changed()
	runManager(t, m)
	if err := m.Apply(Settings{Model: "small", Threads: 3}); err != nil {
		t.Fatal(err)
	}
	waitSettings(t, m, "ready")
	for _, subscriber := range []<-chan struct{}{first, second} {
		select {
		case <-subscriber:
		default:
			t.Fatal("settings change was consumed by only one subscriber")
		}
	}
	if m.Changed() == first {
		t.Fatal("settings broadcast channel was not replaced")
	}
	if m.Settings() != m.Snapshot().Settings {
		t.Fatal("lightweight settings getter disagrees")
	}
}

func TestFreshDisabledConfigurationUsesRecommendedThreads(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "audio"), config.VoiceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Settings().Threads != m.Snapshot().Resources.RecommendedThreads || m.VoiceConfig().Threads != m.Settings().Threads {
		t.Fatal("fresh configuration did not use recommended CPU budget")
	}
}

func TestAudioRepliesCannotCommitWithoutAvailableSpeechEngine(t *testing.T) {
	for _, support := range []SpeechSupport{nil, speechFixture{available: false, prepare: func(context.Context, Settings, func(Operation)) error {
		t.Error("unavailable speech engine prepared")
		return nil
	}}} {
		m, _ := testManager(t)
		previous := m.Settings()
		m.SetSpeechSupport(support)
		runManager(t, m)
		if err := m.Apply(Settings{Enabled: true, Model: "base", Threads: 2, ReplyWithAudio: true}); err != nil {
			t.Fatal(err)
		}
		waitSettings(t, m, "failed")
		if m.Settings() != previous {
			t.Fatal("unavailable speech engine was saved as enabled")
		}
	}
}

func TestPrivateStateRejectsSymlinksAndUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "audio")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(link, config.VoiceConfig{}); err == nil {
		t.Fatal("state symlink accepted")
	}
	for _, mode := range []string{"symlink", "public", "oversized", "arbitrary model location"} {
		t.Run(mode, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "audio")
			if err := os.Mkdir(state, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state, "settings.json")
			switch mode {
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "missing"), path); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 8193), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(path, []byte(`{"version":1,"settings":{"model":"base","threads":2},"model_location":"/tmp/evil"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := New(state, config.VoiceConfig{}); err == nil {
				t.Fatal("unsafe settings accepted")
			}
		})
	}
}

func TestManagedModelDirectoryCannotEscapePrivateStateThroughSymlink(t *testing.T) {
	m, data := testManager(t)
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "ggml-base.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(m.dir, "models")); err != nil {
		t.Fatal(err)
	}
	runManager(t, m)
	before := m.Settings()
	if err := m.Apply(Settings{Enabled: true, Model: "base", Threads: 2}); err != nil {
		t.Fatal(err)
	}
	status := waitSettings(t, m, "failed")
	if m.Settings() != before || status.Models[0].Installed {
		t.Fatal("managed model symlink directory reused")
	}
}

func TestPinnedDownloadOriginPolicy(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		allowed bool
	}{
		{"https://huggingface.co/model", true}, {"https://cdn-lfs.hf.co/model?token=secret", true}, {"https://cas-bridge.xethub.hf.co/model", true},
		{"http://huggingface.co/model", false}, {"https://huggingface.co.evil.example/model", false}, {"https://evil-hf.co/model", false}, {"https://user:secret@hf.co/model", false}, {"https://hf.co:444/model", false}, {"https://hf.co/model#fragment", false}, {"file:///tmp/model", false},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := allowedModelURL(u); got != tc.allowed {
			t.Errorf("origin %q accepted=%v", tc.raw, got)
		}
	}
	for _, spec := range pinnedModels() {
		if !strings.Contains(spec.url, modelRevision) || spec.size < 100_000_000 || len(spec.sha256) != 64 {
			t.Fatalf("invalid production model pin: %+v", spec)
		}
	}
}
