package embedding

import "fmt"

// GSM 06.10 full-rate speech in a WAV (WAVE_FORMAT_GSM610, 0x31; #1651),
// Microsoft's packing: two 260-bit frames, 320 samples at 8 kHz, in each
// 65-byte block, the bits least significant first. Decoded as ETSI EN 300
// 961 (GSM 06.10) specifies, in its 16-bit fixed-point arithmetic: RPE
// decoding and APCM inverse quantization, long-term synthesis, LAR
// decoding and interpolation, the short-term synthesis lattice, then
// de-emphasis and upscaling. Written for CloudBurrow from the
// specification's description.

const waveGSM610 = 0x31

func gsmSat(v int32) int32    { return min(max(v, -32768), 32767) }
func gsmAdd(a, b int32) int32 { return gsmSat(a + b) }
func gsmSub(a, b int32) int32 { return gsmSat(a - b) }

// gsmMultR is mult_r: the rounded Q15 product.
func gsmMultR(a, b int32) int32 {
	if a == -32768 && b == -32768 {
		return 32767
	}
	return (a*b + 16384) >> 15
}

func gsmAbs(a int32) int32 {
	if a < 0 {
		if a == -32768 {
			return 32767
		}
		return -a
	}
	return a
}

func gsmAsr(a, n int32) int32 {
	switch {
	case n >= 16:
		if a < 0 {
			return -1
		}
		return 0
	case n <= -16:
		return 0
	case n < 0:
		return gsmSat(a << -n)
	}
	return a >> n
}

func gsmAsl(a, n int32) int32 {
	switch {
	case n >= 16:
		return 0
	case n <= -16:
		if a < 0 {
			return -1
		}
		return 0
	case n < 0:
		return gsmAsr(a, -n)
	}
	return gsmSat(a << n)
}

var (
	gsmFAC     = [8]int32{18431, 20479, 22527, 24575, 26623, 28671, 30719, 32767}
	gsmQLB     = [4]int32{3277, 11469, 21299, 32767}
	gsmA       = [8]int32{20480, 20480, 20480, 20480, 13964, 15360, 8534, 9036}
	gsmB       = [8]int32{0, 0, 2048, -2560, 94, -1792, -341, -1144}
	gsmMIC     = [8]int32{-32, -32, -16, -16, -8, -8, -4, -4}
	gsmINVA    = [8]int32{13107, 13107, 13107, 13107, 19223, 17476, 31454, 29708}
	gsmLARBits = [8]uint{6, 6, 5, 5, 4, 4, 3, 3}
)

// gsmState is the decoder's memory across frames.
type gsmState struct {
	dp    [120]int32 // the reconstructed long-term residual's history
	larpp [8]int32   // the previous frame's decoded LARs
	v     [9]int32   // the lattice's state
	msr   int32      // de-emphasis memory
}

// gsmBits reads a little-endian bit stream.
type gsmBits struct {
	b   []byte
	pos uint
}

func (r *gsmBits) get(n uint) int32 {
	var v int32
	for i := uint(0); i < n; i++ {
		p := r.pos + i
		if r.b[p>>3]>>(p&7)&1 != 0 {
			v |= 1 << i
		}
	}
	r.pos += n
	return v
}

// frame decodes one 260-bit frame to 160 samples.
func (s *gsmState) frame(r *gsmBits, out []int16) {
	var larc [8]int32
	for i := range larc {
		larc[i] = r.get(gsmLARBits[i])
	}
	var wt [160]int32
	for j := 0; j < 4; j++ {
		nc := r.get(7)
		bc := r.get(2)
		mc := r.get(2)
		xmaxc := r.get(6)
		var xmc [13]int32
		for i := range xmc {
			xmc[i] = r.get(3)
		}
		// APCM inverse quantization.
		exp := int32(0)
		if xmaxc > 15 {
			exp = (xmaxc >> 3) - 1
		}
		mant := xmaxc - exp<<3
		if mant == 0 {
			exp, mant = -4, 7
		} else {
			for mant <= 7 {
				mant = mant<<1 | 1
				exp--
			}
			mant -= 8
		}
		temp1 := gsmFAC[mant]
		temp2 := gsmSub(6, exp)
		temp3 := gsmAsl(1, gsmSub(temp2, 1))
		var erp [40]int32
		for i := range xmc {
			t := (xmc[i]<<1 - 7) << 12
			t = gsmMultR(temp1, t)
			t = gsmAdd(t, temp3)
			erp[int(mc)+3*i] = gsmAsr(t, temp2)
		}
		// Long-term synthesis.
		nr := nc
		// Out of range (40 to 120), the lag is clamped into it, as ffmpeg 9
		// decodes it (ETSI's decoder takes the previous lag).
		nr = min(max(nc, 40), 120)
		brp := gsmQLB[bc]
		var drp [40]int32
		for k := range drp {
			var prev int32
			if i := k - int(nr); i < 0 {
				prev = s.dp[120+i]
			} else {
				prev = drp[i]
			}
			drp[k] = gsmAdd(erp[k], gsmMultR(brp, prev))
		}
		copy(s.dp[:], s.dp[40:])
		copy(s.dp[80:], drp[:])
		copy(wt[40*j:], drp[:])
	}
	// LAR decoding.
	var larpp [8]int32
	for i := range larpp {
		t := gsmAdd(larc[i], gsmMIC[i]) << 10
		t = gsmSub(t, gsmB[i]<<1)
		t = gsmMultR(gsmINVA[i], t)
		larpp[i] = gsmAdd(t, t)
	}
	prev := s.larpp
	s.larpp = larpp
	var rp [8]int32
	segment := func(start, end int, interp func(p, c int32) int32) {
		for i := range rp {
			l := interp(prev[i], larpp[i])
			t := gsmAbs(l)
			switch {
			case t < 11059:
				t <<= 1
			case t < 20070:
				t += 11059
			default:
				t = gsmAdd(t>>2, 26112)
			}
			if l < 0 {
				t = -t
			}
			rp[i] = t
		}
		for k := start; k < end; k++ {
			sri := wt[k]
			for i := 7; i >= 0; i-- {
				sri = gsmSub(sri, gsmMultR(rp[i], s.v[i]))
				s.v[i+1] = gsmAdd(s.v[i], gsmMultR(rp[i], sri))
			}
			s.v[0] = sri
			// De-emphasis and upscaling.
			s.msr = gsmAdd(sri, gsmMultR(s.msr, 28180))
			out[k] = int16(gsmAdd(s.msr, s.msr) &^ 7)
		}
	}
	segment(0, 13, func(p, c int32) int32 { return gsmAdd(gsmAdd(p>>2, c>>2), p>>1) })
	segment(13, 27, func(p, c int32) int32 { return gsmAdd(p>>1, c>>1) })
	segment(27, 40, func(p, c int32) int32 { return gsmAdd(gsmAdd(p>>2, c>>2), c>>1) })
	segment(40, 160, func(_, c int32) int32 { return c })
}

// decodeGSM610 decodes a WAV's Microsoft GSM 6.10 data chunk (65-byte
// blocks of two frames) to 16-bit samples. GSM is mono speech.
func decodeGSM610(data []byte, channels, blockAlign int) ([]int16, error) {
	if channels != 1 {
		return nil, fmt.Errorf("a GSM 6.10 WAV of %d channels; GSM 6.10 is mono", channels)
	}
	if blockAlign != 65 {
		return nil, notDecodedf("GSM 6.10 in %d-byte blocks is not decoded; Microsoft's 65-byte blocks are", blockAlign)
	}
	s := &gsmState{}
	n := len(data) / 65
	out := make([]int16, n*320)
	for b := 0; b < n; b++ {
		r := &gsmBits{b: data[b*65 : b*65+65]}
		s.frame(r, out[b*320:])
		s.frame(r, out[b*320+160:])
	}
	return out, nil
}
