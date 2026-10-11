package releasemanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const voiceModelPath = "/usr/local/share/codex-whisper/ggml-base.bin"
const voiceModelRevision = "5359861c739e955e79d9a303bcbc70fb988958b1"
const voiceModelSHA256 = "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe"
const voiceModelSize int64 = 147951465
const voiceModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/" + voiceModelRevision + "/ggml-base.bin?download=true"
const voiceSmallModelPath = "/usr/local/share/codex-whisper/ggml-small.bin"
const voiceSmallModelRevision = "5359861c739e955e79d9a303bcbc70fb988958b1"
const voiceSmallModelSHA256 = "1be3a9b2063867b937e64e2ec7483364a79917e157fa98c5d94b5c1fffea987b"
const voiceSmallModelSize int64 = 487601967
const voiceSmallModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/" + voiceSmallModelRevision + "/ggml-small.bin?download=true"

// VoiceSetupOptions selects a pinned multilingual model and its CPU threads.
// Empty Model and zero Threads preserve supported installed settings, or use
// base and two threads when voice has not been configured.
type VoiceSetupOptions struct {
	Model   string
	Threads int
}

type voiceModelSpec struct {
	Name, Path, URL, SHA256 string
	Size                    int64
}

func pinnedVoiceModel(name string) (voiceModelSpec, error) {
	switch name {
	case "base":
		return voiceModelSpec{"base", voiceModelPath, voiceModelURL, voiceModelSHA256, voiceModelSize}, nil
	case "small":
		return voiceModelSpec{"small", voiceSmallModelPath, voiceSmallModelURL, voiceSmallModelSHA256, voiceSmallModelSize}, nil
	default:
		return voiceModelSpec{}, errors.New("voice model must be base or small")
	}
}

func validateVoiceSetupOptions(opts VoiceSetupOptions) error {
	if opts.Model != "" {
		if _, err := pinnedVoiceModel(opts.Model); err != nil {
			return err
		}
	}
	if opts.Threads < 0 || opts.Threads > 32 {
		return errors.New("voice threads must be between 1 and 32")
	}
	return nil
}

func parseVoiceSetupOptions(args []string) (VoiceSetupOptions, error) {
	var opts VoiceSetupOptions
	flags := flag.NewFlagSet("setup voice", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.Model, "model", "", "multilingual Whisper model: base or small")
	flags.IntVar(&opts.Threads, "threads", 0, "CPU threads: 1 through 32")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return opts, errors.New("usage: codex-telegramgw setup voice [--model base|small] [--threads 1..32]")
	}
	var emptyModel, zeroThreads bool
	flags.Visit(func(f *flag.Flag) {
		emptyModel = emptyModel || (f.Name == "model" && opts.Model == "")
		zeroThreads = zeroThreads || (f.Name == "threads" && opts.Threads == 0)
	})
	if emptyModel {
		return opts, errors.New("voice model must be base or small")
	}
	if zeroThreads {
		return opts, errors.New("voice threads must be between 1 and 32")
	}
	return opts, validateVoiceSetupOptions(opts)
}

func resolveVoiceSetupOptions(cfg map[string]any, opts VoiceSetupOptions) (voiceModelSpec, int, error) {
	if err := validateVoiceSetupOptions(opts); err != nil {
		return voiceModelSpec{}, 0, err
	}
	voice, _ := cfg["voice"].(map[string]any)
	if opts.Model == "" {
		opts.Model = "base"
		if path, exists := voice["model_path"]; exists && path != "" {
			switch path {
			case voiceModelPath:
			case voiceSmallModelPath:
				opts.Model = "small"
			default:
				return voiceModelSpec{}, 0, errors.New("configured Whisper model is unsupported by setup; select --model base or --model small")
			}
		}
	}
	if opts.Threads == 0 {
		opts.Threads = 2
		if saved, exists := voice["threads"]; exists {
			threads, ok := saved.(float64)
			if !ok || threads < 0 || threads > 32 || threads != float64(int(threads)) {
				return voiceModelSpec{}, 0, errors.New("configured voice threads are invalid; select --threads between 1 and 32")
			}
			if threads != 0 {
				opts.Threads = int(threads)
			}
		}
	}
	model, err := pinnedVoiceModel(opts.Model)
	return model, opts.Threads, err
}

func validateVoiceModelInstallation(path string, stat func(string) (os.FileInfo, error)) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := stat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if err := validateGatewayInstallerPath(info, current == path); err != nil || (current != path && !info.Mode().IsDir()) {
				return errors.New("Whisper model and its installation directories must be root-owned without symlinks or group/world write access")
			}
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func gatewayVoiceEnabled(path string) (bool, error) {
	cfg, err := ReadJSON(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	voice, _ := cfg["voice"].(map[string]any)
	enabled, _ := voice["enabled"].(bool)
	return enabled, nil
}

// SetupVoice installs an authenticated release matching the installed gateway,
// verifies the pinned multilingual model, then enables gateway voice support.
func (m *Manager) SetupVoice(ctx context.Context) error {
	return m.SetupVoiceWithOptions(ctx, VoiceSetupOptions{})
}

// SetupVoiceWithOptions installs voice using supported pinned model settings.
func (m *Manager) SetupVoiceWithOptions(ctx context.Context, opts VoiceSetupOptions) error {
	if err := validateVoiceSetupOptions(opts); err != nil {
		return err
	}
	l, err := NewLayout("gateway")
	if err != nil {
		return err
	}
	if err = l.RequireUser(); err != nil {
		return err
	}
	unlock, err := Lock(l.Lock)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
	}
	model, threads, err := resolveVoiceSetupOptionsWithAudio(l, cfg, opts)
	if err != nil {
		return err
	}
	if err := validateVoiceModelInstallation(model.Path, os.Lstat); err != nil {
		return err
	}
	settings, err := SavedSettings(l)
	if err != nil {
		return errors.New("set up or adopt the gateway before enabling voice")
	}
	repo, _ := settings["repo"].(string)
	if repo == "" {
		repo = DefaultRepo
	}
	installed, err := m.command(ctx, l.Binary, "version")
	if err != nil {
		return err
	}
	release, err := m.FetchRelease(ctx, repo, strings.TrimSpace(string(installed)))
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "codex-voice-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	pkg, err := m.Package(ctx, release, l, "codex-whisper", stage)
	if err != nil {
		return err
	}
	return m.installVoiceModel(ctx, l, filepath.Join(pkg, "codex-whisper"), model.Path, filepath.Join(pkg, "native/whisper"), model, threads)
}

func modelRedirectURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := u.Hostname()
	return host == "huggingface.co" || host == "hf.co" || strings.HasSuffix(host, ".hf.co") || host == "cdn-lfs.huggingface.co" || host == "cdn-lfs-us-1.huggingface.co"
}

// Stream the pinned model to its final filesystem while hashing it. Signed CDN
// redirect URLs and transport errors never appear in diagnostics.
func (m *Manager) downloadVoiceModel(ctx context.Context, target string) (string, error) {
	model, _ := pinnedVoiceModel("base")
	return m.downloadPinnedVoiceModel(ctx, target, model)
}

func (m *Manager) downloadPinnedVoiceModel(ctx context.Context, target string, model voiceModelSpec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".ggml-"+model.Name+"-download-")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(f.Name())
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, model.URL, nil)
	if err != nil {
		return "", errors.New("could not construct model download")
	}
	request.Header.Set("User-Agent", "codex-telegramgw-release-manager")
	client := *m.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !modelRedirectURL(req.URL) {
			return errors.New("unexpected model download redirect")
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("Whisper model download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Whisper model download returned HTTP %d", response.StatusCode)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(response.Body, model.Size+1))
	if err != nil || n != model.Size || hex.EncodeToString(hash.Sum(nil)) != model.SHA256 {
		return "", errors.New("Whisper model checksum or size verification failed")
	}
	if err := f.Chmod(0644); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	keep = true
	return f.Name(), nil
}

func (m *Manager) installVoice(ctx context.Context, l *Layout, helper, model, notices string) (retErr error) {
	spec, _ := pinnedVoiceModel("base")
	return m.installVoiceModel(ctx, l, helper, model, notices, spec, 2)
}

func (m *Manager) installVoiceModel(ctx context.Context, l *Layout, helper, model, notices string, spec voiceModelSpec, threads int) (retErr error) {
	if _, err := ReadJSON(l.Config); err != nil {
		return err
	}
	// Reject symlinks and corrupt pre-existing models before touching service files.
	newModel := false
	if info, err := os.Lstat(model); errors.Is(err, os.ErrNotExist) {
		newModel = true
	} else if err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return errors.New("Whisper model must be a regular file")
	} else if info.Size() != spec.Size {
		return fmt.Errorf("existing Whisper %s model failed verification; move it aside before running setup voice again", spec.Name)
	} else if err := VerifyFile(model, spec.SHA256); err != nil {
		return fmt.Errorf("existing Whisper %s model failed verification; move it aside before running setup voice again", spec.Name)
	}
	if newModel {
		fmt.Fprintf(m.Out, "Downloading the multilingual Whisper %s model (%d MB).\n", spec.Name, (spec.Size+500000)/1000000)
		download, err := m.downloadPinnedVoiceModel(ctx, model, spec)
		if err != nil {
			return err
		}
		defer os.Remove(download)
		if err := os.Rename(download, model); err != nil {
			return err
		}
		if err := SyncDir(filepath.Dir(model)); err != nil {
			return err
		}
	}
	// Loading once validates CPU support and model compatibility before restart.
	ready, err := m.command(ctx, helper, "--model", model, "--threads", strconv.Itoa(threads))
	if err != nil {
		return errors.New("Whisper helper could not load the verified model")
	}
	var status struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
	}
	if json.Unmarshal(ready, &status) != nil || status.Type != "ready" || status.Protocol != 1 {
		return errors.New("Whisper helper did not acknowledge protocol version 1")
	}
	return m.applyVoiceModel(ctx, l, helper, model, notices, spec.Name, threads)
}

// applyVoice runs only after package provenance, helper version, model digest,
// and a real helper/model startup have been verified.
func (m *Manager) applyVoice(ctx context.Context, l *Layout, helper, model, notices string) (retErr error) {
	return m.applyVoiceModel(ctx, l, helper, model, notices, "base", 2)
}

func (m *Manager) applyVoiceModel(ctx context.Context, l *Layout, helper, model, notices, modelName string, threads int) (retErr error) {
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
	}
	audioState, err := openVoiceAudioState(l, cfg)
	if err != nil {
		return err
	}
	if audioState != nil {
		defer audioState.close()
	}
	voice, _ := cfg["voice"].(map[string]any)
	if voice == nil {
		voice = map[string]any{}
	}
	if err := os.MkdirAll(l.Backups, 0700); err != nil {
		return err
	}
	backup, err := os.MkdirTemp(l.Backups, "voice-")
	if err != nil {
		return err
	}
	helperPath := filepath.Join(l.Bin, "codex-whisper")
	configSnapshot, err := snapshotUpdateFile(l.Config, filepath.Join(backup, "gateway.json"))
	if err != nil {
		return err
	}
	helperSnapshot, err := snapshotUpdateFile(helperPath, filepath.Join(backup, "codex-whisper"))
	if err != nil {
		return err
	}
	if audioState != nil {
		if err := AtomicWrite(filepath.Join(backup, "audio-settings.json"), audioState.original, 0600, nil); err != nil {
			return err
		}
	}
	changed, audioChanged, restartAttempted := false, false, false
	defer func() {
		if retErr == nil {
			return
		}
		if changed {
			retErr = errors.Join(retErr, configSnapshot.restore(), helperSnapshot.restore())
		}
		if audioChanged {
			_, err := audioState.replace(audioState.original)
			retErr = errors.Join(retErr, err)
		}
		if restartAttempted {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			retErr = errors.Join(retErr, m.Service(cleanup, l, "restart"))
		}
	}()
	changed = true
	if err := AtomicCopy(helper, helperPath, 0755); err != nil {
		return err
	}
	// Notices are public, immutable source/model licensing material.
	files := []string{"NOTICE.md", "source.env"}
	licenses, err := os.ReadDir(filepath.Join(notices, "licenses"))
	if err != nil {
		return err
	}
	for _, license := range licenses {
		if !license.Type().IsRegular() {
			return errors.New("release license is not a regular file")
		}
		files = append(files, filepath.Join("licenses", license.Name()))
	}
	for _, name := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(filepath.Dir(model), name)), 0755); err != nil {
			return err
		}
		if err := AtomicCopy(filepath.Join(notices, name), filepath.Join(filepath.Dir(model), name), 0644); err != nil {
			return err
		}
	}
	voice["enabled"], voice["helper_path"], voice["model_path"], voice["threads"] = true, helperPath, model, threads
	cfg["voice"] = voice
	if err := WriteJSON(l.Config, cfg); err != nil {
		return err
	}
	if audioState != nil {
		audioChanged, err = audioState.apply(modelName, threads)
		if err != nil {
			return err
		}
	}
	restartAttempted = true
	if err := m.Service(ctx, l, "restart"); err != nil {
		return err
	}
	if err := m.GatewayReady(ctx, l); err != nil {
		return errors.New("gateway did not become ready after enabling voice")
	}
	fmt.Fprintf(m.Out, "Voice transcription enabled with the multilingual %s model and %d threads. Backup: %s\n", modelName, threads, backup)
	return nil
}
