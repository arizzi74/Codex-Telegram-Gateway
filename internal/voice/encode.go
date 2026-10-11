package voice

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math"
	"time"

	"github.com/pion/opus"
)

// Outgoing speech has its own bound; incoming Telegram recordings keep the
// shorter transcription limit defined by HardMaxDuration.
const MaxSpeechDuration = 10 * time.Minute

// EncodeOggOpus encodes mono speech entirely in memory. Telegram accepts this
// container as a voice message. No external encoder or temporary file is used.
// Pocket TTS generates 24 kHz PCM; 16 and 48 kHz are supported as well.
func EncodeOggOpus(ctx context.Context, samples []float32, sampleRate int) ([]byte, error) {
	if sampleRate != 16000 && sampleRate != 24000 && sampleRate != 48000 {
		return nil, errors.New("voice: unsupported output sample rate")
	}
	if len(samples) == 0 || len(samples) > sampleRate*int(MaxSpeechDuration/time.Second) {
		return nil, errors.New("voice: invalid output duration")
	}
	for i, s := range samples {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if math.IsInf(float64(s), 0) || math.IsNaN(float64(s)) {
			return nil, errors.New("voice: non-finite output sample")
		}
	}
	enc, err := opus.NewEncoder(opus.WithChannels(1), opus.WithBitrate(32000), opus.WithComplexity(5))
	if err != nil {
		return nil, err
	}
	var serialBytes [4]byte
	if _, err := rand.Read(serialBytes[:]); err != nil {
		return nil, err
	}
	serial := binary.LittleEndian.Uint32(serialBytes[:])
	var out bytes.Buffer
	var sequence uint32
	// The encoder currently produces CELT-only packets, whose overlap is 120
	// samples at 48 kHz. Flush its tail and use the final granule to trim padding.
	const preSkip = 120
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 1
	binary.LittleEndian.PutUint16(head[10:], preSkip)
	binary.LittleEndian.PutUint32(head[12:], uint32(sampleRate))
	writeVoiceOggPage(&out, head, serial, sequence, 0, 2)
	sequence++
	vendor := "Codex Gateway"
	tags := make([]byte, 16+len(vendor))
	copy(tags, "OpusTags")
	binary.LittleEndian.PutUint32(tags[8:], uint32(len(vendor)))
	copy(tags[12:], vendor)
	writeVoiceOggPage(&out, tags, serial, sequence, 0, 0)
	sequence++
	factor := 48000 / sampleRate
	count := len(samples) * factor
	var frame [960]float32
	var packet [1275]byte
	for offset := 0; offset < count+preSkip; offset += len(frame) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clear(frame[:])
		for i := range frame {
			pos := offset + i
			if pos >= count {
				break
			}
			index := pos / factor
			a, b := samples[index], samples[min(index+1, len(samples)-1)]
			v := a + (b-a)*float32(pos%factor)/float32(factor)
			frame[i] = max(-1, min(1, v))
		}
		n, err := enc.EncodeFloat32(frame[:], packet[:])
		if err != nil {
			return nil, err
		}
		granule := offset + len(frame)
		flags := byte(0)
		if granule >= count+preSkip {
			granule = count + preSkip
			flags = 4
		}
		writeVoiceOggPage(&out, packet[:n], serial, sequence, uint64(granule), flags)
		sequence++
	}
	return out.Bytes(), nil
}

// Each packet gets a complete Ogg page. A terminating zero lacing value is
// required for packet lengths divisible by 255.
func writeVoiceOggPage(out *bytes.Buffer, packet []byte, serial, sequence uint32, granule uint64, flags byte) {
	segments := len(packet)/255 + 1
	page := make([]byte, 27+segments+len(packet))
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:], granule)
	binary.LittleEndian.PutUint32(page[14:], serial)
	binary.LittleEndian.PutUint32(page[18:], sequence)
	page[26] = byte(segments)
	for i := 0; i < segments-1; i++ {
		page[27+i] = 255
	}
	page[27+segments-1] = byte(len(packet) % 255)
	copy(page[27+segments:], packet)
	binary.LittleEndian.PutUint32(page[22:], oggChecksum(page))
	_, _ = out.Write(page)
}
