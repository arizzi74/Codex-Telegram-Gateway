package releasemanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const voiceModelPath = "/usr/local/share/codex-whisper/ggml-base.bin"
const voiceModelRevision = "5359861c739e955e79d9a303bcbc70fb988958b1"
const voiceModelSHA256 = "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe"
const voiceModelSize int64 = 147951465
const voiceModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/" + voiceModelRevision + "/ggml-base.bin?download=true"

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
	l, err := NewLayout("gateway")
	if err != nil {
		return err
	}
	if err = l.RequireUser(); err != nil {
		return err
	}
	for _, path := range []string{filepath.Dir(voiceModelPath), voiceModelPath} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := validateGatewayInstallerPath(info, false); err != nil {
			return errors.New("Whisper model installation paths must be root-owned without symlinks or group/world write access")
		}
	}
	unlock, err := Lock(l.Lock)
	if err != nil {
		return err
	}
	defer unlock()
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
	return m.installVoice(ctx, l, filepath.Join(pkg, "codex-whisper"), voiceModelPath, filepath.Join(pkg, "native/whisper"))
}

func modelRedirectURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := u.Hostname()
	return host == "huggingface.co" || host == "hf.co" || strings.HasSuffix(host, ".hf.co") || host == "cdn-lfs.huggingface.co" || host == "cdn-lfs-us-1.huggingface.co"
}

// Stream the 148 MB model to its final filesystem while hashing it. Signed CDN
// redirect URLs and transport errors never appear in diagnostics.
func (m *Manager) downloadVoiceModel(ctx context.Context, target string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".ggml-base-download-")
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, voiceModelURL, nil)
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
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(response.Body, voiceModelSize+1))
	if err != nil || n != voiceModelSize || hex.EncodeToString(hash.Sum(nil)) != voiceModelSHA256 {
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
	} else if err := VerifyFile(model, voiceModelSHA256); err != nil {
		return errors.New("existing Whisper base model failed verification; move it aside before running setup voice again")
	}
	if newModel {
		fmt.Fprintln(m.Out, "Downloading the multilingual Whisper base model (148 MB).")
		download, err := m.downloadVoiceModel(ctx, model)
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
	ready, err := m.command(ctx, helper, "--model", model, "--threads", "2")
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
	return m.applyVoice(ctx, l, helper, model, notices)
}

// applyVoice runs only after package provenance, helper version, model digest,
// and a real helper/model startup have been verified.
func (m *Manager) applyVoice(ctx context.Context, l *Layout, helper, model, notices string) (retErr error) {
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
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
	changed, restartAttempted := false, false
	defer func() {
		if retErr == nil {
			return
		}
		if changed {
			retErr = errors.Join(retErr, configSnapshot.restore(), helperSnapshot.restore())
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
	voice["enabled"], voice["helper_path"], voice["model_path"], voice["threads"] = true, helperPath, model, 2
	cfg["voice"] = voice
	if err := WriteJSON(l.Config, cfg); err != nil {
		return err
	}
	restartAttempted = true
	if err := m.Service(ctx, l, "restart"); err != nil {
		return err
	}
	if err := m.GatewayReady(ctx, l); err != nil {
		return errors.New("gateway did not become ready after enabling voice")
	}
	fmt.Fprintf(m.Out, "Voice transcription enabled with the multilingual base model. Backup: %s\n", backup)
	return nil
}
