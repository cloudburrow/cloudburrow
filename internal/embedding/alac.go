package embedding

import (
	"fmt"
	"math/bits"

	"github.com/Eyevinn/mp4ff/mp4"
)

// ALAC (#1651): a video's Apple Lossless audio track, a Go port of the
// algorithms of Apple's reference decoder (github.com/macosforge/alac,
// ALACDecoder.cpp, ag_dec.c, dp_dec.c and matrix_dec.c; Copyright (c) 2011
// Apple Inc., Apache License 2.0, attributed in NOTICE): single and
// channel-pair elements, the adaptive Golomb residual, the adaptive
// predictor, stereo unmixing and shifted low bytes, 16 to 32 bits a sample
// and 1 to 8 channels. Checked sample for sample against ffmpeg 9's
// decodes (alac_test.go).

// alacConfig is the ALACSpecificConfig of the track's magic cookie.
type alacConfig struct {
	frameLength          int
	bitDepth             int
	pb, mb, kb, channels int
	maxRun               int
}

const (
	alacQBShift   = 9
	alacQB        = 1 << alacQBShift
	alacMMulShift = 2
	alacMDenShift = alacQBShift - alacMMulShift - 1
	alacMOff      = 1 << (alacMDenShift - 2)
	alacBitOff    = 24
	alacMaxPrefix = 9
	alacMeanClamp = 0xffff
)

// peek32 returns the 32 bits at r's position (zeros past the end).
func (r *bitReader) peek32() uint32 {
	var v uint64
	i := r.pos >> 3
	for k := range 5 {
		v <<= 8
		if i+k < len(r.b) {
			v |= uint64(r.b[i+k])
		}
	}
	return uint32(v << (r.pos & 7) >> 8)
}

func lead(x uint32) int { return bits.LeadingZeros32(x) }

// alacDynGet is ag_dec.c's dyn_get_32bit (escaping to maxBits raw bits);
// with maxBits 16 it is dyn_get, which the zero runs use.
func alacDynGet(r *bitReader, m uint32, k, maxBits int) (uint32, error) {
	if r.pos > 8*len(r.b) {
		return 0, errShort
	}
	s := r.peek32()
	pre := lead(^s)
	if pre >= alacMaxPrefix {
		r.pos += alacMaxPrefix
		v, err := r.bits(maxBits)
		return uint32(v), err
	}
	r.pos += pre + 1
	v := uint32(0)
	if k > 0 {
		v = s << (pre + 1) >> (32 - k)
	}
	r.pos += k
	if v < 2 {
		r.pos--
		return uint32(pre) * m, nil
	}
	return uint32(pre)*m + v - 1, nil
}

// alacDynDecomp is ag_dec.c's dyn_decomp: n residuals of chanBits bits.
func alacDynDecomp(r *bitReader, out []int32, cfg alacConfig, pb, chanBits int) error {
	mb := uint32(cfg.mb)
	wb := uint32(1)<<cfg.kb - 1
	zmode := uint32(0)
	for c := 0; c < len(out); {
		m := mb >> alacQBShift
		k := min(31-lead(m+3), cfg.kb)
		n, err := alacDynGet(r, uint32(1)<<k-1, k, chanBits)
		if err != nil {
			return err
		}
		nd := n + zmode
		mult := -int32(nd&1) | 1
		out[c] = int32((nd+1)>>1) * mult
		c++
		mb = uint32(pb)*(n+zmode) + mb - (uint32(pb)*mb)>>alacQBShift
		if n > alacMeanClamp {
			mb = alacMeanClamp
		}
		zmode = 0
		if mb<<alacMMulShift < alacQB && c < len(out) {
			zmode = 1
			k := lead(mb) - alacBitOff + int((mb+alacMOff)>>alacMDenShift)
			mz := (uint32(1)<<k - 1) & wb
			n, err := alacDynGet(r, mz, k, 16)
			if err != nil {
				return err
			}
			if c+int(n) > len(out) {
				return fmt.Errorf("an ALAC zero run past the frame")
			}
			for range n {
				out[c] = 0
				c++
			}
			if n >= 65535 {
				zmode = 0
			}
			mb = 0
		}
	}
	return nil
}

func signOf(v int32) int32 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// alacUnpc is dp_dec.c's unpc_block: the adaptive predictor, in place over
// pc (out may be pc).
func alacUnpc(pc, out []int32, coefs []int32, active, chanBits, denShift int) {
	shift := 32 - chanBits
	denHalf := int32(0)
	if denShift > 0 {
		denHalf = 1 << (denShift - 1)
	}
	num := len(pc)
	if num == 0 {
		return
	}
	out[0] = pc[0]
	if active == 0 {
		copy(out, pc)
		return
	}
	if active == 31 {
		prev := out[0]
		for j := 1; j < num; j++ {
			del := pc[j] + prev
			prev = del << shift >> shift
			out[j] = prev
		}
		return
	}
	for j := 1; j <= active && j < num; j++ {
		del := pc[j] + out[j-1]
		out[j] = del << shift >> shift
	}
	lim := active + 1
	for j := lim; j < num; j++ {
		var sum int32
		top := out[j-lim]
		for k := range active {
			sum += coefs[k] * (out[j-1-k] - top)
		}
		del := pc[j]
		del0 := del
		sg := signOf(del)
		del += top + (sum+denHalf)>>denShift
		out[j] = del << shift >> shift
		if sg > 0 {
			for k := active - 1; k >= 0; k-- {
				dd := top - out[j-1-k]
				sgn := signOf(dd)
				coefs[k] -= sgn
				del0 -= int32(active-k) * ((sgn * dd) >> denShift)
				if del0 <= 0 {
					break
				}
			}
		} else if sg < 0 {
			for k := active - 1; k >= 0; k-- {
				dd := top - out[j-1-k]
				sgn := signOf(dd)
				coefs[k] += sgn
				del0 -= int32(active-k) * ((-sgn * dd) >> denShift)
				if del0 >= 0 {
					break
				}
			}
		}
	}
}

// alacChannelNames are the channels of an ALAC stream of n channels in
// bitstream order (ALAC's channel layouts, as ffmpeg names them).
var alacChannelNames = [9][]string{
	1: {"FC"},
	2: {"FL", "FR"},
	3: {"FC", "FL", "FR"},
	4: {"FC", "FL", "FR", "BC"},
	5: {"FC", "FL", "FR", "BL", "BR"},
	6: {"FC", "FL", "FR", "BL", "BR", "LFE"},
	7: {"FC", "FL", "FR", "BL", "BR", "BC", "LFE"},
	8: {"FC", "FLC", "FRC", "FL", "FR", "BL", "BR", "LFE"},
}

// decodeALACPacket decodes one packet to per-channel samples of
// cfg.bitDepth bits.
func decodeALACPacket(b []byte, cfg alacConfig) ([][]int32, error) {
	r := &bitReader{b: b}
	var chans [][]int32
	for {
		tag, err := r.bits(3)
		if err != nil {
			return nil, err
		}
		switch tag {
		case 7: // ID_END
			return chans, nil
		case 0, 1, 3: // SCE, CPE, LFE
			n := 1
			if tag == 1 {
				n = 2
			}
			if len(chans)+n > cfg.channels {
				return nil, fmt.Errorf("an ALAC packet of more channels than its %d", cfg.channels)
			}
			c, err := decodeALACElement(r, cfg, n)
			if err != nil {
				return nil, err
			}
			chans = append(chans, c...)
		case 4: // DSE: skipped
			r.bits(4)
			align, _ := r.bits(1)
			count, _ := r.bits(8)
			if count == 255 {
				more, _ := r.bits(8)
				count += more
			}
			if align != 0 {
				r.pos = (r.pos + 7) &^ 7
			}
			r.pos += 8 * int(count)
		case 6: // FIL: skipped
			count, _ := r.bits(4)
			if count == 15 {
				more, _ := r.bits(8)
				count += more - 1
			}
			r.pos += 8 * int(count)
		default:
			return nil, notDecodedf("an ALAC element of type %d is not decoded", tag)
		}
		if r.pos > 8*len(b) {
			return nil, errShort
		}
	}
}

func decodeALACElement(r *bitReader, cfg alacConfig, nch int) ([][]int32, error) {
	r.bits(4) // element instance tag
	if unused, err := r.bits(12); err != nil || unused != 0 {
		return nil, fmt.Errorf("an ALAC element header's unused bits are set")
	}
	h, _ := r.bits(4)
	partial, shifted, escape := h>>3 != 0, int(h>>1&3), h&1 != 0
	num := cfg.frameLength
	if partial {
		v, err := r.bits(32)
		if err != nil {
			return nil, err
		}
		num = int(v)
	}
	if num < 1 || num > cfg.frameLength {
		return nil, fmt.Errorf("an ALAC element of %d samples, for frames of %d", num, cfg.frameLength)
	}
	out := make([][]int32, nch)
	for c := range out {
		out[c] = make([]int32, num)
	}
	var mixBits, mixRes int32
	if !escape {
		chanBits := cfg.bitDepth - 8*shifted + nch - 1
		if chanBits < 1 || chanBits > 32 {
			return nil, fmt.Errorf("an ALAC element of %d bits a sample", chanBits)
		}
		mb, _ := r.bits(8)
		mr, _ := r.bits(8)
		mixBits, mixRes = int32(mb), int32(int8(mr))
		type chanParams struct {
			mode, den, pbf int
			coefs          []int32
		}
		ps := make([]chanParams, nch)
		for c := range ps {
			h1, _ := r.bits(8)
			h2, _ := r.bits(8)
			ps[c] = chanParams{mode: int(h1 >> 4), den: int(h1 & 15), pbf: int(h2 >> 5)}
			ps[c].coefs = make([]int32, h2&31)
			for i := range ps[c].coefs {
				v, err := r.signed(16)
				if err != nil {
					return nil, err
				}
				ps[c].coefs[i] = int32(v)
			}
		}
		shiftAt := r.pos
		r.pos += 8 * shifted * nch * num
		pred := make([]int32, num)
		for c, p := range ps {
			if err := alacDynDecomp(r, pred, cfg, cfg.pb*p.pbf/4, chanBits); err != nil {
				return nil, err
			}
			if p.mode != 0 {
				alacUnpc(pred, pred, nil, 31, chanBits, 0)
			}
			alacUnpc(pred, out[c], p.coefs, len(p.coefs), chanBits, p.den)
		}
		if nch == 2 && mixRes != 0 {
			u, v := out[0], out[1]
			for j := range u {
				l := u[j] + v[j] - (mixRes*v[j])>>mixBits
				u[j], v[j] = l, l-v[j]
			}
		}
		if shifted > 0 {
			end := r.pos
			r.pos = shiftAt
			for j := range num {
				for c := range nch {
					s, err := r.bits(8 * shifted)
					if err != nil {
						return nil, err
					}
					out[c][j] = out[c][j]<<(8*shifted) | int32(s)
				}
			}
			r.pos = end
		}
	} else {
		bitsN := cfg.bitDepth
		for j := range num {
			for c := range nch {
				v, err := r.signed(bitsN)
				if err != nil {
					return nil, err
				}
				out[c][j] = int32(v)
			}
		}
	}
	return out, nil
}

// decodeALACTrack decodes a video's ALAC track to interleaved samples, as
// ffmpeg's alac decoder outputs them (s16 for 16 bits, else s32, then
// float).
func decodeALACTrack(tr *soundTrack, e *mp4.AudioSampleEntryBox) (pcm []float32, channels, rate int, layout []string, err error) {
	if e == nil || e.Alac == nil {
		return nil, 0, 0, nil, fmt.Errorf("the alac track has no ALACSpecificConfig")
	}
	a := e.Alac
	cfg := alacConfig{frameLength: int(a.FrameLength), bitDepth: int(a.BitDepth), pb: int(a.Pb),
		mb: int(a.Mb), kb: int(a.Kb), channels: int(a.NumChannels), maxRun: int(a.MaxRun)}
	switch {
	case cfg.channels < 1 || cfg.channels > 8:
		return nil, 0, 0, nil, notDecodedf("ALAC of %d channels is not decoded; 1 to 8 are", cfg.channels)
	case cfg.bitDepth != 16 && cfg.bitDepth != 20 && cfg.bitDepth != 24 && cfg.bitDepth != 32:
		return nil, 0, 0, nil, fmt.Errorf("an ALAC bit depth of %d", cfg.bitDepth)
	case cfg.frameLength < 1 || cfg.frameLength > 1<<16:
		return nil, 0, 0, nil, fmt.Errorf("an ALAC frame length of %d", cfg.frameLength)
	case cfg.kb < 1 || cfg.kb > 30:
		return nil, 0, 0, nil, fmt.Errorf("an ALAC kb of %d", cfg.kb)
	case a.SampleRate < 1:
		return nil, 0, 0, nil, fmt.Errorf("an ALAC sample rate of 0")
	}
	channels, rate = cfg.channels, int(a.SampleRate)
	scale, shift := float32(1.0/32768), 16-cfg.bitDepth
	if cfg.bitDepth > 16 {
		scale, shift = 1.0/(1<<31), 32-cfg.bitDepth
	}
	for i, au := range tr.samples {
		ch, err := decodeALACPacket(au, cfg)
		if err != nil {
			return nil, 0, 0, nil, fmt.Errorf("the ALAC track's packet %d: %w", i, err)
		}
		if len(ch) != channels {
			return nil, 0, 0, nil, fmt.Errorf("the ALAC track's packet %d has %d channels of %d", i, len(ch), channels)
		}
		if len(pcm)/channels > (MaxAudioSeconds+60)*rate {
			return nil, 0, 0, nil, &tooLongError{float64(len(pcm)/channels) / float64(rate)}
		}
		for j := range ch[0] {
			for c := range channels {
				pcm = append(pcm, float32(ch[c][j]<<shift)*scale)
			}
		}
	}
	return pcm, channels, rate, alacChannelNames[channels], nil
}
