package embedding

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/cmplx"
	"strings"
	"sync"
)

// Audio for EmbeddingGemma 2's audio encoder (#1651), prepared as its
// processor (Gemma4AudioFeatureExtractor in Hugging Face Transformers and
// Transformers.js) prepares it: mono at 16 kHz, zero-padded to a multiple of
// 128 samples, framed semicausally (160 zeros before, 320-sample periodic
// Hann frames every 160 samples, as many as unfold(321, 160) makes), the
// magnitude of each frame's 512-point FFT through 128 HTK mel filters from 0
// to 8 kHz, plus 0.001 and its natural log; a frame not wholly inside the
// clip is zeroed and masked out. The encoder keeps every fourth frame: one
// soft token per 40 ms, 25 per second.
//
// Only WAV is decoded: Go's standard library has no MP3 decoder, the other
// format Google documents for gemini-embedding-2.

// AudioMIMETypes are the audio types decoded here.
var AudioMIMETypes = []string{"audio/wav"}

// wavMIMETypes are the names WAV goes by.
var wavMIMETypes = map[string]bool{"audio/wav": true, "audio/x-wav": true, "audio/wave": true, "audio/vnd.wave": true}

// IsAudioMIMEType reports whether mt names an audio type, decodable or not.
func IsAudioMIMEType(mt string) bool { return strings.HasPrefix(baseMIME(mt), "audio/") }

func baseMIME(mt string) string {
	mt = strings.ToLower(strings.TrimSpace(mt))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt
}

// AudioSampleRate is the rate the feature extractor takes.
const AudioSampleRate = 16000

// MaxAudioSeconds is the most audio embedded in one input, all clips
// together: Google's documented 180 seconds per gemini-embedding-2 request.
// (The reference processors truncate a clip at 30 seconds by default; the
// model takes minutes.)
const MaxAudioSeconds = 180

// Feature extractor values, from the model's processor_config.json.
const (
	audioFrameLength = 320
	audioHopLength   = 160
	audioFFTLength   = 512
	audioMelBins     = 128
	audioMelFloor    = 0.001
	audioMaxHz       = 8000.0
	audioPadMultiple = 128
	// audioFrameStride is how many frames make one soft token: the
	// encoder's two stride-2 convolutions.
	audioFrameStride = 4
)

// DecodeAudio decodes a clip of one of AudioMIMETypes to mono samples at
// AudioSampleRate.
func DecodeAudio(mimeType string, data []byte) ([]float32, error) {
	mt := baseMIME(mimeType)
	if !wavMIMETypes[mt] {
		return nil, fmt.Errorf("mime_type %q is not supported; send one of %s (Go's standard library has no decoder for it)",
			mimeType, strings.Join(AudioMIMETypes, ", "))
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the audio data is empty")
	}
	w, err := decodeWAV(data)
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid WAV file: %v", err)
	}
	if secs := float64(len(w.samples)) / float64(w.channels) / float64(w.rate); secs > MaxAudioSeconds {
		return nil, fmt.Errorf("the clip is %.1f seconds; at most %d seconds are embedded", secs, MaxAudioSeconds)
	}
	mono := downmix(w.samples, w.channels)
	if w.rate != AudioSampleRate {
		mono = resample(mono, w.rate, AudioSampleRate)
	}
	if len(mono) == 0 {
		return nil, fmt.Errorf("the clip has no samples")
	}
	return mono, nil
}

type wav struct {
	samples  []float32 // interleaved, in [-1, 1]
	channels int
	rate     int
}

// decodeWAV reads a RIFF WAVE file of integer PCM (8, 16, 24 or 32 bits) or
// IEEE float (32 or 64 bits), plain or WAVE_FORMAT_EXTENSIBLE.
func decodeWAV(b []byte) (wav, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return wav{}, fmt.Errorf("no RIFF WAVE header")
	}
	var (
		format, channels, bits uint16
		rate                   uint32
		haveFmt                bool
		data                   []byte
	)
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		body := b[p+8:]
		if size > len(body) {
			if id != "data" {
				return wav{}, fmt.Errorf("chunk %q is cut short", id)
			}
			size = len(body) // a streamed file's data size may be unset
		}
		body = body[:size]
		switch id {
		case "fmt ":
			if size < 16 {
				return wav{}, fmt.Errorf("fmt chunk of %d bytes", size)
			}
			format = binary.LittleEndian.Uint16(body[0:2])
			channels = binary.LittleEndian.Uint16(body[2:4])
			rate = binary.LittleEndian.Uint32(body[4:8])
			bits = binary.LittleEndian.Uint16(body[14:16])
			if format == 0xFFFE {
				if size < 26 {
					return wav{}, fmt.Errorf("extensible fmt chunk of %d bytes", size)
				}
				format = binary.LittleEndian.Uint16(body[24:26]) // the sub-format GUID's first two bytes
			}
			haveFmt = true
		case "data":
			data = body
		}
		p += 8 + size + size&1
	}
	if !haveFmt {
		return wav{}, fmt.Errorf("no fmt chunk")
	}
	if data == nil {
		return wav{}, fmt.Errorf("no data chunk")
	}
	if channels != 1 && channels != 2 {
		return wav{}, fmt.Errorf("%d channels; mono and stereo are embedded", channels)
	}
	if rate < 8000 || rate > 192000 {
		return wav{}, fmt.Errorf("a sample rate of %d Hz; 8000 to 192000 Hz are embedded", rate)
	}
	width := int(bits) / 8
	switch {
	case format == 1 && (bits == 8 || bits == 16 || bits == 24 || bits == 32):
	case format == 3 && (bits == 32 || bits == 64):
	default:
		return wav{}, fmt.Errorf("format %d with %d bits per sample; integer PCM of 8, 16, 24 or 32 bits and float of 32 or 64 bits are embedded", format, bits)
	}
	frame := width * int(channels)
	n := len(data) / frame * int(channels)
	out := make([]float32, n)
	for i := range out {
		s := data[i*width:]
		switch {
		case format == 3 && bits == 32:
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(s))
		case format == 3:
			out[i] = float32(math.Float64frombits(binary.LittleEndian.Uint64(s)))
		case bits == 8:
			out[i] = (float32(s[0]) - 128) / 128
		case bits == 16:
			out[i] = float32(int16(binary.LittleEndian.Uint16(s))) / 32768
		case bits == 24:
			v := int32(uint32(s[0])<<8|uint32(s[1])<<16|uint32(s[2])<<24) >> 8
			out[i] = float32(v) / 8388608
		default:
			out[i] = float32(float64(int32(binary.LittleEndian.Uint32(s))) / 2147483648)
		}
	}
	return wav{samples: out, channels: int(channels), rate: int(rate)}, nil
}

// downmix makes stereo mono as Transformers.js's load_audio does (after
// Hugging Face's ffmpeg_read): sqrt(2) * (left + right) / 2.
func downmix(s []float32, channels int) []float32 {
	if channels == 1 {
		return s
	}
	out := make([]float32, len(s)/2)
	for i := range out {
		out[i] = float32(math.Sqrt2 * (float64(s[2*i]) + float64(s[2*i+1])) / 2)
	}
	return out
}

// Resampling: band-limited interpolation with a Kaiser-windowed sinc, the
// filter libswresample (ffmpeg) uses by default (cutoff 0.97 of the lower
// Nyquist rate, beta 9, 32 zero crossings), from a table of 512 phases per
// zero crossing, linearly interpolated.
const (
	resampleZeros  = 32
	resamplePhases = 512
	resampleCutoff = 0.97
	resampleBeta   = 9.0
)

var (
	resampleTableOnce sync.Once
	resampleTable     []float64
)

func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; k < 50; k++ {
		term *= (x / (2 * float64(k))) * (x / (2 * float64(k)))
		sum += term
		if term < 1e-17*sum {
			break
		}
	}
	return sum
}

// kernel is the windowed sinc at x zero crossings from its centre, 0 <= x.
func kernel() []float64 {
	resampleTableOnce.Do(func() {
		n := resampleZeros*resamplePhases + 1
		resampleTable = make([]float64, n+1)
		i0 := besselI0(resampleBeta)
		for i := 0; i < n; i++ {
			x := float64(i) / resamplePhases
			s := 1.0
			if x > 0 {
				s = math.Sin(math.Pi*x) / (math.Pi * x)
			}
			r := x / resampleZeros
			resampleTable[i] = s * besselI0(resampleBeta*math.Sqrt(max(0, 1-r*r))) / i0
		}
	})
	return resampleTable
}

func resample(in []float32, from, to int) []float32 {
	table := kernel()
	ratio := float64(to) / float64(from)
	fc := resampleCutoff * min(1, ratio) // cutoff in cycles per input sample, x2
	half := float64(resampleZeros) / fc  // the filter's half width in input samples
	n := int(math.Ceil(float64(len(in)) * ratio))
	out := make([]float32, n)
	at := func(x float64) float64 { // the kernel at x input samples, scaled
		u := math.Abs(x) * fc * resamplePhases
		i := int(u)
		if i >= len(table)-2 {
			return 0
		}
		f := u - float64(i)
		return table[i] + f*(table[i+1]-table[i])
	}
	for o := range out {
		t := float64(o) / ratio
		lo := max(int(math.Ceil(t-half)), 0)
		hi := min(int(math.Floor(t+half)), len(in)-1)
		var v float64
		for k := lo; k <= hi; k++ {
			v += float64(in[k]) * at(t-float64(k))
		}
		out[o] = float32(v * fc)
	}
	return out
}

// AudioFeatures are one clip's audio-encoder inputs.
type AudioFeatures struct {
	// Features is Frames x 128 log-mel values, zero in masked frames.
	Features []float32
	// Mask is true for each frame wholly inside the clip.
	Mask []bool
	// Frames is the number of frames; SoftTokens the number of soft tokens
	// the encoder makes of them.
	Frames, SoftTokens int
}

var (
	melOnce    sync.Once
	melFilters [][]float32 // audioMelBins x (audioFFTLength/2 + 1)
	hannWindow []float64
)

func hzToMel(f float64) float64 { return 2595 * math.Log10(1+f/700) }
func melToHz(m float64) float64 { return 700 * (math.Pow(10, m/2595) - 1) }

func linspace(a, b float64, n int) []float64 {
	out := make([]float64, n)
	step := (b - a) / float64(n-1)
	for i := range out {
		out[i] = a + step*float64(i)
	}
	return out
}

// melBank is mel_filter_bank(257, 128, 0, 8000, 16000, norm=None, "htk"),
// and the periodic Hann window of the frame length.
func melBank() ([][]float32, []float64) {
	melOnce.Do(func() {
		bins := audioFFTLength/2 + 1
		fft := linspace(0, float64(AudioSampleRate/2), bins)
		mels := linspace(hzToMel(0), hzToMel(audioMaxHz), audioMelBins+2)
		hz := make([]float64, len(mels))
		for i, m := range mels {
			hz[i] = melToHz(m)
		}
		melFilters = make([][]float32, audioMelBins)
		for i := range melFilters {
			melFilters[i] = make([]float32, bins)
			for j, f := range fft {
				down := -(hz[i] - f) / (hz[i+1] - hz[i])
				up := (hz[i+2] - f) / (hz[i+2] - hz[i+1])
				melFilters[i][j] = float32(max(0, min(down, up)))
			}
		}
		// hanning(321)[:320]: 0.5 - 0.5 cos(2 pi i / 320).
		hannWindow = make([]float64, audioFrameLength)
		for i := range hannWindow {
			hannWindow[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(audioFrameLength))
		}
	})
	return melFilters, hannWindow
}

// fft is an in-place radix-2 FFT; len(x) is a power of two.
func fft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			wk := complex(1, 0)
			for k := 0; k < size/2; k++ {
				a, b := x[start+k], x[start+k+size/2]*wk
				x[start+k], x[start+k+size/2] = a+b, a-b
				wk *= w
			}
		}
	}
}

// ExtractAudioFeatures prepares mono 16 kHz samples for the audio encoder.
func ExtractAudioFeatures(samples []float32) (AudioFeatures, error) {
	if len(samples) == 0 {
		return AudioFeatures{}, fmt.Errorf("the clip has no samples")
	}
	filters, window := melBank()
	orig := len(samples)
	padded := orig
	if r := orig % audioPadMultiple; r != 0 {
		padded += audioPadMultiple - r
	}
	padLeft := audioFrameLength / 2
	frames := (padded+padLeft-(audioFrameLength+1))/audioHopLength + 1
	if frames <= 0 {
		return AudioFeatures{}, fmt.Errorf("the clip (%d samples) is too short to embed", orig)
	}
	// The waveform with its semicausal left padding.
	wave := make([]float64, padLeft+padded)
	for i, s := range samples {
		wave[padLeft+i] = float64(s)
	}
	bins := audioFFTLength/2 + 1
	out := AudioFeatures{
		Features: make([]float32, frames*audioMelBins),
		Mask:     make([]bool, frames),
		Frames:   frames,
	}
	buf := make([]complex128, audioFFTLength)
	mag := make([]float32, bins)
	for f := 0; f < frames; f++ {
		// The frame is valid when all of its unfold(321) samples are audio.
		last := f*audioHopLength + audioFrameLength
		out.Mask[f] = last >= padLeft && last < padLeft+orig
		if !out.Mask[f] {
			continue // zeroed
		}
		for j := range buf {
			buf[j] = 0
		}
		off := f * audioHopLength
		for j := 0; j < audioFrameLength && off+j < len(wave); j++ {
			buf[j] = complex(wave[off+j]*window[j], 0)
		}
		fft(buf)
		for k := 0; k < bins; k++ {
			mag[k] = float32(cmplx.Abs(buf[k]))
		}
		row := out.Features[f*audioMelBins : (f+1)*audioMelBins]
		for m, filt := range filters {
			var v float32
			for k, c := range filt {
				v += c * mag[k]
			}
			row[m] = float32(math.Log(float64(v) + audioMelFloor))
		}
	}
	for f := 0; f < frames; f += audioFrameStride {
		if out.Mask[f] {
			out.SoftTokens++
		}
	}
	if out.SoftTokens == 0 {
		return AudioFeatures{}, fmt.Errorf("the clip (%d samples) is too short to embed", orig)
	}
	return out, nil
}
