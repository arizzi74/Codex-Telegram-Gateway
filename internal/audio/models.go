package audio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Match the release manager's pinned multilingual models. Neither URLs nor
// destination paths are accepted from browser requests or private settings.
const modelRevision = "5359861c739e955e79d9a303bcbc70fb988958b1"

type modelSpec struct {
	name, systemPath, url, sha256 string
	size                          int64
}

func pinnedModels() []modelSpec {
	return []modelSpec{
		{"base", "/usr/local/share/codex-whisper/ggml-base.bin", "https://huggingface.co/ggerganov/whisper.cpp/resolve/" + modelRevision + "/ggml-base.bin?download=true", "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe", 147951465},
		{"small", "/usr/local/share/codex-whisper/ggml-small.bin", "https://huggingface.co/ggerganov/whisper.cpp/resolve/" + modelRevision + "/ggml-small.bin?download=true", "1be3a9b2063867b937e64e2ec7483364a79917e157fa98c5d94b5c1fffea987b", 487601967},
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func verifyModel(ctx context.Context, path string, spec modelSpec) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != spec.size {
		return errors.New("model failed verification")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("model changed during verification")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, contextReader{ctx, io.LimitReader(f, spec.size+1)})
	if err != nil {
		return err
	}
	if n != spec.size || hex.EncodeToString(hash.Sum(nil)) != spec.sha256 {
		return errors.New("model failed verification")
	}
	return nil
}

func allowedModelURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := u.Hostname()
	return host == "huggingface.co" || host == "hf.co" || strings.HasSuffix(host, ".hf.co") || host == "cdn-lfs.huggingface.co" || host == "cdn-lfs-us-1.huggingface.co"
}

type progressWriter struct{ m *Manager }

func (w progressWriter) Write(p []byte) (int, error) {
	w.m.mu.Lock()
	w.m.operation.DownloadedBytes += int64(len(p))
	w.m.mu.Unlock()
	return len(p), nil
}

func (m *Manager) download(ctx context.Context, spec modelSpec, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := privateDirectory(filepath.Dir(target)); err != nil {
		return errors.New("The local model storage is unavailable.")
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".model-download-")
	if err != nil {
		return errors.New("The local model storage is unavailable.")
	}
	defer file.Close()
	defer os.Remove(file.Name())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.url, nil)
	if err != nil || !allowedModelURL(request.URL) {
		return errors.New("The pinned model download is unavailable.")
	}
	request.Header.Set("User-Agent", "codex-telegramgw-audio")
	client := *m.deps.http
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !allowedModelURL(req.URL) {
			return errors.New("unexpected model redirect")
		}
		return nil
	}
	m.stage("downloading", "Downloading the selected multilingual model.", spec)
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Whisper model download failed. Previous settings remain active.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Whisper model download failed. Previous settings remain active.")
	}
	if response.ContentLength > spec.size {
		return errors.New("Whisper model size verification failed. Previous settings remain active.")
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash, progressWriter{m}), contextReader{ctx, io.LimitReader(response.Body, spec.size+1)})
	if err != nil {
		return errors.New("Whisper model download failed. Previous settings remain active.")
	}
	if n != spec.size || hex.EncodeToString(hash.Sum(nil)) != spec.sha256 {
		return errors.New("Whisper model checksum or size verification failed. Previous settings remain active.")
	}
	if err := file.Chmod(0600); err != nil {
		return errors.New("The verified model could not be saved.")
	}
	if err := file.Sync(); err != nil {
		return errors.New("The verified model could not be saved.")
	}
	if err := file.Close(); err != nil {
		return errors.New("The verified model could not be saved.")
	}
	if ctx.Err() != nil {
		return errors.New("Audio configuration was canceled.")
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return errors.New("The verified model could not be saved.")
	}
	if dir, err := os.Open(filepath.Dir(target)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
