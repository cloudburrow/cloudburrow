package embedding

import (
	"encoding/binary"
	"errors"
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
// WAV and MP3 (mp3.go) are decoded, the formats Google documents for
// gemini-embedding-2.

// AudioMIMETypes are the audio types decoded here.
var AudioMIMETypes = []string{"audio/wav", "audio/mpeg"}

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
	if !wavMIMETypes[mt] && !mp3MIMETypes[mt] {
		return nil, fmt.Errorf("mime_type %q is not supported; send one of %s",
			mimeType, strings.Join(AudioMIMETypes, ", "))
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the audio data is empty")
	}
	var (
		mono []float32
		rate int
	)
	if mp3MIMETypes[mt] {
		var err error
		if mono, rate, err = decodeMP3(data); err != nil {
			var long *tooLongError
			if errors.As(err, &long) {
				return nil, err
			}
			return nil, fmt.Errorf("the data is not a valid MP3 file: %v", err)
		}
	} else {
		w, err := decodeWAV(data)
		if err != nil {
			var long *tooLongError
			if errors.As(err, &long) {
				return nil, err
			}
			if errors.Is(err, ErrNotDecoded) {
				return nil, fmt.Errorf("the WAV file: %w", err)
			}
			return nil, fmt.Errorf("the data is not a valid WAV file: %v", err)
		}
		if secs := float64(len(w.samples)) / float64(w.channels) / float64(w.rate); secs > MaxAudioSeconds {
			return nil, &tooLongError{secs}
		}
		if mono, err = downmixNamed(w.samples, w.layout); err != nil {
			return nil, fmt.Errorf("the WAV file: %w", err)
		}
		rate = w.rate
	}
	if secs := float64(len(mono)) / float64(rate); secs > MaxAudioSeconds {
		return nil, &tooLongError{secs}
	}
	if rate != AudioSampleRate {
		mono = resample(mono, rate, AudioSampleRate)
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
	layout   []string // each channel's name, as ffmpeg names them
}

// WAVE format tags (the extensible sub-format GUID's first two bytes).
const (
	wavePCM   = 1
	waveFloat = 3
	waveALaw  = 6
	waveMuLaw = 7
	// waveMP3 is MPEG Layer III in a WAV (WAVE_FORMAT_MPEGLAYER3).
	waveMP3 = 0x55
)

// waveSpeakers are WAVE_FORMAT_EXTENSIBLE's dwChannelMask bits, in order,
// by ffmpeg's channel names.
var waveSpeakers = []string{"FL", "FR", "FC", "LFE", "BL", "BR", "FLC", "FRC", "BC", "SL", "SR",
	"TC", "TFL", "TFC", "TFR", "TBL", "TBC", "TBR"}

// defaultLayouts are the channels ffmpeg assumes for a WAV of n channels
// with no channel mask (or one that does not match n), measured with
// ffmpeg 9: mono, stereo, 2.1, 4.0, 5.0, 5.1, 6.1 and 7.1.
var defaultLayouts = [9][]string{
	1: {"FC"},
	2: {"FL", "FR"},
	3: {"FL", "FR", "LFE"},
	4: {"FL", "FR", "FC", "BC"},
	5: {"FL", "FR", "FC", "BL", "BR"},
	6: {"FL", "FR", "FC", "LFE", "BL", "BR"},
	7: {"FL", "FR", "FC", "LFE", "BC", "SL", "SR"},
	8: {"FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"},
}

// wavLayout names a WAV's channels from its channel mask, when the mask
// names exactly as many speakers as there are channels (as ffmpeg reads
// it), or else from ffmpeg's default layout for the count.
func wavLayout(channels int, mask uint32) ([]string, error) {
	if channels <= 2 {
		// Mono is itself and stereo √2·(L + R)/2 whatever the mask says,
		// as Transformers.js's load_audio takes them.
		return defaultLayouts[channels], nil
	}
	if mask != 0 && mask>>len(waveSpeakers) == 0 && bitsSet(mask) == channels {
		var names []string
		for i, n := range waveSpeakers {
			if mask&(1<<i) != 0 {
				names = append(names, n)
			}
		}
		return names, nil
	}
	if channels < len(defaultLayouts) {
		return defaultLayouts[channels], nil
	}
	return nil, notDecodedf("%d channels with no channel mask naming them are not decoded; up to 8 channels, or as many as the mask names, are", channels)
}

func bitsSet(m uint32) int {
	n := 0
	for ; m != 0; m &= m - 1 {
		n++
	}
	return n
}

// monoWeights are each channel's weight in ffmpeg's downmix to mono (`-ac 1`,
// its default centre and surround mix levels of -3 dB), measured with
// ffmpeg 9 one channel at a time: the front, front-of-centre and top front
// pairs at √½, the centre at 1, the back, side and back centre at ½, the
// LFE and the other top channels dropped. A stereo pair is √2·(L + R)/2, as
// Transformers.js's load_audio makes stereo mono.
var monoWeights = map[string]float64{
	"FL": math.Sqrt2 / 2, "FR": math.Sqrt2 / 2, "FLC": math.Sqrt2 / 2, "FRC": math.Sqrt2 / 2,
	"TFL": math.Sqrt2 / 2, "TFR": math.Sqrt2 / 2, "FC": 1,
	"BL": 0.5, "BR": 0.5, "SL": 0.5, "SR": 0.5, "BC": 0.5,
	"LFE": 0, "TC": 0, "TFC": 0, "TBL": 0, "TBC": 0, "TBR": 0,
}

// speakerPairs are the left and right channels ffmpeg's resampler takes only
// together: a layout with one and not the other it refuses to downmix.
var speakerPairs = [][2]string{{"FL", "FR"}, {"BL", "BR"}, {"FLC", "FRC"}, {"SL", "SR"}, {"TFL", "TFR"}, {"TBL", "TBR"}}

// downmixNamed makes interleaved samples of the named channels mono as
// ffmpeg's resampler does; one channel is itself.
func downmixNamed(s []float32, names []string) ([]float32, error) {
	channels := len(names)
	if channels == 1 {
		return s, nil
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, p := range speakerPairs {
		if have[p[0]] != have[p[1]] {
			return nil, notDecodedf("its channel layout (%s) has %s without %s, which is not downmixed",
				strings.Join(names, "+"), map[bool]string{true: p[0], false: p[1]}[have[p[0]]], map[bool]string{true: p[1], false: p[0]}[have[p[0]]])
		}
	}
	w := make([]float64, channels)
	for c, n := range names {
		w[c] = monoWeights[n]
	}
	out := make([]float32, len(s)/channels)
	for i := range out {
		var v float64
		for c, wc := range w {
			v += wc * float64(s[i*channels+c])
		}
		out[i] = float32(v)
	}
	return out, nil
}

// g711 decodes an 8-bit A-law or µ-law sample to 16-bit linear PCM, as
// ffmpeg's pcm_alaw and pcm_mulaw decoders do (ITU-T G.711).
func g711(b byte, alaw bool) int {
	if alaw {
		b ^= 0x55
		t := int(b & 0x0f)
		if seg := int(b&0x70) >> 4; seg != 0 {
			t = (t + t + 1 + 32) << (seg + 2)
		} else {
			t = (t + t + 1) << 3
		}
		if b&0x80 != 0 {
			return t
		}
		return -t
	}
	b = ^b
	t := (int(b&0x0f) << 3) + 0x84
	t <<= int(b&0x70) >> 4
	if b&0x80 != 0 {
		return 0x84 - t
	}
	return t - 0x84
}

// decodeWAV reads a RIFF WAVE file of integer PCM (1 to 32 bits), IEEE
// float (32 or 64 bits) or (#1651) G.711 A-law or µ-law, IMA or Microsoft
// ADPCM (adpcm.go), GSM 6.10 (gsm.go), G.726 (g726.go), MPEG Layer II (mp2.go) or MP3
// (WAVE_FORMAT_MPEGLAYER3), plain or
// WAVE_FORMAT_EXTENSIBLE, of up to 8 channels (or as many as its channel
// mask names).
func decodeWAV(b []byte) (wav, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return wav{}, fmt.Errorf("no RIFF WAVE header")
	}
	var (
		format, channels, bits, align uint16
		rate, mask                    uint32
		haveFmt                       bool
		data                          []byte
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
			align = binary.LittleEndian.Uint16(body[12:14])
			bits = binary.LittleEndian.Uint16(body[14:16])
			if format == 0xFFFE {
				if size < 26 {
					return wav{}, fmt.Errorf("extensible fmt chunk of %d bytes", size)
				}
				mask = binary.LittleEndian.Uint32(body[20:24])
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
	if channels == 0 {
		return wav{}, fmt.Errorf("the fmt chunk says 0 channels")
	}
	layout, err := wavLayout(int(channels), mask)
	if err != nil {
		return wav{}, err
	}
	if rate == 0 {
		return wav{}, fmt.Errorf("the fmt chunk says a sample rate of 0 Hz")
	}
	if rate < 8000 || rate > 192000 {
		return wav{}, notDecodedf("a sample rate of %d Hz is not decoded; 8000 to 192000 Hz are", rate)
	}
	if format == waveMP3 {
		return decodeWAVMP3(data)
	}
	if format == waveMPEG {
		return decodeWAVMPEG(data)
	}
	if format == waveG726APICOM {
		format = waveG726
	}
	if format == waveIMAADPCM || format == waveMSADPCM || format == waveGSM610 || format == waveG726 {
		dec := decodeIMAADPCM
		switch format {
		case waveMSADPCM:
			dec = decodeMSADPCM
		case waveGSM610:
			dec = func(data []byte, channels, align, _ int) ([]int16, error) { return decodeGSM610(data, channels, align) }
		case waveG726:
			dec = func(data []byte, channels, _, bits int) ([]int16, error) { return decodeG726(data, channels, bits) }
		}
		pcm, err := dec(data, int(channels), int(align), int(bits))
		if err != nil {
			return wav{}, err
		}
		out := make([]float32, len(pcm))
		for i, v := range pcm {
			out[i] = float32(v) / 32768
		}
		return wav{samples: out, channels: int(channels), rate: int(rate), layout: layout}, nil
	}
	// Integer PCM of a width that is not a whole number of bytes (12 or 20
	// bits) is its container's width, as ffmpeg reads it: the samples are
	// left-justified, so 12-bit PCM is read as 16-bit.
	width := (int(bits) + 7) / 8
	if format == wavePCM {
		bits = uint16(8 * width)
	}
	switch {
	case format == wavePCM && (bits == 8 || bits == 16 || bits == 24 || bits == 32):
	case format == waveFloat && (bits == 32 || bits == 64):
	case (format == waveALaw || format == waveMuLaw) && bits == 8:
	default:
		return wav{}, notDecodedf("WAVE format %d with %d bits per sample is not decoded; integer PCM of 1 to 32 bits, float of 32 or 64 bits, 8-bit A-law and µ-law, 2- to 5-bit IMA ADPCM, 4-bit Microsoft ADPCM, GSM 6.10, 16 to 40 kbit/s G.726, MPEG Layer II and MP3 are", format, bits)
	}
	frame := width * int(channels)
	n := len(data) / frame * int(channels)
	out := make([]float32, n)
	for i := range out {
		s := data[i*width:]
		switch {
		case format == waveALaw || format == waveMuLaw:
			out[i] = float32(g711(s[0], format == waveALaw)) / 32768
		case format == waveFloat && bits == 32:
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(s))
		case format == waveFloat:
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
	return wav{samples: out, channels: int(channels), rate: int(rate), layout: layout}, nil
}

// decodeWAVMP3 decodes a WAV's MPEG Layer III data chunk (#1651) as
// ffmpeg's wav demuxer and mp3 decoder do: every frame decoded, nothing
// trimmed (only the mp3 demuxer reads an Info tag's delay and padding),
// at the stream's own rate and channel count.
func decodeWAVMP3(data []byte) (wav, error) {
	s, err := parseMP3Stream(data)
	if err != nil {
		return wav{}, fmt.Errorf("its MP3 data: %v", err)
	}
	pcm, rate, err := decodeMP3PCM(data, MaxAudioSeconds)
	if err != nil {
		var long *tooLongError
		if errors.As(err, &long) {
			return wav{}, err
		}
		return wav{}, fmt.Errorf("its MP3 data: %v", err)
	}
	if rate < 8000 || rate > 48000 {
		return wav{}, fmt.Errorf("its MP3 data has a sample rate of %d Hz", rate)
	}
	if len(pcm) < 2 {
		return wav{}, errors.New("its MP3 data has no audio samples")
	}
	channels := 2
	if s.mono {
		channels = 1
		mono := make([]float32, len(pcm)/2)
		for i := range mono {
			mono[i] = pcm[2*i]
		}
		pcm = mono
	}
	layout, err := wavLayout(channels, 0)
	if err != nil {
		return wav{}, err
	}
	return wav{samples: pcm, channels: channels, rate: rate, layout: layout}, nil
}

// MuLaw decodes one G.711 mu-law sample to [-1, 1].
func MuLaw(b byte) float32 { return float32(g711(b, false)) / 32768 }

// ALaw decodes one G.711 A-law sample to [-1, 1].
func ALaw(b byte) float32 { return float32(g711(b, true)) / 32768 }

// DecodeWAVSpeech decodes a WAV file of any length and of up to 8 channels
// (or as many as its channel mask names) to mono samples at
// AudioSampleRate, the channels averaged: speech recognition's reading
// (internal/speech, #1799), which has no embedding limit.
func DecodeWAVSpeech(data []byte) ([]float32, error) {
	w, err := decodeWAV(data)
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid WAV file: %v", err)
	}
	return Resample(Average(w.samples, w.channels), w.rate, AudioSampleRate), nil
}

// DecodeWAVChannels decodes a WAV file as DecodeWAVSpeech does but keeps
// its channels apart: interleaved samples in [-1, 1], their count and the
// file's rate (#1799, Speech-to-Text's per-channel recognition).
func DecodeWAVChannels(data []byte) (interleaved []float32, channels, rate int, err error) {
	w, err := decodeWAV(data)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("the data is not a valid WAV file: %v", err)
	}
	return w.samples, w.channels, w.rate, nil
}

// Average makes interleaved samples of channels channels mono, averaging
// them.
func Average(s []float32, channels int) []float32 {
	if channels <= 1 {
		return s
	}
	out := make([]float32, len(s)/channels)
	for i := range out {
		var sum float32
		for c := 0; c < channels; c++ {
			sum += s[i*channels+c]
		}
		out[i] = sum / float32(channels)
	}
	return out
}

// Resample converts mono samples from one rate to another, as DecodeAudio
// does.
func Resample(in []float32, from, to int) []float32 {
	if from == to {
		return in
	}
	return resample(in, from, to)
}

// IsTooLong reports whether err is a clip longer than MaxAudioSeconds.
func IsTooLong(err error) bool {
	var long *tooLongError
	return errors.As(err, &long)
}

// downmix makes stereo mono as Transformers.js's load_audio does (after
// Hugging Face's ffmpeg_read): sqrt(2) * (left + right) / 2. More channels
// (a video's 3- to 8-channel PCM track) are downmixed as ffmpeg's default
// layout for their count is (#1651; they were taken as pairs before).
func downmix(s []float32, channels int) []float32 {
	if channels == 1 {
		return s
	}
	if channels > 2 && channels < len(defaultLayouts) {
		if mono, err := downmixNamed(s, defaultLayouts[channels]); err == nil {
			return mono
		}
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
	// As many samples as libswresample gives for the whole stream: the
	// length times the ratio, rounded (#1739: 71616 samples at 44.1 kHz
	// are 25983 at 16 kHz, 71936 at 48 kHz 23979).
	n := int(math.Round(float64(len(in)) * ratio))
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
