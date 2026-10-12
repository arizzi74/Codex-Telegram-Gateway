package speech

import (
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/buildinfo"
)

// These opt-in tests use only explicitly supplied local public model/helper
// artifacts. They never download anything or use a deployed gateway state.
// Piper: TELEGRAMGW_PIPER_ARCHIVE and TELEGRAMGW_PIPER_MODEL_ARCHIVE.
// Optional Qwen switch: TELEGRAMGW_QWEN_ARCHIVE and TELEGRAMGW_QWEN_MODEL_DIR.
// Archives are normal build-piper/build-qwen-tts output tar.gz bundles.
type nativeIntegrationFixture struct {
	runtime, models string
}

func nativeIntegrationRuntime(t *testing.T, archive, program string) string {
	t.Helper()
	dir := t.TempDir()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	z, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	if err := unpackSpeech(t.Context(), z, dir, program, "", 256<<20); err != nil {
		t.Fatalf("extract real %s runtime: %v", program, err)
	}
	if err := verifySpeechInventory(t.Context(), dir); err != nil {
		t.Fatalf("verify real %s runtime inventory: %v", program, err)
	}
	return dir
}

func nativeIntegrationPiper(t *testing.T, archive, modelArchive string) nativeIntegrationFixture {
	t.Helper()
	fixture := nativeIntegrationFixture{runtime: nativeIntegrationRuntime(t, archive, "codex-piper"), models: t.TempDir()}
	if err := verifyAsset(t.Context(), modelArchive, nativeModels("piper")[0]); err != nil {
		t.Fatalf("verify pinned Miro archive: %v", err)
	}
	file, err := os.Open(modelArchive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unpackSpeech(t.Context(), bzip2.NewReader(file), fixture.models, "", piperModelDir, 90<<20); err != nil {
		t.Fatalf("extract real Miro model through production parser: %v", err)
	}
	model := filepath.Join(fixture.models, piperModelDir)
	manifest, err := speechInventory(t.Context(), model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "SHA256SUMS"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifySpeechInventory(t.Context(), model); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func nativeIntegrationQwen(t *testing.T, archive, models string) nativeIntegrationFixture {
	t.Helper()
	fixture := nativeIntegrationFixture{runtime: nativeIntegrationRuntime(t, archive, "codex-qwen-tts"), models: t.TempDir()}
	for _, spec := range nativeModels("qwen3") {
		source := filepath.Join(models, spec.name)
		if err := verifyAsset(t.Context(), source, spec); err != nil {
			t.Fatalf("verify original Qwen model %s: %v", spec.name, err)
		}
		if err := os.Link(source, filepath.Join(fixture.models, spec.name)); err != nil {
			t.Fatalf("hardlink public Qwen model into private fixture (same filesystem required): %v", err)
		}
	}
	return fixture
}

// Hardlinks avoid copying 1.5 GB of immutable public weights. Never chmod,
// rewrite, truncate or otherwise modify a linked file: cleanup unlinks only
// the private fixture, and all original source assets remain untouched.
func nativeIntegrationLinkTree(source, destination string) error {
	return filepath.WalkDir(source, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("integration fixtures must contain only regular files")
		}
		return os.Link(name, target)
	})
}

func nativeIntegrationPCM(t *testing.T, s *Service, text, language string, threads int) {
	t.Helper()
	started := time.Now()
	pcm, err := s.Synthesize(t.Context(), text, language, threads)
	if err != nil {
		t.Fatalf("real %s synthesis: %v", language, err)
	}
	defer clear(pcm)
	if len(pcm) < SampleRate/10 || len(pcm) > maxSamples {
		t.Fatalf("unexpected PCM length: %d", len(pcm))
	}
	var energy, peak float64
	for _, sample := range pcm {
		v := float64(sample)
		if math.IsNaN(v) || math.IsInf(v, 0) || v < -1 || v > 1 {
			t.Fatal("non-finite or out-of-range float32 PCM")
		}
		energy += v * v
		peak = max(peak, math.Abs(v))
	}
	if peak < 0.01 || energy/float64(len(pcm)) < 0.000001 {
		t.Fatal("real synthesis was silent")
	}
	t.Logf("%s: %d samples, %.3fs audio, %s generation", language, len(pcm), float64(len(pcm))/SampleRate, time.Since(started).Round(time.Millisecond))
}

func TestNativeSpeechLifecycleIntegration(t *testing.T) {
	piperArchive, piperModel := os.Getenv("TELEGRAMGW_PIPER_ARCHIVE"), os.Getenv("TELEGRAMGW_PIPER_MODEL_ARCHIVE")
	if piperArchive == "" && piperModel == "" {
		t.Skip("set TELEGRAMGW_PIPER_ARCHIVE and TELEGRAMGW_PIPER_MODEL_ARCHIVE for real native lifecycle checks")
	}
	if piperArchive == "" || piperModel == "" {
		t.Fatal("both local Piper artifacts must be explicitly supplied")
	}
	qwenArchive, qwenModels := os.Getenv("TELEGRAMGW_QWEN_ARCHIVE"), os.Getenv("TELEGRAMGW_QWEN_MODEL_DIR")
	if (qwenArchive == "") != (qwenModels == "") {
		t.Fatal("both local Qwen artifacts must be supplied to test engine switching")
	}
	previousVersion := buildinfo.Version
	buildinfo.Version = "v0.0.0"
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	fixtures := map[string]nativeIntegrationFixture{"piper": nativeIntegrationPiper(t, piperArchive, piperModel)}
	if qwenArchive != "" {
		fixtures["qwen3"] = nativeIntegrationQwen(t, qwenArchive, qwenModels)
	}
	state := t.TempDir()
	s := New(state, os.Args[0])
	t.Cleanup(s.Close)
	var downloads int
	s.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		downloads++
		return nil, errors.New("opt-in native integration tests forbid network access")
	})}
	failedEngine := ""
	s.nativeInstall = func(ctx context.Context, dir, engine string, _ func(audio.Operation)) error {
		if engine == failedEngine {
			return errors.New("injected local candidate installation failure")
		}
		fixture, ok := fixtures[engine]
		if !ok {
			return fmt.Errorf("no explicitly supplied local fixture for %s", engine)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := nativeIntegrationLinkTree(fixture.runtime, runtimeDirectory(dir)); err != nil {
			return err
		}
		return nativeIntegrationLinkTree(fixture.models, dir)
	}
	settings := audio.Settings{Enabled: true, Threads: 2, ReplyWithAudio: true, TTSEngine: "piper", TTSVoice: "italian"}
	var operations []audio.Operation
	if err := s.Prepare(t.Context(), settings, func(op audio.Operation) { operations = append(operations, op) }); err != nil {
		t.Fatalf("real Piper preparation: %v", err)
	}
	prepared := s.nativePending
	if prepared == nil {
		t.Fatal("preparation did not retain a native candidate")
	}
	piper := speechProcess(prepared)
	warming := false
	for _, operation := range operations {
		warming = warming || operation.State == "warming"
	}
	if piper == nil || s.nativeActive != nil || !warming {
		t.Fatal("preparation did not retain the real warmed candidate")
	}
	if err := s.Activate(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if s.nativeActive != prepared || speechProcess(prepared) != piper || s.nativePending != nil {
		t.Fatal("activation did not reuse the already warmed resident helper")
	}
	for _, request := range []struct{ text, language string }{{"Prima risposta. Il servizio è pronto.", "italian"}, {"Second reply. Check the gateway status.", "english"}} {
		nativeIntegrationPCM(t, s, request.text, request.language, settings.Threads)
		if speechProcess(prepared) != piper || piper.cmd.ProcessState != nil || piper.language != "italian" {
			t.Fatal("Piper failed to reuse the same Italian voice PID for both request languages")
		}
	}
	failedEngine = "qwen3"
	qwenSettings := settings
	qwenSettings.TTSEngine = "qwen3"
	if err := s.Prepare(t.Context(), qwenSettings, nil); err == nil {
		t.Fatal("injected candidate failure was accepted")
	}
	if s.nativeActive != prepared || speechProcess(prepared) != piper || s.nativePending != nil || piper.cmd.ProcessState != nil {
		t.Fatal("failed switch altered or stopped the real working Piper helper")
	}
	nativeIntegrationPCM(t, s, "La voce precedente funziona ancora.", "italian", settings.Threads)
	failedEngine = ""
	t.Run("switch to real Qwen and remove unused models", func(t *testing.T) {
		if qwenArchive == "" {
			t.Skip("set TELEGRAMGW_QWEN_ARCHIVE and TELEGRAMGW_QWEN_MODEL_DIR for the real Qwen switch")
		}
		for _, legacy := range []string{"italian", "english"} {
			if err := os.Mkdir(filepath.Join(state, legacy), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, legacy, "unused-fixture"), []byte("public legacy model fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		unrelated := filepath.Join(state, "unrelated-owner-file")
		if err := os.WriteFile(unrelated, []byte("fixture owner data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.Prepare(t.Context(), qwenSettings, nil); err != nil {
			t.Fatalf("real Qwen preparation: %v", err)
		}
		candidate := s.nativePending
		if candidate == nil {
			t.Fatal("Qwen preparation did not retain its candidate")
		}
		qwen := speechProcess(candidate)
		if qwen == nil || s.nativeActive != prepared || piper.cmd.ProcessState != nil {
			t.Fatal("warming Qwen prematurely replaced the real active Piper helper")
		}
		if err := s.Activate(t.Context(), qwenSettings); err != nil {
			t.Fatal(err)
		}
		if s.nativeActive != candidate || s.nativeEngine != "qwen3" || piper.cmd.ProcessState == nil || speechProcess(prepared) != nil {
			t.Fatal("real engine switch did not reap the previous helper")
		}
		for _, obsolete := range []string{"piper", "italian", "english"} {
			if _, err := os.Lstat(filepath.Join(state, obsolete)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unused %s files remain after activation: %v", obsolete, err)
			}
		}
		if data, err := os.ReadFile(unrelated); err != nil || string(data) != "fixture owner data" {
			t.Fatal("activation changed unrelated private owner data")
		}
		for _, text := range []string{"La nuova voce è pronta.", "Questa è la seconda risposta."} {
			nativeIntegrationPCM(t, s, text, "italian", qwenSettings.Threads)
			if speechProcess(candidate) != qwen || qwen.cmd.ProcessState != nil {
				t.Fatal("Qwen did not reuse its warmed model process")
			}
		}
		failedEngine = "piper"
		if err := s.Prepare(t.Context(), settings, nil); err == nil {
			t.Fatal("injected switch back failure was accepted")
		}
		if s.nativeActive != candidate || speechProcess(candidate) != qwen || qwen.cmd.ProcessState != nil {
			t.Fatal("failed switch back stopped the working Qwen helper")
		}
		nativeIntegrationPCM(t, s, "La voce rimane disponibile.", "italian", qwenSettings.Threads)
	})
	active := s.nativeActive
	resident := speechProcess(active)
	s.Close()
	if resident == nil || resident.cmd.ProcessState == nil || speechProcess(active) != nil {
		t.Fatal("Close did not kill and reap the real resident helper")
	}
	if _, err := s.Synthesize(t.Context(), "Una richiesta dopo la chiusura.", "italian", 2); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed native service accepted synthesis: %v", err)
	}
	if downloads != 0 {
		t.Fatal("native integration unexpectedly accessed the network")
	}
	for engine, fixture := range fixtures {
		if err := verifySpeechInventory(t.Context(), fixture.runtime); err != nil {
			t.Fatalf("source %s runtime changed after fixture cleanup: %v", engine, err)
		}
		if engine == "piper" {
			if err := verifySpeechInventory(t.Context(), filepath.Join(fixture.models, piperModelDir)); err != nil {
				t.Fatal(err)
			}
		} else {
			for _, spec := range nativeModels(engine) {
				if err := verifyAsset(t.Context(), filepath.Join(qwenModels, spec.name), spec); err != nil {
					t.Fatalf("original Qwen source was modified: %v", err)
				}
			}
		}
	}
}

// CI uses the same CPU capability predicate as production before executing the
// optional optimized binary. The portable binary is always smoked separately.
func TestOptimizedNativeSpeechSmoke(t *testing.T) {
	helper, models := os.Getenv("TELEGRAMGW_QWEN_OPTIMIZED_HELPER"), os.Getenv("TELEGRAMGW_QWEN_OPTIMIZED_MODEL_DIR")
	if helper == "" && models == "" {
		t.Skip("set local optimized Qwen helper and model directory to enable this check")
	}
	if helper == "" || models == "" {
		t.Fatal("both TELEGRAMGW_QWEN_OPTIMIZED_HELPER and TELEGRAMGW_QWEN_OPTIMIZED_MODEL_DIR are required")
	}
	if !optimizedSpeechCPU() {
		t.Skip("host CPU lacks the production optimized helper capabilities")
	}
	s := New(models, helper)
	s.command = func(language string, threads int) *exec.Cmd {
		cmd := exec.Command(helper, "--dir", models, "--language", language, "--threads", strconv.Itoa(threads))
		cmd.Env = []string{"LANG=C.UTF-8"}
		return cmd
	}
	t.Cleanup(s.Close)
	for _, tc := range []struct {
		language string
		phrases  []string
	}{
		{"italian", []string{"La sintesi vocale è pronta.", "Questa è la seconda risposta."}},
		{"english", []string{"Voice replies are ready.", "This is the second reply."}},
	} {
		var resident *helperProcess
		for _, text := range tc.phrases {
			nativeIntegrationPCM(t, s, text, tc.language, 2)
			current := speechProcess(s)
			if current == nil || current.cmd.ProcessState != nil || (resident != nil && current != resident) {
				t.Fatal("optimized Qwen helper did not retain its engine for both requests")
			}
			resident = current
		}
	}
}
