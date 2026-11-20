package aac

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/cloudburrow/cloudburrow/internal/isoconformance"
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
	return heAccessUnits(t, name, data)
}

// heAccessUnits is heFixture on a file's bytes.
func heAccessUnits(t *testing.T, name string, data []byte) (asc []byte, aus [][]byte) {
	t.Helper()
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
// The goal of 1e-4 is met by six of the ten, and their RMS (1.8e-5) is the
// rounding of ffmpeg's 16-bit output. The larger differences sit in SBR
// bands whose source is near silent (energies 80 to 90 dB below the frame's
// peak) and whose gains are tens of dB (the limiter's G_max reaches 7866 in
// he-implicit-44k), where the output follows the core's last bits and any
// error in the QMF window is amplified. Taking the window's c[64k] from
// OpenCORE instead of interpolating them (see sbr_tables.go) cut these two
// to four times (he-lim1-44k from 2.84e-3 to 1.38e-3, he-implicit-44k from
// 1.58e-3 to 5.67e-4); the
// window's odd coefficients are still interpolated (about 1e-5 off), which
// is the one known remaining source.
func TestHEAACMatchesFFmpeg(t *testing.T) {
	for _, c := range []struct {
		file     string
		rate     int
		tol, rms float64
	}{
		{"he-mono-44k.m4a", 44100, 6e-5, 2.5e-5},     // measured 4.52e-5, 1.82e-5
		{"he-stereo-48k.m4a", 48000, 5e-5, 2.5e-5},   // measured 3.78e-5, 1.80e-5
		{"hev2-44k.m4a", 44100, 3e-4, 3e-5},          // measured 2.05e-4, 2.01e-5
		{"he-implicit-44k.aac", 44100, 8e-4, 6e-5},   // measured 5.67e-4, 4.03e-5
		{"hev2-implicit-48k.aac", 48000, 2e-4, 3e-5}, // measured 1.31e-4, 1.95e-5
		{"he-ds-22k.m4a", 22050, 6e-5, 2.5e-5},       // measured 4.23e-5, 1.81e-5
		{"he-lim0-44k.aac", 44100, 6e-4, 4.5e-5},     // measured 4.19e-4, 3.11e-5
		{"he-lim1-44k.aac", 44100, 2e-3, 1.3e-4},     // measured 1.38e-3, 9.16e-5
		{"he-lim3-44k.aac", 44100, 1e-3, 7e-5},       // measured 6.84e-4, 5.08e-5
		{"hev2-speech-44k.m4a", 44100, 6e-5, 2.5e-5}, // measured 4.43e-5, 1.78e-5
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

// TestPSFullConformance (#1754): the parametric stereo tools beyond
// baseline decode to the ISO/IEC 14496-26 (2nd edition) conformance
// references, the "_ur" (unrestricted PS decoder) WAVs, 24-bit:
//
//   - al_sbr_ps_03: fine IID (iid_mode 4) and mixing procedure B
//     (icc_mode 4), 20 bands;
//   - al_sbr_ps_04: IPD/OPD (ps_extension id 0), 20 bands;
//   - al_sbr_ps_06: 34 bands, first ICC (icc_mode 2, with 10 IID bands
//     mapped to 34), then IID (iid_mode 2, with 10 ICC bands), switching
//     between the 20- and 34-band configurations.
//
// The streams and references are ISO's files, never committed:
// internal/isoconformance fetches them at test time from
// https://standards.iso.org/iso-iec/14496/-26/ed-2/en/ (compressedMp4.zip
// and referencesWav.zip's add-opt directory) into the user's cache
// directory, pinned by SHA-256 and ISO's MD5, and the test skips when they
// are not cached and CLOUDBURROW_TEST_DOWNLOADS is unset (PROVENANCE.md).
// Each stream is compared over windows of its reference where the "_ur"
// and "_bl" (baseline) references differ, cut here from the WAV's data
// chunk: refs gives each window's first sample frame and length. The
// references are the reference decoder's float output rounded to 24 bits;
// the measured differences are a max of 3.6e-6 and an RMS of 5e-7 (102 dB
// SNR), the same as on the baseline streams al_sbr_ps_00 to _02 and _05.
// AudioToolbox's decoder, the one other decoder at hand, implements
// baseline PS only: its decodes match the "_bl" references (121 dB) and
// not these.
func TestPSFullConformance(t *testing.T) {
	for _, c := range []struct {
		stream string
		refs   [][2]int // first sample frame and length of each window
	}{
		{"al_sbr_ps_03", [][2]int{{126976, 32000}}},
		{"al_sbr_ps_04", [][2]int{{118784, 20000}}},
		{"al_sbr_ps_06", [][2]int{{229376, 16000}, {344064, 16000}}},
	} {
		asc, aus := heAccessUnits(t, c.stream+".mp4", isoconformance.File(t, c.stream+".mp4"))
		ref := wavData24(t, c.stream+"_ur.wav", isoconformance.File(t, c.stream+"_ur.wav"))
		d, err := NewDecoderASC(asc)
		if err != nil {
			t.Fatal(err)
		}
		if d.OutputChannels() != 2 || d.OutputSampleRate() != 32000 {
			t.Fatalf("%s: %d channels at %d Hz", c.stream, d.OutputChannels(), d.OutputSampleRate())
		}
		var out []float32
		for i, au := range aus {
			pcm, err := d.DecodeAU(au)
			if err != nil {
				t.Fatalf("%s: access unit %d: %v", c.stream, i, err)
			}
			out = append(out, pcm...)
		}
		// The core is one SCE in a program_config_element; with PS the
		// output is a stereo pair, which has no speaker names.
		if names := d.ChannelNames(); names != nil {
			t.Errorf("%s: channel names %q for stereo output", c.stream, names)
		}
		if len(out) != len(ref)/3 {
			t.Errorf("%s: %d frames decoded, the reference has %d", c.stream, len(out)/2, len(ref)/6)
		}
		for _, w := range c.refs {
			first, frames := w[0], w[1]
			if (first+frames)*6 > len(ref) || (first+frames)*2 > len(out) {
				t.Fatalf("%s: window %d+%d is past the reference (%d frames) or the decode (%d)",
					c.stream, first, frames, len(ref)/6, len(out)/2)
			}
			var maxDiff, se, sig float64
			n := 2 * frames
			b := ref[6*first:]
			for j := range n {
				r := float64(int32(uint32(b[3*j])<<8|uint32(b[3*j+1])<<16|uint32(b[3*j+2])<<24)>>8) / (1 << 23)
				diff := math.Abs(float64(out[2*first+j]) - r)
				maxDiff = math.Max(maxDiff, diff)
				se += diff * diff
				sig += r * r
			}
			rms := math.Sqrt(se / float64(n))
			name := fmt.Sprintf("%s from %d for %d frames", c.stream, first, frames)
			t.Logf("%s: max %.3g, RMS %.3g, SNR %.1f dB", name, maxDiff, rms, 10*math.Log10(sig/se))
			if maxDiff > 1e-5 || rms > 1e-6 {
				t.Errorf("%s: max %.3g, RMS %.3g, want at most 1e-5 and 1e-6", name, maxDiff, rms)
			}
		}
	}
}

// wavData24 returns the data chunk of a 24-bit stereo PCM WAV: raw
// little-endian samples, interleaved.
func wavData24(t *testing.T, name string, b []byte) []byte {
	t.Helper()
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatalf("%s: not a WAV file", name)
	}
	var format bool
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8 : min(p+8+size, len(b))]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				t.Fatalf("%s: short fmt chunk", name)
			}
			tag, ch := binary.LittleEndian.Uint16(body), binary.LittleEndian.Uint16(body[2:])
			rate, depth := binary.LittleEndian.Uint32(body[4:]), binary.LittleEndian.Uint16(body[14:])
			if (tag != 1 && tag != 0xfffe) || ch != 2 || rate != 32000 || depth != 24 {
				t.Fatalf("%s: format %#x, %d channels, %d Hz, %d bits; want 24-bit stereo PCM at 32 kHz", name, tag, ch, rate, depth)
			}
			format = true
		case "data":
			if !format {
				t.Fatalf("%s: data before fmt", name)
			}
			return body
		}
		p += 8 + size + size&1
	}
	t.Fatalf("%s: no data chunk", name)
	return nil
}

// TestPSFullSyntax: ps_data() with 34 stereo bands, mixing procedure B
// and IPD/OPD parses (they were refused with ErrUnsupported before
// #1754), and an IPD/OPD extension's data is read to its byte count.
func TestPSFullSyntax(t *testing.T) {
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
	// One envelope of 10 IID and 10 ICC bands, all zero deltas in
	// frequency ("0" for IID and ICC zero delta is the one-bit code 0).
	oneEnv := []field{{0, 1}, {1, 2}, {0, 1}}
	for range 10 {
		oneEnv = append(oneEnv, field{0, 1})
	}
	oneEnv = append(oneEnv, field{0, 1})
	for range 10 {
		oneEnv = append(oneEnv, field{0, 1})
	}
	// IPD/OPD for 5 bands, zero deltas ("1"), then reserved_ps: 1 + 2 *
	// (1 + 5) + 1 = 14 bits after the id, 16 with it: two bytes.
	ipd := []field{{2, 4}, {0, 2}, {1, 1}, {0, 1}}
	for range 5 {
		ipd = append(ipd, field{1, 1})
	}
	ipd = append(ipd, field{0, 1})
	for range 5 {
		ipd = append(ipd, field{1, 1})
	}
	ipd = append(ipd, field{0, 1})
	for _, c := range []struct {
		name   string
		fields []field
		ipdopd bool
	}{
		{"baseline", append(header(1, 1, 0), noEnv...), false},
		{"34 bands (iid_mode 2)", append(header(2, 1, 0), noEnv...), false},
		{"34 bands (icc_mode 5)", append(header(1, 5, 0), noEnv...), false},
		{"mixing B (icc_mode 3)", append(header(1, 3, 0), noEnv...), false},
		{"IPD/OPD", append(append(header(0, 0, 1), oneEnv...), ipd...), true},
		{"other extension", append(append(header(1, 1, 1), noEnv...), field{1, 4}, field{1, 2}, field{0, 6}), false},
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
		b := &bitr{r: bits.NewReader(w.Bytes())}
		if err := p.parse(b, n); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if b.pos() != n {
			t.Errorf("%s: read %d bits of %d", c.name, b.pos(), n)
		}
		if p.ipdopd != c.ipdopd {
			t.Errorf("%s: ipdopd %v, want %v", c.name, p.ipdopd, c.ipdopd)
		}
	}
	p := newPSDecoder()
	w := bits.NewBitWriter()
	_ = w.WriteBits(1, 1)
	_ = w.WriteBits(1, 1)
	_ = w.WriteBits(6, 3) // iid_mode 6: reserved
	_ = w.WriteBits(0, 1)
	_ = w.WriteBits(0, 1)
	_ = w.WriteBits(0, 3)
	w.Align()
	if err := p.parse(&bitr{r: bits.NewReader(w.Bytes())}, 9); !errors.Is(err, ErrUnsupported) {
		t.Errorf("iid_mode 6: %v, want ErrUnsupported", err)
	}
}
