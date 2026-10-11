package speech

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/ThiraSoft/golem/token/sentencepiece"
)

func TestSpeechSegmentsBoundPreparedSentencesAndLongClauses(t *testing.T) {
	count := func(text string) int { return utf8.RuneCountInString(text) }
	for _, input := range []string{
		strings.Repeat("A complete sentence. ", 12),
		strings.Repeat("words without sentence punctuation ", 12),
		"Etichetta:\n" + strings.Repeat("Una frase con parole accentate è pronta, ", 10),
		strings.Repeat("identificatore", 12),
		strings.Repeat("a", maxSpeechTokens), // The added period also consumes a token.
	} {
		segments, err := speechSegments(input, "italian", count)
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) < 2 {
			t.Fatalf("oversized input was not split: %q", segments)
		}
		for _, segment := range segments {
			if count(segment) > maxSpeechTokens || !strings.ContainsRune(".!?…", []rune(segment)[utf8.RuneCountInString(segment)-1]) {
				t.Fatalf("unbounded or unfinished model prompt: %q", segment)
			}
		}
		// Added pauses and capitalization may change; words and identifiers
		// must remain present, including a long word that itself was split.
		letters := func(text string) string {
			return strings.ToLower(strings.Map(func(r rune) rune {
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					return r
				}
				return -1
			}, text))
		}
		if got, want := letters(strings.Join(segments, " ")), letters(input); got != want {
			t.Fatalf("substantive text changed: got %q, want %q", got, want)
		}
	}
}

func TestSpeechPreparationHandlesPunctuationAndLiterals(t *testing.T) {
	count := func(text string) int { return len(strings.Fields(text)) }
	for _, tc := range []struct{ input, language, want string }{
		{"La versione 0.5.94 è pronta:", "italian", "La versione zero punto cinque punto novantaquattro è pronta."},
		{"Version 0.5.94 is ready:", "english", "Version 0 point 5 point 94 is ready."},
		{"Apri “settings.json” e l’audio (locale).", "italian", "Apri settings punto json e l'audio locale."},
		{"Read \"the final answer\"", "english", "Read the final answer."},
		{"The gateway(worker) handles audio[locally].", "english", "The gateway worker handles audio locally."},
		{"Impostazioni:\n- Apri /var/lib/audio.\n• Voce pronta:", "italian", "Impostazioni. Apri barra var barra lib barra audio. Voce pronta."},
		{"Una frase.\n•\nAltra frase.", "italian", "Una frase. Altra frase."},
		{"Prima frase. Seconda frase! Terza frase?", "italian", "Prima frase. Seconda frase! Terza frase?"},
	} {
		segments, err := speechSegments(tc.input, tc.language, count)
		if err != nil || len(segments) != 1 || segments[0] != tc.want {
			t.Fatalf("speechSegments(%q) = %q, %v, want %q", tc.input, segments, err, tc.want)
		}
	}
	parts := splitSpeechText("First sentence. Second sentence.\nThird sentence!", ".!?…\n")
	if len(parts) != 3 {
		t.Fatalf("punctuation followed by whitespace did not split: %q", parts)
	}
}

func TestItalianVersionNumbersPreserveComponents(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Versione v0.5.94.", "Versione v zero punto cinque punto novantaquattro ."},
		{"Versione 01.003.12345678901234567890.", "Versione zero uno punto zero zero tre punto uno due tre quattro cinque sei sette otto nove zero uno due tre quattro cinque sei sette otto nove zero ."},
		{"È pronta 21.23.28 e 100.180.999.", "È pronta ventuno punto ventitré punto ventotto  e cento punto cento ottanta punto novecento novantanove ."},
		{"La voce è pronta. Nessun numero.", "La voce è pronta. Nessun numero."},
	} {
		if got := expandItalianDottedNumbers(tc.input); strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(tc.want), " ") {
			t.Fatalf("expandItalianDottedNumbers(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSpeechGenerationRejectsUnfinishedAudioAndClearsPriorSegments(t *testing.T) {
	count := func(text string) int { return len(text) }
	input := strings.Repeat("A complete public sentence. ", 8)
	for _, mode := range []string{"duration_limit", "engine_error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var generated [][]float32
			calls := 0
			sound, err := synthesizeSpeechSegments(ctx, input, "english", count, func(segment string) ([]float32, error) {
				calls++
				samples := []float32{0.25}
				var err error
				if calls == 2 {
					switch mode {
					case "duration_limit":
						frames := int((float64(count(segment))/3 + 2) * 12.5)
						samples = make([]float32, frames*1920)
						samples[0] = .25
					case "engine_error":
						err = errors.New("public failure")
					case "cancel":
						cancel()
					}
				}
				generated = append(generated, samples)
				return samples, err
			})
			if err == nil || sound != nil || calls != 2 {
				t.Fatalf("unfinished audio accepted: samples=%d, err=%v, calls=%d", len(sound), err, calls)
			}
			for _, samples := range generated {
				for _, sample := range samples {
					if sample != 0 {
						t.Fatal("generated audio survived failure")
					}
				}
			}
		})
	}
}

func TestNativeSpeechSegmentsUseTheModelTokenizer(t *testing.T) {
	dir := os.Getenv("TELEGRAMGW_SPEECH_NATIVE_DIR")
	if dir == "" {
		t.Skip("set TELEGRAMGW_SPEECH_NATIVE_DIR to the verified stock model directory")
	}
	for _, language := range []string{"english", "italian"} {
		tokenizer, err := sentencepiece.Load(filepath.Join(dir, language, "tokenizer.model"))
		if err != nil {
			t.Fatal(err)
		}
		count := func(text string) int { return len(tokenizer.Encode(text)) }
		text := "Release 0.5.94 is ready. Audio settings: open settings.json and review the path /var/lib/app/audio. The public documentation is at https://example.com/help. The local voice reads complete sentences and keeps all the words. " + strings.Repeat("This deliberately long clause has ordinary words without punctuation and must be split before it reaches the model context bound ", 3) + "and finishes here:"
		if language == "italian" {
			text = "La versione 0.5.94 è pronta. Impostazioni audio: apri settings.json e controlla il percorso /var/lib/app/audio. La documentazione pubblica è su https://example.com/help. La voce locale legge frasi complete e conserva tutte le parole. " + strings.Repeat("Questa frase lunga contiene parole normali senza punteggiatura e deve essere divisa prima di superare il contesto del modello ", 3) + "e termina qui:"
		}
		segments, err := speechSegments(text, language, count)
		if err != nil || len(segments) < 4 {
			t.Fatalf("native segmentation = %q, %v", segments, err)
		}
		for _, segment := range segments {
			if n := count(segment); n > maxSpeechTokens {
				t.Fatalf("%s prompt has %d model tokens", language, n)
			}
		}
	}
}
