package speech

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/releasemanager"
)

// SetActive restores a persisted selection without network or model loading.
// Legacy installations retain Pocket TTS until an administrator selects a new
// engine; merely viewing the settings never downloads or changes a voice.
func (s *Service) SetActive(settings audio.Settings) {
	if nativeProgram(settings.TTSEngine) == "" {
		return
	}
	backend := s.newNativeBackend(settings.TTSEngine)
	s.mu.Lock()
	s.nativeEngine, s.nativeActive = settings.TTSEngine, backend
	s.mu.Unlock()
}

func (s *Service) newNativeBackend(engine string) *Service {
	if s.nativeFactory != nil {
		return s.nativeFactory(engine)
	}
	dir := filepath.Join(s.dir, engine)
	child := New(dir, s.executable)
	var runtimeDir string
	child.beforeStart = func(ctx context.Context) error {
		if err := privateDirectory(s.dir); err != nil {
			return err
		}
		if err := privateDirectory(dir); err != nil {
			return err
		}
		var err error
		runtimeDir, err = installedSpeechRuntime(ctx, dir, engine)
		if err != nil {
			return err
		}
		if engine == "piper" {
			return verifySpeechInventory(ctx, filepath.Join(dir, piperModelDir))
		}
		for _, model := range nativeModels(engine) {
			if err := verifyAsset(ctx, filepath.Join(dir, model.name), model); err != nil {
				return err
			}
		}
		return nil
	}
	child.command = func(language string, threads int) *exec.Cmd {
		cmd := exec.Command(nativeCommandPath(runtimeDir, engine), "--dir", dir, "--language", language, "--threads", strconv.Itoa(threads))
		cmd.Env = []string{"LANG=C.UTF-8"}
		return cmd
	}
	return child
}

// Compatible installed helpers remain usable after a gateway-only update.
// Saving audio settings installs the helper from that gateway's signed release.
func installedSpeechRuntime(ctx context.Context, dir, engine string) (string, error) {
	root := filepath.Join(dir, "runtime")
	if err := privateDirectory(root); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, entry := range entries {
		if _, err := releasemanager.ParseVersion(entry.Name()); err == nil && entry.IsDir() {
			candidates = append(candidates, entry.Name())
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		n, _ := releasemanager.CompareVersions(candidates[i], candidates[j])
		return n > 0
	})
	for _, name := range candidates {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		candidate := filepath.Join(root, name)
		if verifySpeechInventory(ctx, candidate) != nil {
			continue
		}
		info, err := os.Lstat(filepath.Join(candidate, nativeProgram(engine)))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0100 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("speech: selected engine must be prepared in audio settings")
}

func (s *Service) prepareNative(ctx context.Context, settings audio.Settings, progress func(audio.Operation)) (err error) {
	if !s.Available() || nativeProgram(settings.TTSEngine) == "" || settings.Threads < 1 || settings.Threads > 32 {
		return errors.New("speech: invalid engine configuration")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	case <-s.prepareGate:
	}
	defer func() { s.prepareGate <- struct{}{} }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	s.DiscardPrepared()
	defer func() {
		if err != nil {
			s.DiscardPrepared()
		}
	}()
	if err = privateDirectory(s.dir); err != nil {
		return err
	}
	dir := filepath.Join(s.dir, settings.TTSEngine)
	if err = privateDirectory(dir); err != nil {
		return err
	}
	if s.nativeInstall != nil {
		err = s.nativeInstall(ctx, dir, settings.TTSEngine, progress)
	} else {
		err = s.prepareRuntime(ctx, dir, settings.TTSEngine, progress)
		if err == nil {
			err = s.prepareNativeModels(ctx, dir, settings.TTSEngine, progress)
		}
	}
	if err != nil {
		return err
	}
	backend := s.newNativeBackend(settings.TTSEngine)
	s.mu.Lock()
	limiter := s.limiter
	sameEngine := s.nativeActive != nil && s.nativeEngine == settings.TTSEngine
	s.nativePending, s.pendingEngine = backend, settings.TTSEngine
	s.mu.Unlock()
	backend.SetInferenceLimiter(limiter)
	if progress != nil {
		progress(audio.Operation{State: "warming", Model: settings.TTSEngine, Message: "Testing the selected voice before activation. The current voice remains active."})
	}
	languages := []string{"italian"}
	if settings.TTSEngine == "qwen3" {
		switch settings.TTSVoice {
		case "english":
			languages = []string{"english"}
		case "italian":
		case "", "auto":
			languages = append(languages, "english")
		default:
			return errors.New("speech: invalid language")
		}
	}
	warm := func() error {
		for _, language := range languages {
			phrase := "La sintesi vocale è pronta."
			if language == "english" {
				phrase = "Voice replies are ready."
			}
			if err := backend.warmup(ctx, phrase, language, settings.Threads); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if !sameEngine {
		return warm()
	}
	// An unchanged Qwen selection must not map two full models while checking
	// new threads or language settings. Drain inference before the parent gate,
	// stop only the old child's resident process, and keep its service usable
	// if saving later fails. Both language checks share this one CPU budget.
	if limiter != nil {
		release, acquireErr := limiter.AcquireInference(ctx)
		if acquireErr != nil {
			return acquireErr
		}
		defer release()
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	s.mu.Lock()
	active := s.nativeActive
	s.mu.Unlock()
	if active != nil {
		if err := active.stopNativeResident(ctx); err != nil {
			return err
		}
	}
	// This candidate is private until activation. Warmup already owns the
	// shared budget above, so acquiring it a second time would deadlock.
	backend.SetInferenceLimiter(nil)
	defer func() {
		// Drop the checked candidate before allowing an old-voice reply to
		// reload its model between preparation and the settings commit.
		_ = backend.stopNativeResident(context.WithoutCancel(ctx))
		backend.SetInferenceLimiter(limiter)
	}()
	return warm()
}

// stopNativeResident synchronously reaps an idle backend child while leaving
// the service open. The caller owns the global budget and the parent gate.
func (s *Service) stopNativeResident(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	s.mu.Lock()
	process := s.process
	s.mu.Unlock()
	if process != nil {
		s.discard(process)
	}
	return nil
}

// DiscardPrepared stops a candidate after a failed settings commit. It cannot
// touch the currently selected model or a synthesis already serving a reply.
func (s *Service) DiscardPrepared() {
	s.mu.Lock()
	pending := s.nativePending
	s.nativePending, s.pendingEngine = nil, ""
	s.mu.Unlock()
	if pending != nil {
		pending.Close()
	}
}

// Activate is called after the settings commit with the global inference
// budget held. Switching cannot fail; errors report only incomplete cleanup.
func (s *Service) Activate(_ context.Context, settings audio.Settings) error {
	<-s.gate
	defer s.release()
	s.mu.Lock()
	old, legacy := s.nativeActive, s.process
	next := s.nativePending
	if next == nil || s.pendingEngine != settings.TTSEngine {
		// A restored selection can be activated without downloading at startup.
		// Normal configuration always supplies the successfully warmed candidate.
		next = nil
	}
	if settings.TTSEngine != "" && next == nil {
		next = s.newNativeBackend(settings.TTSEngine)
	}
	s.nativeActive, s.nativeEngine = next, settings.TTSEngine
	s.nativePending, s.pendingEngine = nil, ""
	closed := s.closed
	s.mu.Unlock()
	if old != nil && old != next {
		old.Close()
	}
	if legacy != nil && next != nil {
		s.discard(legacy)
	}
	if closed && next != nil {
		next.Close()
	}
	if settings.TTSEngine == "" {
		return nil
	}
	if err := privateDirectory(s.dir); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	var cleanup []error
	for _, name := range []string{"piper", "qwen3", "italian", "english"} {
		if name != settings.TTSEngine {
			cleanup = append(cleanup, root.RemoveAll(name))
		}
	}
	// Retain only this release's verified helper for the selected engine.
	parent := filepath.Join(s.dir, settings.TTSEngine, "runtime")
	if entries, err := os.ReadDir(parent); err == nil {
		for _, entry := range entries {
			if entry.Name() != filepath.Base(runtimeDirectory(filepath.Join(s.dir, settings.TTSEngine))) {
				cleanup = append(cleanup, root.RemoveAll(filepath.Join(settings.TTSEngine, "runtime", entry.Name())))
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanup = append(cleanup, err)
	}
	return errors.Join(cleanup...)
}
