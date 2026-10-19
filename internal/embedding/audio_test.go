package embedding

import (
	"bytes"
	"encoding/binary"
	"image"
	"math"
	"strings"
	"testing"
)

// wavFile writes samples (interleaved, in [-1, 1]) as a WAV file of format
// (1 integer PCM, 3 float) and bits per sample.
func wavFile(t *testing.T, samples []float64, channels, rate, format, bits int) []byte {
	t.Helper()
	var data bytes.Buffer
	for _, s := range samples {
		switch {
		case format == 3 && bits == 32:
			_ = binary.Write(&data, binary.LittleEndian, float32(s))
		case format == 3:
			_ = binary.Write(&data, binary.LittleEndian, s)
		case bits == 8:
			data.WriteByte(uint8(math.Round(s*127) + 128))
		case bits == 16:
			_ = binary.Write(&data, binary.LittleEndian, int16(math.Round(s*32767)))
		case bits == 24:
			v := int32(math.Round(s * 8388607))
			data.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16)})
		default:
			_ = binary.Write(&data, binary.LittleEndian, int32(math.Round(s*2147483647)))
		}
	}
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(4 + 8 + 16 + 8 + 4 + 8 + data.Len()))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	w(uint32(16))
	w(uint16(format))
	w(uint16(channels))
	w(uint32(rate))
	w(uint32(rate * channels * bits / 8))
	w(uint16(channels * bits / 8))
	w(uint16(bits))
	b.WriteString("LIST") // a chunk to skip
	w(uint32(4))
	b.WriteString("INFO")
	b.WriteString("data")
	w(uint32(data.Len()))
	b.Write(data.Bytes())
	return b.Bytes()
}

func tone(n, rate int, hz float64, amp float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = amp * math.Sin(2*math.Pi*hz*float64(i)/float64(rate))
	}
	return out
}

func TestDecodeAudioFormats(t *testing.T) {
	src := tone(1600, 16000, 440, 0.5)
	for _, c := range []struct{ format, bits int }{{1, 8}, {1, 16}, {1, 24}, {1, 32}, {3, 32}, {3, 64}} {
		got, err := DecodeAudio("audio/wav", wavFile(t, src, 1, 16000, c.format, c.bits))
		if err != nil {
			t.Fatalf("format %d, %d bits: %v", c.format, c.bits, err)
		}
		tol := 1e-4
		if c.bits == 8 {
			tol = 1e-2
		}
		if len(got) != len(src) {
			t.Fatalf("format %d, %d bits: %d samples, want %d", c.format, c.bits, len(got), len(src))
		}
		for i := range src {
			if math.Abs(float64(got[i])-src[i]) > tol {
				t.Fatalf("format %d, %d bits: sample %d is %v, want %v", c.format, c.bits, i, got[i], src[i])
			}
		}
	}
	// Stereo is downmixed as Transformers.js does: sqrt(2) * (L + R) / 2.
	st := []float64{0.5, 0.1, -0.2, 0.4}
	got, err := DecodeAudio("audio/x-wav", wavFile(t, st, 2, 16000, 3, 32))
	if err != nil || len(got) != 2 || math.Abs(float64(got[0])-math.Sqrt2*0.3) > 1e-6 || math.Abs(float64(got[1])-math.Sqrt2*0.1) > 1e-6 {
		t.Errorf("stereo: %v %v", got, err)
	}
	for mt, data := range map[string][]byte{
		"audio/mpeg":  {0xff, 0xfb},
		"audio/wav":   []byte("RIFF\x04\x00\x00\x00WAVE"),
		"audio/wav ":  wavFile(t, tone(10, 16000, 1, 1), 3, 16000, 1, 16),
		"audio/wave":  wavFile(t, tone(10, 16000, 1, 1), 1, 16000, 1, 12),
		"audio/vnd.x": {1},
	} {
		if _, err := DecodeAudio(mt, data); err == nil {
			t.Errorf("%s %d bytes was decoded", mt, len(data))
		}
	}
	long := wavFile(t, make([]float64, (MaxAudioSeconds+1)*8000), 1, 8000, 1, 8)
	if _, err := DecodeAudio("audio/wav", long); err == nil || !strings.Contains(err.Error(), "seconds") {
		t.Errorf("a clip over %d seconds: %v", MaxAudioSeconds, err)
	}
}

// TestResampleKeepsATone resamples a 1 kHz tone from 44.1 and 48 kHz to 16
// kHz and finds it there, at its amplitude, and a tone above the new Nyquist
// rate removed.
func TestResampleKeepsATone(t *testing.T) {
	for _, rate := range []int{44100, 48000, 8000} {
		in := tone(rate, rate, 1000, 0.5)
		f32 := make([]float32, len(in))
		for i, v := range in {
			f32[i] = float32(v)
		}
		out := resample(f32, rate, 16000)
		if len(out) != 16000 {
			t.Fatalf("%d Hz: %d samples, want 16000", rate, len(out))
		}
		want := tone(16000, 16000, 1000, 0.5)
		var worst float64
		for i := 200; i < 15800; i++ { // away from the edges
			worst = max(worst, math.Abs(float64(out[i])-want[i]))
		}
		if worst > 2e-3 {
			t.Errorf("%d Hz: worst error %.5f", rate, worst)
		}
	}
	in := tone(44100, 44100, 10000, 0.5)
	f32 := make([]float32, len(in))
	for i, v := range in {
		f32[i] = float32(v)
	}
	var peak float64
	for _, v := range resample(f32, 44100, 16000)[200:15800] {
		peak = max(peak, math.Abs(float64(v)))
	}
	if peak > 1e-3 {
		t.Errorf("a 10 kHz tone resampled to 16 kHz: peak %.5f, want it filtered out", peak)
	}
}

// TestAudioFeaturesMatchTheProcessor compares the log-mel features of a
// synthetic clip with Transformers.js 4.3.1's EmbeddingGemma2Processor
// (Gemma4AudioFeatureExtractor) on the same samples, 2026-10-19.
func TestAudioFeaturesMatchTheProcessor(t *testing.T) {
	for _, c := range []struct {
		n, frames, soft int
		lastMasked      bool
		vals            [][3]float64 // frame, mel bin, value
	}{
		{16050, 100, 25, false, [][3]float64{{0, 0, -6.907755374908447}, {0, 5, 0.5676465630531311}, {1, 7, -3.5885448455810547},
			{10, 7, -3.498460054397583}, {10, 60, -6.348125457763672}, {50, 100, -6.67824125289917}, {99, 7, -3.5885448455810547}}},
		{15830, 99, 25, true, [][3]float64{{0, 5, 0.5676465630531311}, {10, 60, -6.348125457763672}, {98, 7, 0}}},
	} {
		s := make([]float32, c.n)
		for i := range s {
			s[i] = float32(0.5*math.Sin(2*math.Pi*440*float64(i)/16000) + 0.1*math.Sin(2*math.Pi*3000*float64(i)/16000))
		}
		af, err := ExtractAudioFeatures(s)
		if err != nil {
			t.Fatal(err)
		}
		if af.Frames != c.frames || af.SoftTokens != c.soft || af.Mask[af.Frames-1] == c.lastMasked {
			t.Errorf("%d samples: %d frames, %d soft tokens, last frame valid %v; want %d, %d, %v",
				c.n, af.Frames, af.SoftTokens, af.Mask[af.Frames-1], c.frames, c.soft, !c.lastMasked)
		}
		for _, v := range c.vals {
			got := float64(af.Features[int(v[0])*audioMelBins+int(v[1])])
			if math.Abs(got-v[2]) > 1e-4 {
				t.Errorf("%d samples: frame %v bin %v is %.6f, want %.6f", c.n, v[0], v[1], got, v[2])
			}
		}
	}
	if _, err := ExtractAudioFeatures(make([]float32, 100)); err == nil {
		t.Error("a 100-sample clip was accepted")
	}
}

func TestMultimodalPromptWithAudio(t *testing.T) {
	clip := []float32{0}
	for _, c := range []struct {
		in   Input
		want string
		n    int
	}{
		{Input{Parts: []Part{{Audio: clip}}}, "<|audio|>", 1},
		{Input{TaskType: "RETRIEVAL_DOCUMENT", Parts: []Part{{Text: "Speech:"}, {Audio: clip}}}, "title: none | text: Speech:<|audio|>", 1},
	} {
		got, n, err := MultimodalPrompt(c.in)
		if err != nil || got != c.want || n != c.n {
			t.Errorf("%+v: %q %d %v, want %q %d", c.in, got, n, err, c.want, c.n)
		}
	}
	if got, n, err := MultimodalPrompt(Input{Parts: []Part{{Audio: clip}, {Image: image.NewRGBA(image.Rect(0, 0, 1, 1))}, {Audio: clip}}}); err != nil || got != "<|audio|> <|image|> <|audio|>" || n != 3 {
		t.Errorf("two clips and an image: %q %d %v", got, n, err)
	}
}
