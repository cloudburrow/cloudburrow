package ac3

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// syncframes splits a raw AC-3 or E-AC-3 stream at its syncframes.
func syncframes(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		h, _, err := parseHeader(b)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b[:h.frameBytes])
		b = b[h.frameBytes:]
	}
	return out
}

// decodeAll decodes a raw stream a syncframe at a time.
func decodeAll(t *testing.T, d *Decoder, frames [][]byte) (pcm []float32, channels, rate int) {
	t.Helper()
	for i, f := range frames {
		p, c, r, err := d.Decode(f)
		if err != nil {
			t.Fatalf("syncframe %d: %v", i, err)
		}
		pcm, channels, rate = append(pcm, p...), c, r
	}
	return pcm, channels, rate
}

// TestDecodeMatchesFFmpeg: each testdata stream decodes to ffmpeg 9's float
// decode of it (testdata/*.f32, `ffmpeg -i f -f f32le f.f32`), sample for
// sample, within the stated tolerance (full scale 1.0).
//
// The difference left is dither: A/52 (clause 6.3.4) has a decoder fill
// mantissas allocated no bits with noise of its own choosing, and ffmpeg's
// encoders set dithflag, so two conforming decoders differ there by noise
// at the level of those coefficients. At high bit rates a channel (few such
// mantissas, and small) the decodes agree to under 2e-5 (ffmpeg decodes in
// 32-bit float): the tolerance is 3e-5. At 96 kbit/s stereo, and from 3
// channels at 384 to 640 kbit/s (75 to 128 kbit/s a channel), the two
// decoders' dither is what remains (up to 0.01 at 96 kbit/s, where most
// coupled coefficients get no bits): with this decoder's dither turned off,
// the RMS difference falls by the square root of 2, so the two dithers have
// the same power and nothing else differs measurably, which the test
// checks.
//
// The streams (ffmpeg 9's ac3 and eac3 encoders; S2 and S6 are a stereo
// and a 5.1 test signal with 10 ms bursts every 100 ms):
//
//	S2="aevalsrc=0.3*sin(2*PI*440*t)+0.25*(lt(mod(t\,0.1)\,0.01))*sin(2*PI*3000*t)|0.3*sin(2*PI*660*t)+0.1*sin(2*PI*5000*t):s=48000:d=0.25"
//	S6="aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*550*t)+0.2*(lt(mod(t\,0.1)\,0.01))|0.3*sin(2*PI*330*t)|0.3*sin(2*PI*60*t)|0.2*sin(2*PI*880*t)*sin(2*PI*2*t)|0.2*sin(2*PI*1760*t):s=48000:d=0.15:c=5.1"
//	ffmpeg -f lavfi -i "$S2" -ac 1 -c:a ac3 -b:a 448k mono-448k.ac3
//	ffmpeg -f lavfi -i "$S2" -c:a ac3 -b:a 640k -channel_coupling 1 -cpl_start_band 1 stereo-cpl-640k.ac3
//	ffmpeg -f lavfi -i "$S2" -c:a ac3 -b:a 96k stereo-96k.ac3
//	ffmpeg -f lavfi -i "$S6" -c:a ac3 -b:a 640k -channel_coupling 1 51-640k.ac3
//	ffmpeg -f lavfi -i "aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*660*t)|0.2*sin(2*PI*1000*t):s=44100:d=0.2:c=3.0(back)" -c:a ac3 -b:a 384k 21-44k.ac3
//	ffmpeg -f lavfi -i "aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*660*t)|0.2*sin(2*PI*1000*t)|0.2*sin(2*PI*300*t):s=32000:d=0.2:c=4.0" -c:a ac3 -b:a 384k 31-32k.ac3
//	ffmpeg -f lavfi -i "aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*660*t)|0.2*sin(2*PI*1000*t)|0.2*sin(2*PI*300*t):s=48000:d=0.2:c=quad(side)" -c:a ac3 -b:a 448k 22-48k.ac3
//	ffmpeg -f lavfi -i "$S2" -c:a eac3 -b:a 640k stereo-640k.eac3
//	ffmpeg -f lavfi -i "$S2" -c:a eac3 -b:a 96k stereo-96k.eac3
//	ffmpeg -f lavfi -i "$S6" -c:a eac3 -b:a 640k 51-640k.eac3
//
// Between them they use every audio coding mode but 1+1, the LFE, the
// three sample rates (44.1 kHz's odd frame sizes), coupling, rematrixing,
// dither, and E-AC-3's audio frame with frame exponent strategies. ffmpeg's
// encoders never switch blocks, nor use delta bit allocation, AHT, spectral
// extension, enhanced coupling or dependent substreams; TestIMDCTReconstructs
// checks the block-switched transform and TestDependentSubstream the
// substream assembly.
func TestDecodeMatchesFFmpeg(t *testing.T) {
	for _, c := range []struct {
		file         string
		layout       string
		rate         int
		maxDiff, rms float64 // the tolerance
		dither       bool    // whether dither is what differs
		uses         []string
	}{
		{"mono-448k.ac3", "FC", 48000, 3e-5, 2e-6, false, nil},
		{"stereo-cpl-640k.ac3", "FL FR", 48000, 3e-5, 2e-6, false, []string{"coupled channel", "rematrix band"}},
		{"31-32k.ac3", "FL FR FC BC", 32000, 3e-5, 3e-6, false, nil},
		{"stereo-640k.eac3", "FL FR", 48000, 3e-5, 2e-6, false, []string{"frame exponent strategy", "rematrix band"}},
		{"stereo-96k.ac3", "FL FR", 48000, 0.015, 2e-3, true, []string{"coupled channel", "rematrix band"}},
		{"51-640k.ac3", "FL FR FC LFE SL SR", 48000, 1e-3, 4e-5, true, []string{"coupled channel", "lfe"}},
		{"21-44k.ac3", "FL FR BC", 44100, 1e-3, 4e-5, true, nil},
		{"22-48k.ac3", "FL FR SL SR", 48000, 1e-3, 4e-5, true, nil},
		{"stereo-96k.eac3", "FL FR", 48000, 0.015, 2e-3, true, []string{"coupled channel"}},
		{"51-640k.eac3", "FL FR FC LFE SL SR", 48000, 2e-3, 6e-5, true, []string{"lfe"}},
	} {
		tally = map[string]int{}
		maxDiff, rms := compareFFmpeg(t, c.file, c.layout, c.rate)
		uses := tally
		tally = nil
		t.Logf("%s: max difference %.3g, RMS %.3g", c.file, maxDiff, rms)
		if maxDiff > c.maxDiff || rms > c.rms {
			t.Errorf("%s: differs from ffmpeg by up to %.3g (RMS %.3g), more than %.3g (%.3g)", c.file, maxDiff, rms, c.maxDiff, c.rms)
		}
		for _, u := range c.uses {
			if uses[u] == 0 {
				t.Errorf("%s does not use %s", c.file, u)
			}
		}
		if c.dither {
			// Without this decoder's dither, ffmpeg's alone is left:
			// the RMS difference falls by about the square root of 2.
			ditherLevel = 0
			_, rms0 := compareFFmpeg(t, c.file, c.layout, c.rate)
			ditherLevel = 0.707106781
			if ratio := rms / rms0; ratio < 1.2 || ratio > 1.7 {
				t.Errorf("%s: the RMS difference is %.3g with dither, %.3g without, a ratio of %.2f, not about 1.41", c.file, rms, rms0, ratio)
			}
		}
	}
}

// compareFFmpeg decodes a testdata stream and compares it with ffmpeg's
// decode.
func compareFFmpeg(t *testing.T, file, layout string, rate int) (maxDiff, rms float64) {
	t.Helper()
	d := NewDecoder()
	got, channels, r := decodeAll(t, d, syncframes(t, readTestdata(t, file)))
	ref := readTestdata(t, file+".f32")
	if l := strings.Join(d.Layout(), " "); l != layout || channels != len(d.Layout()) || r != rate || len(got) != len(ref)/4 {
		t.Fatalf("%s: %d samples of %s at %d Hz, ffmpeg's %d of %s at %d", file, len(got), l, r, len(ref)/4, layout, rate)
	}
	var sum float64
	for i := range got {
		d := math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(ref[4*i:]))) - float64(got[i]))
		maxDiff = max(maxDiff, d)
		sum += d * d
	}
	return maxDiff, math.Sqrt(sum / float64(len(got)))
}

// TestMixLevels: the downmix levels are the ones ffmpeg's decoder exports
// (and its `-ac 1` downmix uses, measured by least squares against its
// 6-channel decode: centre cmix*sqrt2, surround surmix/sqrt2): AC-3's
// cmixlev and surmixlev, the Lo/Ro levels of AC-3's Annex D extended bsi or
// E-AC-3's mixing metadata when present, else -4.5 and -6 dB. Each fixture
// is the first syncframe of a 5.1 stream encoded by
//
//	ffmpeg -f lavfi -i "$S6" -c:a eac3 -b:a 640k -ltrt_cmixlev 1.0 -loro_cmixlev 0.5 -ltrt_surmixlev 0.841 -loro_surmixlev 0.707 -dmix_mode 2 mix1.eac3
//	ffmpeg -f lavfi -i "$S6" -c:a ac3 -b:a 640k -center_mixlev 0.707 -surround_mixlev 0.707 mix3.ac3
//	ffmpeg -f lavfi -i "$S6" -c:a ac3 -b:a 640k -ltrt_cmixlev 1.0 -loro_cmixlev 0.5 -ltrt_surmixlev 0.841 -loro_surmixlev 0.707 -dmix_mode 2 -center_mixlev 0.707 mix4.ac3
func TestMixLevels(t *testing.T) {
	for _, c := range []struct {
		file             string
		center, surround float64
	}{
		{"frame-mix1.eac3", 0.5, math.Sqrt2 / 2},
		{"frame-mix3.ac3", math.Sqrt2 / 2, math.Sqrt2 / 2},
		{"frame-mix4.ac3", 0.5, math.Sqrt2 / 2},
		{"51-640k.ac3", math.Pow(2, -0.75), 0.5},
		{"51-640k.eac3", math.Pow(2, -0.75), 0.5},
	} {
		d := NewDecoder()
		if _, _, _, err := d.Decode(syncframes(t, readTestdata(t, c.file))[0]); err != nil {
			t.Fatal(err)
		}
		if cm, s := d.MixLevels(); math.Abs(cm-c.center) > 1e-9 || math.Abs(s-c.surround) > 1e-9 {
			t.Errorf("%s: mix levels %.4f, %.4f, want %.4f, %.4f", c.file, cm, s, c.center, c.surround)
		}
	}
}

// TestDependentSubstream: an E-AC-3 access unit of an independent 5.1
// substream and a dependent stereo substream whose channel map (E.1.3.1.8)
// names the Lrs/Rrs pair decodes to 7.1, as ffmpeg lays it out: the
// independent substream's channels, and the dependent one's as BL and BR,
// each exactly as decoding that substream alone gives. ffmpeg's encoder
// writes no dependent substreams, so the dependent one is a stereo
// stream's syncframe that Decode is told (testAsDependent) to take as a
// dependent substream with that channel map.
func TestDependentSubstream(t *testing.T) {
	five := syncframes(t, readTestdata(t, "51-640k.eac3"))
	two := syncframes(t, readTestdata(t, "stereo-640k.eac3"))
	n := min(len(five), len(two))
	alone5, _, _ := decodeAll(t, NewDecoder(), five[:n])
	alone2, _, _ := decodeAll(t, NewDecoder(), two[:n])
	testAsDependent = func(h *header) bool {
		if h.acmod != 2 {
			return false
		}
		h.chanmap = 1 << (15 - 6) // Lrs/Rrs
		return true
	}
	defer func() { testAsDependent = nil }()
	d := NewDecoder()
	var got []float32
	for i := range n {
		p, c, _, err := d.Decode(append(append([]byte(nil), five[i]...), two[i]...))
		if err != nil {
			t.Fatal(err)
		}
		if c != 8 {
			t.Fatalf("%d channels", c)
		}
		got = append(got, p...)
	}
	if want := []string{"FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"}; !slices.Equal(d.Layout(), want) {
		t.Fatalf("layout %v, want %v", d.Layout(), want)
	}
	frames := len(got) / 8
	if frames != len(alone5)/6 || frames != len(alone2)/2 {
		t.Fatalf("%d samples, alone %d and %d", frames, len(alone5)/6, len(alone2)/2)
	}
	from5 := []int{0, 1, 2, 3, -1, -1, 4, 5}
	for i := range frames {
		for c, k := range from5 {
			want := float32(0)
			if k >= 0 {
				want = alone5[i*6+k]
			} else {
				want = alone2[i*2+c-4]
			}
			if got[i*8+c] != want {
				t.Fatalf("sample %d channel %s: %g, alone %g", i, d.Layout()[c], got[i*8+c], want)
			}
		}
	}
}
