package voice

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in integration test against a built helper and an existing local model.
// The ordinary test suite never downloads a model or requires native tooling.
func TestNativeHelperWarmTranscription(t *testing.T) {
	binary, model, audio := os.Getenv("TELEGRAMGW_VOICE_TEST_HELPER"), os.Getenv("TELEGRAMGW_VOICE_TEST_MODEL"), os.Getenv("TELEGRAMGW_VOICE_TEST_AUDIO")
	if binary == "" || model == "" || audio == "" {
		t.Skip("set TELEGRAMGW_VOICE_TEST_HELPER, TELEGRAMGW_VOICE_TEST_MODEL, and TELEGRAMGW_VOICE_TEST_AUDIO for native integration")
	}
	threads := 0
	if value := os.Getenv("TELEGRAMGW_VOICE_TEST_THREADS"); value != "" {
		var err error
		threads, err = strconv.Atoi(value)
		if err != nil {
			t.Fatalf("invalid TELEGRAMGW_VOICE_TEST_THREADS: %v", err)
		}
	}
	file, err := os.Open(audio)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	pcm, err := DecodeOggOpus(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{BinaryPath: binary, ModelPath: model, Threads: threads})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	pid := c.process.cmd.Process.Pid
	for range 2 {
		result, err := c.Transcribe(ctx, pcm)
		if err != nil {
			t.Fatal(err)
		}
		if result.Text == "" || result.Language == "" {
			t.Fatalf("missing native transcription: %+v", result)
		}
		if want := os.Getenv("TELEGRAMGW_VOICE_TEST_EXPECT"); want != "" && !strings.Contains(strings.ToLower(result.Text), strings.ToLower(want)) {
			t.Fatalf("transcription did not contain expected phrase %q", want)
		}
		if c.process.cmd.Process.Pid != pid {
			t.Fatal("native helper restarted between successful jobs")
		}
	}
}
