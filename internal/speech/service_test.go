package speech

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/voice"
)

// TestSpeechChild is a test-only substitute for the gateway's early dispatch.
// It never loads weights and does not persist request text or generated PCM.
func TestSpeechChild(t *testing.T) {
	if os.Getenv("TELEGRAMGW_SPEECH_TEST_CHILD") != "1" {
		return
	}
	mode := os.Getenv("TELEGRAMGW_SPEECH_TEST_MODE")
	if mode == "native" {
		err := RunHelper([]string{"--dir", os.Getenv("TELEGRAMGW_SPEECH_NATIVE_DIR"), "--language", os.Getenv("TELEGRAMGW_SPEECH_NATIVE_LANGUAGE"), "--threads", "3"}, os.Stdin, os.Stdout)
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode == "badready" {
		_ = writeJSON(os.Stdout, response{Type: "ready", Protocol: 99, SampleRate: SampleRate})
		os.Exit(0)
	}
	if mode == "oversize" || mode == "nan" || mode == "wrongid" {
		_ = writeJSON(os.Stdout, response{Type: "ready", Protocol: protocol, SampleRate: SampleRate})
		var req request
		_ = readJSON(bufio.NewReaderSize(os.Stdin, maxHeader+1), &req)
		reply := response{ID: req.ID, SampleCount: 1, SampleRate: SampleRate}
		if mode == "oversize" {
			reply.SampleCount = maxSamples + 1
		}
		if mode == "wrongid" {
			reply.ID = "wrong"
		}
		_ = writeJSON(os.Stdout, reply)
		var raw [4]byte
		binary.LittleEndian.PutUint32(raw[:], math.Float32bits(float32(math.NaN())))
		_, _ = os.Stdout.Write(raw[:])
		os.Exit(0)
	}
	err := serve(os.Stdin, os.Stdout, func(context.Context, string) ([]float32, error) {
		if mode == "block" {
			for {
				time.Sleep(time.Hour)
			}
		}
		return []float32{0.25, -0.5, 0}, nil
	})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeService(t *testing.T, mode string) *Service {
	t.Helper()
	s := New(t.TempDir(), os.Args[0])
	s.command = func(string, int) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSpeechChild$")
		cmd.Env = append(os.Environ(), "TELEGRAMGW_SPEECH_TEST_CHILD=1", "TELEGRAMGW_SPEECH_TEST_MODE="+mode)
		return cmd
	}
	t.Cleanup(s.Close)
	return s
}

func TestServiceReusesSwitchesAndReapsHelper(t *testing.T) {
	s := fakeService(t, "ok")
	for i := 0; i < 2; i++ {
		samples, err := s.Synthesize(context.Background(), "A public test phrase.", "en", 3)
		if err != nil || len(samples) != 3 || samples[0] != 0.25 || samples[1] != -0.5 {
			t.Fatalf("synthesis = %v, %v", samples, err)
		}
		if s.process == nil {
			t.Fatal("helper was not retained")
		}
	}
	first := s.process
	if _, err := s.Synthesize(context.Background(), "Una frase pubblica.", "it", 2); err != nil {
		t.Fatal(err)
	}
	if first == s.process || first.cmd.ProcessState == nil || s.process.language != "italian" || s.process.threads != 2 {
		t.Fatal("changing model/threads did not reap and replace helper")
	}
	second := s.process
	s.Close()
	s.Close()
	if second.cmd.ProcessState == nil || s.Available() {
		t.Fatal("close did not stop the helper")
	}
	if _, err := s.Synthesize(context.Background(), "Test.", "english", 2); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close = %v", err)
	}
}

func TestServiceCancellationAndCloseUnblockJobs(t *testing.T) {
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			s := fakeService(t, "block")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := s.Synthesize(ctx, "Public phrase.", "english", 2); result <- err }()
			var p *helperProcess
			deadline := time.Now().Add(5 * time.Second)
			for p == nil && time.Now().Before(deadline) {
				s.mu.Lock()
				p = s.process
				s.mu.Unlock()
				if p == nil {
					time.Sleep(time.Millisecond)
				}
			}
			if p == nil {
				t.Fatal("helper never started")
			}
			queued, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer stop()
			if _, err := s.Synthesize(queued, "Queued phrase.", "italian", 1); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("queued cancellation = %v", err)
			}
			if action == "cancel" {
				cancel()
			} else {
				s.Close()
			}
			select {
			case err := <-result:
				if err == nil || p.cmd.ProcessState == nil {
					t.Fatalf("canceled helper was not reaped: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("job did not unblock")
			}
		})
	}
}

func TestServiceRejectsMalformedOutputAndOversizeInput(t *testing.T) {
	for _, mode := range []string{"badready", "oversize", "nan", "wrongid"} {
		t.Run(mode, func(t *testing.T) {
			s := fakeService(t, mode)
			if samples, err := s.Synthesize(context.Background(), "Public phrase.", "english", 1); err == nil || samples != nil || s.process != nil {
				t.Fatalf("malformed response accepted: %v, %v", samples, err)
			}
		})
	}
	s := fakeService(t, "ok")
	for _, input := range []string{"", strings.Repeat("a", MaxTextBytes+1), "nul\x00text", string([]byte{0xff})} {
		if _, err := s.Synthesize(context.Background(), input, "english", 1); err == nil || s.process != nil {
			t.Fatal("invalid input reached helper")
		}
	}
}

func TestUnloadCanRestartAndInterruptActiveInference(t *testing.T) {
	s := fakeService(t, "ok")
	if _, err := s.Synthesize(context.Background(), "Public phrase.", "english", 1); err != nil {
		t.Fatal(err)
	}
	p := s.process
	s.Unload()
	if _, err := s.Synthesize(context.Background(), "Public phrase.", "english", 1); err != nil {
		t.Fatal(err)
	}
	if s.process == p || p.cmd.ProcessState == nil {
		t.Fatal("unload did not reap and replace resident child")
	}
	s.Close()
	s = fakeService(t, "block")
	result := make(chan error, 1)
	go func() { _, err := s.Synthesize(context.Background(), "Public phrase.", "english", 1); result <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		started := s.process != nil
		s.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.Unload()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("unloaded active inference succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unload blocked active inference")
	}
}

func TestNativeStockVoiceSynthesis(t *testing.T) {
	dir := os.Getenv("TELEGRAMGW_SPEECH_NATIVE_DIR")
	if dir == "" {
		t.Skip("set TELEGRAMGW_SPEECH_NATIVE_DIR to the verified stock model directory")
	}
	executable := os.Getenv("TELEGRAMGW_SPEECH_NATIVE_HELPER")
	if executable == "" {
		executable = os.Args[0]
	}
	s := New(dir, executable)
	if executable == os.Args[0] {
		s.command = func(language string, _ int) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run=^TestSpeechChild$")
			cmd.Env = []string{"TELEGRAMGW_SPEECH_TEST_CHILD=1", "TELEGRAMGW_SPEECH_TEST_MODE=native", "TELEGRAMGW_SPEECH_NATIVE_DIR=" + dir, "TELEGRAMGW_SPEECH_NATIVE_LANGUAGE=" + language}
			return cmd
		}
	}
	defer s.Close()
	var transcriber *voice.Client
	if whisper := os.Getenv("TELEGRAMGW_SPEECH_NATIVE_WHISPER"); whisper != "" {
		var err error
		transcriber, err = voice.New(voice.Config{BinaryPath: whisper, ModelPath: os.Getenv("TELEGRAMGW_SPEECH_NATIVE_WHISPER_MODEL"), Threads: 3})
		if err != nil {
			t.Fatal(err)
		}
		defer transcriber.Close()
	}
	for _, language := range []string{"english", "italian"} {
		phrase := "I updated the audio settings. You can now send a voice message and receive the final answer as audio too."
		if language == "italian" {
			phrase = "Ho aggiornato le impostazioni audio. Ora puoi inviare un messaggio vocale e ricevere anche la risposta audio."
		}
		started := time.Now()
		samples, err := s.Synthesize(context.Background(), phrase, language, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(samples) < SampleRate/2 || len(samples) > 30*SampleRate {
			t.Fatalf("unexpected speech duration: %d", len(samples))
		}
		var energy float64
		for _, value := range samples {
			energy += float64(value * value)
		}
		if energy < 0.01 {
			t.Fatal("generated audio was silent")
		}
		t.Logf("%s: %.2fs of speech synthesized in %s", language, float64(len(samples))/SampleRate, time.Since(started).Round(time.Millisecond))
		encoded, err := voice.EncodeOggOpus(context.Background(), samples, SampleRate)
		clear(samples)
		if err != nil {
			t.Fatal(err)
		}
		var decoded []float32
		if decoder := os.Getenv("TELEGRAMGW_SPEECH_NATIVE_OPUS_DECODER"); decoder != "" {
			// Optional development-only independent decoder: encoded Ogg on
			// stdin, mono 16 kHz float32 PCM on stdout. No production dependency.
			cmd := exec.Command(decoder)
			cmd.Stdin = bytes.NewReader(encoded)
			raw, decodeErr := cmd.Output()
			if decodeErr != nil || len(raw)%4 != 0 {
				clear(raw)
				t.Fatalf("independent Opus decode failed: %v", decodeErr)
			}
			decoded = make([]float32, len(raw)/4)
			for i := range decoded {
				decoded[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
			}
			clear(raw)
		} else {
			decoded, err = voice.DecodeOggOpus(context.Background(), bytes.NewReader(encoded))
		}
		clear(encoded)
		if err != nil || len(decoded) < voice.SampleRate/2 {
			t.Fatalf("Opus round trip failed: %v", err)
		}
		if transcriber != nil {
			result, err := transcriber.Transcribe(context.Background(), decoded)
			clear(decoded)
			if err != nil {
				t.Fatal(err)
			}
			expectedLanguage := "en"
			if language == "italian" {
				expectedLanguage = "it"
			}
			if result.Language != expectedLanguage || !strings.Contains(strings.ToLower(result.Text), "audio") {
				t.Fatalf("stock speech was not recognized: %#v", result)
			}
			t.Logf("%s Opus → Whisper: %s", language, result.Text)
		} else {
			clear(decoded)
		}
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestGeneratedSamplesAreClearedOnWriteFailure(t *testing.T) {
	samples := []float32{0.2, -0.3}
	if err := writeGenerated(failedWriter{}, "1", samples); err == nil {
		t.Fatal("write failure was hidden")
	}
	if samples[0] != 0 || samples[1] != 0 {
		t.Fatal("generated PCM survived failed delivery")
	}
}

func TestProtocolRejectsTruncationAndUnknownFields(t *testing.T) {
	for _, input := range []string{`{"id":"1","text":"hello"}`, "{\"id\":\"1\",\"text\":\"hello\",\"other\":true}\n", strings.Repeat("a", maxHeader+1) + "\n"} {
		var out strings.Builder
		if err := serve(strings.NewReader(input), &out, func(context.Context, string) ([]float32, error) {
			t.Fatal("invalid request reached synthesis")
			return nil, nil
		}); err == nil {
			t.Fatal("invalid protocol input accepted")
		}
	}
	var value request
	if err := readJSON(bufio.NewReader(strings.NewReader("")), &value); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestRunHelperRejectsUnsafeArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"--dir", "relative", "--language", "english", "--threads", "2"}, {"--dir", filepath.Join(t.TempDir(), "x"), "--language", "french", "--threads", "2"}} {
		if err := RunHelper(args, strings.NewReader(""), io.Discard); err == nil {
			t.Fatal("invalid helper arguments accepted")
		}
	}
}
