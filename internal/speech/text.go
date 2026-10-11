package speech

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Pocket TTS was trained on short prompts. Golem's punctuation splitter does
// not split when a mark is followed by a space, and its fallback does not bound
// long clauses. Bound the fully prepared prompt with the model's own tokenizer
// before handing any individual segment to the engine.
const maxSpeechTokens = 50

func speechSegments(text, language string, count func(string) int) ([]string, error) {
	text = normalizeSpeechText(text, language)
	var units []string
	for _, sentence := range splitSpeechText(text, ".!?…\n") {
		if count(prepareSpeechChunk(sentence)) <= maxSpeechTokens {
			units = append(units, sentence)
			continue
		}
		for _, clause := range splitSpeechText(sentence, ",;:") {
			if count(prepareSpeechChunk(clause)) <= maxSpeechTokens {
				units = append(units, clause)
				continue
			}
			var current string
			for _, word := range strings.Fields(clause) {
				if candidate := strings.TrimSpace(current + " " + word); count(prepareSpeechChunk(candidate)) <= maxSpeechTokens {
					current = candidate
					continue
				}
				if current != "" {
					units = append(units, current)
					current = ""
				}
				// Even an unbroken identifier must not bypass the token bound.
				for _, r := range word {
					candidate := current + string(r)
					if count(prepareSpeechChunk(candidate)) > maxSpeechTokens {
						if current == "" {
							return nil, errors.New("speech: text cannot fit a model prompt")
						}
						units = append(units, current)
						candidate = string(r)
						if count(prepareSpeechChunk(candidate)) > maxSpeechTokens {
							return nil, errors.New("speech: text cannot fit a model prompt")
						}
					}
					current = candidate
				}
			}
			if current != "" {
				units = append(units, current)
			}
		}
	}
	var segments []string
	for _, unit := range units {
		// The model is trained on individual sentences. Even under fifty
		// tokens, merging a status sentence with a technical label can cause
		// an early EOS or filler in the label. Preserve each natural boundary.
		segments = append(segments, prepareSpeechChunk(unit))
	}
	if len(segments) == 0 {
		return nil, errors.New("speech: empty prepared text")
	}
	return segments, nil
}

func normalizeSpeechText(text, language string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if first, size := utf8.DecodeRuneInString(line); strings.ContainsRune("-*•", first) {
			if next, _ := utf8.DecodeRuneInString(line[size:]); line[size:] == "" || unicode.IsSpace(next) {
				line = strings.TrimSpace(line[size:])
			}
		}
		lines[i] = line
	}
	text = strings.Join(lines, "\n")
	// These delimiters are silent in prose. Italian's upstream model config
	// explicitly removes them and converts typographic apostrophes.
	text = strings.NewReplacer("\"", "", "“", "", "”", "", "„", "", "«", "", "»", "", "’", "'", "‘", "'", "(", " ", ")", " ", "[", " ", "]", " ").Replace(text)
	if language == "italian" {
		text = expandItalianDottedNumbers(text)
		text = expandItalianIdentifierDigits(text)
	}
	runes := []rune(text)
	var out strings.Builder
	for i, r := range runes {
		if r == '/' || r == '\\' {
			if language == "italian" {
				out.WriteString(" barra ")
			} else {
				out.WriteString(" slash ")
			}
			continue
		}
		if r == '.' && i > 0 && i+1 < len(runes) && (unicode.IsLetter(runes[i-1]) || unicode.IsDigit(runes[i-1])) && (unicode.IsLetter(runes[i+1]) || unicode.IsDigit(runes[i+1])) {
			// Do not let the engine turn versions, decimals and filenames into
			// separate sentences. Speak their separator in the selected language.
			separator := " dot "
			if unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
				separator = " point "
			}
			if language == "italian" {
				separator = " punto "
			}
			out.WriteString(separator)
			continue
		}
		out.WriteRune(r)
		if strings.ContainsRune(".!?…,;:", r) && i+1 < len(runes) && !unicode.IsSpace(runes[i+1]) && !strings.ContainsRune(".!?…,;:", runes[i+1]) {
			out.WriteByte(' ')
		}
	}
	return out.String()
}

func expandItalianIdentifierDigits(text string) string {
	runes := []rune(text)
	var out strings.Builder
	for i := 0; i < len(runes); {
		if runes[i] < '0' || runes[i] > '9' {
			out.WriteRune(runes[i])
			i++
			continue
		}
		end := i + 1
		for end < len(runes) && runes[end] >= '0' && runes[end] <= '9' {
			end++
		}
		if i > 0 && unicode.IsLetter(runes[i-1]) || end < len(runes) && unicode.IsLetter(runes[end]) {
			out.WriteByte(' ')
			for j, digit := range runes[i:end] {
				if j > 0 {
					out.WriteByte(' ')
				}
				out.WriteString(italianNumber(string(digit)))
			}
			out.WriteByte(' ')
		} else {
			out.WriteString(string(runes[i:end]))
		}
		i = end
	}
	return out.String()
}

func expandItalianDottedNumbers(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] < '0' || text[i] > '9' {
			out.WriteByte(text[i])
			i++
			continue
		}
		end := i
		for end < len(text) && (text[end] >= '0' && text[end] <= '9' || text[end] == '.') {
			end++
		}
		for end > i && text[end-1] == '.' {
			end--
		}
		components := strings.Split(text[i:end], ".")
		valid := len(components) > 1
		for _, component := range components {
			valid = valid && component != ""
		}
		if valid {
			out.WriteByte(' ')
			for j, component := range components {
				if j > 0 {
					out.WriteString(" punto ")
				}
				out.WriteString(italianNumber(component))
			}
			out.WriteByte(' ')
		} else {
			out.WriteString(text[i:end])
		}
		i = end
	}
	return out.String()
}

func italianNumber(digits string) string {
	units := [...]string{"zero", "uno", "due", "tre", "quattro", "cinque", "sei", "sette", "otto", "nove", "dieci", "undici", "dodici", "tredici", "quattordici", "quindici", "sedici", "diciassette", "diciotto", "diciannove"}
	// Spell long and zero-prefixed components digit by digit. Never parse an
	// arbitrary-length identifier into an integer or discard its leading zeros.
	if len(digits) > 3 || len(digits) > 1 && digits[0] == '0' {
		var words []string
		for _, digit := range digits {
			words = append(words, units[digit-'0'])
		}
		return strings.Join(words, " ")
	}
	n := 0
	for _, digit := range digits {
		n = n*10 + int(digit-'0')
	}
	var say func(int) string
	say = func(n int) string {
		if n < 20 {
			return units[n]
		}
		if n < 100 {
			tens := [...]string{"", "", "venti", "trenta", "quaranta", "cinquanta", "sessanta", "settanta", "ottanta", "novanta"}
			word := tens[n/10]
			if n%10 == 0 {
				return word
			}
			if n%10 == 1 || n%10 == 8 {
				word = word[:len(word)-1]
			}
			if n%10 == 3 {
				return word + "tré"
			}
			return word + units[n%10]
		}
		word := "cento"
		if n/100 > 1 {
			word = units[n/100] + word
		}
		if n%100 == 0 {
			return word
		}
		return word + " " + say(n%100)
	}
	return say(n)
}

func splitSpeechText(text, marks string) []string {
	var parts []string
	start := 0
	afterMark := false
	for i, r := range text {
		if strings.ContainsRune(marks, r) {
			afterMark = true
			continue
		}
		if afterMark && !unicode.IsSpace(r) {
			if part := strings.TrimSpace(text[start:i]); part != "" {
				parts = append(parts, part)
			}
			start = i
			afterMark = false
		}
	}
	if part := strings.TrimSpace(text[start:]); part != "" {
		parts = append(parts, part)
	}
	return parts
}

func prepareSpeechChunk(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	text = strings.NewReplacer(" .", ".", " !", "!", " ?", "?", " …", "…", " ,", ",", " ;", ";", " :", ":").Replace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	runes[0] = unicode.ToUpper(runes[0])
	text = string(runes)
	if !strings.ContainsRune(".!?…", runes[len(runes)-1]) {
		text = strings.TrimRightFunc(text, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",;:-–—", r) }) + "."
	}
	return text
}
