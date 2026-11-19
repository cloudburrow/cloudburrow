package embedding

// G.726 ADPCM in a WAV (WAVE_FORMAT_G726_ADPCM, 0x45; #1651), at 16, 24,
// 32 and 40 kbit/s (2- to 5-bit codes), decoded to linear PCM as ITU-T
// G.726 specifies (the adaptive quantizer, the two-pole six-zero adaptive
// predictor, tone and transition detection and the adaptation speed
// control), each code read most significant bit first, as ffmpeg's wav
// demuxer hands the data to its adpcm_g726 decoder. Written for
// CloudBurrow from the ITU-T recommendation's block descriptions, in the
// integer arithmetic of the public-domain Sun Microsystems G.72x reference
// implementation ("provided for unrestricted use"); the 2-bit tables are
// G.726's 16 kbit/s ones. It matches ffmpeg 9's decode sample for sample
// (decoders_test.go), which differs from the reference in two places: the
// step size mix (decode) and the 16-bit wrap of the reconstructed signal,
// whose output is clipped to ±0xFFFF after the shift and then truncated to
// 16 bits. The zero predictor leaks 2^-8 at every rate, 40 kbit/s too.

// waveG726 is WAVE_FORMAT_G726_ADPCM's ITU tag, 0x45; ffmpeg reads the
// older 0x64 (WAVE_FORMAT_G726_ADPCM of APICOM) the same way, as
// decodeWAV does.
const (
	waveG726       = 0x45
	waveG726APICOM = 0x64
)

// g726Rate is one bit rate's tables: each code's log-domain quantized
// difference (dqln), scale factor multiplier (wi) and speed control
// (fi), and the leak of the zero predictor's coefficients.
type g726Rate struct {
	dqln, wi, fi []int32
	leak         uint
}

var g726Rates = [6]*g726Rate{
	2: {
		dqln: []int32{116, 365, 365, 116},
		wi:   []int32{-704, 14048, 14048, -704},
		fi:   []int32{0, 0xE00, 0xE00, 0},
		leak: 8,
	},
	3: {
		dqln: []int32{-2048, 135, 273, 373, 373, 273, 135, -2048},
		wi:   []int32{-128, 960, 4384, 18624, 18624, 4384, 960, -128},
		fi:   []int32{0, 0x200, 0x400, 0xE00, 0xE00, 0x400, 0x200, 0},
		leak: 8,
	},
	4: {
		dqln: []int32{-2048, 4, 135, 213, 273, 323, 373, 425, 425, 373, 323, 273, 213, 135, 4, -2048},
		wi:   []int32{-384, 576, 1312, 2048, 3584, 6336, 11360, 35904, 35904, 11360, 6336, 3584, 2048, 1312, 576, -384},
		fi:   []int32{0, 0, 0, 0x200, 0x200, 0x200, 0x600, 0xE00, 0xE00, 0x600, 0x200, 0x200, 0x200, 0, 0, 0},
		leak: 8,
	},
	5: {
		dqln: []int32{-2048, -66, 28, 104, 169, 224, 274, 318, 358, 395, 429, 459, 488, 514, 539, 566,
			566, 539, 514, 488, 459, 429, 395, 358, 318, 274, 224, 169, 104, 28, -66, -2048},
		wi: []int32{448, 448, 768, 1248, 1280, 1312, 1856, 3200, 4512, 5728, 7008, 8960, 11456, 14080, 16928, 22272,
			22272, 16928, 14080, 11456, 8960, 7008, 5728, 4512, 3200, 1856, 1312, 1280, 1248, 768, 448, 448},
		fi: []int32{0, 0, 0, 0, 0, 0x200, 0x200, 0x200, 0x200, 0x200, 0x400, 0x600, 0x800, 0xA00, 0xC00, 0xC00,
			0xC00, 0xC00, 0xA00, 0x800, 0x600, 0x400, 0x200, 0x200, 0x200, 0x200, 0x200, 0, 0, 0, 0, 0},
		leak: 8,
	},
}

// g726State is the decoder's state, in G.726's fixed-point formats.
type g726State struct {
	yl       int32 // slow quantizer scale factor (19 bits)
	yu       int32 // fast quantizer scale factor
	dms, dml int32 // short- and long-term energy averages
	ap       int32 // speed control
	a        [2]int32
	b        [6]int32
	pk       [2]int32
	dq       [6]int32 // floating-point quantized differences
	sr       [2]int32 // floating-point reconstructed signal
	td       int32    // tone detected
}

func newG726State() *g726State {
	s := &g726State{yl: 34816, yu: 544}
	s.sr = [2]int32{32, 32}
	s.dq = [6]int32{32, 32, 32, 32, 32, 32}
	return s
}

// g726Quan is the number of entries of power2 at or below val.
func g726Quan(val int32) int32 {
	var i int32
	for ; i < 15; i++ {
		if val < 1<<i {
			break
		}
	}
	return i
}

// g726Fmult multiplies a predictor coefficient by a floating-point
// sample (FMULT).
func g726Fmult(an, srn int32) int32 {
	anmag := an
	if an <= 0 {
		anmag = (-an) & 0x1FFF
	}
	anexp := g726Quan(anmag) - 6
	var anmant int32
	switch {
	case anmag == 0:
		anmant = 32
	case anexp >= 0:
		anmant = anmag >> anexp
	default:
		anmant = anmag << -anexp
	}
	wanexp := anexp + (srn>>6)&0xF - 13
	wanmant := (anmant*(srn&0x3F) + 0x30) >> 4
	var r int32
	if wanexp >= 0 {
		r = (wanmant << wanexp) & 0x7FFF
	} else {
		r = wanmant >> -wanexp
	}
	if int16(an)^int16(srn) < 0 {
		return -r
	}
	return r
}

// g726Float is a value in G.726's 4-bit exponent, 6-bit mantissa form,
// negative ones offset by -0x400 as 16-bit words.
func g726Float(v int32) int32 {
	mag := v
	if v < 0 {
		mag = -v
	}
	if mag == 0 {
		if v >= 0 {
			return 0x20
		}
		return int32(int16(-0x3E0)) // 0xFC20
	}
	exp := g726Quan(mag)
	f := exp<<6 + (mag<<6)>>exp
	if v < 0 {
		f -= 0x400
	}
	return int32(int16(f))
}

// decode is one code's 14-bit linear sample.
func (s *g726State) decode(code int32, r *g726Rate, bits uint) int32 {
	var sezi int32
	for i := range s.b {
		sezi += g726Fmult(s.b[i]>>2, s.dq[i])
	}
	sez := sezi >> 1
	sei := sezi + g726Fmult(s.a[1]>>2, s.sr[1]) + g726Fmult(s.a[0]>>2, s.sr[0])
	se := sei >> 1

	// Step size: the fast and slow scale factors mixed by the speed
	// control. The slow factor's six fraction bits stay in the sum, as
	// ffmpeg's decoder keeps them: truncating YL to YLP first (G.726's MIX
	// block, and Sun's step_size) lands one step off ffmpeg wherever the
	// fraction carries, from about the thousandth sample of speech on.
	y := s.yu
	if s.ap < 256 {
		y = (s.yl + (s.yu-s.yl>>6)*(s.ap>>2)) >> 6
	}

	// Reconstruct the quantized difference.
	sign := code&(1<<(bits-1)) != 0
	var dq int32
	dql := r.dqln[code] + y>>2
	if dql < 0 {
		if sign {
			dq = -0x8000
		}
	} else {
		dex := (dql >> 7) & 15
		dqt := 128 + dql&127
		dq = (dqt << 7) >> (14 - dex)
		if sign {
			dq -= 0x8000
		}
	}
	var sr int32
	if dq < 0 {
		sr = se - dq&0x7FFF
	} else {
		sr = se + dq
	}
	sr = int32(int16(sr)) // SR is a 16-bit two's complement word
	dqsez := sr - se + sez
	s.update(bits, y, r.wi[code], r.fi[code], dq, sr, dqsez, r.leak)
	return sr
}

func (s *g726State) update(bits uint, y, wi, fi, dq, sr, dqsez int32, leak uint) {
	var pk0 int32
	if dqsez < 0 {
		pk0 = 1
	}
	mag := dq & 0x7FFF
	// Transition detection.
	ylint := s.yl >> 15
	ylfrac := (s.yl >> 10) & 0x1F
	thr1 := (32 + ylfrac) << ylint
	thr2 := thr1
	if ylint > 9 {
		thr2 = 31 << 10
	}
	dqthr := (thr2 + thr2>>1) >> 1
	tr := s.td != 0 && mag > dqthr

	// Quantizer scale factor adaptation.
	s.yu = y + (wi-y)>>5
	s.yu = min(max(s.yu, 544), 5120)
	s.yl += s.yu + (-s.yl)>>6

	var a2p int32
	if tr {
		s.a = [2]int32{}
		s.b = [6]int32{}
	} else {
		pks1 := pk0 ^ s.pk[0]
		a2p = s.a[1] - s.a[1]>>7
		if dqsez != 0 {
			fa1 := -s.a[0]
			if pks1 != 0 {
				fa1 = s.a[0]
			}
			switch {
			case fa1 < -8191:
				a2p -= 0x100
			case fa1 > 8191:
				a2p += 0xFF
			default:
				a2p += fa1 >> 5
			}
			if pk0^s.pk[1] != 0 {
				a2p -= 0x80
			} else {
				a2p += 0x80
			}
			a2p = min(max(a2p, -12288), 12288)
		}
		s.a[1] = a2p
		s.a[0] -= s.a[0] >> 8
		if dqsez != 0 {
			if pks1 == 0 {
				s.a[0] += 192
			} else {
				s.a[0] -= 192
			}
		}
		a1ul := 15360 - a2p
		s.a[0] = min(max(s.a[0], -a1ul), a1ul)
		for i := range s.b {
			s.b[i] -= s.b[i] >> leak
			if dq&0x7FFF != 0 {
				if int16(dq)^int16(s.dq[i]) >= 0 {
					s.b[i] += 128
				} else {
					s.b[i] -= 128
				}
			}
			s.b[i] = int32(int16(s.b[i]))
		}
	}
	copy(s.dq[1:], s.dq[:5])
	if mag == 0 {
		if dq >= 0 {
			s.dq[0] = 0x20
		} else {
			s.dq[0] = int32(int16(-0x3E0))
		}
	} else {
		exp := g726Quan(mag)
		f := exp<<6 + (mag<<6)>>exp
		if dq < 0 {
			f -= 0x400
		}
		s.dq[0] = int32(int16(f))
	}
	s.sr[1] = s.sr[0]
	switch {
	case sr == 0:
		s.sr[0] = 0x20
	case sr > -32768:
		s.sr[0] = g726Float(sr)
	default:
		s.sr[0] = int32(int16(-0x3E0))
	}
	s.pk[1], s.pk[0] = s.pk[0], pk0

	// Tone detection.
	s.td = 0
	if !tr && a2p < -11776 {
		s.td = 1
	}
	// Adaptation speed control.
	s.dms += (fi - s.dms) >> 5
	s.dml += (fi<<2 - s.dml) >> 7
	d := s.dms<<2 - s.dml
	if d < 0 {
		d = -d
	}
	switch {
	case tr:
		s.ap = 256
	case y < 1536, s.td == 1, d >= s.dml>>3:
		s.ap += (0x200 - s.ap) >> 4
	default:
		s.ap += (-s.ap) >> 4
	}
}

// decodeG726 decodes a WAV's G.726 data chunk of bits-bit codes, packed
// most significant bit first, to 16-bit samples (the 14-bit linear output
// shifted up by 2, as the decoders' linear output is). Mono only: G.726 is
// a narrowband speech codec, and ffmpeg's decoder takes one channel.
func decodeG726(data []byte, channels, bits int) ([]int16, error) {
	if bits < 2 || bits > 5 {
		return nil, notDecodedf("G.726 with %d bits a sample is not decoded; 2 to 5 bits (16 to 40 kbit/s) are", bits)
	}
	if channels != 1 {
		return nil, notDecodedf("a G.726 WAV of %d channels is not decoded; mono is", channels)
	}
	r := g726Rates[bits]
	s := newG726State()
	n := len(data) * 8 / bits
	out := make([]int16, 0, n)
	var acc uint32
	var have uint
	for _, b := range data {
		acc = acc<<8 | uint32(b)
		have += 8
		for have >= uint(bits) {
			have -= uint(bits)
			code := int32(acc>>have) & (1<<bits - 1)
			v := s.decode(code, r, uint(bits))
			out = append(out, int16(min(max(v<<2, -0xFFFF), 0xFFFF)))
		}
	}
	return out, nil
}
