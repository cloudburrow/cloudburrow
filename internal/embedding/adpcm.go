package embedding

import (
	"encoding/binary"
	"fmt"
)

// ADPCM WAV (#1651): IMA ADPCM (WAVE format 0x11, the DVI/Intel layout),
// 4 or 3 bits a sample, and Microsoft ADPCM (WAVE format 2), 4 bits a
// sample, mono or stereo, decoded as ffmpeg's
// adpcm_ima_wav and adpcm_ms decoders decode them (checked sample for
// sample against ffmpeg 9's decodes in adpcm_test.go). Each block of
// block_align bytes starts afresh from its header.

const (
	waveMSADPCM  = 2
	waveIMAADPCM = 0x11
)

// imaIndexTable and imaStepTable are the IMA ADPCM step-size adaptation
// tables (IMA's Digital Audio Recommended Practices, 1992).
var imaIndexTable = [16]int{-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8}

var imaStepTable = [89]int{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230,
	253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963,
	1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327,
	3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442,
	11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794,
	32767,
}

func clip16(v int) int { return max(-32768, min(32767, v)) }

type imaState struct{ predictor, index int }

// nibble expands one 4-bit code as the IMA reference's shift-and-add does
// (ffmpeg's adpcm_ima_qt_expand_nibble, which its adpcm_ima_wav decoder
// uses for 4-bit samples).
func (s *imaState) nibble(n int) int {
	step := imaStepTable[s.index]
	s.index = max(0, min(88, s.index+imaIndexTable[n]))
	diff := step >> 3
	if n&4 != 0 {
		diff += step
	}
	if n&2 != 0 {
		diff += step >> 1
	}
	if n&1 != 0 {
		diff += step >> 2
	}
	if n&8 != 0 {
		s.predictor -= diff
	} else {
		s.predictor += diff
	}
	s.predictor = clip16(s.predictor)
	return s.predictor
}

// ima3Index is the 3-bit step index adjustment (IMA's Recommended
// Practices), by the code's two magnitude bits.
var ima3Index = [4]int{-1, -1, 1, 2}

// nibble3 expands one 3-bit code (a sign bit and two magnitude bits) with
// the same shift-and-add (checked against ffmpeg 9's decodes).
func (s *imaState) nibble3(n int) int {
	step := imaStepTable[s.index]
	s.index = max(0, min(88, s.index+ima3Index[n&3]))
	diff := step >> 2
	if n&2 != 0 {
		diff += step
	}
	if n&1 != 0 {
		diff += step >> 1
	}
	if n&4 != 0 {
		s.predictor -= diff
	} else {
		s.predictor += diff
	}
	s.predictor = clip16(s.predictor)
	return s.predictor
}

// decodeIMAADPCM decodes IMA ADPCM blocks to interleaved 16-bit samples.
// Each block has a 4-byte header a channel (the first sample and the step
// index), then 4 bytes of each channel in turn, 8 samples, low nibble
// first; or (#1651), for 3-bit IMA ADPCM, 32 samples of each channel in 12
// bytes, a little-endian bit stream, its three 4-byte words interleaved
// with the other channels' as the 4-bit words are.
func decodeIMAADPCM(data []byte, channels, blockAlign, bits int) ([]int16, error) {
	if bits == 3 {
		return decodeIMA3(data, channels, blockAlign)
	}
	if bits != 4 {
		return nil, notDecodedf("%d-bit IMA ADPCM is not decoded; 3- and 4-bit IMA ADPCM are", bits)
	}
	if blockAlign < 4*channels || blockAlign > 1<<16 {
		return nil, fmt.Errorf("an IMA ADPCM block_align of %d for %d channels", blockAlign, channels)
	}
	var out []int16
	st := make([]imaState, channels)
	for len(data) >= 4*channels {
		blk := data[:min(len(data), blockAlign)]
		data = data[len(blk):]
		frames := 1 + (len(blk)-4*channels)/(4*channels)*8
		base := len(out)
		out = append(out, make([]int16, frames*channels)...)
		for c := range channels {
			h := blk[4*c:]
			st[c].predictor = int(int16(binary.LittleEndian.Uint16(h)))
			st[c].index = int(int16(binary.LittleEndian.Uint16(h[2:])))
			if st[c].index < 0 || st[c].index > 88 {
				return nil, fmt.Errorf("an IMA ADPCM step index of %d", st[c].index)
			}
			out[base+c] = int16(st[c].predictor)
		}
		p := 4 * channels
		for n := 0; n < (frames-1)/8; n++ {
			for c := range channels {
				for m := 0; m < 8; m += 2 {
					v := int(blk[p])
					p++
					f := 1 + n*8 + m
					out[base+f*channels+c] = int16(st[c].nibble(v & 15))
					out[base+(f+1)*channels+c] = int16(st[c].nibble(v >> 4))
				}
			}
		}
	}
	return out, nil
}

// decodeIMA3 decodes 3-bit IMA ADPCM blocks (decodeIMAADPCM).
func decodeIMA3(data []byte, channels, blockAlign int) ([]int16, error) {
	if blockAlign < 4*channels || blockAlign > 1<<16 {
		return nil, fmt.Errorf("an IMA ADPCM block_align of %d for %d channels", blockAlign, channels)
	}
	var out []int16
	st := make([]imaState, channels)
	for len(data) >= 4*channels {
		blk := data[:min(len(data), blockAlign)]
		data = data[len(blk):]
		groups := (len(blk) - 4*channels) / (12 * channels)
		frames := 1 + groups*32
		base := len(out)
		out = append(out, make([]int16, frames*channels)...)
		for c := range channels {
			h := blk[4*c:]
			st[c].predictor = int(int16(binary.LittleEndian.Uint16(h)))
			st[c].index = int(int16(binary.LittleEndian.Uint16(h[2:])))
			if st[c].index < 0 || st[c].index > 88 {
				return nil, fmt.Errorf("an IMA ADPCM step index of %d", st[c].index)
			}
			out[base+c] = int16(st[c].predictor)
		}
		for g := range groups {
			p := 4*channels + g*12*channels
			for c := range channels {
				var v uint128le
				for w := range 3 {
					o := p + (w*channels+c)*4
					v.set(w, binary.LittleEndian.Uint32(blk[o:]))
				}
				for k := range 32 {
					f := 1 + g*32 + k
					out[base+f*channels+c] = int16(st[c].nibble3(v.bits(3*k, 3)))
				}
			}
		}
	}
	return out, nil
}

// uint128le is three little-endian 32-bit words read as one bit stream.
type uint128le struct{ lo, hi uint64 }

func (v *uint128le) set(w int, x uint32) {
	switch w {
	case 0:
		v.lo |= uint64(x)
	case 1:
		v.lo |= uint64(x) << 32
	default:
		v.hi = uint64(x)
	}
}

func (v *uint128le) bits(at, n int) int {
	var x uint64
	switch {
	case at >= 64:
		x = v.hi >> (at - 64)
	case at+n <= 64:
		x = v.lo >> at
	default:
		x = v.lo>>at | v.hi<<(64-at)
	}
	return int(x & (1<<n - 1))
}

// msAdaptation and the coefficient pairs are Microsoft ADPCM's standard
// tables; ffmpeg decodes with these whatever coefficients the fmt chunk
// lists.
var (
	msAdaptation = [16]int{230, 230, 230, 230, 307, 409, 512, 614, 768, 614, 512, 409, 307, 230, 230, 230}
	msCoeff1     = [7]int{256, 512, 0, 192, 240, 460, 392}
	msCoeff2     = [7]int{0, -256, 0, 64, 0, -208, -232}
)

type msState struct{ c1, c2, delta, s1, s2 int }

// nibble expands one code as ffmpeg's adpcm_ms_expand_nibble does (its
// coefficient tables are these over 4, divided by 64).
func (s *msState) nibble(n int) int {
	p := (s.s1*s.c1 + s.s2*s.c2) / 256
	p += int(int32(n)<<28>>28) * s.delta
	s.s2, s.s1 = s.s1, clip16(p)
	s.delta = max(16, msAdaptation[n]*s.delta>>8)
	s.delta = min(s.delta, (1<<31-1)/768)
	return s.s1
}

// decodeMSADPCM decodes Microsoft ADPCM blocks of one or two channels to
// interleaved 16-bit samples. Each block has each channel's predictor
// index, then its delta, its second and its first sample (16 bits each,
// channels interleaved); the samples follow two to a byte, high nibble
// first (the left channel's, in stereo).
func decodeMSADPCM(data []byte, channels, blockAlign, bits int) ([]int16, error) {
	if bits != 4 {
		return nil, notDecodedf("%d-bit Microsoft ADPCM is not decoded; 4-bit is", bits)
	}
	// More channels have no layout to verify against: Microsoft's codec
	// writes one or two, and ffmpeg 9 decodes a packet of several blocks
	// of more as one block (checked: a 3-channel file of two 768-byte
	// blocks comes back as 1012 samples a channel, not 1000).
	if channels > 2 {
		return nil, notDecodedf("Microsoft ADPCM of %d channels is not decoded; mono and stereo are", channels)
	}
	if blockAlign < 7*channels || blockAlign > 1<<16 {
		return nil, fmt.Errorf("a Microsoft ADPCM block_align of %d for %d channels", blockAlign, channels)
	}
	var out []int16
	st := make([]msState, 2)
	for len(data) >= 7*channels {
		blk := data[:min(len(data), blockAlign)]
		data = data[len(blk):]
		frames := (len(blk) - 6*channels) * 2 / channels
		for c := range channels {
			pi := int(blk[c])
			if pi > 6 {
				return nil, notDecodedf("a Microsoft ADPCM block predictor of %d is not decoded; the 7 standard coefficient pairs are", pi)
			}
			st[c].c1, st[c].c2 = msCoeff1[pi], msCoeff2[pi]
			le := func(k int) int { return int(int16(binary.LittleEndian.Uint16(blk[channels+2*(k*channels+c):]))) }
			st[c].delta, st[c].s1, st[c].s2 = le(0), le(1), le(2)
		}
		for c := range channels {
			out = append(out, int16(st[c].s2))
		}
		for c := range channels {
			out = append(out, int16(st[c].s1))
		}
		p := 7 * channels
		r := 0
		if channels == 2 {
			r = 1
		}
		for n := (frames - 2) >> (1 - r); n > 0; n-- {
			b := int(blk[p])
			p++
			out = append(out, int16(st[0].nibble(b>>4)), int16(st[r].nibble(b&15)))
		}
	}
	return out, nil
}
