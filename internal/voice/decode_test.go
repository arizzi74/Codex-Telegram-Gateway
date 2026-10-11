package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"testing"
	"time"
)

// These samples are independently decoded by libopus 1.3.1 at 16 kHz mono,
// not by this package. The fixture license/provenance is in testdata/README.
func TestDecodeGoldenLibopus(t *testing.T) {
	for _, name := range []string{"tiny", "silk-stereo", "hybrid-stereo", "celt-mono", "telegram-ios-no-eos"} {
		t.Run(name, func(t *testing.T) {
			input := fixture(t, name+".ogg")
			got, err := DecodeOggOpus(context.Background(), bytes.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			golden := fixture(t, name+".f32le")
			if len(got)*4 != len(golden) {
				t.Fatalf("got %d samples, libopus returned %d", len(got), len(golden)/4)
			}
			var signal, noise float64
			for i, sample := range got {
				want := math.Float32frombits(binary.LittleEndian.Uint32(golden[i*4:]))
				delta := float64(sample - want)
				signal += float64(want) * float64(want)
				noise += delta * delta
			}
			if signal <= 1e-8 {
				t.Fatal("fixture must contain audible nonzero PCM")
			}
			snr := 10 * math.Log10(signal/noise)
			t.Logf("libopus agreement: %.1f dB SNR", snr)
			if snr < 35 {
				t.Fatalf("decoded audio differs from libopus: %.1f dB SNR", snr)
			}
		})
	}
}

func TestDecodeTelegramIOSCleanEOFMatchesEOS(t *testing.T) {
	data := fixture(t, "telegram-ios-no-eos.ogg")
	lastOffset := lastFixturePage(t, data)
	if data[lastOffset+5]&4 != 0 {
		t.Fatal("Telegram iOS fixture must end without EOS")
	}
	got, err := DecodeOggOpus(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 38296 { // (120 * 960 - 312 pre-skip) / 3.
		t.Fatalf("got %d samples after physical EOF and pre-skip", len(got))
	}
	control := append([]byte(nil), data...)
	control[lastOffset+5] |= 4
	binary.LittleEndian.PutUint32(control[lastOffset+22:], oggChecksum(control[lastOffset:]))
	want, err := DecodeOggOpus(context.Background(), bytes.NewReader(control))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("physical EOF changed sample count: %d != %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("physical EOF changed sample %d", i)
		}
	}
	if _, err := DecodeOggOpusWithLimit(context.Background(), bytes.NewReader(data), time.Second); !errors.Is(err, ErrAudioTooLong) {
		t.Fatalf("physical EOF bypassed duration bound: %v", err)
	}
}

func TestDecodeTelegramIOSRejectsIncompleteEOF(t *testing.T) {
	data := fixture(t, "telegram-ios-no-eos.ogg")
	lastOffset := lastFixturePage(t, data)
	headerSize := 27 + int(data[lastOffset+26])
	// Clean EOF is allowed only at a complete audio-page boundary. Partial
	// headers, lacing, payloads and packets, and non-audio tails still fail.
	cases := map[string][]byte{
		"partial header":    data[:lastOffset+10],
		"partial lacing":    data[:lastOffset+headerSize-1],
		"partial payload":   data[:len(data)-1],
		"empty tail":        appendPages(data, makePage(0, math.MaxUint64, 5, nil, nil)),
		"unfinished packet": appendPages(data, makePage(0, math.MaxUint64, 5, []byte{255}, make([]byte, 255))),
	}
	for name, mutate := range map[string]func([]byte){
		"checksum":        func(page []byte) { page[len(page)-1] ^= 1 },
		"page sequence":   func(page []byte) { binary.LittleEndian.PutUint32(page[18:22], 6) },
		"missing granule": func(page []byte) { binary.LittleEndian.PutUint64(page[6:14], math.MaxUint64) },
		"non-final trim":  func(page []byte) { binary.LittleEndian.PutUint64(page[6:14], 120*960-3) },
	} {
		changed := append([]byte(nil), data...)
		page := changed[lastOffset:]
		mutate(page)
		if name != "checksum" {
			binary.LittleEndian.PutUint32(page[22:26], oggChecksum(page))
		}
		cases[name] = changed
	}
	for name, malformed := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOggOpus(context.Background(), bytes.NewReader(malformed)); !errors.Is(err, ErrInvalidAudio) {
				t.Fatalf("accepted incomplete or invalid EOF: %v", err)
			}
		})
	}
}

func lastFixturePage(t *testing.T, data []byte) int {
	t.Helper()
	last := 0
	for offset := 0; offset < len(data); {
		_, used, err := parseOggPage(data[offset:])
		if err != nil {
			t.Fatal(err)
		}
		last = offset
		offset += used
	}
	return last
}

func TestDecodePacketContinuationAndTags(t *testing.T) {
	packets := fixturePackets(t, fixture(t, "celt-mono.ogg"))
	if len(packets[2]) <= 255 {
		t.Fatal("fixture needs a real audio packet spanning lacing segments")
	}
	head, tags, audio := packets[0], packets[1], packets[2]
	// Legal OpusTags padding is carried across pages independently of audio.
	tags = append(tags, make([]byte, 300)...)
	baseline := appendPages(makePage(2, 0, 0, packetLacing(head), head),
		makePage(0, 0, 1, packetLacing(tags), tags),
		makePage(4, 960, 2, packetLacing(audio), audio))
	want, err := DecodeOggOpus(context.Background(), bytes.NewReader(baseline))
	if err != nil {
		t.Fatal(err)
	}
	continued := appendPages(makePage(2, 0, 0, packetLacing(head), head),
		makePage(0, math.MaxUint64, 1, []byte{255}, tags[:255]),
		makePage(1, 0, 2, packetLacing(tags[255:]), tags[255:]),
		makePage(0, math.MaxUint64, 3, []byte{255}, audio[:255]),
		makePage(5, 960, 4, packetLacing(audio[255:]), audio[255:]))
	got, err := DecodeOggOpus(context.Background(), bytes.NewReader(continued))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("continuation changed duration: %d != %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("continuation changed sample %d", i)
		}
	}
}

func TestDecodePreSkipGainAndGranuleTrim(t *testing.T) {
	data := fixture(t, "tiny.ogg")
	baseline, err := DecodeOggOpus(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline) != 93 { // (591 final granule - 312 pre-skip) / 3.
		t.Fatalf("expected 93 samples after pre-skip and final trim, got %d", len(baseline))
	}
	packets := fixturePackets(t, data)
	binary.LittleEndian.PutUint16(packets[0][16:18], 6*256)
	data = appendPages(makePage(2, 0, 0, packetLacing(packets[0]), packets[0]),
		makePage(0, 0, 1, packetLacing(packets[1]), packets[1]),
		makePage(4, 591, 2, packetLacing(packets[2]), packets[2]))
	got, err := DecodeOggOpus(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	gain := float32(math.Pow(10, 6.0/20))
	for i := range got {
		if math.Abs(float64(got[i]-baseline[i]*gain)) > 1e-7 {
			t.Fatalf("output gain was not applied to sample %d", i)
		}
	}
	binary.LittleEndian.PutUint16(packets[0][16:18], math.MaxInt16)
	data = appendPages(makePage(2, 0, 0, packetLacing(packets[0]), packets[0]),
		makePage(0, 0, 1, packetLacing(packets[1]), packets[1]),
		makePage(4, 591, 2, packetLacing(packets[2]), packets[2]))
	clipped, err := DecodeOggOpus(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var peak float32
	for _, sample := range clipped {
		if sample < -1 || sample > 1 {
			t.Fatal("gain produced unnormalized PCM")
		}
		peak = max(peak, float32(math.Abs(float64(sample))))
	}
	if peak != 1 {
		t.Fatal("high-gain fixture did not exercise saturation")
	}
}

func TestDecodeRejectsMalformedAndTruncated(t *testing.T) {
	valid := fixture(t, "tiny.ogg")
	for i := range len(valid) {
		if _, err := DecodeOggOpus(context.Background(), bytes.NewReader(valid[:i])); err == nil {
			t.Fatalf("accepted truncation at byte %d", i)
		}
	}
	packets := fixturePackets(t, valid)
	cases := map[string][]byte{
		"wrong sequence":      appendPages(makePage(2, 0, 0, packetLacing(packets[0]), packets[0]), makePage(0, 0, 2, packetLacing(packets[1]), packets[1])),
		"missing tags":        appendPages(makePage(2, 0, 0, packetLacing(packets[0]), packets[0]), makePage(4, 591, 1, packetLacing(packets[2]), packets[2])),
		"chained":             append(append([]byte(nil), valid...), valid...),
		"orphan continuation": appendPages(makePage(2, 0, 0, packetLacing(packets[0]), packets[0]), makePage(1, 0, 1, packetLacing(packets[1]), packets[1])),
		"bad final granule":   appendPages(makePage(2, 0, 0, packetLacing(packets[0]), packets[0]), makePage(0, 0, 1, packetLacing(packets[1]), packets[1]), makePage(4, 100000, 2, packetLacing(packets[2]), packets[2])),
		"empty audio":         appendPages(makePage(2, 0, 0, packetLacing(packets[0]), packets[0]), makePage(0, 0, 1, packetLacing(packets[1]), packets[1]), makePage(4, 591, 2, []byte{0}, nil)),
	}
	corrupt := append([]byte(nil), valid...)
	corrupt[len(corrupt)-1] ^= 1
	cases["checksum"] = corrupt
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOggOpus(context.Background(), bytes.NewReader(data)); !errors.Is(err, ErrInvalidAudio) {
				t.Fatalf("expected invalid audio, got %v", err)
			}
		})
	}
}

func TestDecodeBoundsAndCancellation(t *testing.T) {
	if _, err := DecodeOggOpus(context.Background(), io.LimitReader(zeroReader{}, MaxInputBytes+1)); !errors.Is(err, ErrAudioTooLarge) {
		t.Fatalf("expected size limit, got %v", err)
	}
	if _, err := DecodeOggOpusWithLimit(context.Background(), bytes.NewReader(fixture(t, "silk-stereo.ogg")), time.Millisecond); !errors.Is(err, ErrAudioTooLong) {
		t.Fatalf("expected actual decoded duration limit, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DecodeOggOpus(ctx, bytes.NewReader(fixture(t, "tiny.ogg"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if _, err := DecodeOggOpus(context.Background(), stalledReader{}); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("expected stalled-reader bound, got %v", err)
	}
	for _, input := range [][]byte{[]byte("ID3\x03\x00"), []byte("RIFF0000WAVE"), nil} {
		if _, err := DecodeOggOpus(context.Background(), bytes.NewReader(input)); !errors.Is(err, ErrUnsupportedAudio) {
			t.Fatalf("expected unsupported format, got %v", err)
		}
	}
	packets := fixturePackets(t, fixture(t, "tiny.ogg"))
	packets[0][9], packets[0][18] = 3, 1
	if _, err := DecodeOggOpus(context.Background(), bytes.NewReader(makePage(2, 0, 0, packetLacing(packets[0]), packets[0]))); !errors.Is(err, ErrUnsupportedAudio) {
		t.Fatalf("expected unsupported channel mapping, got %v", err)
	}
	malformedTags := make([]byte, 16)
	copy(malformedTags, "OpusTags")
	binary.LittleEndian.PutUint32(malformedTags[12:16], math.MaxUint32)
	if err := validateOpusTags(malformedTags); !errors.Is(err, ErrInvalidAudio) {
		t.Fatalf("expected bounded rejection of huge comment count, got %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type stalledReader struct{}

func (stalledReader) Read([]byte) (int, error) { return 0, nil }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixturePackets(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var packets [][]byte
	var partial []byte
	for len(data) > 0 {
		p, used, err := parseOggPage(data)
		if err != nil {
			t.Fatal(err)
		}
		data = data[used:]
		offset := 0
		for _, n := range p.lacing {
			partial = append(partial, p.data[offset:offset+int(n)]...)
			offset += int(n)
			if n < 255 {
				packets = append(packets, partial)
				partial = nil
			}
		}
	}
	return packets
}

func packetLacing(data []byte) []byte {
	lacing := make([]byte, len(data)/255+1)
	for i := range len(lacing) - 1 {
		lacing[i] = 255
	}
	lacing[len(lacing)-1] = byte(len(data) % 255)
	return lacing
}

func makePage(flags byte, granule uint64, sequence uint32, lacing []byte, payload []byte) []byte {
	page := make([]byte, 27+len(lacing)+len(payload))
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], 42)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	page[26] = byte(len(lacing))
	copy(page[27:], lacing)
	copy(page[27+len(lacing):], payload)
	binary.LittleEndian.PutUint32(page[22:26], oggChecksum(page))
	return page
}

func appendPages(pages ...[]byte) []byte { return bytes.Join(pages, nil) }
