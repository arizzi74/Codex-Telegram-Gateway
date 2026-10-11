package speech

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"io"
	"math"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ThiraSoft/golem/pockettts"
	"github.com/ThiraSoft/golem/token/sentencepiece"
)

// RunHelper is called before loading gateway configuration when argv[1] is
// speech-helper. It reads final text and writes framed PCM only through pipes;
// neither text nor audio is stored in a file. Native libraries are not used.
func RunHelper(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("speech-helper", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", "", "private model directory")
	language := flags.String("language", "", "Italian or English")
	threads := flags.Int("threads", 0, "CPU threads")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !filepath.IsAbs(*dir) || *threads < 1 || *threads > 32 {
		return errors.New("speech: invalid helper arguments")
	}
	lang, err := languageName(*language)
	if err != nil {
		return err
	}
	runtime.GOMAXPROCS(*threads)
	// Verify before mmap: local corruption must never reach a weight decoder.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, spec := range pinnedAssets() {
		if spec.language == lang && verifyAsset(ctx, filepath.Join(*dir, lang, spec.name), spec) != nil {
			return errors.New("speech: model verification failed")
		}
	}
	path := filepath.Join(*dir, lang)
	engine, err := pockettts.Open(pockettts.Options{Weights: filepath.Join(path, "model.safetensors"), Tokenizer: filepath.Join(path, "tokenizer.model"), Language: lang})
	if err != nil {
		return errors.New("speech: model loading failed")
	}
	defer engine.Close()
	tokenizer, err := sentencepiece.Load(filepath.Join(path, "tokenizer.model"))
	if err != nil {
		return errors.New("speech: tokenizer loading failed")
	}
	voice, err := engine.LoadVoice(filepath.Join(path, "voice.safetensors"))
	if err != nil {
		return errors.New("speech: voice loading failed")
	}
	return serve(stdin, stdout, func(ctx context.Context, text string) ([]float32, error) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		settings := pockettts.DefaultSettings(engine.Language())
		settings.Ctx = ctx
		// Kyutai recommends 0.3 for both shipped six-layer models. Golem's
		// older default of 0.7 makes technical Italian text less reliable.
		settings.Temperature = 0.3
		count := 0
		settings.Frame = func(frame []float32) {
			count += len(frame)
			if count > maxSamples {
				cancel()
			}
		}
		return synthesizeSpeechSegments(ctx, text, lang, func(text string) int { return len(tokenizer.Encode(text)) }, func(segment string) ([]float32, error) {
			return engine.Synthesize(segment, voice, &settings)
		})
	})
}

func synthesizeSpeechSegments(ctx context.Context, text, language string, count func(string) int, generate func(string) ([]float32, error)) (sound []float32, err error) {
	segments, err := speechSegments(text, language, count)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			clear(sound)
			sound = nil
		}
	}()
	for _, segment := range segments {
		if err := ctx.Err(); err != nil {
			return sound, err
		}
		samples, generateErr := generate(segment)
		// Golem silently returns audio when its duration bound is exhausted.
		// That means EOS did not finish the utterance; never deliver that
		// potentially repeated or truncated speech as a successful reply.
		frames := int((float64(count(segment))/3 + 2) * pockettts.FramesPerSecond)
		limit := frames * (SampleRate * 2 / 25)
		if generateErr != nil || ctx.Err() != nil || len(samples) == 0 || len(samples) >= limit || len(sound)+len(samples) > maxSamples {
			clear(samples)
			if generateErr != nil {
				return sound, generateErr
			}
			if ctx.Err() != nil {
				return sound, ctx.Err()
			}
			return sound, errors.New("speech: generation did not finish within its model bound")
		}
		sound = append(sound, samples...)
		clear(samples)
	}
	return sound, nil
}

func serve(stdin io.Reader, stdout io.Writer, synthesize func(context.Context, string) ([]float32, error)) error {
	if err := writeJSON(stdout, response{Type: "ready", Protocol: protocol, SampleRate: SampleRate}); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(stdin, maxHeader+1)
	for {
		var req request
		if err := readJSON(reader, &req); err != nil {
			if errors.Is(err, io.EOF) && reader.Buffered() == 0 {
				return nil
			}
			return errors.New("speech: invalid request")
		}
		if req.ID == "" || len(req.ID) > 32 || !validText(req.Text) {
			return errors.New("speech: invalid request")
		}
		ctx, cancel := context.WithTimeout(context.Background(), synthesisLimit)
		samples, err := synthesize(ctx, req.Text)
		cancel()
		if err != nil || len(samples) == 0 || len(samples) > maxSamples {
			clear(samples)
			if err := writeJSON(stdout, response{ID: req.ID, Error: "synthesis_failed"}); err != nil {
				return err
			}
			continue
		}
		if err := writeGenerated(stdout, req.ID, samples); err != nil {
			return err
		}
	}
}

func writeGenerated(stdout io.Writer, id string, samples []float32) error {
	defer clear(samples)
	for i, sample := range samples {
		if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
			return errors.New("speech: invalid generated audio")
		}
		// Model waveforms can overshoot the PCM range slightly.
		samples[i] = max(float32(-1), min(float32(1), sample))
	}
	if err := writeJSON(stdout, response{ID: id, SampleCount: len(samples), SampleRate: SampleRate}); err != nil {
		return err
	}
	var chunk [32 << 10]byte
	defer clear(chunk[:])
	for offset := 0; offset < len(samples); {
		n := min(len(chunk)/4, len(samples)-offset)
		for i, sample := range samples[offset : offset+n] {
			binary.LittleEndian.PutUint32(chunk[i*4:], math.Float32bits(sample))
		}
		if err := writeAll(stdout, chunk[:n*4]); err != nil {
			return err
		}
		offset += n
	}
	return nil
}
