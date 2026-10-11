package voice

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	helperProtocol     = 1
	maxResponseBytes   = 128 << 10
	maxStderrBytes     = 8 << 10
	helperStartupLimit = 120 * time.Second
	transcriptionLimit = 10 * time.Minute
)

var ErrClientClosed = errors.New("voice: Whisper client is closed")

type Config struct {
	BinaryPath string
	ModelPath  string
	Threads    int
}

type Result struct {
	Text     string
	Language string
}

// Client owns one local helper process. The helper loads the model once and
// sleeps while its input pipe is idle. Jobs are serialized; there is no polling
// loop or listening socket. Canceling an active job kills and reaps the helper
// so no previous audio or model context can leak into a later request.
type Client struct {
	config         Config
	gate           chan struct{}
	done           chan struct{}
	mu             sync.Mutex
	closed         bool
	process        *helperProcess
	sequence       uint64
	startupTimeout time.Duration
}

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.BinaryPath) == "" || strings.TrimSpace(config.ModelPath) == "" {
		return nil, errors.New("voice: helper binary and model paths are required")
	}
	if config.Threads == 0 {
		config.Threads = 2
	}
	if config.Threads < 1 || config.Threads > 2 {
		return nil, errors.New("voice: Whisper threads must be 1 or 2")
	}
	c := &Client{config: config, gate: make(chan struct{}, 1), done: make(chan struct{}), startupTimeout: helperStartupLimit}
	c.gate <- struct{}{}
	return c, nil
}

// Start loads the model and verifies the helper protocol. Transcribe also
// starts the helper lazily and restarts it after a canceled or failed process.
func (c *Client) Start(ctx context.Context) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	_, err := c.start(ctx)
	return err
}

func (c *Client) Transcribe(ctx context.Context, samples []float32) (Result, error) {
	if len(samples) == 0 {
		return Result{}, errors.New("voice: no audio samples")
	}
	if len(samples) > SampleRate*int(HardMaxDuration/time.Second) {
		return Result{}, ErrAudioTooLong
	}
	for i, sample := range samples {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
		}
		if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
			return Result{}, errors.New("voice: audio contains non-finite samples")
		}
		if sample < -1 || sample > 1 {
			return Result{}, errors.New("voice: audio samples must be normalized to [-1,1]")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, transcriptionLimit)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return Result{}, err
	}
	defer c.release()
	p, err := c.start(ctx)
	if err != nil {
		return Result{}, err
	}
	c.sequence++
	id := strconv.FormatUint(c.sequence, 10)
	var response helperResponse
	err = c.exchange(ctx, p, func() error {
		request := struct {
			ID          string `json:"id"`
			SampleCount int    `json:"sample_count"`
			Language    string `json:"language"`
		}{ID: id, SampleCount: len(samples), Language: "auto"}
		header, err := json.Marshal(request)
		if err != nil {
			return err
		}
		if err := writeAll(p.stdin, append(header, '\n')); err != nil {
			return fmt.Errorf("write request: %w", err)
		}
		var chunk [32 << 10]byte
		for offset := 0; offset < len(samples); {
			n := min(len(chunk)/4, len(samples)-offset)
			for i, sample := range samples[offset : offset+n] {
				binary.LittleEndian.PutUint32(chunk[i*4:], math.Float32bits(sample))
			}
			if err := writeAll(p.stdin, chunk[:n*4]); err != nil {
				return fmt.Errorf("write PCM: %w", err)
			}
			offset += n
		}
		if err := readHelperMessage(p.stdout, &response); err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		if response.ID != id {
			return errors.New("response ID does not match request")
		}
		return nil
	})
	if err != nil {
		c.discard(p)
		return Result{}, c.helperError("transcription", err, p)
	}
	if response.Error != "" {
		return Result{}, fmt.Errorf("voice: transcription failed: %s", response.Error)
	}
	return Result{Text: strings.TrimSpace(response.Text), Language: response.Language}, nil
}

type helperResponse struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Language string `json:"language"`
	Error    string `json:"error"`
}

func (c *Client) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return ErrClientClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-c.gate:
		select {
		case <-c.done:
			c.release()
			return ErrClientClosed
		default:
			return nil
		}
	}
}

func (c *Client) release() { c.gate <- struct{}{} }

func (c *Client) start(ctx context.Context) (*helperProcess, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClientClosed
	}
	p := c.process
	c.mu.Unlock()
	if p != nil {
		select {
		case <-p.done:
			c.discard(p)
		default:
			return p, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.config.BinaryPath, "--model", c.config.ModelPath, "--threads", strconv.Itoa(c.config.Threads))
	// The native helper needs only local absolute paths. Do not give it the
	// gateway's bot token, webhook secret, cloud credentials, or proxy settings.
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("voice: create helper input: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("voice: create helper output: %w", err)
	}
	p = &helperProcess{cmd: cmd, stdin: stdin, stdoutPipe: stdout, stdout: bufio.NewReaderSize(stdout, 32<<10), done: make(chan struct{})}
	cmd.Stderr = &p.stderr
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, ErrClientClosed
	}
	if err := cmd.Start(); err != nil {
		c.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("voice: start Whisper helper: %w", err)
	}
	c.process = p
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	c.mu.Unlock()
	startupCtx, cancel := context.WithTimeout(ctx, c.startupTimeout)
	defer cancel()
	err = c.exchange(startupCtx, p, func() error {
		var ready struct {
			Type     string `json:"type"`
			Protocol int    `json:"protocol"`
		}
		if err := readHelperMessage(p.stdout, &ready); err != nil {
			return err
		}
		if ready.Type != "ready" || ready.Protocol != helperProtocol {
			return errors.New("unsupported Whisper helper protocol")
		}
		return nil
	})
	if err != nil {
		c.discard(p)
		return nil, c.helperError("startup", err, p)
	}
	return p, nil
}

// exchange is the only goroutine allowed to read/write the protocol pipes.
// Closing the pipes and killing the process unblocks it on cancellation, and
// the goroutine and child are both joined before another job can begin.
func (c *Client) exchange(ctx context.Context, p *helperProcess, operation func() error) error {
	result := make(chan error, 1)
	go func() { result <- operation() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		p.stop()
		<-result
		return ctx.Err()
	case <-c.done:
		p.stop()
		<-result
		return ErrClientClosed
	}
}

func (c *Client) discard(p *helperProcess) {
	p.stop()
	c.mu.Lock()
	if c.process == p {
		c.process = nil
	}
	c.mu.Unlock()
}

func (c *Client) helperError(action string, err error, p *helperProcess) error {
	// Context and lifecycle errors retain errors.Is semantics and omit helper
	// diagnostics, which could include an unrelated earlier model warning.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrClientClosed) {
		return fmt.Errorf("voice: Whisper %s: %w", action, err)
	}
	if diagnostic := strings.TrimSpace(p.stderr.String()); diagnostic != "" {
		return fmt.Errorf("voice: Whisper %s: %w (%s)", action, err, diagnostic)
	}
	return fmt.Errorf("voice: Whisper %s: %w", action, err)
}

// Close interrupts any active job, closes the pipes, and reaps the child. It
// is safe to call concurrently with Start, Transcribe, or another Close.
func (c *Client) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	p := c.process
	c.mu.Unlock()
	if p != nil {
		p.stop()
	}
	<-c.gate
	c.mu.Lock()
	c.process = nil
	c.mu.Unlock()
	c.release()
	return nil
}

type helperProcess struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdoutPipe io.ReadCloser
	stdout     *bufio.Reader
	stderr     boundedStderr
	done       chan struct{}
	stopOnce   sync.Once
}

func (p *helperProcess) stop() {
	p.stopOnce.Do(func() {
		_ = p.stdin.Close()
		_ = p.stdoutPipe.Close()
		_ = p.cmd.Process.Kill()
	})
	<-p.done
}

type boundedStderr struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedStderr) Write(data []byte) (int, error) {
	n := len(data)
	b.mu.Lock()
	defer b.mu.Unlock()
	if n >= maxStderrBytes {
		b.data = append(b.data[:0], data[n-maxStderrBytes:]...)
	} else {
		if len(b.data)+n > maxStderrBytes {
			b.data = append(b.data[:0], b.data[len(b.data)+n-maxStderrBytes:]...)
		}
		b.data = append(b.data, data...)
	}
	return n, nil
}

func (b *boundedStderr) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(string(b.data), "?")
}

func readHelperMessage(reader *bufio.Reader, value any) error {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(line)+len(chunk) > maxResponseBytes {
			return errors.New("helper response exceeds 128 KiB")
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if !utf8.Valid(line) {
		return errors.New("helper response is not valid UTF-8")
	}
	if err := json.Unmarshal(line, value); err != nil {
		return fmt.Errorf("invalid helper JSON: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
