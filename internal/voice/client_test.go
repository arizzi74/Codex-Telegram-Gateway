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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientReusesModelProcessAndPCMProtocol(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-parent-credential")
	c, marker := testClient(t, "normal")
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var pid string
	for i := range 3 {
		result, err := c.Transcribe(context.Background(), []float32{.25, -.5, 1})
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(result.Text, ":")
		if len(parts) != 2 || parts[1] != strconv.Itoa(i+1) || result.Language != "it" {
			t.Fatalf("unexpected result: %+v", result)
		}
		if pid != "" && pid != parts[0] {
			t.Fatal("helper restarted between successful jobs")
		}
		pid = parts[0]
	}
	if starts := helperStarts(t, marker); len(starts) != 1 {
		t.Fatalf("expected one model load, got %d helper starts", len(starts))
	}
	c.mu.Lock()
	p := c.process
	c.mu.Unlock()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	default:
		t.Fatal("Close returned without reaping helper")
	}
	if _, err := c.Transcribe(context.Background(), []float32{.25}); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("expected closed client, got %v", err)
	}
}

func TestClientCancellationReapsAndRestarts(t *testing.T) {
	c, marker := testClient(t, "cancel-once")
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	first := c.process
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Transcribe(ctx, []float32{.25, -.5, 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected active-job deadline, got %v", err)
	}
	select {
	case <-first.done:
	default:
		t.Fatal("canceled helper was not reaped")
	}
	result, err := c.Transcribe(context.Background(), []float32{.25, -.5, 1})
	if err != nil || result.Text == "" {
		t.Fatalf("restart failed: %+v, %v", result, err)
	}
	if starts := helperStarts(t, marker); len(starts) != 2 {
		t.Fatalf("expected replacement helper, got %d starts", len(starts))
	}
}

func TestClientStartupCancellation(t *testing.T) {
	c, _ := testClient(t, "block-start")
	c.startupTimeout = 100 * time.Millisecond
	if err := c.Start(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded model startup, got %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.process != nil {
		t.Fatal("failed startup retained helper")
	}
}

func TestClientSerializesJobsAndWaiterCancellation(t *testing.T) {
	c, marker := testClient(t, "block-job")
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	active := make(chan error, 1)
	go func() { _, err := c.Transcribe(context.Background(), []float32{.25}); active <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker + ".job"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not receive active job")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Transcribe(ctx, []float32{.25}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected queued-job deadline, got %v", err)
	}
	if starts := helperStarts(t, marker); len(starts) != 1 {
		t.Fatal("queued-job cancellation restarted active helper")
	}
	select {
	case err := <-active:
		t.Fatalf("queued-job cancellation interrupted active job: %v", err)
	default:
	}
	var closes sync.WaitGroup
	for range 3 {
		closes.Go(func() { _ = c.Close() })
	}
	closes.Wait()
	select {
	case err := <-active:
		if !errors.Is(err, ErrClientClosed) {
			t.Fatalf("expected close interruption, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock active job")
	}
}

func TestClientRejectsProtocolFailures(t *testing.T) {
	for _, mode := range []string{"bad-ready", "oversized", "bad-id", "invalid-utf8", "truncated", "stderr"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := testClient(t, mode)
			var err error
			if mode == "bad-ready" || mode == "stderr" {
				err = c.Start(context.Background())
			} else {
				_, err = c.Transcribe(context.Background(), []float32{.25, -.5, 1})
			}
			if err == nil {
				t.Fatal("accepted malformed helper protocol")
			}
			if len(err.Error()) > maxStderrBytes+1024 {
				t.Fatalf("unbounded diagnostic: %d bytes", len(err.Error()))
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.process != nil {
				t.Fatal("retained helper after protocol failure")
			}
		})
	}
}

func TestClientStructuredJobErrorKeepsHelper(t *testing.T) {
	c, marker := testClient(t, "job-error")
	if _, err := c.Transcribe(context.Background(), []float32{.25, -.5, 1}); err == nil || !strings.Contains(err.Error(), "test job failure") {
		t.Fatalf("expected structured helper error, got %v", err)
	}
	if _, err := c.Transcribe(context.Background(), []float32{.25, -.5, 1}); err != nil {
		t.Fatal(err)
	}
	if starts := helperStarts(t, marker); len(starts) != 1 {
		t.Fatal("job error needlessly reloaded model")
	}
}

func TestClientInputBounds(t *testing.T) {
	for _, cfg := range []Config{{}, {BinaryPath: "helper"}, {BinaryPath: "helper", ModelPath: "model", Threads: -1}, {BinaryPath: "helper", ModelPath: "model", Threads: 33}} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("accepted bad configuration: %+v", cfg)
		}
	}
	c, marker := testClient(t, "normal")
	for _, samples := range [][]float32{nil, {float32(math.NaN())}, {float32(math.Inf(1))}, {1.01}, make([]float32, SampleRate*601)} {
		if _, err := c.Transcribe(context.Background(), samples); err == nil {
			t.Fatal("accepted invalid PCM")
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid PCM started helper")
	}
}

func TestClientForwardsThreadCount(t *testing.T) {
	for _, tc := range []struct {
		threads int
		want    int
	}{{0, 2}, {1, 1}, {2, 2}, {3, 3}, {32, 32}} {
		t.Run(strconv.Itoa(tc.threads), func(t *testing.T) {
			c, marker := testClientWithThreads(t, "normal", tc.threads)
			result, err := c.Transcribe(context.Background(), []float32{.25, -.5, 1})
			if err != nil || result.Text == "" {
				t.Fatalf("transcription failed: %+v, %v", result, err)
			}
			data, err := os.ReadFile(marker + ".threads")
			if err != nil {
				t.Fatal(err)
			}
			if got := string(data); got != strconv.Itoa(tc.want) {
				t.Fatalf("helper received --threads %q, want %d", got, tc.want)
			}
		})
	}
}

// A test subprocess exercises real pipes/process lifecycle, without needing a
// native Whisper build or model in unit tests. Test production helper/model
// compatibility separately using the native helper's smoke test.
func TestVoiceHelperProcess(t *testing.T) {
	var model, threadArg string
	for i, arg := range os.Args {
		if arg == "--model" && i+1 < len(os.Args) {
			model = os.Args[i+1]
		}
		if arg == "--threads" && i+1 < len(os.Args) {
			threadArg = os.Args[i+1]
		}
	}
	if model == "" {
		return
	}
	threads, err := strconv.Atoi(threadArg)
	if err != nil || threads < 1 || threads > 32 {
		os.Exit(70)
	}
	if os.Getenv("TELEGRAM_BOT_TOKEN") != "" {
		os.Exit(77)
	}
	parts := strings.SplitN(model, "|", 2)
	if len(parts) != 2 {
		os.Exit(71)
	}
	mode, marker := parts[0], parts[1]
	if err := os.WriteFile(marker+".threads", []byte(threadArg), 0600); err != nil {
		os.Exit(72)
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(72)
	}
	_, _ = fmt.Fprintln(file, os.Getpid())
	_ = file.Close()
	starts, _ := os.ReadFile(marker)
	firstStart := len(strings.Fields(string(starts))) == 1
	if mode == "block-start" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if mode == "stderr" {
		_, _ = io.WriteString(os.Stderr, strings.Repeat("x", maxStderrBytes*4))
	}
	if mode == "bad-ready" || mode == "stderr" {
		_, _ = fmt.Println(`{"type":"ready","protocol":99}`)
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	_, _ = fmt.Println(`{"type":"ready","protocol":1}`)
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for job := 1; ; job++ {
		header, err := reader.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var request struct {
			ID          string `json:"id"`
			SampleCount int    `json:"sample_count"`
			Language    string `json:"language"`
		}
		if json.Unmarshal(header, &request) != nil || request.SampleCount <= 0 || request.Language != "auto" {
			os.Exit(73)
		}
		pcm := make([]byte, request.SampleCount*4)
		if _, err := io.ReadFull(reader, pcm); err != nil {
			os.Exit(74)
		}
		for i := range request.SampleCount {
			sample := math.Float32frombits(binary.LittleEndian.Uint32(pcm[i*4:]))
			if sample != []float32{.25, -.5, 1}[i%3] {
				os.Exit(75)
			}
		}
		_ = os.WriteFile(marker+".job", []byte("received"), 0600)
		if mode == "block-job" || (mode == "cancel-once" && firstStart) {
			time.Sleep(time.Hour)
			os.Exit(0)
		}
		if mode == "oversized" {
			_, _ = io.WriteString(os.Stdout, strings.Repeat("x", maxResponseBytes+1)+"\n")
			continue
		}
		if mode == "invalid-utf8" {
			_, _ = os.Stdout.Write([]byte{'"', 0xff, '"', '\n'})
			continue
		}
		if mode == "truncated" {
			_, _ = io.WriteString(os.Stdout, `{"id":`)
			os.Exit(0)
		}
		response := helperResponse{ID: request.ID, Text: fmt.Sprintf(" %d:%d ", os.Getpid(), job), Language: "it"}
		if mode == "bad-id" {
			response.ID = "unexpected"
		}
		if mode == "job-error" && job == 1 {
			response.Error = "test job failure"
		}
		if encoder.Encode(response) != nil {
			os.Exit(76)
		}
	}
}

func testClient(t *testing.T, mode string) (*Client, string) {
	t.Helper()
	return testClientWithThreads(t, mode, 0)
}

func testClientWithThreads(t *testing.T, mode string, threads int) (*Client, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "helper")
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\\''") + "'"
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quoted+" -test.run=TestVoiceHelperProcess -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "starts")
	c, err := New(Config{BinaryPath: wrapper, ModelPath: mode + "|" + marker, Threads: threads})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, marker
}

func helperStarts(t *testing.T, marker string) []string {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}
