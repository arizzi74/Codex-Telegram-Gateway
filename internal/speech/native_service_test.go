package speech

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/audio"
)

func nativeTestSettings(engine string) audio.Settings {
	return audio.Settings{Enabled: true, Model: "base", Threads: 3, ReplyWithAudio: true, TTSVoice: "auto", TTSEngine: engine}
}

func nativeTestService(t *testing.T) *Service {
	t.Helper()
	s := fakeService(t, "ok")
	s.nativeInstall = func(context.Context, string, string, func(audio.Operation)) error { return nil }
	s.nativeFactory = func(string) *Service { return fakeService(t, "ok") }
	return s
}

func speechProcess(s *Service) *helperProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.process
}

func waitSpeechProcess(t *testing.T, s *Service, present bool) *helperProcess {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		p := speechProcess(s)
		if (p != nil) == present {
			return p
		}
		select {
		case <-deadline.C:
			t.Fatalf("speech child presence never became %v", present)
		case <-tick.C:
		}
	}
}

func TestNativeRestoreAndPreparationPreserveActiveEngine(t *testing.T) {
	s := nativeTestService(t)
	var installs atomic.Int32
	s.nativeInstall = func(context.Context, string, string, func(audio.Operation)) error { installs.Add(1); return nil }
	s.SetActive(nativeTestSettings(""))
	if s.nativeActive != nil || s.nativeEngine != "" || installs.Load() != 0 {
		t.Fatal("legacy startup migrated speech")
	}
	s.SetActive(nativeTestSettings("piper"))
	old := s.nativeActive
	if old == nil || speechProcess(old) != nil || installs.Load() != 0 {
		t.Fatal("restoring an engine performed I/O or warmup")
	}
	if _, err := s.Synthesize(t.Context(), "An English public test.", "english", 3); err != nil {
		t.Fatal(err)
	}
	p := speechProcess(old)
	if p == nil || p.language != "italian" {
		t.Fatal("Piper did not keep its Italian voice for English text")
	}

	if err := s.Prepare(t.Context(), nativeTestSettings("qwen3"), nil); err != nil {
		t.Fatal(err)
	}
	pending := s.nativePending
	if pending == nil || pending == old || s.nativeActive != old || s.nativeEngine != "piper" || p.cmd.ProcessState != nil {
		t.Fatal("warming a candidate replaced or stopped the active engine")
	}
	if speechProcess(pending) == nil || speechProcess(pending).language != "english" {
		t.Fatal("automatic Qwen preparation did not check both languages")
	}
	s.DiscardPrepared()
	if s.nativePending != nil || s.pendingEngine != "" || !pending.closed || speechProcess(pending) != nil {
		t.Fatal("discard kept a warmed candidate resident")
	}
	if s.nativeActive != old || old.closed || speechProcess(old) != p {
		t.Fatal("discard changed the active engine")
	}
	if _, err := s.Synthesize(t.Context(), "Una frase dopo il salvataggio fallito.", "italian", 3); err != nil {
		t.Fatal(err)
	}
}

func TestNativePreparationFailurePreservesActiveEngine(t *testing.T) {
	for _, failure := range []string{"install", "warmup"} {
		t.Run(failure, func(t *testing.T) {
			s := nativeTestService(t)
			s.SetActive(nativeTestSettings("piper"))
			old := s.nativeActive
			if _, err := s.Synthesize(t.Context(), "Una frase pubblica.", "italian", 3); err != nil {
				t.Fatal(err)
			}
			p := speechProcess(old)
			var candidate *Service
			if failure == "install" {
				s.nativeInstall = func(context.Context, string, string, func(audio.Operation)) error {
					return errors.New("fixture download failed")
				}
			} else {
				s.nativeFactory = func(string) *Service { candidate = fakeService(t, "badready"); return candidate }
			}
			if err := s.Prepare(t.Context(), nativeTestSettings("qwen3"), nil); err == nil {
				t.Fatal("failed candidate was accepted")
			}
			if s.nativeActive != old || s.nativeEngine != "piper" || old.closed || speechProcess(old) != p || s.nativePending != nil {
				t.Fatal("preparation failure changed the active engine")
			}
			if candidate != nil && (!candidate.closed || speechProcess(candidate) != nil) {
				t.Fatal("warmup failure left a child resident")
			}
		})
	}
}

func TestNativeActivationReapsOldChildrenAndRemovesOnlyUnusedManagedModels(t *testing.T) {
	s := nativeTestService(t)
	if _, err := s.Synthesize(t.Context(), "Legacy public phrase.", "english", 3); err != nil {
		t.Fatal(err)
	}
	legacy := speechProcess(s)
	s.SetActive(nativeTestSettings("piper"))
	old := s.nativeActive
	if _, err := s.Synthesize(t.Context(), "Una frase pubblica.", "italian", 3); err != nil {
		t.Fatal(err)
	}
	oldProcess := speechProcess(old)
	for _, name := range []string{"piper", "qwen3", "italian", "english", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(s.dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.dir, name, "keep-or-remove"), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	currentRuntime := runtimeDirectory(filepath.Join(s.dir, "qwen3"))
	oldRuntime := filepath.Join(s.dir, "qwen3", "runtime", "v0.0.1")
	for _, dir := range []string{currentRuntime, oldRuntime} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prepare(t.Context(), nativeTestSettings("qwen3"), nil); err != nil {
		t.Fatal(err)
	}
	pending := s.nativePending
	if err := s.Activate(t.Context(), nativeTestSettings("qwen3")); err != nil {
		t.Fatal(err)
	}
	if s.nativeActive != pending || s.nativeEngine != "qwen3" || s.nativePending != nil || !old.closed || speechProcess(old) != nil || speechProcess(s) != nil {
		t.Fatal("activation did not swap and reap the previous engines")
	}
	if oldProcess.cmd.ProcessState == nil || legacy.cmd.ProcessState == nil {
		t.Fatal("activation returned before old children were reaped")
	}
	for _, name := range []string{"piper", "italian", "english"} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unused %s assets survived activation: %v", name, err)
		}
	}
	for _, name := range []string{"qwen3/keep-or-remove", "unrelated/keep-or-remove"} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err != nil {
			t.Fatalf("activation removed retained %s: %v", name, err)
		}
	}
	if _, err := os.Stat(currentRuntime); err != nil {
		t.Fatal("selected runtime removed")
	}
	if _, err := os.Stat(oldRuntime); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old selected-engine runtime retained")
	}
	if _, err := s.Synthesize(t.Context(), "A public English phrase.", "english", 3); err != nil {
		t.Fatal(err)
	}
	if speechProcess(pending).language != "english" {
		t.Fatal("Qwen did not honor the selected language")
	}
}

func TestNativeWarmupLeavesActiveSynthesisGateAvailable(t *testing.T) {
	s := nativeTestService(t)
	s.SetActive(nativeTestSettings("piper"))
	limiter := &testInferenceLimiter{budget: make(chan struct{}, 1), requested: make(chan struct{}, 1)}
	s.SetInferenceLimiter(limiter)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	prepared := make(chan error, 1)
	go func() { prepared <- s.Prepare(ctx, nativeTestSettings("qwen3"), nil) }()
	select {
	case <-limiter.requested:
	case <-ctx.Done():
		t.Fatal("warmup did not request inference")
	}
	if _, err := s.Synthesize(ctx, "Una frase durante la preparazione.", "italian", 3); err != nil {
		t.Fatalf("candidate warmup blocked active synthesis: %v", err)
	}
	limiter.budget <- struct{}{}
	select {
	case err := <-prepared:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("candidate warmup did not release inference")
	}
	s.DiscardPrepared()
}

func TestNativePendingCancellationAndParentLifecycle(t *testing.T) {
	for _, activeEngine := range []string{"piper", "qwen3"} {
		for _, action := range []string{"cancel", "unload", "close"} {
			t.Run(activeEngine+"/"+action, func(t *testing.T) {
				s := nativeTestService(t)
				s.SetActive(nativeTestSettings(activeEngine))
				old := s.nativeActive
				if _, err := s.Synthesize(t.Context(), "Una frase pubblica.", "italian", 3); err != nil {
					t.Fatal(err)
				}
				candidate := fakeService(t, "block")
				s.nativeFactory = func(string) *Service { return candidate }
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				prepared := make(chan error, 1)
				go func() { prepared <- s.Prepare(ctx, nativeTestSettings("qwen3"), nil) }()
				waitSpeechProcess(t, candidate, true)
				switch action {
				case "cancel":
					cancel()
				case "unload":
					s.Unload()
				case "close":
					s.Close()
				}
				select {
				case err := <-prepared:
					if err == nil {
						t.Fatal("interrupted preparation succeeded")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("interrupted preparation blocked")
				}
				if !candidate.closed || speechProcess(candidate) != nil || s.nativePending != nil {
					t.Fatal("interrupted preparation retained candidate")
				}
				if action == "cancel" {
					if old.closed || (activeEngine == "piper" && speechProcess(old) == nil) {
						t.Fatal("candidate cancellation stopped the active engine")
					}
				} else {
					waitSpeechProcess(t, old, false)
				}
				if action == "unload" || action == "cancel" {
					if _, err := s.Synthesize(t.Context(), "La voce può ripartire.", "italian", 3); err != nil {
						t.Fatalf("unload prevented active restart: %v", err)
					}
				}
				if action == "close" && !old.closed {
					t.Fatal("closing parent did not close active backend")
				}
			})
		}
	}
}

func TestNativeSameEnginePreparationKeepsOnlyOneModelResident(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "discard after failed save", true: "activate after successful save"}[commit], func(t *testing.T) {
			s := nativeTestService(t)
			limiter := &testInferenceLimiter{budget: make(chan struct{}, 1), requested: make(chan struct{}, 8)}
			limiter.budget <- struct{}{}
			s.SetInferenceLimiter(limiter)
			s.SetActive(nativeTestSettings("qwen3"))
			old := s.nativeActive
			if _, err := s.Synthesize(t.Context(), "An existing public voice.", "english", 3); err != nil {
				t.Fatal(err)
			}
			oldProcess := speechProcess(old)
			candidate := fakeService(t, "ok")
			s.nativeFactory = func(string) *Service { return candidate }
			command := candidate.command
			var languageChecks atomic.Int32
			candidate.command = func(language string, threads int) *exec.Cmd {
				if speechProcess(old) != nil || oldProcess.cmd.ProcessState == nil {
					t.Error("same-engine warmup retained the old resident model")
				}
				if len(limiter.budget) != 0 {
					t.Error("same-engine warmup did not hold the shared inference budget")
				}
				languageChecks.Add(1)
				return command(language, threads)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			requested := nativeTestSettings("qwen3")
			requested.Threads = 2
			if err := s.Prepare(ctx, requested, nil); err != nil {
				t.Fatal(err)
			}
			candidate.command = command
			if languageChecks.Load() != 2 || speechProcess(candidate) != nil || speechProcess(old) != nil || candidate.closed || old.closed || s.nativeActive != old {
				t.Fatal("same-engine preparation did not check both languages and release both resident models")
			}
			// A reply may arrive after Prepare releases its budget and before the
			// save commits. Only the old model may reload in that interval.
			oldCommand := old.command
			old.command = func(language string, threads int) *exec.Cmd {
				if speechProcess(candidate) != nil {
					t.Error("old reply and prepared candidate were resident together")
				}
				return oldCommand(language, threads)
			}
			release, err := limiter.AcquireInference(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Synthesize(ctx, "The old voice remains usable before commit.", "english", 3)
			release()
			if err != nil {
				t.Fatal(err)
			}
			if commit {
				if err := s.Activate(ctx, requested); err != nil {
					t.Fatal(err)
				}
				if !old.closed || s.nativeActive != candidate {
					t.Fatal("successful save did not activate the checked candidate")
				}
			} else {
				s.DiscardPrepared()
				if old.closed || s.nativeActive != old || !candidate.closed {
					t.Fatal("failed save discarded the active service")
				}
			}
			if _, err := s.Synthesize(ctx, "A public voice after the save.", "italian", 2); err != nil {
				t.Fatalf("saved or preserved engine could not reload: %v", err)
			}
		})
	}
}
