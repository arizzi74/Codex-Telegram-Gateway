package speech

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/audio"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func testAsset(language, name string, data []byte) asset {
	h := sha256.Sum256(data)
	return asset{language, name, name, int64(len(data)), hex.EncodeToString(h[:])}
}

type testInferenceLimiter struct {
	budget    chan struct{}
	requested chan struct{}
}

func (l *testInferenceLimiter) AcquireInference(ctx context.Context) (func(), error) {
	select {
	case l.requested <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.budget:
	}
	var once sync.Once
	return func() { once.Do(func() { l.budget <- struct{}{} }) }, nil
}

func TestPrepareDownloadAndWarmupDoNotInvertInferenceLocks(t *testing.T) {
	s := fakeService(t, "ok")
	limiter := &testInferenceLimiter{budget: make(chan struct{}, 1), requested: make(chan struct{}, 2)}
	limiter.budget <- struct{}{}
	s.SetInferenceLimiter(limiter)
	data := []byte("public model fixture")
	s.assets = []asset{testAsset("italian", "model.safetensors", data)}
	started, downloaded := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(downloaded) })
	s.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-downloaded:
			return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
		}
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	prepared := make(chan error, 1)
	go func() {
		prepared <- s.Prepare(ctx, audio.Settings{Threads: 2, TTSVoice: "italian"}, nil)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("download did not start")
	}
	// A production synthesis owns the shared budget before acquiring the
	// service gate. Downloads must leave both resources available to it.
	release, err := limiter.AcquireInference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	<-limiter.requested
	if _, err := s.Synthesize(ctx, "A public phrase.", "english", 2); err != nil {
		t.Fatalf("download blocked old-voice synthesis: %v", err)
	}
	unblock.Do(func() { close(downloaded) })
	select {
	case <-limiter.requested:
	case <-ctx.Done():
		t.Fatal("warmup did not request the shared CPU budget")
	}
	// Warmup is waiting for our budget. It must not hold the service gate,
	// otherwise the already admitted synthesis and warmup would deadlock.
	if _, err := s.Synthesize(ctx, "Another public phrase.", "english", 2); err != nil {
		t.Fatalf("warmup inverted inference locks: %v", err)
	}
	release()
	select {
	case err := <-prepared:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("warmup did not finish after the budget was released")
	}
}

func TestCloseCancelsPreparationDownload(t *testing.T) {
	s := fakeService(t, "ok")
	s.assets = []asset{testAsset("english", "model.safetensors", []byte("fixture"))}
	started := make(chan struct{})
	s.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	prepared := make(chan error, 1)
	go func() {
		prepared <- s.Prepare(context.Background(), audio.Settings{Threads: 2, TTSVoice: "english"}, nil)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel the model download")
	}
	if err := <-prepared; err == nil {
		t.Fatal("canceled download succeeded")
	}
}

func TestDownloadVerifiesAtomicFilesAndCleansFailures(t *testing.T) {
	data := []byte("a public model fixture")
	spec := testAsset("english", "model.safetensors", data)
	for _, mode := range []string{"good", "checksum", "long", "status", "network"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, spec.name)
			old := []byte("previous verified model")
			if err := os.WriteFile(target, old, 0600); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Scheme != "https" || req.URL.Host != "huggingface.co" || !strings.Contains(req.URL.Path, modelRevision) {
					t.Fatal("download is not pinned")
				}
				if mode == "network" {
					return nil, errors.New("test connection failed")
				}
				body := append([]byte(nil), data...)
				status := http.StatusOK
				if mode == "checksum" {
					body[0] ^= 1
				}
				if mode == "long" {
					body = append(body, 'x')
				}
				if mode == "status" {
					status = http.StatusNotFound
				}
				return &http.Response{StatusCode: status, ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})}
			err := downloadAsset(context.Background(), client, target, spec, nil)
			if (err == nil) != (mode == "good") {
				t.Fatalf("download = %v", err)
			}
			got, _ := os.ReadFile(target)
			want := old
			if mode == "good" {
				want = data
				if err := verifyAsset(context.Background(), target, spec); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(got, want) {
				t.Fatal("failed download replaced the old model")
			}
			files, _ := filepath.Glob(filepath.Join(dir, ".download-*"))
			if len(files) != 0 {
				t.Fatal("download temporary files survived")
			}
		})
	}
}

func TestModelFilesAndRedirectsRejectSymlinksAndForeignHosts(t *testing.T) {
	dir := t.TempDir()
	data := []byte("model fixture")
	spec := testAsset("english", "model.safetensors", data)
	original := filepath.Join(dir, "original")
	if err := os.WriteFile(original, data, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(original, link); err != nil {
		t.Fatal(err)
	}
	if err := verifyAsset(context.Background(), link, spec); err == nil {
		t.Fatal("symlink model accepted")
	}
	for _, raw := range []string{"http://huggingface.co/model", "https://huggingface.co.evil.test/model", "https://user@huggingface.co/model", "https://127.0.0.1/model", "https://huggingface.co:444/model"} {
		u, _ := url.Parse(raw)
		if allowedURL(u) {
			t.Fatalf("unsafe URL accepted: %s", raw)
		}
	}
	u, _ := url.Parse("https://cas-bridge.xethub.hf.co/model")
	if !allowedURL(u) {
		t.Fatal("official Hugging Face CDN rejected")
	}
}

func TestPrepareSelectedVoiceAndAutoDownloadsBoth(t *testing.T) {
	for _, voice := range []string{"english", "italian", "auto"} {
		t.Run(voice, func(t *testing.T) {
			s := fakeService(t, "ok")
			data := []byte("public fixture")
			s.assets = []asset{testAsset("english", "model.safetensors", data), testAsset("italian", "model.safetensors", data)}
			requests := 0
			s.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
			})}
			var operations []audio.Operation
			settings := audio.Settings{Enabled: true, Model: "base", Threads: 2, ReplyWithAudio: true, TTSVoice: voice}
			if err := s.Prepare(context.Background(), settings, func(op audio.Operation) { operations = append(operations, op) }); err != nil {
				t.Fatal(err)
			}
			want := 1
			if voice == "auto" {
				want = 2
			}
			if requests != want || len(operations) == 0 {
				t.Fatal("preparation downloaded the wrong languages")
			}
			if err := s.Prepare(context.Background(), settings, nil); err != nil || requests != want {
				t.Fatalf("verified models downloaded again: %v", err)
			}
			for _, name := range []string{"NOTICE.md", "Golem-MIT.txt", "CC-BY-4.0.txt"} {
				info, err := os.Stat(filepath.Join(s.dir, name))
				if err != nil || info.Size() == 0 || info.Mode().Perm()&0077 != 0 {
					t.Fatalf("model attribution was not retained privately: %s, %v", name, err)
				}
			}
		})
	}
}

func TestPrepareRejectsCachedModelInSymlinkDirectory(t *testing.T) {
	s := fakeService(t, "ok")
	data := []byte("public fixture")
	s.assets = []asset{testAsset("english", "model.safetensors", data)}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "model.safetensors"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.dir, "english")); err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare(context.Background(), audio.Settings{Threads: 2, TTSVoice: "english"}, nil); err == nil {
		t.Fatal("cached model escaped private storage through a language-directory symlink")
	}
	if s.process != nil {
		t.Fatal("unsafe model reached the helper")
	}
}
