package aac

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/cloudburrow/cloudburrow/third_party/aac-go/adts"
	"github.com/cloudburrow/cloudburrow/third_party/aac-go/internal/bits"
)

// heFixture reads a test file's AudioSpecificConfig and access units: an
// MP4's sample entry and samples, or an ADTS stream's frames with the
// AAC-LC AudioSpecificConfig its first header implies.
func heFixture(t *testing.T, name string) (asc []byte, aus [][]byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(name) == ".aac" {
		for len(data) >= adts.HeaderSize {
			h, err := adts.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if asc == nil {
				aot := int(h.Profile) + 1
				v := aot<<11 | int(h.SamplingFrequencyIndex)<<7 | int(h.ChannelConfiguration)<<3
				asc = []byte{byte(v >> 8), byte(v)}
			}
			aus = append(aus, data[h.PayloadOffset():h.FrameLength])
			data = data[h.FrameLength:]
		}
		return asc, aus
	}
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	trak := f.Moov.Trak
	stbl := trak.Mdia.Minf.Stbl
	asc = stbl.Stsd.Mp4a.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig
	var sizes []uint32
	for i := range stbl.Stsz.SampleNumber {
		sizes = append(sizes, stbl.Stsz.GetSampleSize(int(i)+1))
	}
	for i, size := range sizes {
		chunk, first, err := stbl.Stsc.ChunkNrFromSampleNr(i + 1)
		if err != nil {
			t.Fatal(err)
		}
		var off uint64
		if stbl.Stco != nil {
			off = uint64(stbl.Stco.ChunkOffset[chunk-1])
		} else {
			off = stbl.Co64.ChunkOffset[chunk-1]
		}
		for j := first; j < i+1; j++ {
			off += uint64(sizes[j-1])
		}
		aus = append(aus, data[off:off+uint64(size)])
	}
	return asc, aus
}

// readS16 reads interleaved little-endian 16-bit samples as floats.
func readS16(t *testing.T, name string) []float64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]float64, len(b)/2)
	for i := range out {
		out[i] = float64(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32768
	}
	return out
}

// TestHEAACMatchesFFmpeg (#1739): HE-AAC (SBR) and HE-AAC v2 (PS) decode
// to ffmpeg 9's native decoder's samples (`ffmpeg -ignore_editlist 1 -c:a
// aac -i f.m4a -f s16le f.ffmpeg.s16`; `ffmpeg -c:a aac -i f.aac -f s16le
// f.ffmpeg.s16` for ADTS; every access unit's output, priming included).
// ffmpeg's decoder works in float32 and rounds to 16 bits for s16le, so one
// 16-bit step (3.05e-5) of any tolerance is that rounding. The fixtures, 0.6
// s of tones, sweeps and bursts above the SBR crossover, are AudioToolbox's
// HE-AAC (`aac_at`):
//
//	S='aevalsrc=0.25*sin(2*PI*440*t)+0.15*sin(2*PI*(300+8000*t)*t)+0.1*sin(2*PI*9000*t)*lt(mod(t\,0.3)\,0.15)|0.25*sin(2*PI*660*t)+0.12*sin(2*PI*(12000-8000*t)*t)+0.08*sin(2*PI*7000*t):s=RATE:d=0.6'
//	ffmpeg -f lavfi -i "$S" -ac 1 -c:a aac_at -profile:a 4 -b:a 32k -map_metadata -1 he-mono-44k.m4a      # RATE=44100
//	ffmpeg -f lavfi -i "$S" -ac 2 -c:a aac_at -profile:a 4 -b:a 48k -map_metadata -1 he-stereo-48k.m4a    # RATE=48000
//	ffmpeg -f lavfi -i "$S" -ac 2 -c:a aac_at -profile:a 28 -b:a 32k -map_metadata -1 hev2-44k.m4a        # RATE=44100
//	ffmpeg -f lavfi -i "$S" -ac 2 -c:a aac_at -profile:a 4 -b:a 40k he-implicit-44k.aac                   # RATE=44100
//	ffmpeg -f lavfi -i "$S" -ac 2 -c:a aac_at -profile:a 28 -b:a 32k hev2-implicit-48k.aac                # RATE=48000
//
// hev2-speech-44k.m4a is 1.5 s of real speech (the JFK clip in
// test/compat/testdata), whose PS frames include num_env 0 (keep the last
// parameters), which the tones never produce:
//
//	ffmpeg -i turtle-jfk.mp4 -vn -t 1.5 -af "aresample=44100,pan=stereo|c0=c0|c1=0.8*c0" -c:a aac_at -profile:a 28 -b:a 48k -map_metadata -1 hev2-speech-44k.m4a
//
// Derived fixtures, decoded by ffmpeg the same way:
//
//   - he-ds-22k.m4a is he-mono-44k.m4a with its AudioSpecificConfig's SBR
//     extension sampling frequency index set from 4 (44.1 kHz) to 7 (22.05
//     kHz, the core's rate), which asks for downsampled SBR: ffmpeg then
//     decodes it at 22.05 kHz.
//   - he-lim0-44k.aac, he-lim1-44k.aac and he-lim3-44k.aac are
//     he-implicit-44k.aac with its one SBR header given bs_header_extra2 = 1
//     and (bs_limiter_bands, bs_limiter_gains, bs_interpol_freq,
//     bs_smoothing_mode) = (0, 0, 0, 0), (1, 3, 1, 0) and (3, 1, 0, 1): the
//     six bits are inserted after the flag, the SBR payload padded by two
//     bits, and the fill element's count and the ADTS frame_length grown by
//     one byte. aac_at only ever sends the defaults (2, 2, 1, 1), so these
//     cover the gain smoothing, per-band energy estimation off and the
//     other limiter tables and gains.
//
// The MP4s signal SBR (and PS) in their AudioSpecificConfig's backward
// compatible extension; the ADTS streams' headers say AAC-LC at the core
// rate, so SBR and PS are found in the stream (implicit signalling).
//
// The tolerances are the measured differences (max, RMS) with a margin.
// The goal of 1e-4 is met only by he-stereo-48k. The larger differences sit
// in SBR bands whose source is near silent (energies 80 to 90 dB below the
// frame's peak) and whose gains are tens of dB (the limiter's G_max reaches
// 7866 in he-implicit-44k): there the output follows the core's last bits.
// Adding noise of 1e-3 of a 16-bit step (3e-8 of full scale, below
// ffmpeg's float32 precision) to our core output moves he-lim1-44k's max
// difference from 2.8e-3 to 3.7e-3, so differences of this size are
// ffmpeg's float32 arithmetic, amplified, rather than a different
// algorithm; every alternative reading of the gain, limiter, sinusoid,
// noise and chirp rules tried made the match worse.
func TestHEAACMatchesFFmpeg(t *testing.T) {
	for _, c := range []struct {
		file     string
		rate     int
		tol, rms float64
	}{
		{"he-mono-44k.m4a", 44100, 2.5e-4, 3.5e-5},   // measured 1.77e-4, 2.72e-5
		{"he-stereo-48k.m4a", 48000, 1e-4, 2.5e-5},   // measured 6.62e-5, 1.81e-5
		{"hev2-44k.m4a", 44100, 1.2e-3, 1.1e-4},      // measured 8.83e-4, 8.15e-5
		{"he-implicit-44k.aac", 44100, 2e-3, 2e-4},   // measured 1.58e-3, 1.59e-4
		{"hev2-implicit-48k.aac", 48000, 2e-4, 3e-5}, // measured 1.30e-4, 2.01e-5
		{"he-ds-22k.m4a", 22050, 2.5e-4, 3.5e-5},     // measured 1.70e-4, 2.72e-5
		{"he-lim0-44k.aac", 44100, 1e-3, 8e-5},       // measured 7.84e-4, 5.93e-5
		{"he-lim1-44k.aac", 44100, 3.5e-3, 4e-4},     // measured 2.84e-3, 3.05e-4
		{"he-lim3-44k.aac", 44100, 3.5e-3, 4e-4},     // measured 2.85e-3, 2.99e-4
		{"hev2-speech-44k.m4a", 44100, 1e-4, 2.5e-5}, // measured 7.61e-5, 1.80e-5
	} {
		asc, aus := heFixture(t, c.file)
		d, err := NewDecoderASC(asc)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		var got []float32
		for i, au := range aus {
			p, err := d.DecodeAU(au)
			if err != nil {
				t.Fatalf("%s: access unit %d: %v", c.file, i, err)
			}
			got = append(got, p...)
		}
		base := c.file[:len(c.file)-len(filepath.Ext(c.file))]
		want := readS16(t, base+".ffmpeg.s16")
		if d.OutputSampleRate() != c.rate || d.OutputChannels() != 2 || len(got) != len(want) {
			t.Fatalf("%s: %d samples, %d channels at %d Hz; ffmpeg %d, 2 at %d", c.file, len(got),
				d.OutputChannels(), d.OutputSampleRate(), len(want), c.rate)
		}
		var maxDiff, sum float64
		at := 0
		for i := range got {
			v := math.Max(-1, math.Min(32767.0/32768, float64(got[i])))
			diff := math.Abs(v - want[i])
			sum += diff * diff
			if diff > maxDiff {
				maxDiff, at = diff, i
			}
		}
		rms := math.Sqrt(sum / float64(len(got)))
		t.Logf("%s: max %.3g (sample %d), rms %.3g", c.file, maxDiff, at, rms)
		if maxDiff > c.tol || rms > c.rms {
			t.Errorf("%s: max difference %.3g at sample %d, rms %.3g; want at most %.3g and %.3g",
				c.file, maxDiff, at, rms, c.tol, c.rms)
		}
	}
}

// TestHEAACImplicitADTS: Decode, the ADTS path, finds SBR and PS in an
// AAC-LC stream as DecodeAU does, and reports the doubled rate and two
// channels.
func TestHEAACImplicitADTS(t *testing.T) {
	for _, file := range []string{"he-implicit-44k.aac", "hev2-implicit-48k.aac"} {
		data, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		d, _ := NewDecoder()
		pcm, err := d.Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		asc, aus := heFixture(t, file)
		ref, err := NewDecoderASC(asc)
		if err != nil {
			t.Fatal(err)
		}
		var want []float32
		for _, au := range aus {
			p, err := ref.DecodeAU(au)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, p...)
		}
		if d.SampleRate() != ref.OutputSampleRate() || d.Channels() != 2 || len(pcm) != len(want) {
			t.Fatalf("%s: Decode gave %d samples, %d channels at %d Hz; DecodeAU %d at %d Hz",
				file, len(pcm), d.Channels(), d.SampleRate(), len(want), ref.OutputSampleRate())
		}
		for i, v := range want {
			if math.Abs(float64(pcm[i])-math.Max(-32768, math.Min(32767, float64(v)*32768))) > 0.501 {
				t.Fatalf("%s: sample %d: Decode %d, DecodeAU %g", file, i, pcm[i], float64(v)*32768)
			}
		}
	}
}

// TestDecodeAUMatchesDecodeLC: plain AAC-LC through NewDecoderASC and
// DecodeAU gives Decode's samples (as floats, 1.0 being full scale).
func TestDecodeAUMatchesDecodeLC(t *testing.T) {
	for _, channels := range []int{1, 2} {
		enc, err := NewEncoder(Config{SampleRate: 44100, Channels: channels, Quality: 0.5})
		if err != nil {
			t.Fatal(err)
		}
		in := make([]int16, 44100/4*channels)
		for i := range in {
			n := float64(i / channels)
			in[i] = int16(8000*math.Sin(2*math.Pi*440*n/44100) + 3000*math.Sin(2*math.Pi*5000*n/44100))
		}
		stream, err := enc.Encode(in)
		if err != nil {
			t.Fatal(err)
		}
		tail, err := enc.Flush()
		if err != nil {
			t.Fatal(err)
		}
		stream = append(stream, tail...)

		d, _ := NewDecoder()
		pcm, err := d.Decode(stream)
		if err != nil {
			t.Fatal(err)
		}
		h, err := adts.Parse(stream)
		if err != nil {
			t.Fatal(err)
		}
		v := 2<<11 | int(h.SamplingFrequencyIndex)<<7 | int(h.ChannelConfiguration)<<3
		au, err := NewDecoderASC([]byte{byte(v >> 8), byte(v)})
		if err != nil {
			t.Fatal(err)
		}
		var got []float32
		for data := stream; len(data) > 0; {
			h, err := adts.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			p, err := au.DecodeAU(data[h.PayloadOffset():h.FrameLength])
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, p...)
			data = data[h.FrameLength:]
		}
		if au.OutputChannels() != channels || au.OutputSampleRate() != 44100 || len(got) != len(pcm) {
			t.Fatalf("%d channels: DecodeAU gave %d samples, %d channels at %d Hz; Decode %d",
				channels, len(got), au.OutputChannels(), au.OutputSampleRate(), len(pcm))
		}
		for i, v := range got {
			// Within rounding: DecodeAU's float32 samples carry 24 bits.
			if math.Abs(float64(pcm[i])-math.Max(-32768, math.Min(32767, float64(v)*32768))) > 0.501 {
				t.Fatalf("%d channels: sample %d: Decode %d, DecodeAU %g", channels, i, pcm[i], float64(v)*32768)
			}
		}
	}
}

// TestPSRefusals: ps_data() asking for 34 stereo bands, mixing procedure
// B or IPD/OPD is refused with ErrUnsupported, frame by frame, so that a
// stream switching to one mid-way fails rather than decoding wrongly;
// baseline ps_data() is accepted. (aac_at, the only HE-AAC v2 encoder
// available, emits iid_mode and icc_mode 1 without extensions.)
func TestPSRefusals(t *testing.T) {
	type field struct {
		v uint64
		n int
	}
	header := func(iid, icc, ext uint64) []field {
		return []field{{1, 1}, {1, 1}, {iid, 3}, {1, 1}, {icc, 3}, {ext, 1}}
	}
	// frame_class 0 and num_env index 0: no envelopes, so no parameters
	// follow.
	noEnv := []field{{0, 1}, {0, 2}}
	for _, c := range []struct {
		name   string
		fields []field
		want   string // "" when accepted
	}{
		{"baseline", append(header(1, 1, 0), noEnv...), ""},
		{"34 bands (iid_mode 2)", append(header(2, 1, 0), noEnv...), "34 stereo bands"},
		{"34 bands (icc_mode 5)", append(header(1, 5, 0), noEnv...), "34 stereo bands"},
		{"mixing B (icc_mode 3)", append(header(1, 3, 0), noEnv...), "mixing procedure B"},
		{"IPD/OPD", append(append(header(1, 1, 1), noEnv...), field{1, 4}, field{0, 2}, field{1, 1}, field{0, 5}), "IPD/OPD"},
		{"other extension", append(append(header(1, 1, 1), noEnv...), field{1, 4}, field{1, 2}, field{0, 6}), ""},
	} {
		w := bits.NewBitWriter()
		n := 0
		for _, f := range c.fields {
			if err := w.WriteBits(f.v, f.n); err != nil {
				t.Fatal(err)
			}
			n += f.n
		}
		w.Align()
		p := newPSDecoder()
		err := p.parse(&bitr{r: bits.NewReader(w.Bytes())}, n)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (!errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want ErrUnsupported naming %q", c.name, err, c.want)
		}
	}
}
