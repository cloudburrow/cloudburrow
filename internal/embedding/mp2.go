package embedding

import (
	"errors"
	"fmt"
	"math"

	"github.com/cloudburrow/cloudburrow/third_party/go-mp3/polyphase"
)

// MPEG-1 and MPEG-2 (LSF) Audio Layer II in a WAV (WAVE_FORMAT_MPEG, 0x50;
// #1651), decoded as ISO/IEC 11172-3 and 13818-3 specify: the bit
// allocation (the four MPEG-1 tables, chosen by sample rate and bit rate a
// channel, and the LSF table), scale factor selection, grouped and
// ungrouped samples, dequantization ((2c + 1 - steps) / steps, the
// standard's C·(s'' + D)), intensity stereo above the bound, and the
// polyphase synthesis filterbank go-mp3's Layer III uses
// (third_party/go-mp3/polyphase). Layer I (mp1Frame) too: 4-bit
// allocations of 2 to 15 bits a sample, one scale factor a subband and 12
// samples of each of 32 subbands a frame, intensity stereo above the
// bound alike. Written for CloudBurrow from the standards' descriptions.
// ffmpeg 9 encodes neither Layer I nor joint stereo, so their tests decode
// frames of random parameters written to the standard's syntax
// (decoders_test.go), against ffmpeg's decode of the same frames.

const waveMPEG = 0x50

// mp2Alloc is one subband's allocation: the width of its allocation field
// and the number of quantization steps of each allocation value.
type mp2Alloc struct {
	nbal  uint
	steps []int
}

var (
	mp2A4 = []int{0, 3, 7, 15, 31, 63, 127, 255, 511, 1023, 2047, 4095, 8191, 16383, 32767, 65535}
	mp2B4 = []int{0, 3, 5, 7, 9, 15, 31, 63, 127, 255, 511, 1023, 2047, 4095, 8191, 65535}
	mp2B3 = []int{0, 3, 5, 7, 9, 15, 31, 65535}
	mp2B2 = []int{0, 3, 5, 65535}
	mp2C4 = []int{0, 3, 5, 9, 15, 31, 63, 127, 255, 511, 1023, 2047, 4095, 8191, 16383, 32767}
	mp2C3 = []int{0, 3, 5, 9, 15, 31, 63, 127}
	mp2L4 = []int{0, 3, 5, 7, 9, 15, 31, 63, 127, 255, 511, 1023, 2047, 4095, 8191, 16383}
	mp2L3 = []int{0, 3, 5, 9, 15, 31, 63, 127}
	mp2L2 = []int{0, 3, 5, 9}
)

// mp2Table is an allocation table of sblimit subbands.
func mp2Table(which byte) []mp2Alloc {
	var t []mp2Alloc
	add := func(n int, nbal uint, steps []int) {
		for range n {
			t = append(t, mp2Alloc{nbal, steps})
		}
	}
	switch which {
	case 'a', 'b': // 27 or 30 subbands
		add(3, 4, mp2A4)
		add(8, 4, mp2B4)
		add(12, 3, mp2B3)
		if which == 'a' {
			add(4, 2, mp2B2)
		} else {
			add(7, 2, mp2B2)
		}
	case 'c', 'd': // 8 or 12 subbands
		add(2, 4, mp2C4)
		if which == 'c' {
			add(6, 3, mp2C3)
		} else {
			add(10, 3, mp2C3)
		}
	default: // MPEG-2 LSF, 30 subbands
		add(4, 4, mp2L4)
		add(7, 3, mp2L3)
		add(19, 2, mp2L2)
	}
	return t
}

var (
	mp2Bitrates1 = [15]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384}
	mp2Bitrates2 = [15]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
	mp1Bitrates1 = [15]int{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}
	mp1Bitrates2 = [15]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256}
	mp2Rates1    = [3]int{44100, 48000, 32000}
	mp2Rates2    = [3]int{22050, 24000, 16000}
)

// mp2Bits reads a big-endian bit stream.
type mp2Bits struct {
	b   []byte
	pos int
}

var errMP2Short = errors.New("a frame ends early")

func (r *mp2Bits) get(n uint) (int, error) {
	if r.pos+int(n) > 8*len(r.b) {
		return 0, errMP2Short
	}
	v := 0
	for range n {
		v = v<<1 | int(r.b[r.pos>>3]>>(7-r.pos&7)&1)
		r.pos++
	}
	return v, nil
}

// decodeWAVMPEG decodes a WAV's MPEG audio data chunk of Layer II frames
// to interleaved samples, every frame as ffmpeg's wav demuxer passes them.
func decodeWAVMPEG(data []byte) (wav, error) {
	var (
		out      []float32
		channels int
		rate     int
		synth    [2]polyphase.Synth
		first    byte // the first frame's layer field
	)
	for p := 0; p+4 <= len(data); {
		h := data[p:]
		if h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
			p++ // not a frame: resynchronise, as a decoder does
			continue
		}
		version := h[1] >> 3 & 3 // 3 MPEG-1, 2 MPEG-2, 0 MPEG-2.5
		layer := h[1] >> 1 & 3   // 3 Layer I, 2 Layer II, 1 Layer III
		if layer != 2 && layer != 3 {
			return wav{}, fmt.Errorf("its MPEG data mixes Layer III with Layers I and II")
		}
		if first == 0 {
			first = layer
		} else if layer != first {
			return wav{}, fmt.Errorf("its MPEG data mixes layers")
		}
		if version == 0 || version == 1 {
			return wav{}, fmt.Errorf("its MPEG data is MPEG-2.5 or reserved, which Layers I and II are not")
		}
		lsf := version == 2
		crc := h[1]&1 == 0
		bi, fi := int(h[2]>>4), int(h[2]>>2&3)
		padding := int(h[2] >> 1 & 1)
		mode, modeExt := int(h[3]>>6), int(h[3]>>4&3)
		if bi == 0 || bi == 15 || fi == 3 {
			return wav{}, fmt.Errorf("its MPEG data has a free-format or invalid frame header")
		}
		br, fs := mp2Bitrates1[bi], mp2Rates1[fi]
		if lsf {
			br, fs = mp2Bitrates2[bi], mp2Rates2[fi]
		}
		size := 144*br*1000/fs + padding
		if layer == 3 {
			br = mp1Bitrates1[bi]
			if lsf {
				br = mp1Bitrates2[bi]
			}
			size = (12*br*1000/fs + padding) * 4
		}
		if p+size > len(data) {
			break // a cut frame at the end
		}
		nch := 2
		if mode == 3 {
			nch = 1
		}
		if channels == 0 {
			channels, rate = nch, fs
		} else if channels != nch || rate != fs {
			return wav{}, fmt.Errorf("its MPEG data changes channels or sample rate between frames")
		}
		bound := 32
		if mode == 1 {
			bound = 4 * (modeExt + 1) // intensity stereo above it
		}
		r := &mp2Bits{b: data[p : p+size], pos: 32}
		if crc {
			r.pos += 16
		}
		if layer == 3 {
			pcm, err := mp1Frame(r, nch, bound, &synth)
			if err != nil {
				return wav{}, fmt.Errorf("its MPEG data: %v", err)
			}
			out = append(out, pcm...)
			p += size
			continue
		}
		var table []mp2Alloc
		switch perCh := br / nch; {
		case lsf:
			table = mp2Table('l')
		case perCh <= 48 && fs == 32000:
			table = mp2Table('d')
		case perCh <= 48:
			table = mp2Table('c')
		case perCh <= 80 || fs == 48000:
			table = mp2Table('a')
		default:
			table = mp2Table('b')
		}
		pcm, err := mp2Frame(r, table, nch, min(bound, len(table)), &synth)
		if err != nil {
			return wav{}, fmt.Errorf("its MPEG data: %v", err)
		}
		out = append(out, pcm...)
		p += size
	}
	if channels == 0 || len(out) == 0 {
		return wav{}, errors.New("its MPEG data has no Layer I or II frames")
	}
	return wav{samples: out, channels: channels, rate: rate, layout: defaultLayouts[channels]}, nil
}

// mp2Scale is scale factor index i's value, 2^(1 - i/3).
func mp2Scale(i int) float64 { return math.Pow(2, 1-float64(i)/3) }

// mp2Frame decodes one Layer II frame's 1152 samples a channel,
// interleaved.
func mp2Frame(r *mp2Bits, table []mp2Alloc, nch, bound int, synth *[2]polyphase.Synth) ([]float32, error) {
	sblimit := len(table)
	var alloc [2][32]int
	for sb := 0; sb < sblimit; sb++ {
		for ch := 0; ch < nch; ch++ {
			if sb >= bound && ch == 1 {
				alloc[1][sb] = alloc[0][sb]
				continue
			}
			v, err := r.get(table[sb].nbal)
			if err != nil {
				return nil, err
			}
			alloc[ch][sb] = v
		}
	}
	var scfsi [2][32]int
	for sb := 0; sb < sblimit; sb++ {
		for ch := 0; ch < nch; ch++ {
			if alloc[ch][sb] != 0 {
				v, err := r.get(2)
				if err != nil {
					return nil, err
				}
				scfsi[ch][sb] = v
			}
		}
	}
	var scale [2][32][3]float64
	for sb := 0; sb < sblimit; sb++ {
		for ch := 0; ch < nch; ch++ {
			if alloc[ch][sb] == 0 {
				continue
			}
			var s [3]int
			var err error
			read := func() int {
				v, e := r.get(6)
				if e != nil {
					err = e
				}
				return v
			}
			switch scfsi[ch][sb] {
			case 0:
				s[0], s[1], s[2] = read(), read(), read()
			case 1:
				s[0] = read()
				s[1], s[2] = s[0], read()
			case 2:
				s[0] = read()
				s[1], s[2] = s[0], s[0]
			case 3:
				s[0] = read()
				s[1] = read()
				s[2] = s[1]
			}
			if err != nil {
				return nil, err
			}
			for i := range s {
				if s[i] == 63 {
					return nil, errors.New("a scale factor index of 63")
				}
				scale[ch][sb][i] = mp2Scale(s[i])
			}
		}
	}
	out := make([]float32, 1152*nch)
	var sub [2][32]float32
	var pcm [32]float32
	for gr := 0; gr < 12; gr++ {
		part := gr / 4
		var smp [2][32][3]float64
		for sb := 0; sb < sblimit; sb++ {
			for ch := 0; ch < nch; ch++ {
				if sb >= bound && ch == 1 {
					// Intensity stereo: the left channel's samples, each
					// channel its own scale factor.
					for i := range 3 {
						smp[1][sb][i] = smp[0][sb][i]
					}
					continue
				}
				a := alloc[ch][sb]
				if a == 0 {
					continue
				}
				steps := table[sb].steps[a]
				var codes [3]int
				switch steps {
				case 3, 5, 9:
					width := map[int]uint{3: 5, 5: 7, 9: 10}[steps]
					v, err := r.get(width)
					if err != nil {
						return nil, err
					}
					for i := range codes {
						codes[i] = v % steps
						v /= steps
					}
				default:
					nb := uint(math.Round(math.Log2(float64(steps + 1))))
					for i := range codes {
						v, err := r.get(nb)
						if err != nil {
							return nil, err
						}
						codes[i] = v
					}
				}
				for i, c := range codes {
					smp[ch][sb][i] = float64(2*c+1-steps) / float64(steps)
				}
			}
		}
		for i := range 3 {
			for ch := 0; ch < nch; ch++ {
				for sb := 0; sb < 32; sb++ {
					if sb < sblimit && alloc[ch][sb] != 0 {
						sub[ch][sb] = float32(smp[ch][sb][i] * scale[ch][sb][part])
					} else {
						sub[ch][sb] = 0
					}
				}
				synth[ch].Synthesize(&sub[ch], pcm[:])
				base := (gr*3 + i) * 32
				for k, v := range pcm {
					out[(base+k)*nch+ch] = v
				}
			}
		}
	}
	return out, nil
}

// mp1Frame decodes one Layer I frame's 384 samples a channel, interleaved:
// for each of 32 subbands a 4-bit allocation a channel (one shared above
// the intensity bound), allocation a coding a+1 bits a sample (15 is
// forbidden), then a 6-bit scale factor for each allocated subband, then
// 12 samples of each, dequantized as Layer II's are.
func mp1Frame(r *mp2Bits, nch, bound int, synth *[2]polyphase.Synth) ([]float32, error) {
	var alloc [2][32]int
	for sb := range 32 {
		for ch := 0; ch < nch; ch++ {
			if sb >= bound && ch == 1 {
				alloc[1][sb] = alloc[0][sb]
				continue
			}
			v, err := r.get(4)
			if err != nil {
				return nil, err
			}
			if v == 15 {
				return nil, errors.New("a Layer I allocation of 15")
			}
			alloc[ch][sb] = v
		}
	}
	var scale [2][32]float64
	for sb := range 32 {
		for ch := 0; ch < nch; ch++ {
			if alloc[ch][sb] == 0 {
				continue
			}
			v, err := r.get(6)
			if err != nil {
				return nil, err
			}
			if v == 63 {
				return nil, errors.New("a scale factor index of 63")
			}
			scale[ch][sb] = mp2Scale(v)
		}
	}
	out := make([]float32, 384*nch)
	var sub [2][32]float32
	var pcm [32]float32
	for s := range 12 {
		var smp [2][32]float64
		for sb := range 32 {
			for ch := 0; ch < nch; ch++ {
				if sb >= bound && ch == 1 {
					smp[1][sb] = smp[0][sb]
					continue
				}
				a := alloc[ch][sb]
				if a == 0 {
					continue
				}
				nb := uint(a + 1)
				c, err := r.get(nb)
				if err != nil {
					return nil, err
				}
				steps := 1<<nb - 1
				smp[ch][sb] = float64(2*c+1-steps) / float64(steps)
			}
		}
		for ch := 0; ch < nch; ch++ {
			for sb := range 32 {
				sub[ch][sb] = float32(smp[ch][sb] * scale[ch][sb])
			}
			synth[ch].Synthesize(&sub[ch], pcm[:])
			for k, v := range pcm {
				out[(s*32+k)*nch+ch] = v
			}
		}
	}
	return out, nil
}
