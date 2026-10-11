// Package voice decodes Telegram voice messages and runs a persistent local
// Whisper helper. Audio and transcripts stay in the gateway process and its
// child process; this package does not call a remote transcription service.
package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/pion/opus"
)

const (
	SampleRate         = 16000
	MaxInputBytes      = 20 << 20
	DefaultMaxDuration = 300 * time.Second
	HardMaxDuration    = 600 * time.Second
	maxAudioPacket     = 64 << 10
	maxTagsPacket      = 1 << 20
	maxPacketSamples   = SampleRate * 120 / 1000
)

var (
	ErrUnsupportedAudio = errors.New("unsupported audio format: expected Ogg/Opus with mono or stereo mapping family 0")
	ErrInvalidAudio     = errors.New("invalid Ogg/Opus audio")
	ErrAudioTooLarge    = errors.New("audio input exceeds 20 MiB")
	ErrAudioTooLong     = errors.New("audio exceeds maximum duration")
)

// DecodeOggOpus returns mono float32 PCM at 16 kHz. Only a single Ogg/Opus
// logical stream with complete pages and packets is supported. Telegram iOS
// streams can end at a complete audio page without an Ogg end-of-stream flag.
// All container checks and decoding run in Go, without ffmpeg, cgo, or temporary
// audio files.
func DecodeOggOpus(ctx context.Context, input io.Reader) ([]float32, error) {
	return DecodeOggOpusWithLimit(ctx, input, DefaultMaxDuration)
}

// DecodeOggOpusWithLimit permits a duration up to HardMaxDuration. The bound is
// enforced against decoded samples, independently of untrusted granule values.
// Cancellation is checked between input reads, pages, and Opus packets; callers
// must arrange cancellation of a reader that can itself block indefinitely.
func DecodeOggOpusWithLimit(ctx context.Context, input io.Reader, limit time.Duration) ([]float32, error) {
	if limit <= 0 || limit > HardMaxDuration {
		return nil, fmt.Errorf("voice: duration limit must be positive and at most %s", HardMaxDuration)
	}
	data, err := readAudio(ctx, input)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 || string(data[:4]) != "OggS" {
		return nil, ErrUnsupportedAudio
	}
	d, err := opus.NewDecoderWithOutput(SampleRate, 1)
	if err != nil {
		return nil, fmt.Errorf("voice: initialize Opus decoder: %w", err)
	}
	state := oggDecodeState{decoder: d, maxSamples: int(limit * SampleRate / time.Second)}
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, used, err := parseOggPage(data[offset:])
		if err != nil {
			return nil, err
		}
		offset += used
		if err := state.page(ctx, page); err != nil {
			return nil, err
		}
	}
	if len(state.partial) != 0 || state.audioPackets == 0 {
		return nil, invalidAudio("unfinished packet or missing audio packets")
	}
	if !state.ended {
		// Telegram iOS finishes with writeFrame(nil, 0), flushing buffered
		// pages without submitting an EOS-marked packet:
		// https://github.com/TelegramMessenger/Telegram-iOS/blob/f1dd7a2dbd02cbbf513e75d5695d8d36d1cf5838/submodules/OpusBinding/Sources/opusenc/opusenc.m#L592-L613
		// Permit physical EOF only after a fully validated audio page. A
		// missing whole page cannot be distinguished from this producer's
		// intentional EOF; partial pages/packets are always rejected.
		if !state.lastPageHasAudio || !state.haveAudioGranule {
			return nil, invalidAudio("missing complete final audio page")
		}
		state.finalGranule = state.lastGranule
	}
	// Granule positions and pre-skip use 48 kHz units, regardless of the
	// decoder output rate. Fractional 16 kHz samples are rounded down.
	if state.finalGranule < state.granuleOffset || state.finalGranule-state.granuleOffset < uint64(state.preSkip) {
		return nil, invalidAudio("final granule precedes pre-skip")
	}
	count := int((state.finalGranule - state.granuleOffset - uint64(state.preSkip)) / 3)
	start := int(state.preSkip) / 3
	if count <= 0 || start+count > len(state.samples) {
		return nil, invalidAudio("invalid final sample count")
	}
	if count > state.maxSamples {
		return nil, ErrAudioTooLong
	}
	pcm := state.samples[start : start+count]
	for i := range pcm {
		// Ogg output gain and codec overshoot can exceed unit amplitude.
		// Whisper consumes normalized PCM; saturate after applying gain.
		pcm[i] = max(-1, min(1, pcm[i]*state.gain))
	}
	return pcm, nil
}

func readAudio(ctx context.Context, input io.Reader) ([]byte, error) {
	if input == nil {
		return nil, invalidAudio("nil input")
	}
	data := make([]byte, 0, 32<<10)
	var buf [32 << 10]byte
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := input.Read(buf[:min(len(buf), MaxInputBytes+1-len(data))])
		if n < 0 || n > len(buf) {
			return nil, invalidAudio("invalid input reader")
		}
		data = append(data, buf[:n]...)
		if len(data) > MaxInputBytes {
			return nil, ErrAudioTooLarge
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return data, nil
			}
			return nil, fmt.Errorf("voice: read audio: %w", err)
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return nil, fmt.Errorf("voice: read audio: %w", io.ErrNoProgress)
			}
		} else {
			emptyReads = 0
		}
	}
}

type oggPage struct {
	flags    byte
	granule  uint64
	serial   uint32
	sequence uint32
	lacing   []byte
	data     []byte
}

func parseOggPage(data []byte) (oggPage, int, error) {
	if len(data) < 27 {
		return oggPage{}, 0, invalidAudio("truncated page header")
	}
	if string(data[:4]) != "OggS" || data[4] != 0 || data[5]&^byte(7) != 0 {
		return oggPage{}, 0, invalidAudio("invalid page signature, version, or flags")
	}
	headerSize := 27 + int(data[26])
	if len(data) < headerSize {
		return oggPage{}, 0, invalidAudio("truncated page lacing")
	}
	pageSize := headerSize
	for _, n := range data[27:headerSize] {
		pageSize += int(n)
	}
	if len(data) < pageSize {
		return oggPage{}, 0, invalidAudio("truncated page payload")
	}
	if oggChecksum(data[:pageSize]) != binary.LittleEndian.Uint32(data[22:26]) {
		return oggPage{}, 0, invalidAudio("page checksum mismatch")
	}
	return oggPage{
		flags: data[5], granule: binary.LittleEndian.Uint64(data[6:14]),
		serial: binary.LittleEndian.Uint32(data[14:18]), sequence: binary.LittleEndian.Uint32(data[18:22]),
		lacing: data[27:headerSize], data: data[headerSize:pageSize],
	}, pageSize, nil
}

var oggCRCTable = func() (table [256]uint32) {
	for i := range table {
		v := uint32(i) << 24
		for range 8 {
			if v&0x80000000 != 0 {
				v = v<<1 ^ 0x04c11db7
			} else {
				v <<= 1
			}
		}
		table[i] = v
	}
	return
}()

func oggChecksum(page []byte) uint32 {
	var crc uint32
	for i, b := range page {
		if i >= 22 && i < 26 {
			b = 0
		}
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}

type oggDecodeState struct {
	decoder                    opus.Decoder
	serial, nextSequence       uint32
	packetNumber, audioPackets int
	partial                    []byte
	samples                    []float32
	maxSamples                 int
	preSkip                    uint16
	gain                       float32
	granuleOffset              uint64
	lastGranule, finalGranule  uint64
	haveAudioGranule, ended    bool
	lastPageHasAudio           bool
}

func (s *oggDecodeState) page(ctx context.Context, p oggPage) error {
	if s.ended {
		return invalidAudio("chained streams or data after end of stream")
	}
	first := s.packetNumber == 0
	if first {
		if p.flags != 2 || p.sequence != 0 || p.granule != 0 {
			return invalidAudio("invalid beginning of stream")
		}
		s.serial = p.serial
	} else if p.flags&2 != 0 {
		return invalidAudio("unexpected beginning of stream")
	}
	if p.serial != s.serial || p.sequence != s.nextSequence {
		return invalidAudio("stream serial or page sequence mismatch")
	}
	s.nextSequence++
	if (len(s.partial) != 0) != (p.flags&1 != 0) {
		return invalidAudio("invalid packet continuation")
	}
	before := s.audioPackets
	offset, completed := 0, 0
	for _, n := range p.lacing {
		if err := ctx.Err(); err != nil {
			return err
		}
		packetLimit := maxAudioPacket
		if s.packetNumber == 1 {
			packetLimit = maxTagsPacket
		} else if s.packetNumber == 0 {
			packetLimit = 512
		}
		if len(s.partial)+int(n) > packetLimit {
			return invalidAudio("packet exceeds size limit")
		}
		s.partial = append(s.partial, p.data[offset:offset+int(n)]...)
		offset += int(n)
		if n == 255 {
			continue
		}
		if err := s.packet(s.partial); err != nil {
			return err
		}
		s.packetNumber++
		s.partial = s.partial[:0]
		completed++
	}
	if first && (completed != 1 || len(s.partial) != 0) {
		return invalidAudio("OpusHead must be alone on first page")
	}
	s.lastPageHasAudio = s.audioPackets > before
	if s.audioPackets == before {
		if p.flags&4 != 0 {
			if len(s.partial) != 0 || s.audioPackets == 0 || p.granule != s.lastGranule {
				return invalidAudio("invalid empty end page")
			}
			s.finalGranule, s.ended = p.granule, true
			return nil
		}
		if (completed == 0 && p.granule != math.MaxUint64) || (completed > 0 && p.granule != 0) {
			return invalidAudio("invalid header or continuation granule")
		}
		return nil
	}
	decoded := uint64(len(s.samples)) * 3
	if p.granule > math.MaxInt64 {
		return invalidAudio("missing audio granule")
	}
	if !s.haveAudioGranule && p.flags&4 == 0 {
		if p.granule < decoded {
			return invalidAudio("granule precedes decoded samples")
		}
		s.granuleOffset = p.granule - decoded
	}
	if p.granule < s.lastGranule || p.granule < s.granuleOffset || p.granule-s.granuleOffset > decoded {
		return invalidAudio("inconsistent audio granule")
	}
	if p.flags&4 == 0 && p.granule-s.granuleOffset != decoded {
		return invalidAudio("non-final granule truncates audio")
	}
	s.lastGranule, s.haveAudioGranule = p.granule, true
	if p.flags&4 != 0 {
		if len(s.partial) != 0 {
			return invalidAudio("unfinished packet at end of stream")
		}
		s.finalGranule, s.ended = p.granule, true
	}
	return nil
}

func (s *oggDecodeState) packet(packet []byte) error {
	switch s.packetNumber {
	case 0:
		if len(packet) < 8 || string(packet[:8]) != "OpusHead" {
			return ErrUnsupportedAudio
		}
		if len(packet) < 19 || packet[8] == 0 || packet[8] > 15 {
			return invalidAudio("invalid OpusHead length or version")
		}
		if (packet[9] != 1 && packet[9] != 2) || packet[18] != 0 {
			return ErrUnsupportedAudio
		}
		if len(packet) != 19 {
			return invalidAudio("invalid mapping family 0 OpusHead length")
		}
		s.preSkip = binary.LittleEndian.Uint16(packet[10:12])
		gainQ8 := int16(binary.LittleEndian.Uint16(packet[16:18]))
		s.gain = float32(math.Pow(10, float64(gainQ8)/(20*256)))
		return nil
	case 1:
		return validateOpusTags(packet)
	default:
		if len(packet) == 0 {
			return invalidAudio("empty audio packet")
		}
		var frame [maxPacketSamples]float32
		n, err := s.decoder.DecodeToFloat32(packet, frame[:])
		if err != nil {
			return fmt.Errorf("%w: Opus decode: %v", ErrInvalidAudio, err)
		}
		if n <= 0 || n > len(frame) {
			return invalidAudio("invalid decoded sample count")
		}
		// Pre-skip and one final packet may be discarded, but cannot permit
		// unbounded decoding even when the final granule claims short audio.
		if len(s.samples)+n > s.maxSamples+int(s.preSkip)/3+maxPacketSamples {
			return ErrAudioTooLong
		}
		for _, sample := range frame[:n] {
			if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
				return invalidAudio("non-finite decoded sample")
			}
		}
		s.samples = append(s.samples, frame[:n]...)
		s.audioPackets++
		return nil
	}
}

func validateOpusTags(packet []byte) error {
	if len(packet) < 16 || string(packet[:8]) != "OpusTags" {
		return invalidAudio("missing or invalid OpusTags")
	}
	offset := 8
	readLength := func() (int, bool) {
		if len(packet)-offset < 4 {
			return 0, false
		}
		n := uint64(binary.LittleEndian.Uint32(packet[offset : offset+4]))
		offset += 4
		if n > uint64(len(packet)-offset) {
			return 0, false
		}
		return int(n), true
	}
	n, ok := readLength()
	if !ok {
		return invalidAudio("truncated OpusTags vendor")
	}
	offset += n
	if len(packet)-offset < 4 {
		return invalidAudio("truncated OpusTags comment count")
	}
	comments := uint64(binary.LittleEndian.Uint32(packet[offset : offset+4]))
	offset += 4
	if comments > uint64((len(packet)-offset)/4) {
		return invalidAudio("invalid OpusTags comment count")
	}
	for range comments {
		n, ok = readLength()
		if !ok {
			return invalidAudio("truncated OpusTags comment")
		}
		offset += n
	}
	return nil
}

func invalidAudio(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidAudio, reason)
}
