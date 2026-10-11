package speech

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	SampleRate   = 24000
	MaxTextBytes = 32 << 10
	maxSamples   = SampleRate * 600
	maxHeader    = 64 << 10
	protocol     = 1
)

type request struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type response struct {
	Type        string `json:"type,omitempty"`
	Protocol    int    `json:"protocol,omitempty"`
	ID          string `json:"id,omitempty"`
	SampleCount int    `json:"sample_count,omitempty"`
	SampleRate  int    `json:"sample_rate,omitempty"`
	Error       string `json:"error,omitempty"`
}

func readJSON(r *bufio.Reader, into any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) != 0 {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if len(line) > maxHeader {
		return errors.New("speech: protocol header too large")
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return errors.New("speech: invalid protocol header")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("speech: invalid protocol header")
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > maxHeader {
		return errors.New("speech: protocol header too large")
	}
	return writeAll(w, append(data, '\n'))
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
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

func validText(text string) bool {
	return len(text) <= MaxTextBytes && utf8.ValidString(text) && strings.TrimSpace(text) != "" && !strings.ContainsRune(text, '\x00')
}

func languageName(language string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "it", "italian":
		return "italian", nil
	case "en", "english":
		return "english", nil
	default:
		return "", errors.New("speech: select Italian or English")
	}
}
