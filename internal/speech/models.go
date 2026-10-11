package speech

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

const modelRepository = "kyutai/pocket-tts-without-voice-cloning"
const modelRevision = "1e08e6a23401048648a9fdcfde2f89348215c2a7"

type asset struct {
	language string
	name     string
	remote   string
	size     int64
	sha256   string
}

// This immutable, ungated snapshot supplies six-layer models and stock voices.
// Downloads never accept a URL or a filename from an administrator or a turn.
func pinnedAssets() []asset {
	return []asset{
		{"english", "model.safetensors", "model.safetensors", 219029196, "916ccd2686e9311cb40054893a3c4284393d658825ffc714a276f3e9b152344f"},
		{"english", "tokenizer.model", "tokenizer.model", 59339, "d461765ae179566678c93091c5fa6f2984c31bbe990bf1aa62d92c64d91bc3f6"},
		{"english", "voice.safetensors", "embeddings/alba.safetensors", 6195000, "d291428b416d6c36a1de7835e51dbe1e334b75e5af512bb18dd23a1047fe8f3b"},
		{"italian", "model.safetensors", "model.safetensors", 219029196, "16365c43eaabee9b222cd414d6a48f0976b4800a21fe5d55253f645eff7eeb1f"},
		{"italian", "tokenizer.model", "tokenizer.model", 60905, "1ef32ed65d0264239fb9d9ecb36a089a637f6e6c5c5a1a8e7df37ce486ec21da"},
		{"italian", "voice.safetensors", "embeddings/giovanni.safetensors", 4130608, "5cede7852c7076f611750e4a8d3c95d19c6386852abc55940b3279f775fc5f9b"},
	}
}

func (a asset) url() string {
	return "https://huggingface.co/" + modelRepository + "/resolve/" + modelRevision + "/languages/" + a.language + "/" + a.remote + "?download=true"
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("speech: private model storage is unavailable")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func verifyAsset(ctx context.Context, path string, spec asset) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != spec.size {
		return errors.New("speech: model verification failed")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("speech: model changed during verification")
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, io.LimitReader(f, spec.size+1)})
	if err != nil {
		return err
	}
	if n != spec.size || hex.EncodeToString(h.Sum(nil)) != spec.sha256 {
		return errors.New("speech: model verification failed")
	}
	return nil
}

func allowedURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "huggingface.co" || host == "hf.co" || strings.HasSuffix(host, ".hf.co") || host == "cdn-lfs.huggingface.co" || host == "cdn-lfs-us-1.huggingface.co"
}

type downloadProgress func(int64)

func (f downloadProgress) Write(p []byte) (int, error) {
	if f != nil {
		f(int64(len(p)))
	}
	return len(p), nil
}

func downloadAsset(ctx context.Context, client *http.Client, target string, spec asset, progress downloadProgress) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := privateDirectory(filepath.Dir(target)); err != nil {
		return errors.New("The local speech model storage is unavailable.")
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".download-")
	if err != nil {
		return errors.New("The local speech model storage is unavailable.")
	}
	defer f.Close()
	defer os.Remove(f.Name())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.url(), nil)
	if err != nil || !allowedURL(request.URL) {
		return errors.New("The pinned speech model download is unavailable.")
	}
	request.Header.Set("User-Agent", "codex-telegramgw-speech")
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !allowedURL(req.URL) {
			return errors.New("speech: unexpected model redirect")
		}
		return nil
	}
	response, err := c.Do(request)
	if err != nil {
		return errors.New("The speech model download failed.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > spec.size {
		return errors.New("The speech model download failed.")
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h, progress), contextReader{ctx, io.LimitReader(response.Body, spec.size+1)})
	if err != nil {
		return errors.New("The speech model download failed.")
	}
	if n != spec.size || hex.EncodeToString(h.Sum(nil)) != spec.sha256 {
		return errors.New("The speech model checksum or size verification failed.")
	}
	if f.Chmod(0600) != nil || f.Sync() != nil || f.Close() != nil {
		return errors.New("The verified speech model could not be saved.")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return errors.New("The verified speech model could not be saved.")
	}
	if dir, err := os.Open(filepath.Dir(target)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
