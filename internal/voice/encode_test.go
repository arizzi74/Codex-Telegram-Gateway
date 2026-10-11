package voice

import (
	"bytes"
	"context"
	"math"
	"testing"
)

func TestEncodeOggOpusRoundTrip(t *testing.T) {
	for _, rate := range []int{16000, 24000, 48000} {
		samples := make([]float32, rate+rate/7)
		for i := range samples {
			samples[i] = float32(.25 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		}
		encoded, err := EncodeOggOpus(context.Background(), samples, rate)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeOggOpus(context.Background(), bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("rate %d: %v", rate, err)
		}
		if want := len(samples) * SampleRate / rate; len(decoded) != want {
			t.Fatalf("rate %d: duration %d samples want %d", rate, len(decoded), want)
		}
		var energy float64
		for _, s := range decoded {
			if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
				t.Fatal("nonfinite decoded output")
			}
			energy += float64(s * s)
		}
		if energy/float64(len(decoded)) < .001 {
			t.Fatal("encoded speech is silent")
		}
		encoded[len(encoded)-1] ^= 1
		if _, err := DecodeOggOpus(context.Background(), bytes.NewReader(encoded)); err == nil {
			t.Fatal("corrupted page accepted")
		}
	}
}

func TestEncodeOggOpusRejectsInvalidAndCancelled(t *testing.T) {
	for _, test := range []struct {
		samples []float32
		rate    int
	}{{nil, 24000}, {[]float32{0}, 22050}, {[]float32{float32(math.NaN())}, 24000}, {[]float32{float32(math.Inf(1))}, 24000}} {
		if _, err := EncodeOggOpus(context.Background(), test.samples, test.rate); err == nil {
			t.Fatal("invalid audio accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := EncodeOggOpus(ctx, []float32{0}, 24000); err != context.Canceled {
		t.Fatalf("cancelled encode: %v", err)
	}
}
