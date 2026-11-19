package embedding

import (
	"bytes"
	"encoding/binary"
	"image"
	"math"
	"os"
	"path/filepath"
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
		"audio/wav ":  wavFile(t, tone(10, 16000, 1, 1), 9, 16000, 1, 16),    // nine channels, no mask (three decode since #1651)
		"audio/wave":  wavFile(t, tone(10, 16000, 1, 1), 1, 16000, 0x55, 16), // MP3 in WAV (12-bit PCM decodes since #1651)
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

// TestDecodeMP3 decodes MP3s of a one-second 440 Hz tone at half scale
// that ffmpeg 8's libmp3lame made (testdata/tone-*.mp3, with its Info
// frame and LAME tag): stereo at 44.1 kHz (MPEG-1,
// downmixed as a stereo WAV is) and mono at 16 kHz (MPEG-2, read as one
// channel). Each trims the Info frame and LAME's delay and padding as
// ffmpeg does, to the source's 16,000 samples at 16 kHz, in phase with the
// tone.
func TestDecodeMP3(t *testing.T) {
	for file, amp := range map[string]float64{"tone-stereo-44k.mp3": 0.5 * math.Sqrt2, "tone-mono-16k.mp3": 0.5} {
		b, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		for _, mt := range []string{"audio/mpeg", "audio/mp3", "AUDIO/MPEG; codecs=mp3"} {
			got, err := DecodeAudio(mt, b)
			if err != nil {
				t.Fatalf("%s as %s: %v", file, mt, err)
			}
			if len(got) != AudioSampleRate {
				t.Fatalf("%s: %d samples, want %d", file, len(got), AudioSampleRate)
			}
			want := tone(len(got), AudioSampleRate, 440, amp)
			var xy, xx, yy float64
			for i := 1600; i < len(got)-1600; i++ {
				xy += float64(got[i]) * want[i]
				xx += float64(got[i]) * float64(got[i])
				yy += want[i] * want[i]
			}
			if c := xy / math.Sqrt(xx*yy); c < 0.999 {
				t.Errorf("%s: correlation with the tone %.5f", file, c)
			}
			// ffmpeg 8 decodes these files to 0.950 of the source's level too
			// (the encoder's), the mono one within one 16-bit step of this.
			if r := math.Sqrt(xx / yy); math.Abs(r-0.95) > 0.01 {
				t.Errorf("%s: level %.3f of the tone's, want 0.950", file, r)
			}
		}
	}
}

// TestDecodeMPEG25MP3 decodes MPEG-2.5 MP3s (#1734) that ffmpeg 9's
// libmp3lame made of a 1.5-second sweep with tone bursts, whose transients
// make LAME switch to start and short blocks: 8 kHz mono, 11.025 and 12 kHz
// joint stereo. Each decodes, its Info frame, delay and padding trimmed, to
// ffmpeg 9's samples (testdata/*.ffmpeg.s16, `ffmpeg -i f.mp3 -f s16le`):
// the mono within one 16-bit step, the joint stereo within two a channel
// (go-mp3's float32 mid/side, as on MPEG-1 and 2 streams).
func TestDecodeMPEG25MP3(t *testing.T) {
	for _, c := range []struct {
		file           string
		rate, channels int
	}{
		{"sweep-mpeg25-8k-mono", 8000, 1},
		{"sweep-mpeg25-11k-stereo", 11025, 2},
		{"sweep-mpeg25-12k-stereo", 12000, 2},
	} {
		b, err := os.ReadFile(filepath.Join("testdata", c.file+".mp3"))
		if err != nil {
			t.Fatal(err)
		}
		ref, err := os.ReadFile(filepath.Join("testdata", c.file+".ffmpeg.s16"))
		if err != nil {
			t.Fatal(err)
		}
		got, rate, err := decodeMP3(b)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		n := len(ref) / 2 / c.channels
		if rate != c.rate || len(got) != n {
			t.Fatalf("%s: %d samples at %d Hz, want ffmpeg's %d at %d Hz", c.file, len(got), rate, n, c.rate)
		}
		var maxDiff float64
		for i := range n {
			want := float64(int16(binary.LittleEndian.Uint16(ref[2*c.channels*i:]))) / 32768
			if c.channels == 2 {
				r := float64(int16(binary.LittleEndian.Uint16(ref[2*c.channels*i+2:]))) / 32768
				want = math.Sqrt2 * (want + r) / 2
			}
			maxDiff = max(maxDiff, math.Abs(want-float64(got[i])))
		}
		// A downmix of two channels, √2 · (L + R) / 2, within two steps
		// each is within 2√2 steps.
		limit := 1.0
		if c.channels == 2 {
			limit = 2 * math.Sqrt2
		}
		if maxDiff*32768 > limit*1.001 {
			t.Errorf("%s: %.2f 16-bit steps from ffmpeg's decode", c.file, maxDiff*32768)
		}
		// Through DecodeAudio, resampled to 16 kHz: 1.5 seconds.
		if pcm, err := DecodeAudio("audio/mpeg", b); err != nil || math.Abs(float64(len(pcm)-AudioSampleRate*3/2)) > 1 {
			t.Errorf("%s: DecodeAudio gave %d samples, %v; want %d", c.file, len(pcm), err, AudioSampleRate*3/2)
		}
	}
	// The 8 kHz tone that was refused before #1734.
	b, err := os.ReadFile(filepath.Join("testdata", "tone-mpeg25-8k.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeAudio("audio/mpeg", b); err != nil || len(got) < AudioSampleRate*9/10 {
		t.Errorf("tone-mpeg25-8k.mp3: %d samples, %v", len(got), err)
	}
}

// TestDecodeMP3Refusals: what is not an MP3 the decoder reads is refused
// naming why, never a panic.
func TestDecodeMP3Refusals(t *testing.T) {
	t.Parallel() // the package's slow tests overlap (#1761)
	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	stereo := read("tone-stereo-44k.mp3")
	// 181 copies of a 1.08-second MP3 without an Info frame (nothing
	// trimmed) are 195 seconds; 166 are 179.3.
	long := bytes.Repeat(read("tone-notag-16k.mp3"), 181)
	cut := append([]byte(nil), stereo[:len(stereo)/2]...)
	for i := 600; i < len(cut); i += 7 {
		cut[i] ^= 0x5a // corrupt the frames after the first few
	}
	for what, c := range map[string]struct {
		data []byte
		want string
	}{
		"empty":    {nil, "the audio data is empty"},
		"text":     {[]byte("this is not audio at all"), "not a valid MP3 file: no MPEG audio frame"},
		"a WAV":    {wavFile(t, tone(1600, 16000, 440, 0.5), 1, 16000, 1, 16), "not a valid MP3 file"},
		"layer II": {[]byte{0xff, 0xfd, 0x90, 0x00, 0, 0, 0, 0}, "layer II"},
		"too long": {long, "at most 180 seconds are embedded"},
		"id3 only": {append([]byte("ID3\x04\x00\x00\x00\x00\x00\x0a"), make([]byte, 10)...), "no MPEG audio frame"},
	} {
		_, err := DecodeAudio("audio/mpeg", c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", what, err, c.want)
		}
	}
	// A corrupt stream decodes to something or fails; it never panics.
	_, _ = DecodeAudio("audio/mpeg", cut)
	// Up to 180 seconds are embedded.
	if got, err := DecodeAudio("audio/mpeg", bytes.Repeat(read("tone-notag-16k.mp3"), 166)); err != nil || len(got) < 179*AudioSampleRate {
		t.Errorf("166 copies: %d samples, %v", len(got), err)
	}
}
