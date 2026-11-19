package embedding

import (
	"encoding/binary"
	"fmt"
)

// ADPCM WAV (#1651): IMA ADPCM (WAVE format 0x11, the DVI/Intel layout),
// 2 to 5 bits a sample, and Microsoft ADPCM (WAVE format 2), 4 bits a
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

// imaIndexTables are the step index adjustments of 2-, 3- and 5-bit IMA
// ADPCM (IMA's Recommended Practices), by the code's magnitude bits.
var imaIndexTables = map[int][]int{
	2: {-1, 2},
	3: {-1, -1, 1, 2},
	5: {-1, -1, -1, -1, -1, -1, -1, -1, 1, 2, 4, 6, 8, 10, 13, 16},
}

// imaGroup is, for 2-, 3- and 5-bit IMA ADPCM, the bytes of one channel's
// group and the samples it holds (ffmpeg's ff_adpcm_ima_block_sizes and
// ff_adpcm_ima_block_samples).
var imaGroup = map[int][2]int{2: {4, 16}, 3: {12, 32}, 5: {20, 32}}

// code expands one code of bits bits (a sign bit, then bits-1 magnitude
// bits) with the shift-and-add the 4-bit nibble uses: step >> (bits-1),
// plus step for the top magnitude bit, step >> 1 for the next... (checked
// against ffmpeg 9's decodes of 2-, 3- and 5-bit WAVs).
func (s *imaState) code(n, bits int) int {
	m := bits - 1
	step := imaStepTable[s.index]
	s.index = max(0, min(88, s.index+imaIndexTables[bits][n&(1<<m-1)]))
	diff := step >> m
	for i := range m {
		if n&(1<<(m-1-i)) != 0 {
			diff += step >> i
		}
	}
	if n&(1<<m) != 0 {
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
	if _, ok := imaGroup[bits]; ok {
		return decodeIMAPacked(data, channels, blockAlign, bits)
	}
	if bits != 4 {
		return nil, fmt.Errorf("IMA ADPCM of %d bits a sample; it has 2 to 5", bits)
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

// decodeIMAPacked decodes 2-, 3- and 5-bit IMA ADPCM blocks
// (decodeIMAADPCM): after the channels' headers, groups of each channel's
// bytes in turn, their 4-byte words interleaved with the other channels'
// as the 4-bit words are, each group a little-endian bit stream.
func decodeIMAPacked(data []byte, channels, blockAlign, bits int) ([]int16, error) {
	if blockAlign < 4*channels || blockAlign > 1<<16 {
		return nil, fmt.Errorf("an IMA ADPCM block_align of %d for %d channels", blockAlign, channels)
	}
	size, per := imaGroup[bits][0], imaGroup[bits][1]
	var out []int16
	st := make([]imaState, channels)
	grp := make([]byte, size)
	for len(data) >= 4*channels {
		blk := data[:min(len(data), blockAlign)]
		data = data[len(blk):]
		groups := (len(blk) - 4*channels) / (size * channels)
		frames := 1 + groups*per
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
			p := 4*channels + g*size*channels
			for c := range channels {
				for j := range size {
					grp[j] = blk[p+j%4+j/4*channels*4+c*4]
				}
				for k := range per {
					at, v := k*bits, 0
					for b := range bits {
						v |= int(grp[(at+b)/8]>>((at+b)%8)&1) << b
					}
					f := 1 + g*per + k
					out[base+f*channels+c] = int16(st[c].code(v, bits))
				}
			}
		}
	}
	return out, nil
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
