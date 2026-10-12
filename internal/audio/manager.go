// Package audio manages private, hot-reloadable Telegram audio settings.
// It never modifies the gateway's root-owned configuration or installs helpers.
package audio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/voice"
)

const defaultHelper = "/usr/local/lib/codex-telegramgw/codex-whisper"

var (
	ErrBusy   = errors.New("audio settings are already being applied")
	ErrClosed = errors.New("audio settings manager is stopped")
)

type Settings struct {
	Enabled        bool   `json:"enabled"`
	Model          string `json:"model"`
	Threads        int    `json:"threads"`
	ReplyWithAudio bool   `json:"reply_with_audio"`
	TTSVoice       string `json:"tts_voice,omitempty"`
	TTSEngine      string `json:"tts_engine,omitempty"`
}

func (s Settings) Validate() error {
	if s.Model != "base" && s.Model != "small" {
		return errors.New("Select the base or small multilingual model.")
	}
	if s.Threads < 1 || s.Threads > 32 {
		return errors.New("CPU threads must be between 1 and 32.")
	}
	if s.TTSVoice != "" && s.TTSVoice != "auto" && s.TTSVoice != "italian" && s.TTSVoice != "english" {
		return errors.New("Select automatic, Italian, or English for audio replies.")
	}
	// Empty preserves the legacy engine in existing private settings. Only an
	// explicit selection changes that engine; loading settings never migrates it.
	if s.TTSEngine != "" && s.TTSEngine != "piper" && s.TTSEngine != "qwen3" {
		return errors.New("Select Piper / Miro or Qwen3-TTS 1.7B quantized for audio replies.")
	}
	return nil
}

type Resources struct {
	LogicalCPUs          int    `json:"logical_cpus"`
	AvailableCPUs        int    `json:"available_cpus"`
	MemoryTotalBytes     int64  `json:"memory_total_bytes"`
	MemoryAvailableBytes int64  `json:"memory_available_bytes"`
	RecommendedThreads   int    `json:"recommended_threads"`
	RecommendedModel     string `json:"recommended_model"`
}

type Model struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	SizeBytes   int64  `json:"size_bytes"`
	Installed   bool   `json:"installed"`
	Recommended bool   `json:"recommended"`
	Advanced    bool   `json:"advanced"`
}

type Availability struct {
	Available bool `json:"available"`
}

type SpeechVoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type SpeechAvailability struct {
	Available bool          `json:"available"`
	Voices    []SpeechVoice `json:"voices"`
}

// SpeechSupport supplies an optional local speech engine without coupling
// audio settings to an engine implementation. Prepare must honor cancellation
// and report only public, generic progress messages. It must not change the
// active engine or delete its files before settings have been saved.
type SpeechSupport interface {
	Available() bool
	Prepare(context.Context, Settings, func(Operation)) error
}

// SpeechInitializer selects the saved engine without I/O or a migration when
// speech support is attached at startup. Empty TTSEngine selects legacy speech.
type SpeechInitializer interface {
	SetActive(Settings)
}

// SpeechActivation is an optional commit hook. The manager drains inference
// before saving and calls Activate after the save commits. Activate must switch
// to the prepared engine even if cleanup fails; errors report cleanup warnings,
// never failed activation. The supplied context cannot be canceled after commit.
type SpeechActivation interface {
	Activate(context.Context, Settings) error
}

// SpeechPreparationDiscarder releases a staged, warmed engine after a failed
// settings commit. It must preserve the active engine and its managed files.
type SpeechPreparationDiscarder interface {
	DiscardPrepared()
}

type Operation struct {
	State           string `json:"state"`
	Model           string `json:"model"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
	TotalBytes      int64  `json:"total_bytes"`
	Message         string `json:"message"`
}

type Status struct {
	Settings  Settings           `json:"settings"`
	Resources Resources          `json:"resources"`
	Models    []Model            `json:"models"`
	Helper    Availability       `json:"helper"`
	TTS       SpeechAvailability `json:"tts"`
	Operation Operation          `json:"operation"`
	Busy      bool               `json:"busy"`
}

type savedSettings struct {
	Version       int      `json:"version"`
	Settings      Settings `json:"settings"`
	ModelLocation string   `json:"model_location"`
}

type dependencies struct {
	models    []modelSpec
	http      *http.Client
	preflight func(context.Context, config.VoiceConfig) error
}

type Manager struct {
	dir       string
	helper    string
	deps      dependencies
	mu        sync.RWMutex
	settings  Settings
	voice     config.VoiceConfig
	operation Operation
	installed map[string]bool
	busy      bool
	closed    bool
	speech    SpeechSupport
	queue     chan Settings
	changed   chan struct{}
	inference chan struct{}
	done      chan struct{}
	runOnce   sync.Once
}

// New uses stateDir/audio only when the caller passes that directory. The
// gateway passes filepath.Join(filepath.Dir(cfg.DatabasePath), "audio").
// A missing private settings file preserves the existing voice configuration.
func New(stateDir string, fallback config.VoiceConfig) (*Manager, error) {
	return newManager(stateDir, fallback, dependencies{models: pinnedModels(), http: &http.Client{Timeout: 10 * time.Minute}, preflight: helperPreflight})
}

func newManager(stateDir string, fallback config.VoiceConfig, deps dependencies) (*Manager, error) {
	if !filepath.IsAbs(stateDir) || strings.ContainsRune(stateDir, '\x00') {
		return nil, errors.New("audio: state directory must be an absolute local path")
	}
	if err := privateDirectory(stateDir); err != nil {
		return nil, err
	}
	if fallback.HelperPath == "" {
		fallback.HelperPath = defaultHelper
	}
	if !filepath.IsAbs(fallback.HelperPath) || strings.ContainsRune(fallback.HelperPath, '\x00') {
		return nil, errors.New("audio: helper must be an absolute local path")
	}
	if fallback.ModelPath == "" {
		fallback.ModelPath = deps.models[0].systemPath
	}
	if fallback.Threads == 0 {
		fallback.Threads = 2
	}
	model := "base"
	if filepath.Base(fallback.ModelPath) == "ggml-small.bin" {
		model = "small"
	}
	m := &Manager{dir: stateDir, helper: fallback.HelperPath, deps: deps, settings: Settings{Enabled: fallback.Enabled, Model: model, Threads: fallback.Threads, TTSVoice: "auto"}, voice: fallback, operation: Operation{State: "idle"}, installed: make(map[string]bool), queue: make(chan Settings, 1), changed: make(chan struct{})}
	m.inference = make(chan struct{}, 1)
	m.inference <- struct{}{}
	m.done = make(chan struct{})
	if err := m.settings.Validate(); err != nil {
		return nil, err
	}
	data, err := readPrivate(filepath.Join(stateDir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		if !fallback.Enabled {
			m.settings.Threads = systemResources().RecommendedThreads
			m.voice.Threads = m.settings.Threads
		}
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var saved savedSettings
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if d.Decode(&saved) != nil || d.Decode(&struct{}{}) != io.EOF || saved.Version != 1 || saved.Settings.Validate() != nil || (saved.ModelLocation != "system" && saved.ModelLocation != "managed") {
		return nil, errors.New("audio: private settings are invalid")
	}
	spec := m.model(saved.Settings.Model)
	if saved.Settings.TTSVoice == "" {
		saved.Settings.TTSVoice = "auto"
	}
	path := spec.systemPath
	if saved.ModelLocation == "managed" {
		path = m.managedPath(spec)
	}
	if saved.Settings.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		var err error
		if saved.ModelLocation == "managed" {
			err = privateDirectory(filepath.Dir(path))
		}
		if err == nil {
			err = verifyModel(ctx, path, spec)
		}
		cancel()
		if err != nil {
			m.operation = Operation{State: "failed", Model: saved.Settings.Model, Message: "The saved audio model failed verification. Previous gateway voice settings remain active."}
			return m, nil
		}
		m.installed[spec.name] = true
	}
	m.settings = saved.Settings
	m.voice = m.configFor(saved.Settings, path)
	return m, nil
}

func (m *Manager) model(name string) modelSpec {
	for _, model := range m.deps.models {
		if model.name == name {
			return model
		}
	}
	panic("audio: invalid model")
}

func (m *Manager) managedPath(spec modelSpec) string {
	return filepath.Join(m.dir, "models", "ggml-"+spec.name+".bin")
}

func (m *Manager) configFor(settings Settings, path string) config.VoiceConfig {
	return config.VoiceConfig{Enabled: settings.Enabled, HelperPath: m.helper, ModelPath: path, Threads: settings.Threads}
}

func (m *Manager) VoiceConfig() config.VoiceConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.voice
}

// Changed broadcasts committed settings to every subscriber. Capture this
// channel before reading Settings/VoiceConfig to avoid missing a change.
func (m *Manager) Changed() <-chan struct{} { m.mu.RLock(); defer m.mu.RUnlock(); return m.changed }

func (m *Manager) Settings() Settings { m.mu.RLock(); defer m.mu.RUnlock(); return m.settings }

// AcquireInference shares one CPU budget between transcription and speech
// synthesis. The returned release function is safe to call more than once.
func (m *Manager) AcquireInference(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done:
		return nil, ErrClosed
	case <-m.inference:
	}
	var once sync.Once
	release := func() { once.Do(func() { m.inference <- struct{}{} }) }
	select {
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-m.done:
		release()
		return nil, ErrClosed
	default:
		return release, nil
	}
}

func (m *Manager) SetSpeechSupport(support SpeechSupport) {
	m.mu.Lock()
	m.speech = support
	settings := m.settings
	m.mu.Unlock()
	if initializer, ok := support.(SpeechInitializer); ok {
		initializer.SetActive(settings)
	}
}

func (m *Manager) Snapshot() Status {
	resources := systemResources()
	m.mu.RLock()
	speech := m.speech
	status := Status{Settings: m.settings, Resources: resources, Helper: Availability{Available: helperAvailable(m.helper)}, Operation: m.operation, Busy: m.busy}
	for _, spec := range m.deps.models {
		status.Models = append(status.Models, Model{Name: spec.name, Label: "Whisper " + spec.name + " multilingual", SizeBytes: spec.size, Installed: m.installed[spec.name], Recommended: spec.name == "base", Advanced: spec.name == "small"})
	}
	m.mu.RUnlock()
	status.TTS = SpeechAvailability{Voices: []SpeechVoice{{"auto", "Match the voice note (Italian or English)"}, {"italian", "Italian"}, {"english", "English"}}}
	if speech != nil {
		status.TTS.Available = speech.Available()
	}
	return status
}

// Apply admits one request immediately; Run performs all expensive work. A
// failed download, preflight, or write leaves the previous settings effective.
func (m *Manager) Apply(settings Settings) error {
	if settings.TTSVoice == "" {
		settings.TTSVoice = "auto"
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.busy {
		return ErrBusy
	}
	m.busy = true
	m.operation = Operation{State: "queued", Model: settings.Model, Message: "Audio settings queued."}
	m.queue <- settings
	return nil
}

// Run must share the gateway shutdown context. Downloads, hashes, and helper
// startup all honor cancellation, and no privileged commands are executed.
func (m *Manager) Run(ctx context.Context) {
	m.runOnce.Do(func() { m.run(ctx) })
}

func (m *Manager) run(ctx context.Context) {
	defer func() {
		m.mu.Lock()
		m.closed = true
		close(m.done)
		if m.busy {
			m.operation = Operation{State: "failed", Model: m.operation.Model, Message: "Audio configuration was canceled."}
			m.busy = false
		}
		m.mu.Unlock()
	}()
	// Inventory is verified once in the background, never on each poll.
	for _, spec := range m.deps.models {
		if ctx.Err() != nil {
			return
		}
		managed := m.managedPath(spec)
		installed := verifyModel(ctx, spec.systemPath, spec) == nil || (isPrivateDirectory(filepath.Dir(managed)) && verifyModel(ctx, managed, spec) == nil)
		m.mu.Lock()
		m.installed[spec.name] = installed
		m.mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case settings := <-m.queue:
			if ctx.Err() != nil {
				return
			}
			m.configure(ctx, settings)
		}
	}
}

func (m *Manager) configure(ctx context.Context, settings Settings) {
	spec := m.model(settings.Model)
	path, location := spec.systemPath, "system"
	m.mu.RLock()
	previous := m.voice
	previousSettings := m.settings
	speech := m.speech
	m.mu.RUnlock()
	prepared, committed := false, false
	defer func() {
		if prepared && !committed {
			if discarder, ok := speech.(SpeechPreparationDiscarder); ok {
				discarder.DiscardPrepared()
			}
		}
	}()
	if settings.Enabled {
		if !helperAvailable(m.helper) {
			m.failed("The local Whisper helper is unavailable. Install voice support on the gateway first.")
			return
		}
		m.stage("checking", "Checking the selected model.", spec)
		if verifyModel(ctx, path, spec) != nil {
			path, location = m.managedPath(spec), "managed"
			if err := privateDirectory(filepath.Dir(path)); err != nil {
				m.failed("The local model storage is unavailable.")
				return
			}
			if verifyModel(ctx, path, spec) != nil {
				if err := m.download(ctx, spec, path); err != nil {
					m.failed(err.Error())
					return
				}
			}
		}
		if ctx.Err() != nil {
			m.failed("Audio configuration was canceled.")
			return
		}
		m.mu.Lock()
		m.installed[spec.name] = true
		m.mu.Unlock()
		m.stage("preflight", "Loading the verified model with the local helper.", spec)
		preflightCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		release, err := m.AcquireInference(preflightCtx)
		if err == nil {
			err = m.deps.preflight(preflightCtx, m.configFor(settings, path))
			release()
		}
		cancel()
		if err != nil {
			m.failed("The local Whisper helper could not load the verified model. Previous settings remain active.")
			return
		}
	} else if settings.Model == previousSettings.Model {
		// Disabling never needs a working helper or a model download.
		path = previous.ModelPath
		if path == m.managedPath(spec) {
			location = "managed"
		}
	}
	if settings.Enabled && settings.ReplyWithAudio {
		if speech == nil || !speech.Available() {
			m.failed("The local audio reply engine is unavailable. Previous settings remain active.")
			return
		}
		m.stage("preflight", "Preparing the local audio reply engine.", spec)
		err := speech.Prepare(ctx, settings, func(operation Operation) {
			m.mu.Lock()
			m.operation = operation
			m.mu.Unlock()
		})
		if err != nil {
			m.failed("The local audio reply engine could not be prepared. Previous settings remain active.")
			return
		}
		prepared = true
	}
	if ctx.Err() != nil {
		m.failed("Audio configuration was canceled.")
		return
	}
	var activation SpeechActivation
	if settings.Enabled && settings.ReplyWithAudio {
		activation, _ = speech.(SpeechActivation)
	}
	if activation != nil {
		// Drain old synthesis before the commit point. Cancellation or a failed
		// save leaves the prepared engine staged and the previous one active.
		release, err := m.AcquireInference(ctx)
		if err != nil {
			m.failed("Audio configuration was canceled.")
			return
		}
		defer release()
	}
	m.stage("applying", "Saving audio settings.", spec)
	if err := m.save(savedSettings{Version: 1, Settings: settings, ModelLocation: location}); err != nil {
		m.failed("Audio settings could not be saved. Previous settings remain active.")
		return
	}
	message := "Audio settings saved."
	if activation != nil {
		if err := activation.Activate(context.WithoutCancel(ctx), settings); err != nil {
			message = "Audio settings saved. The selected audio reply engine is active, but unused model files could not all be removed."
		}
	}
	committed = true
	m.mu.Lock()
	m.settings, m.voice = settings, m.configFor(settings, path)
	if settings.Enabled {
		m.installed[spec.name] = true
	}
	m.busy = false
	m.operation = Operation{State: "ready", Model: settings.Model, Message: message}
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
}

func (m *Manager) stage(state, message string, spec modelSpec) {
	m.mu.Lock()
	m.operation = Operation{State: state, Model: spec.name, TotalBytes: spec.size, Message: message}
	m.mu.Unlock()
}

func (m *Manager) failed(message string) {
	m.mu.Lock()
	m.operation.State, m.operation.Message, m.busy = "failed", message, false
	m.mu.Unlock()
}

func helperAvailable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

func helperPreflight(ctx context.Context, cfg config.VoiceConfig) error {
	client, err := voice.New(voice.Config{BinaryPath: cfg.HelperPath, ModelPath: cfg.ModelPath, Threads: cfg.Threads})
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Start(ctx)
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return errors.New("audio: could not create private state directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("audio: state directory must be a local directory without symlinks")
	}
	if err := os.Chmod(path, 0700); err != nil {
		return errors.New("audio: could not protect private state directory")
	}
	return nil
}

func isPrivateDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0077 == 0
}

func readPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return nil, errors.New("audio: settings must be a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 8193))
}

func (m *Manager) save(saved savedSettings) error {
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(m.dir, ".settings-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(m.dir, "settings.json")); err != nil {
		return err
	}
	// The rename is the commit point. Keep memory and the committed file in
	// agreement even on filesystems which cannot sync directory handles.
	if dir, err := os.Open(m.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
