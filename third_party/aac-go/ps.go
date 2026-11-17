package aac

// Parametric Stereo (HE-AAC v2) — ISO/IEC 14496-3 §8.6.4 and Annex 8.A.
// Written for CloudBurrow from the standard's text (#1739): ps_data()
// (Table 8.1), delta decoding (§8.6.4.5), the hybrid filterbank
// (§8.6.4.3), decorrelation with transient detection (§8.6.4.6), and the
// mixing procedure with time interpolation (§8.6.4.6.3), with 10 or 20
// parameter bands. 34 stereo bands, mixing procedure B and IPD/OPD are
// refused with ErrUnsupported: their normative tables (the 34-band hybrid
// filters and maps, the IPD/OPD Huffman books) had no allowed source.

import (
	"math"
)

const (
	psSlots   = sbrSlots
	psBands20 = 71 // 10 hybrid sub-subbands and QMF bands 3 to 63
	psHybrid  = 10
)

var (
	psIIDCoarse = [15]float64{-25, -18, -14, -10, -7, -4, -2, 0, 2, 4, 7, 10, 14, 18, 25}
	psIIDFine   = [31]float64{-50, -45, -40, -35, -30, -25, -22, -19, -16, -13, -10, -8, -6, -4, -2, 0,
		2, 4, 6, 8, 10, 13, 16, 19, 22, 25, 30, 35, 40, 45, 50}
	psICC = [8]float64{1, 0.937, 0.84118, 0.60092, 0.36764, 0, -0.589, -1}

	// The hybrid filters' prototypes — Table 8.A.? (13 taps).
	psG8 = [13]float64{0.00746082949812, 0.02270420949825, 0.04546865930473, 0.07266113929591,
		0.09885108575264, 0.11793710567217, 0.125, 0.11793710567217, 0.09885108575264,
		0.07266113929591, 0.04546865930473, 0.02270420949825, 0.00746082949812}
	psG2 = [13]float64{0, 0.01899487526049, 0, -0.07293139167538, 0, 0.30596630545168, 0.5,
		0.30596630545168, 0, -0.07293139167538, 0, 0.01899487526049, 0}

	psAllpassDelay = [3]int{3, 4, 5}
	psAllpassA     = [3]float64{0.65143905753106, 0.56471812200776, 0.48954165955695}
	psAllpassQ     = [3]float64{0.43, 0.75, 0.347}
)

const (
	psQPhi         = 0.39
	psAlphaDecay   = 0.76592833836465
	psAlphaSmooth  = 0.25
	psGamma        = 1.5
	psAllpassBands = 30 // bands below this are all-pass filtered
	psShortDelay   = 42 // from here, a delay of one slot instead of 14
	psDecayCutoff  = 3
	psDecaySlope   = 0.05
)

// psKToI maps the 71 bands to the 20 parameter bands.
var psKToI [psBands20]int

// psFCenter is each band's centre frequency in QMF bands.
var psFCenter [psBands20]float64

// psCis8 holds the 8-band filter's modulation, exp(i 2pi/8 (k+1/2)(n-6)).
var psCis8 [8][13]complex128

func init() {
	for k := range 8 {
		for i := range 13 {
			psCis8[k][i] = cis(2 * math.Pi / 8 * (float64(k) + 0.5) * float64(i-6))
		}
	}
	copy(psKToI[:], []int{1, 0, 0, 1, 2, 3, 4, 5, 6, 7})
	for q := 3; q < 64; q++ {
		var i int
		switch {
		case q < 9:
			i = q + 5
		case q < 11:
			i = 14
		case q < 14:
			i = 15
		case q < 18:
			i = 16
		case q < 23:
			i = 17
		case q < 35:
			i = 18
		default:
			i = 19
		}
		psKToI[q-3+psHybrid] = i
	}
	for i, f := range []float64{-3, -1, 1, 3, 5, 7, 10, 14, 18, 22} {
		psFCenter[i] = f / 8
	}
	for q := 3; q < 64; q++ {
		psFCenter[q-3+psHybrid] = float64(q) + 0.5
	}
}

type psDecoder struct {
	// header
	haveHeader bool
	enableIID  bool
	iidMode    int
	enableICC  bool
	iccMode    int
	enableExt  bool

	// frame
	numEnv  int
	border  [6]int
	iid     [6][34]int
	icc     [6][34]int
	iidPrev [34]int
	iccPrev [34]int
	nIIDPrv int
	nICCPrv int

	// hybrid analysis history: QMF bands 0 to 2, the last 6 slots
	hist [3][6]complex128

	// decorrelation state
	delay2   [2][psBands20]complex128 // the z^-2 before the all-pass
	apBuf    [3][psBands20][5]complex128
	apPos    [3]int
	longDel  [14][psBands20]complex128
	longPos  int
	peak     [20]float64
	smoothP  [20]float64
	smoothPD [20]float64

	// mixing
	hPrev [4][20]float64
}

func newPSDecoder() *psDecoder {
	return &psDecoder{}
}

var psNumEnv = [2][4]int{{0, 1, 2, 4}, {1, 2, 3, 4}}

// parse reads ps_data() — Table 8.1.
func (p *psDecoder) parse(b *bitr, bitsLeft int) error {
	start := b.pos()
	if b.u(1) == 1 {
		p.haveHeader = true
		p.enableIID = b.u(1) == 1
		if p.enableIID {
			p.iidMode = b.u(3)
		}
		p.enableICC = b.u(1) == 1
		if p.enableICC {
			p.iccMode = b.u(3)
		}
		p.enableExt = b.u(1) == 1
	}
	if p.iidMode > 5 || p.iccMode > 5 {
		return unsupportedf("PS iid_mode %d, icc_mode %d", p.iidMode, p.iccMode)
	}
	if p.haveHeader && ((p.enableIID && p.iidMode%3 == 2) || (p.enableICC && p.iccMode%3 == 2)) {
		return unsupportedf("HE-AACv2 parametric stereo with 34 stereo bands is not decoded")
	}
	if p.enableICC && p.iccMode >= 3 {
		return unsupportedf("HE-AACv2 parametric stereo mixing procedure B (icc_mode %d) is not decoded", p.iccMode)
	}
	frameClass := b.u(1)
	p.numEnv = psNumEnv[frameClass][b.u(2)]
	if frameClass == 1 {
		for e := range p.numEnv {
			p.border[e] = b.u(5)
		}
	}
	nIID := []int{10, 20, 34}[p.iidMode%3]
	nICC := []int{10, 20, 34}[p.iccMode%3]
	fine := p.iidMode >= 3
	if p.enableIID {
		for e := range p.numEnv {
			dt := b.u(1) == 1
			var h *sbrHuffman
			switch {
			case fine && dt:
				h = &huffPSIIDDtFine
			case fine:
				h = &huffPSIIDDfFine
			case dt:
				h = &huffPSIIDDt
			default:
				h = &huffPSIIDDf
			}
			if err := p.readPar(b, h, dt, p.iid[e][:nIID], p.prevIID(e, nIID)); err != nil {
				return err
			}
			lim := 7
			if fine {
				lim = 15
			}
			for i := range nIID {
				if p.iid[e][i] < -lim || p.iid[e][i] > lim {
					return errSBR
				}
			}
		}
	}
	if p.enableICC {
		for e := range p.numEnv {
			dt := b.u(1) == 1
			h := &huffPSICCDf
			if dt {
				h = &huffPSICCDt
			}
			if err := p.readPar(b, h, dt, p.icc[e][:nICC], p.prevICC(e, nICC)); err != nil {
				return err
			}
			for i := range nICC {
				if p.icc[e][i] < 0 || p.icc[e][i] > 7 {
					return errSBR
				}
			}
		}
	}
	if p.enableExt {
		cnt := b.u(4)
		if cnt == 15 {
			cnt += b.u(8)
		}
		// ps_extension(): IPD/OPD (extension id 0) is refused when
		// enabled; anything else is skipped.
		bits := 8 * cnt
		if bits >= 3 {
			id := b.u(2)
			bits -= 2
			if id == 0 {
				bits--
				if b.u(1) == 1 {
					return unsupportedf("HE-AACv2 parametric stereo with IPD/OPD is not decoded")
				}
			}
		}
		for range bits {
			b.u(1)
		}
	}
	if b.err != nil {
		return b.err
	}
	if b.pos()-start > bitsLeft {
		return errSBR
	}
	// Envelopes: fixed borders, the last border at the frame's end.
	if !p.enableIID {
		for e := range p.numEnv {
			p.iid[e] = [34]int{}
		}
	}
	if !p.enableICC {
		for e := range p.numEnv {
			p.icc[e] = [34]int{}
		}
	}
	if p.numEnv == 0 {
		// num_env 0: no new parameters, the last ones hold.
		p.hold()
		return nil
	}
	p.nIIDPrv, p.nICCPrv = nIID, nICC
	p.iidPrev, p.iccPrev = p.iid[p.numEnv-1], p.icc[p.numEnv-1]
	if frameClass == 0 {
		for e := range p.numEnv {
			p.border[e] = (e+1)*psSlots/p.numEnv - 1
		}
	} else if p.numEnv > 0 && p.border[p.numEnv-1] < psSlots-1 {
		p.iid[p.numEnv] = p.iid[p.numEnv-1]
		p.icc[p.numEnv] = p.icc[p.numEnv-1]
		p.border[p.numEnv] = psSlots - 1
		p.numEnv++
	}
	// expand 10 parameter bands to 20
	for e := range p.numEnv {
		if nIID == 10 {
			for i := 9; i >= 0; i-- {
				p.iid[e][2*i+1], p.iid[e][2*i] = p.iid[e][i], p.iid[e][i]
			}
		}
		if nICC == 10 {
			for i := 9; i >= 0; i-- {
				p.icc[e][2*i+1], p.icc[e][2*i] = p.icc[e][i], p.icc[e][i]
			}
		}
	}
	return nil
}

// hold makes the frame one envelope with the last parameters, for a frame
// without PS data or with num_env 0 (§8.6.4.5: the previous frame's
// parameters are used).
func (p *psDecoder) hold() {
	p.numEnv = 1
	p.border[0] = psSlots - 1
	p.iid[0] = p.iidPrev
	p.icc[0] = p.iccPrev
	if p.nIIDPrv == 10 {
		for i := 9; i >= 0; i-- {
			p.iid[0][2*i+1], p.iid[0][2*i] = p.iid[0][i], p.iid[0][i]
		}
	}
	if p.nICCPrv == 10 {
		for i := 9; i >= 0; i-- {
			p.icc[0][2*i+1], p.icc[0][2*i] = p.icc[0][i], p.icc[0][i]
		}
	}
}

func (p *psDecoder) prevIID(e, n int) []int {
	if e > 0 {
		return p.iid[e-1][:n]
	}
	return resample(p.iidPrev[:], p.nIIDPrv, n)
}

func (p *psDecoder) prevICC(e, n int) []int {
	if e > 0 {
		return p.icc[e-1][:n]
	}
	return resample(p.iccPrev[:], p.nICCPrv, n)
}

// resample maps the last envelope's parameters to n bands.
func resample(v []int, have, n int) []int {
	out := make([]int, n)
	if have == 0 {
		return out
	}
	for i := range out {
		out[i] = v[i*have/n]
	}
	return out
}

func (p *psDecoder) readPar(b *bitr, h *sbrHuffman, dt bool, cur, prev []int) error {
	for i := range cur {
		d, err := h.decode(b)
		if err != nil {
			return err
		}
		switch {
		case dt:
			cur[i] = prev[i] + d
		case i == 0:
			cur[i] = d
		default:
			cur[i] = cur[i-1] + d
		}
	}
	return nil
}

// apply turns a mono QMF matrix into left and right ones.
func (p *psDecoder) apply(x *[sbrSlots][64]complex128, ahead *[6][3]complex128, have bool) (l, r *[sbrSlots][64]complex128) {
	if !have || !p.haveHeader {
		p.hold()
	}
	// Hybrid analysis. The filters are centred: slot n's output takes
	// slots n-6 to n+6 of QMF bands 0 to 2, the last six of them from
	// ahead (the SBR matrix's slots 32 to 37, the next frame's first
	// six), so the hybrid bands line up with the QMF bands above them.
	var seq [psSlots + 12][3]complex128
	for n := range 6 {
		for q := range 3 {
			seq[n][q] = p.hist[q][n]
			seq[psSlots+6+n][q] = ahead[n][q]
		}
	}
	for n := range psSlots {
		for q := range 3 {
			seq[n+6][q] = x[n][q]
		}
	}
	for n := range 6 {
		for q := range 3 {
			p.hist[q][n] = x[psSlots-6+n][q]
		}
	}
	var s [psSlots][psBands20]complex128
	for n := range psSlots {
		for q := range 3 {
			var buf [13]complex128
			for i := range 13 {
				buf[i] = seq[n+i][q]
			}
			if q == 0 {
				var t [8]complex128
				for k := range 8 {
					var acc complex128
					for i := range 13 {
						acc += buf[12-i] * complex(psG8[i], 0) * psCis8[k][i]
					}
					t[k] = acc
				}
				s[n][0], s[n][1], s[n][2], s[n][3] = t[6], t[7], t[0], t[1]
				s[n][4], s[n][5] = t[2]+t[5], t[3]+t[4]
			} else {
				var lo, hi complex128
				for i := range 13 {
					v := buf[12-i] * complex(psG2[i], 0)
					lo += v
					if (i-6)&1 != 0 {
						hi -= v
					} else {
						hi += v
					}
				}
				if q == 1 {
					s[n][6], s[n][7] = hi, lo
				} else {
					s[n][8], s[n][9] = lo, hi
				}
			}
		}
		for q := 3; q < 64; q++ {
			s[n][q-3+psHybrid] = x[n][q]
		}
	}

	// Decorrelation.
	var d [psSlots][psBands20]complex128
	for n := range psSlots {
		var pow [20]float64
		for k := range psBands20 {
			v := s[n][k]
			pow[psKToI[k]] += real(v)*real(v) + imag(v)*imag(v)
		}
		var g [20]float64
		for i := range 20 {
			p.peak[i] = math.Max(psAlphaDecay*p.peak[i], pow[i])
			p.smoothP[i] = (1-psAlphaSmooth)*p.smoothP[i] + psAlphaSmooth*pow[i]
			p.smoothPD[i] = (1-psAlphaSmooth)*p.smoothPD[i] + psAlphaSmooth*(p.peak[i]-pow[i])
			if psGamma*p.smoothPD[i] > p.smoothP[i] {
				g[i] = p.smoothP[i] / (psGamma * p.smoothPD[i])
			} else {
				g[i] = 1
			}
		}
		for k := range psBands20 {
			in := s[n][k]
			var out complex128
			switch {
			case k < psAllpassBands:
				fc := psFCenter[k]
				phi := cis(-math.Pi * psQPhi * fc)
				w := p.delay2[0][k] * phi
				p.delay2[0][k], p.delay2[1][k] = p.delay2[1][k], in
				decay := 1.0
				if k >= psHybrid {
					q := k - psHybrid + 3
					if q > psDecayCutoff {
						decay = math.Max(0, 1-psDecaySlope*float64(q-psDecayCutoff))
					}
				}
				for m := range 3 {
					a := psAllpassA[m] * decay
					qf := cis(-math.Pi * psAllpassQ[m] * fc)
					dl := psAllpassDelay[m]
					sOld := p.apRead(m, k, dl)
					sv := w + complex(a, 0)*qf*sOld
					y := qf*sOld - complex(a, 0)*sv
					p.apWrite(m, k, sv)
					w = y
				}
				out = w
			case k < psShortDelay:
				out = p.longDel[(p.longPos)%14][k]
				p.longDel[p.longPos%14][k] = in
			default:
				out = p.delay2[1][k]
				p.delay2[1][k] = in
			}
			d[n][k] = out * complex(g[psKToI[k]], 0)
		}
		p.longPos = (p.longPos + 1) % 14
		for m := range 3 {
			p.apPos[m] = (p.apPos[m] + 1) % 5
		}
	}

	// Mixing, the coefficients interpolated from the last envelope's.
	var hl, hr [psSlots][psBands20]complex128
	prevB := -1
	for e := range p.numEnv {
		var h [4][20]float64
		for i := range 20 {
			var iidDB float64
			if p.iidMode >= 3 {
				iidDB = psIIDFine[p.iid[e][i]+15]
			} else {
				iidDB = psIIDCoarse[p.iid[e][i]+7]
			}
			c := math.Pow(10, iidDB/20)
			cl := math.Sqrt(2 * c * c / (1 + c*c))
			cr := math.Sqrt(2 / (1 + c*c))
			alpha := 0.5 * math.Acos(psICC[p.icc[e][i]])
			beta := alpha * (cr - cl) / math.Sqrt2
			h[0][i] = cl * math.Cos(beta+alpha)
			h[1][i] = cr * math.Cos(beta-alpha)
			h[2][i] = cl * math.Sin(beta+alpha)
			h[3][i] = cr * math.Sin(beta-alpha)
		}
		b := p.border[e]
		for n := prevB + 1; n <= b && n < psSlots; n++ {
			t := float64(n-prevB) / float64(b-prevB)
			for k := range psBands20 {
				i := psKToI[k]
				h11 := p.hPrev[0][i] + t*(h[0][i]-p.hPrev[0][i])
				h12 := p.hPrev[1][i] + t*(h[1][i]-p.hPrev[1][i])
				h21 := p.hPrev[2][i] + t*(h[2][i]-p.hPrev[2][i])
				h22 := p.hPrev[3][i] + t*(h[3][i]-p.hPrev[3][i])
				hl[n][k] = complex(h11, 0)*s[n][k] + complex(h21, 0)*d[n][k]
				hr[n][k] = complex(h12, 0)*s[n][k] + complex(h22, 0)*d[n][k]
			}
		}
		p.hPrev = h
		prevB = b
	}

	// Hybrid synthesis.
	l, r = new([sbrSlots][64]complex128), new([sbrSlots][64]complex128)
	for n := range psSlots {
		for _, o := range []struct {
			src *[psBands20]complex128
			dst *[64]complex128
		}{{&hl[n], &l[n]}, {&hr[n], &r[n]}} {
			o.dst[0] = o.src[0] + o.src[1] + o.src[2] + o.src[3] + o.src[4] + o.src[5]
			o.dst[1] = o.src[6] + o.src[7]
			o.dst[2] = o.src[8] + o.src[9]
			for q := 3; q < 64; q++ {
				o.dst[q] = o.src[q-3+psHybrid]
			}
		}
	}
	return l, r
}

// The all-pass links' delay lines, 5 slots each.
func (p *psDecoder) apRead(m, k, dl int) complex128 {
	return p.apBuf[m][k][(p.apPos[m]+5-dl)%5]
}

func (p *psDecoder) apWrite(m, k int, v complex128) {
	p.apBuf[m][k][p.apPos[m]] = v
}

func cis(a float64) complex128 { return complex(math.Cos(a), math.Sin(a)) }
