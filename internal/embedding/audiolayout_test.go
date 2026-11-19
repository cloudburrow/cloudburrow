package embedding

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
)

// TestDecodeMultichannelAndG711WAV (#1651) decodes WAVs of more than two
// channels and G.711 WAVs against ffmpeg's decode to mono (`ffmpeg -i f -ac 1
// -ar 16000 -f f32le`, testdata/*.ffmpeg-16k.f32), and a video's 5.1 PCM
// track, which was taken as pairs of channels before. The files, 0.5
// seconds at 16 kHz (so nothing is resampled), made with ffmpeg 9:
//
//	S6="aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*660*t)|0.25*sin(2*PI*330*t)|0.2*sin(2*PI*60*t)|0.15*sin(2*PI*1500*t)*sin(2*PI*2*t)|0.15*sin(2*PI*2500*t):s=16000:d=0.5:channel_layout=5.1"
//	S8="aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*660*t)|0.25*sin(2*PI*330*t)|0.2*sin(2*PI*60*t)|0.15*sin(2*PI*1500*t)|0.15*sin(2*PI*2500*t)|0.1*sin(2*PI*900*t)|0.1*sin(2*PI*1200*t):s=16000:d=0.5:channel_layout=7.1"
//	S1="aevalsrc=0.3*sin(2*PI*440*t)+0.1*sin(2*PI*3000*t):s=16000:d=0.5"
//	ffmpeg -f lavfi -i "$S6" -c:a pcm_s16le -map_metadata -1 -fflags +bitexact -flags +bitexact wav-51.wav  # WAVE_FORMAT_EXTENSIBLE, mask 0x60f
//	ffmpeg -f lavfi -i "$S8" -c:a pcm_s16le ... 7.1.wav, its fmt chunk rewritten as plain PCM with no mask: wav-71-plain.wav
//	ffmpeg -f lavfi -i "$S1" -c:a pcm_alaw ... wav-alaw.wav (and pcm_mulaw, wav-mulaw.wav)
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=0.5 -f lavfi -i "$S6" -c:v libx264 -c:a pcm_s16le -shortest -map_metadata -1 va-pcm51.mov
func TestDecodeMultichannelAndG711WAV(t *testing.T) {
	for _, file := range []string{"wav-51.wav", "wav-71-plain.wav", "wav-alaw.wav", "wav-mulaw.wav", "va-pcm51.mov"} {
		data := readTestdata(t, file)
		var (
			got []float32
			err error
		)
		if strings.HasSuffix(file, ".mov") {
			got, err = DecodeVideoAudio("video/quicktime", data, VideoOptions{})
		} else {
			got, err = DecodeAudio("audio/wav", data)
		}
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		ref := readTestdata(t, file[:strings.LastIndexByte(file, '.')]+".ffmpeg-16k.f32")
		want := make([]float32, len(ref)/4)
		for i := range want {
			want[i] = math.Float32frombits(binary.LittleEndian.Uint32(ref[4*i:]))
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d samples, ffmpeg %d", file, len(got), len(want))
		}
		var maxDiff float64
		for i := range got {
			maxDiff = max(maxDiff, math.Abs(float64(got[i]-want[i])))
		}
		t.Logf("%s: max %.2e from ffmpeg", file, maxDiff)
		if maxDiff > 1e-6 {
			t.Errorf("%s: %.2e from ffmpeg's decode, want at most 1e-6", file, maxDiff)
		}
	}
}

// wavBytes is a WAV of 16-bit samples with the given fmt fields; mask zero
// writes a plain fmt chunk.
func wavBytes(format uint16, channels int, rate uint32, bits uint16, mask uint32, frames int) []byte {
	var f []byte
	f = binary.LittleEndian.AppendUint16(f, format)
	if mask != 0 {
		f = binary.LittleEndian.AppendUint16(f[:0], 0xFFFE)
	}
	f = binary.LittleEndian.AppendUint16(f, uint16(channels))
	f = binary.LittleEndian.AppendUint32(f, rate)
	f = binary.LittleEndian.AppendUint32(f, rate*uint32(channels)*uint32(bits/8))
	f = binary.LittleEndian.AppendUint16(f, uint16(channels)*bits/8)
	f = binary.LittleEndian.AppendUint16(f, bits)
	if mask != 0 {
		f = binary.LittleEndian.AppendUint16(f, 22)
		f = binary.LittleEndian.AppendUint16(f, bits)
		f = binary.LittleEndian.AppendUint32(f, mask)
		f = binary.LittleEndian.AppendUint16(f, format)
		f = append(f, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71)
	}
	data := make([]byte, frames*channels*int(bits/8))
	b := []byte("WAVEfmt ")
	b = binary.LittleEndian.AppendUint32(b, uint32(len(f)))
	b = append(b, f...)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	return append(binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(b))), b...)
}

// TestWAVLayoutsAndRefusals checks ffmpeg's measured layouts (a mask that
// names as many speakers as there are channels, else the default for the
// count) and that valid WAVs this build does not decode are ErrNotDecoded,
// not invalid data.
func TestWAVLayoutsAndRefusals(t *testing.T) {
	for _, c := range []struct {
		channels int
		mask     uint32
		want     string
	}{
		{3, 0, "FL+FR+LFE"},
		{4, 0, "FL+FR+FC+BC"},
		{5, 0, "FL+FR+FC+BL+BR"},
		{6, 0x60F, "FL+FR+FC+LFE+SL+SR"},
		{6, 0x3, "FL+FR+FC+LFE+BL+BR"}, // a mask of two speakers for six channels: the default
		{2, 0x5, "FL+FR"},              // stereo is stereo whatever its mask
		{5, 0x7 | 0xC0, "FL+FR+FC+FLC+FRC"},
	} {
		got, err := wavLayout(c.channels, c.mask)
		if err != nil || strings.Join(got, "+") != c.want {
			t.Errorf("%d channels, mask %#x: %v, %v; want %s", c.channels, c.mask, got, err, c.want)
		}
	}
	// ffmpeg's weights (`-ac 1`, measured one channel at a time on FL+FR+X):
	// 5.1 of 0.01 to 0.06 is ffmpeg's 0.10621.
	mono, err := downmixNamed([]float32{0.01, 0.02, 0.03, 0.04, 0.05, 0.06}, defaultLayouts[6])
	if err != nil || math.Abs(float64(mono[0])-0.1062132) > 1e-6 {
		t.Errorf("5.1 downmix: %v, %v; want 0.1062132", mono, err)
	}
	for _, c := range []struct {
		name string
		data []byte
		want string
	}{
		{"5-bit IMA ADPCM", wavBytes(0x11, 1, 16000, 5, 0, 16), "5-bit IMA ADPCM"},
		{"6-channel Microsoft ADPCM", wavBytes(2, 6, 16000, 4, 0, 16), "Microsoft ADPCM of 6 channels"},
		{"MP3 in WAV", wavBytes(0x55, 1, 16000, 16, 0, 16), "WAVE format 85"},
		{"a 4 kHz rate", wavBytes(1, 1, 4000, 16, 0, 16), "4000 Hz"},
		{"nine channels, no mask", wavBytes(1, 9, 16000, 16, 0, 16), "9 channels"},
		{"BL without BR", wavBytes(1, 3, 16000, 16, 0x1|0x2|0x10, 16), "BL without BR"},
	} {
		_, err := DecodeAudio("audio/wav", c.data)
		if !errors.Is(err, ErrNotDecoded) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want ErrNotDecoded naming %q", c.name, err, c.want)
		}
	}
	// Invalid data stays invalid.
	if _, err := DecodeAudio("audio/wav", []byte("RIFF....WAVE")); err == nil || errors.Is(err, ErrNotDecoded) {
		t.Errorf("a WAV with no chunks: %v; want invalid data", err)
	}
}

// TestUndecodedAudioTrackIsNotInvalid: data that is not a video stays
// invalid, not ErrNotDecoded (#1651; an undecoded track codec is
// TestUndecodedTrackCodec).
func TestUndecodedAudioTrackIsNotInvalid(t *testing.T) {
	if _, err := DecodeVideoAudio("video/mp4", []byte("not a video"), VideoOptions{}); err == nil || errors.Is(err, ErrNotDecoded) {
		t.Errorf("not a video: %v; want invalid data", err)
	}
}
