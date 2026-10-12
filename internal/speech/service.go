// Package speech synthesizes optional local audio replies in a Go subprocess.
package speech

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/audio"
)

const (
	startupLimit   = 2 * time.Minute
	synthesisLimit = 10 * time.Minute
)

var ErrClosed = errors.New("speech: service is closed")

// Service owns one resident CPU inference child. Preparation downloads are
// serialized separately from model switching and synthesis. Model mappings and
// inference live in the helper; Golem's small package-level worker pool stays
// parked in the gateway process.
type Service struct {
	dir           string
	executable    string
	assets        []asset
	http          *http.Client
	command       func(string, int) *exec.Cmd
	gate          chan struct{}
	prepareGate   chan struct{}
	limiter       InferenceLimiter
	done          chan struct{}
	mu            sync.Mutex
	closed        bool
	process       *helperProcess
	sequence      uint64
	unloadEpoch   uint64
	nativeEngine  string
	nativeActive  *Service
	nativePending *Service
	pendingEngine string
	beforeStart   func(context.Context) error
	nativeInstall func(context.Context, string, string, func(audio.Operation)) error
	nativeFactory func(string) *Service
}

// InferenceLimiter shares the host CPU budget with transcription. Synthesize's
// caller already owns that budget; Prepare acquires it only for its warmup.
type InferenceLimiter interface {
	AcquireInference(context.Context) (func(), error)
}

func (s *Service) SetInferenceLimiter(limiter InferenceLimiter) {
	s.mu.Lock()
	s.limiter = limiter
	s.mu.Unlock()
}

type helperProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	language string
	threads  int
	stopping bool // guarded by Service.mu
	stop     sync.Once
}

// New uses stateDir directly as its private model directory. It downloads
// nothing until Prepare is called. executable must be the current gateway
// binary, whose early speech-helper dispatch calls RunHelper.
func New(stateDir, executable string) *Service {
	s := &Service{dir: stateDir, executable: executable, assets: pinnedAssets(), http: &http.Client{Timeout: 10 * time.Minute}, gate: make(chan struct{}, 1), prepareGate: make(chan struct{}, 1), done: make(chan struct{})}
	s.gate <- struct{}{}
	s.prepareGate <- struct{}{}
	s.command = func(language string, threads int) *exec.Cmd {
		cmd := exec.Command(s.executable, "speech-helper", "--dir", s.dir, "--language", language, "--threads", strconv.Itoa(threads))
		// Inference needs no gateway configuration, credentials, PATH, or HOME.
		cmd.Env = []string{"LANG=C.UTF-8"}
		return cmd
	}
	return s
}

func (s *Service) Available() bool {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") || !filepath.IsAbs(s.dir) || !filepath.IsAbs(s.executable) || strings.ContainsRune(s.dir+s.executable, '\x00') {
		return false
	}
	info, err := os.Stat(s.executable)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

func (s *Service) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	case <-s.gate:
		select {
		case <-s.done:
			s.gate <- struct{}{}
			return ErrClosed
		default:
			return nil
		}
	}
}

func (s *Service) release() { s.gate <- struct{}{} }

// Prepare installs and verifies the selected stock voice, then synthesizes a
// public warmup phrase. Auto prepares both Italian and English. Failed work
// cannot change the audio manager's active settings.
func (s *Service) Prepare(ctx context.Context, settings audio.Settings, progress func(audio.Operation)) error {
	if settings.TTSEngine != "" {
		return s.prepareNative(ctx, settings, progress)
	}
	return s.prepareLegacy(ctx, settings, progress)
}

func (s *Service) prepareLegacy(ctx context.Context, settings audio.Settings, progress func(audio.Operation)) error {
	if !s.Available() {
		return errors.New("The local speech engine is unavailable.")
	}
	if settings.Threads < 1 || settings.Threads > 32 {
		return errors.New("CPU threads must be between 1 and 32.")
	}
	languages := []string{"english", "italian"}
	if settings.TTSVoice != "" && settings.TTSVoice != "auto" {
		language, err := languageName(settings.TTSVoice)
		if err != nil {
			return err
		}
		languages = []string{language}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := privateDirectory(s.dir); err != nil {
		return errors.New("The local speech model storage is unavailable.")
	}
	if err := writeNotices(s.dir); err != nil {
		return errors.New("The local speech model attribution could not be saved.")
	}
	for _, language := range languages {
		if err := privateDirectory(filepath.Join(s.dir, language)); err != nil {
			return errors.New("The local speech model storage is unavailable.")
		}
		var total int64
		for _, spec := range s.assets {
			if spec.language == language {
				total += spec.size
			}
		}
		op := audio.Operation{State: "verifying", Model: "Pocket TTS " + language, TotalBytes: total, Message: "Verifying the local speech model and stock voice."}
		report := func() {
			if progress != nil {
				progress(op)
			}
		}
		report()
		for _, spec := range s.assets {
			if spec.language != language {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			target := filepath.Join(s.dir, language, spec.name)
			if err := verifyAsset(ctx, target, spec); err == nil {
				op.DownloadedBytes += spec.size
				report()
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			op.State = "downloading"
			op.Message = "Downloading the local speech model and stock voice."
			report()
			lastReport := time.Now()
			if err := downloadAsset(ctx, s.http, target, spec, func(n int64) {
				op.DownloadedBytes += n
				if time.Since(lastReport) >= 200*time.Millisecond {
					report()
					lastReport = time.Now()
				}
			}); err != nil {
				return err
			}
			report()
		}
		op.State = "warming"
		op.Message = "Checking the selected local speech voice."
		report()
		phrase := "Voice replies are ready."
		if language == "italian" {
			phrase = "La sintesi vocale è pronta."
		}
		if err := s.warmup(ctx, phrase, language, settings.Threads); err != nil {
			return errors.New("The local speech voice could not be prepared.")
		}
	}
	return nil
}

func (s *Service) warmup(ctx context.Context, phrase, language string, threads int) error {
	s.mu.Lock()
	limiter := s.limiter
	s.mu.Unlock()
	if limiter != nil {
		release, err := limiter.AcquireInference(ctx)
		if err != nil {
			return err
		}
		defer release()
	}
	// Global budget before service gate matches the production synthesis order.
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	// Reopen only after downloads finish and active synthesis releases its gate.
	// This validates new files even when an old model remains memory mapped.
	s.mu.Lock()
	previous := s.process
	s.mu.Unlock()
	if previous != nil {
		s.discard(previous)
	}
	samples, err := s.synthesize(ctx, phrase, language, threads)
	clear(samples)
	return err
}

// Synthesize returns mono 24 kHz PCM. It requires a previously prepared model.
// A canceled request kills and reaps the child, removing its text, PCM, caches,
// and model mappings from memory before a later request can start.
func (s *Service) Synthesize(ctx context.Context, text, language string, threads int) ([]float32, error) {
	if !validText(text) {
		return nil, errors.New("speech: invalid or oversized text")
	}
	language, err := languageName(language)
	if err != nil {
		return nil, err
	}
	if threads < 1 || threads > 32 {
		return nil, errors.New("speech: CPU threads must be between 1 and 32")
	}
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	s.mu.Lock()
	native, engine := s.nativeActive, s.nativeEngine
	s.mu.Unlock()
	if native != nil {
		if engine == "piper" {
			language = "italian"
		}
		return native.Synthesize(ctx, normalizeSpeechText(text, language), language, threads)
	}
	return s.synthesize(ctx, text, language, threads)
}

func (s *Service) synthesize(ctx context.Context, text, language string, threads int) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, synthesisLimit)
	defer cancel()
	p, err := s.start(ctx, language, threads)
	if err != nil {
		return nil, err
	}
	s.sequence++
	id := strconv.FormatUint(s.sequence, 10)
	var samples []float32
	err = s.exchange(ctx, p, func() error {
		if err := writeJSON(p.stdin, request{ID: id, Text: text}); err != nil {
			return err
		}
		var reply response
		if err := readJSON(p.stdout, &reply); err != nil {
			return err
		}
		if reply.ID != id || reply.Type != "" || reply.Error != "" || reply.SampleRate != SampleRate || reply.SampleCount < 1 || reply.SampleCount > maxSamples {
			return errors.New("speech: synthesis failed")
		}
		samples = make([]float32, reply.SampleCount)
		var chunk [32 << 10]byte
		defer clear(chunk[:])
		for offset := 0; offset < len(samples); {
			n := min(len(chunk)/4, len(samples)-offset)
			if _, err := io.ReadFull(p.stdout, chunk[:n*4]); err != nil {
				return err
			}
			for i := 0; i < n; i++ {
				value := math.Float32frombits(binary.LittleEndian.Uint32(chunk[i*4:]))
				if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) || value < -1 || value > 1 {
					return errors.New("speech: invalid generated audio")
				}
				samples[offset+i] = value
			}
			offset += n
		}
		return nil
	})
	if err != nil {
		clear(samples)
		s.discard(p)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrClosed) {
			return nil, err
		}
		return nil, errors.New("speech: synthesis failed")
	}
	return samples, nil
}

func (s *Service) start(ctx context.Context, language string, threads int) (*helperProcess, error) {
	s.mu.Lock()
	p := s.process
	epoch := s.unloadEpoch
	closed := s.closed
	stopping := p != nil && p.stopping
	s.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if p != nil && !stopping && p.language == language && p.threads == threads {
		return p, nil
	}
	if p != nil {
		s.discard(p)
	}
	if s.beforeStart != nil {
		if err := s.beforeStart(ctx); err != nil {
			return nil, err
		}
	}
	cmd := s.command(language, threads)
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("speech: helper startup failed")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, errors.New("speech: helper startup failed")
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, errors.New("speech: helper startup failed")
	}
	p = &helperProcess{cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, maxHeader+1), language: language, threads: threads}
	s.mu.Lock()
	if s.closed || s.unloadEpoch != epoch {
		closed := s.closed
		s.mu.Unlock()
		s.discard(p)
		if closed {
			return nil, ErrClosed
		}
		return nil, errors.New("speech: helper unloaded")
	}
	s.process = p
	s.mu.Unlock()
	startup, cancel := context.WithTimeout(ctx, startupLimit)
	defer cancel()
	err = s.exchange(startup, p, func() error {
		var ready response
		if err := readJSON(p.stdout, &ready); err != nil {
			return err
		}
		if ready.Type != "ready" || ready.Protocol != protocol || ready.SampleRate != SampleRate || ready.Error != "" {
			return errors.New("speech: incompatible helper")
		}
		return nil
	})
	if err != nil {
		s.discard(p)
		if startup.Err() != nil {
			return nil, startup.Err()
		}
		return nil, errors.New("speech: helper startup failed")
	}
	return p, nil
}

func (s *Service) exchange(ctx context.Context, p *helperProcess, work func() error) error {
	done := make(chan error, 1)
	go func() { done <- work() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		s.discard(p)
		<-done
		return ctx.Err()
	case <-s.done:
		s.discard(p)
		<-done
		return ErrClosed
	}
}

func (s *Service) discard(p *helperProcess) {
	s.mu.Lock()
	p.stopping = true
	s.mu.Unlock()
	p.stop.Do(func() {
		_ = p.stdin.Close()
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	})
	s.mu.Lock()
	if s.process == p {
		s.process = nil
	}
	s.mu.Unlock()
}

// Unload drops resident inference without closing the service. It never waits
// for a queued preparation or active synthesis; stopping its child unblocks
// that operation, while a later request can start a fresh child normally.
func (s *Service) Unload() {
	s.mu.Lock()
	s.unloadEpoch++
	p := s.process
	native, pending := s.nativeActive, s.nativePending
	if p != nil {
		p.stopping = true
	}
	s.mu.Unlock()
	if native != nil {
		native.Unload()
	}
	if pending != nil {
		pending.Unload()
	}
	if p != nil {
		go s.discard(p)
	}
}

func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	p := s.process
	native, pending := s.nativeActive, s.nativePending
	s.mu.Unlock()
	if native != nil {
		native.Close()
	}
	if pending != nil {
		pending.Close()
	}
	if p != nil {
		s.discard(p)
	}
	// A start in flight may not have published its child yet. Waiting for the
	// gate also waits for that path to observe closed and reap the new child.
	<-s.gate
	s.release()
	<-s.prepareGate
	s.prepareGate <- struct{}{}
}
