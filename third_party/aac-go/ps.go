package aac

// Parametric Stereo (HE-AAC v2) — ISO/IEC 14496-3 §8.6.4 and Annex 8.A.
// Written for CloudBurrow from the standard (#1739, #1754): ps_data()
// (Table 8.1) and ps_extension() (IPD/OPD), delta decoding (§8.6.4.5),
// the hybrid filterbank in its 20- and 34-band configurations (§8.6.4.3),
// decorrelation with transient detection (§8.6.4.6), and mixing procedures
// A and B, with IPD/OPD phase smoothing and time interpolation
// (§8.6.4.6.3). Tables are in ps_tables.go.

import (
	"math"
)

const (
	psSlots   = sbrSlots
	psMaxPar  = 34
	psMaxBand = 91 // 32 hybrid sub-subbands and QMF bands 5 to 63
	psMaxEnv  = 5  // four envelopes, and one more to the frame's end
	psAPLen   = 5  // the longest all-pass link delay
)

var (
	psIIDCoarse = [15]float64{-25, -18, -14, -10, -7, -4, -2, 0, 2, 4, 7, 10, 14, 18, 25}
	psIIDFine   = [31]float64{-50, -45, -40, -35, -30, -25, -22, -19, -16, -13, -10, -8, -6, -4, -2, 0,
		2, 4, 6, 8, 10, 13, 16, 19, 22, 25, 30, 35, 40, 45, 50}
	psICC = [8]float64{1, 0.937, 0.84118, 0.60092, 0.36764, 0, -0.589, -1}

	psAllpassDelay = [3]int{3, 4, 5}
	psAllpassA     = [3]float64{0.65143905753106, 0.56471812200776, 0.48954165955695}
	psAllpassQ     = [3]float64{0.43, 0.75, 0.347}
)

const (
	psQPhi        = 0.39
	psAlphaDecay  = 0.76592833836465
	psAlphaSmooth = 0.25
	psGamma       = 1.5
	psDecaySlope  = 0.05
)

// psConfig is one of the two hybrid filterbank configurations.
type psConfig struct {
	nPar      int // stereo parameter bands
	nHybrid   int // hybrid sub-subbands
	qmfSplit  int // QMF bands split by the hybrid filters
	nBands    int // hybrid sub-subbands and the QMF bands above them
	kToI      [psMaxBand]int
	fCenter   [psMaxBand]float64
	allpass   int   // bands below this are all-pass filtered
	short     int   // from here, a delay of one slot instead of 14
	decayCut  int   // the QMF band above which the all-pass decays
	split     []int // per split QMF band: its filter's band count
	synthFrom []int // per split QMF band: its first hybrid sub-subband
}

var ps20, ps34 psConfig

// psCis[Q][k][i] is the Q-band filters' modulation,
// exp(i 2pi/Q (k+1/2)(i-6)).
var psCis = map[int][][13]complex128{}

func init() {
	for _, q := range []int{4, 8, 12} {
		m := make([][13]complex128, q)
		for k := range q {
			for i := range 13 {
				m[k][i] = cis(2 * math.Pi / float64(q) * (float64(k) + 0.5) * float64(i-6))
			}
		}
		psCis[q] = m
	}

	// 20 bands: QMF band 0 into 8 (6 kept, two pairs merged), 1 and 2
	// into 2 each.
	c := &ps20
	c.nPar, c.nHybrid, c.qmfSplit, c.nBands = 20, 10, 3, 71
	c.allpass, c.short, c.decayCut = 30, 42, 3
	copy(c.kToI[:], []int{1, 0, 0, 1, 2, 3, 4, 5, 6, 7})
	for i, f := range []float64{-3, -1, 1, 3, 5, 7, 10, 14, 18, 22} {
		c.fCenter[i] = f / 8
	}
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
		c.kToI[q-3+c.nHybrid] = i
		c.fCenter[q-3+c.nHybrid] = float64(q) + 0.5
	}

	// 34 bands: QMF band 0 into 12, 1 into 8, 2 to 4 into 4 each.
	c = &ps34
	c.nPar, c.nHybrid, c.qmfSplit, c.nBands = 34, 32, 5, 91
	c.allpass, c.short, c.decayCut = 50, 62, 5
	c.split = []int{12, 8, 4, 4, 4}
	c.synthFrom = []int{0, 12, 20, 24, 28}
	copy(c.kToI[:], psKToI34Hybrid[:])
	copy(c.fCenter[:], psFCenter34Hybrid[:])
	for q := 5; q < 64; q++ {
		c.kToI[q-5+c.nHybrid] = psQMFToI34(q)
		c.fCenter[q-5+c.nHybrid] = float64(q) + 0.5
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
	numEnv    int
	border    [psMaxEnv + 1]int
	iid       [psMaxEnv + 1][psMaxPar]int
	icc       [psMaxEnv + 1][psMaxPar]int
	ipd       [psMaxEnv + 1][psMaxPar]int
	opd       [psMaxEnv + 1][psMaxPar]int
	ipdopd    bool // this frame's envelopes carry IPD/OPD
	nIIDFrame int  // this frame's parameter band counts
	nICCFrame int
	nIPDFrame int

	// the last envelope's parameters, for time-differential coding and
	// for frames without new ones
	iidPrev  [psMaxPar]int
	iccPrev  [psMaxPar]int
	ipdPrev  [psMaxPar]int
	opdPrev  [psMaxPar]int
	nIIDPrv  int
	nICCPrv  int
	nIPDPrv  int
	ipdPrvOn bool

	// IPD/OPD smoothing: the phases of the two envelopes before
	ipdHist [2][psMaxPar]int
	opdHist [2][psMaxPar]int

	is34   bool
	cfg    *psConfig
	inited bool

	// hybrid analysis history: QMF bands 0 to 4, the last 6 slots
	hist [5][6]complex128

	// decorrelation state
	delay2   [2][psMaxBand]complex128 // the z^-2 before the all-pass
	apBuf    [3][psMaxBand][psAPLen]complex128
	apPos    [3]int
	longDel  [14][psMaxBand]complex128
	longPos  int
	peak     [psMaxPar]float64
	smoothP  [psMaxPar]float64
	smoothPD [psMaxPar]float64

	// mixing: the last envelope's coefficients (real and imaginary)
	hPrev [4][psMaxPar]complex128
}

func newPSDecoder() *psDecoder {
	return &psDecoder{cfg: &ps20}
}

var psNumEnv = [2][4]int{{0, 1, 2, 4}, {1, 2, 3, 4}}

// psParBands is the number of stereo bands for a mode's iid_mode or
// icc_mode, and psIPDBands the number of IPD/OPD bands (Table 8.?).
func psParBands(mode int) int { return [3]int{10, 20, 34}[mode%3] }
func psIPDBands(mode int) int { return [3]int{5, 11, 17}[mode%3] }

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
	if (p.enableIID && p.iidMode > 5) || (p.enableICC && p.iccMode > 5) {
		return unsupportedf("PS iid_mode %d, icc_mode %d", p.iidMode, p.iccMode)
	}
	frameClass := b.u(1)
	numEnv := psNumEnv[frameClass][b.u(2)]
	if frameClass == 1 {
		for e := range numEnv {
			p.border[e] = b.u(5)
		}
	}
	nIID := psParBands(p.iidMode)
	nICC := psParBands(p.iccMode)
	nIPD := psIPDBands(p.iidMode)
	fine := p.iidMode >= 3
	if p.enableIID {
		for e := range numEnv {
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
			if err := readPar(b, h, dt, p.iid[e][:nIID], p.prevPar(p.iid[:], &p.iidPrev, p.nIIDPrv, e, nIID), 0); err != nil {
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
		for e := range numEnv {
			dt := b.u(1) == 1
			h := &huffPSICCDf
			if dt {
				h = &huffPSICCDt
			}
			if err := readPar(b, h, dt, p.icc[e][:nICC], p.prevPar(p.icc[:], &p.iccPrev, p.nICCPrv, e, nICC), 0); err != nil {
				return err
			}
			for i := range nICC {
				if p.icc[e][i] < 0 || p.icc[e][i] > 7 {
					return errSBR
				}
			}
		}
	}
	p.ipdopd = false
	if p.enableExt {
		cnt := b.u(4)
		if cnt == 15 {
			cnt += b.u(8)
		}
		// ps_extension(): extension id 0 carries IPD/OPD; anything
		// else is skipped.
		left := 8 * cnt
		for left > 7 && b.err == nil {
			extStart := b.pos()
			id := b.u(2)
			if id == 0 {
				if b.u(1) == 1 {
					p.ipdopd = true
					for e := range numEnv {
						dt := b.u(1) == 1
						h := &huffPSIPDDf
						if dt {
							h = &huffPSIPDDt
						}
						if err := readPar(b, h, dt, p.ipd[e][:nIPD], p.prevIPD(p.ipd[:], &p.ipdPrev, e, nIPD), 8); err != nil {
							return err
						}
						dt = b.u(1) == 1
						h = &huffPSOPDDf
						if dt {
							h = &huffPSOPDDt
						}
						if err := readPar(b, h, dt, p.opd[e][:nIPD], p.prevIPD(p.opd[:], &p.opdPrev, e, nIPD), 8); err != nil {
							return err
						}
					}
				}
				b.u(1) // reserved_ps
			}
			used := b.pos() - extStart
			if used > left {
				return errSBR
			}
			// The rest of the extension data is fill.
			for range left - used {
				b.u(1)
			}
			left = 0
		}
	}
	if b.err != nil {
		return b.err
	}
	if b.pos()-start > bitsLeft {
		return errSBR
	}
	if !p.enableIID {
		for e := range numEnv {
			p.iid[e] = [psMaxPar]int{}
		}
		nIID = 10
	}
	if !p.enableICC {
		for e := range numEnv {
			p.icc[e] = [psMaxPar]int{}
		}
		nICC = 10
	}
	if !p.ipdopd {
		for e := range numEnv {
			p.ipd[e] = [psMaxPar]int{}
			p.opd[e] = [psMaxPar]int{}
		}
	}
	if numEnv == 0 {
		// num_env 0: no new parameters, the last ones hold.
		p.numEnv = 0
		p.hold()
		return nil
	}
	p.numEnv = numEnv
	p.nIIDFrame, p.nICCFrame, p.nIPDFrame = nIID, nICC, nIPD
	p.nIIDPrv, p.nICCPrv, p.nIPDPrv = nIID, nICC, nIPD
	p.iidPrev, p.iccPrev = p.iid[numEnv-1], p.icc[numEnv-1]
	p.ipdPrev, p.opdPrev = p.ipd[numEnv-1], p.opd[numEnv-1]
	p.ipdPrvOn = p.ipdopd
	if frameClass == 0 {
		for e := range numEnv {
			p.border[e] = (e+1)*psSlots/numEnv - 1
		}
	} else if p.border[numEnv-1] < psSlots-1 {
		p.iid[numEnv] = p.iid[numEnv-1]
		p.icc[numEnv] = p.icc[numEnv-1]
		p.ipd[numEnv] = p.ipd[numEnv-1]
		p.opd[numEnv] = p.opd[numEnv-1]
		p.border[numEnv] = psSlots - 1
		p.numEnv++
	}
	return nil
}

// hold makes the frame one envelope with the last parameters, for a frame
// without PS data or with num_env 0 (§8.6.4.5: the previous frame's
// parameters are used).
func (p *psDecoder) hold() {
	p.numEnv = 1
	p.border[0] = psSlots - 1
	p.iid[0], p.icc[0] = p.iidPrev, p.iccPrev
	p.ipd[0], p.opd[0] = p.ipdPrev, p.opdPrev
	p.ipdopd = p.ipdPrvOn
	p.nIIDFrame, p.nICCFrame, p.nIPDFrame = p.nIIDPrv, p.nICCPrv, p.nIPDPrv
	if p.nIIDFrame == 0 {
		p.nIIDFrame, p.nICCFrame, p.nIPDFrame = 10, 10, 5
	}
}

// prevPar is the reference for time-differential coding of envelope e:
// the envelope before, or the last frame's last one, mapped to n bands.
func (p *psDecoder) prevPar(cur [][psMaxPar]int, last *[psMaxPar]int, have, e, n int) []int {
	if e > 0 {
		return cur[e-1][:n]
	}
	return resample(last[:], have, n)
}

func (p *psDecoder) prevIPD(cur [][psMaxPar]int, last *[psMaxPar]int, e, n int) []int {
	if e > 0 {
		return cur[e-1][:n]
	}
	if !p.ipdPrvOn {
		return make([]int, n)
	}
	return resample(last[:], p.nIPDPrv, n)
}

// resample maps the last envelope's parameters to n bands.
func resample(v []int, have, n int) []int {
	out := make([]int, n)
	if have == 0 {
		return out
	}
	if have == n {
		copy(out, v)
		return out
	}
	for i := range out {
		out[i] = v[i*have/n]
	}
	return out
}

// readPar decodes one envelope's parameters, delta coded in frequency or
// time; modulo, when not 0, wraps them (IPD and OPD, modulo 8).
func readPar(b *bitr, h *sbrHuffman, dt bool, cur, prev []int, modulo int) error {
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
		if modulo != 0 {
			cur[i] &= modulo - 1
		}
	}
	return nil
}

// mapPar maps n parameter-band values (10, 20 or 34) to the
// configuration's: 10 to 20 by repetition, 10 or 20 to 34 by Table 8.?'s
// map, averaging where a 34-band band straddles two 20-band ones
// (average false for IPD/OPD, which take the lower band).
func mapPar(v []int, n int, is34, average bool) [psMaxPar]int {
	var out [psMaxPar]int
	if n == 34 {
		if is34 {
			copy(out[:], v[:34])
			return out
		}
		// 34 bands in a 20-band configuration does not occur: the
		// configuration is 34 bands whenever either parameter is.
		copy(out[:], v[:20])
		return out
	}
	var p20 [20]int
	if n == 10 {
		for i := range 10 {
			p20[2*i], p20[2*i+1] = v[i], v[i]
		}
	} else if n == 11 || n == 5 || n == 17 {
		// IPD/OPD band counts: 5 of 10, 11 of 20, 17 of 34.
		switch n {
		case 5:
			for i := range 5 {
				p20[2*i], p20[2*i+1] = v[i], v[i]
			}
		case 11:
			copy(p20[:11], v[:11])
		case 17:
			copy(out[:17], v[:17])
			return out
		}
	} else {
		copy(p20[:], v[:20])
	}
	if !is34 {
		copy(out[:20], p20[:])
		return out
	}
	for i, j := range psMap20To34 {
		out[i] = p20[j]
	}
	if average {
		out[1] = (p20[0] + p20[1]) / 2
		out[4] = (p20[2] + p20[3]) / 2
	}
	return out
}

// switchConfig moves the decoder to the 20- or 34-band configuration,
// clearing the decorrelator and mapping the last mixing coefficients.
func (p *psDecoder) switchConfig(is34 bool) {
	if p.inited && is34 == p.is34 {
		return
	}
	if p.inited {
		var h [4][psMaxPar]complex128
		for m := range 4 {
			if is34 {
				for i, j := range psMap20To34 {
					h[m][i] = p.hPrev[m][j]
				}
			} else {
				for i, j := range psMap34To20 {
					h[m][i] = p.hPrev[m][j]
				}
			}
		}
		p.hPrev = h
		p.delay2 = [2][psMaxBand]complex128{}
		p.apBuf = [3][psMaxBand][psAPLen]complex128{}
		p.longDel = [14][psMaxBand]complex128{}
		p.peak = [psMaxPar]float64{}
		p.smoothP = [psMaxPar]float64{}
		p.smoothPD = [psMaxPar]float64{}
	}
	p.inited = true
	p.is34 = is34
	p.cfg = &ps20
	if is34 {
		p.cfg = &ps34
	}
}

// hybridAnalysis splits QMF slot n's low bands into the configuration's
// sub-subbands; buf[q] holds QMF band q's slots n-6 to n+6.
func (c *psConfig) hybridAnalysis(buf *[5][13]complex128, s []complex128) {
	if c.nPar == 20 {
		var t [8]complex128
		for k := range 8 {
			var acc complex128
			for i := range 13 {
				acc += buf[0][12-i] * complex(psG8[i], 0) * psCis[8][k][i]
			}
			t[k] = acc
		}
		s[0], s[1], s[2], s[3] = t[6], t[7], t[0], t[1]
		s[4], s[5] = t[2]+t[5], t[3]+t[4]
		for q := 1; q < 3; q++ {
			var lo, hi complex128
			for i := range 13 {
				v := buf[q][12-i] * complex(psG2[i], 0)
				lo += v
				if (i-6)&1 != 0 {
					hi -= v
				} else {
					hi += v
				}
			}
			if q == 1 {
				s[6], s[7] = hi, lo
			} else {
				s[8], s[9] = lo, hi
			}
		}
		return
	}
	for q, nq := range c.split {
		g := &psG4
		switch nq {
		case 12:
			g = &psG12
		case 8:
			g = &psG8In34
		}
		m := psCis[nq]
		for k := range nq {
			var acc complex128
			for i := range 13 {
				acc += buf[q][12-i] * complex(g[i], 0) * m[k][i]
			}
			s[c.synthFrom[q]+k] = acc
		}
	}
}

// hybridSynthesis sums the sub-subbands back into QMF bands.
func (c *psConfig) hybridSynthesis(src []complex128, dst *[64]complex128) {
	if c.nPar == 20 {
		dst[0] = src[0] + src[1] + src[2] + src[3] + src[4] + src[5]
		dst[1] = src[6] + src[7]
		dst[2] = src[8] + src[9]
	} else {
		for q, nq := range c.split {
			var acc complex128
			for k := range nq {
				acc += src[c.synthFrom[q]+k]
			}
			dst[q] = acc
		}
	}
	for q := c.qmfSplit; q < 64; q++ {
		dst[q] = src[q-c.qmfSplit+c.nHybrid]
	}
}

// mixing returns envelope e's mixing coefficients h11, h12, h21, h22 per
// parameter band (complex with IPD/OPD).
func (p *psDecoder) mixing(e int) [4][psMaxPar]complex128 {
	c := p.cfg
	iid := mapPar(p.iid[e][:], p.nIIDFrame, p.is34, true)
	icc := mapPar(p.icc[e][:], p.nICCFrame, p.is34, true)
	var h [4][psMaxPar]complex128
	for i := range c.nPar {
		var iidDB float64
		if p.iidMode >= 3 {
			iidDB = psIIDFine[iid[i]+15]
		} else {
			iidDB = psIIDCoarse[iid[i]+7]
		}
		cc := math.Pow(10, iidDB/20)
		rho := psICC[icc[i]]
		var h11, h12, h21, h22 float64
		if p.iccMode < 3 {
			// Mixing procedure A.
			cl := math.Sqrt(2 * cc * cc / (1 + cc*cc))
			cr := math.Sqrt(2 / (1 + cc*cc))
			alpha := 0.5 * math.Acos(rho)
			beta := alpha * (cr - cl) / math.Sqrt2
			h11 = cl * math.Cos(beta+alpha)
			h12 = cr * math.Cos(beta-alpha)
			h21 = cl * math.Sin(beta+alpha)
			h22 = cr * math.Sin(beta-alpha)
		} else {
			// Mixing procedure B: the principal axis's angle alpha and
			// the eigenvalue split gamma of the channels' covariance
			// (c^2 : 1, correlation rho), with rho at least 0.05.
			rho = math.Max(rho, 0.05)
			alpha := 0.5 * math.Atan2(2*cc*rho, cc*cc-1)
			mu := cc + 1/cc
			mu = math.Sqrt(1 + (4*rho*rho-4)/(mu*mu))
			gamma := math.Atan(math.Sqrt((1 - mu) / (1 + mu)))
			h11 = math.Sqrt2 * math.Cos(alpha) * math.Cos(gamma)
			h12 = math.Sqrt2 * math.Sin(alpha) * math.Cos(gamma)
			h21 = -math.Sqrt2 * math.Sin(alpha) * math.Sin(gamma)
			h22 = math.Sqrt2 * math.Cos(alpha) * math.Sin(gamma)
		}
		h[0][i], h[1][i] = complex(h11, 0), complex(h12, 0)
		h[2][i], h[3][i] = complex(h21, 0), complex(h22, 0)
	}
	if p.ipdopd {
		ipd := mapPar(p.ipd[e][:], p.nIPDFrame, p.is34, false)
		opd := mapPar(p.opd[e][:], p.nIPDFrame, p.is34, false)
		nIPD := psIPDBands(p.iidMode)
		if p.is34 {
			nIPD = 17
		} else if nIPD < 11 {
			nIPD = 11
		}
		for i := range nIPD {
			// Smoothing over this envelope's and the two before's
			// phases, weighted 1, 1/2 and 1/4.
			ipdS := psSmoothPhase(ipd[i], p.ipdHist[0][i], p.ipdHist[1][i])
			opdS := psSmoothPhase(opd[i], p.opdHist[0][i], p.opdHist[1][i])
			p.ipdHist[1][i], p.ipdHist[0][i] = p.ipdHist[0][i], ipd[i]
			p.opdHist[1][i], p.opdHist[0][i] = p.opdHist[0][i], opd[i]
			phi1 := opdS
			phi2 := opdS - ipdS
			h[0][i] *= cis(phi1)
			h[2][i] *= cis(phi1)
			h[1][i] *= cis(phi2)
			h[3][i] *= cis(phi2)
		}
	}
	return h
}

// psSmoothPhase is the angle of e^{j a} + e^{j b}/2 + e^{j c}/4, the
// indices in steps of pi/4.
func psSmoothPhase(a, b, c int) float64 {
	re := math.Cos(float64(a)*math.Pi/4) + 0.5*math.Cos(float64(b)*math.Pi/4) + 0.25*math.Cos(float64(c)*math.Pi/4)
	im := math.Sin(float64(a)*math.Pi/4) + 0.5*math.Sin(float64(b)*math.Pi/4) + 0.25*math.Sin(float64(c)*math.Pi/4)
	return math.Atan2(im, re)
}

// apply turns a mono QMF matrix into left and right ones.
func (p *psDecoder) apply(x *[sbrSlots][64]complex128, ahead *[6][5]complex128, have bool) (l, r *[sbrSlots][64]complex128) {
	if !have || !p.haveHeader {
		p.hold()
	}
	is34 := (p.enableIID && p.iidMode%3 == 2) || (p.enableICC && p.iccMode%3 == 2)
	p.switchConfig(is34)
	c := p.cfg

	// Hybrid analysis. The filters are centred: slot n's output takes
	// slots n-6 to n+6 of the low QMF bands, the last six of them from
	// ahead (the SBR matrix's slots 32 to 37, the next frame's first
	// six), so the hybrid bands line up with the QMF bands above them.
	var seq [psSlots + 12][5]complex128
	for n := range 6 {
		for q := range 5 {
			seq[n][q] = p.hist[q][n]
			seq[psSlots+6+n][q] = ahead[n][q]
		}
	}
	for n := range psSlots {
		for q := range 5 {
			seq[n+6][q] = x[n][q]
		}
	}
	for n := range 6 {
		for q := range 5 {
			p.hist[q][n] = x[psSlots-6+n][q]
		}
	}
	nb := c.nBands
	var s [psSlots][psMaxBand]complex128
	for n := range psSlots {
		var buf [5][13]complex128
		for q := range c.qmfSplit {
			for i := range 13 {
				buf[q][i] = seq[n+i][q]
			}
		}
		c.hybridAnalysis(&buf, s[n][:])
		for q := c.qmfSplit; q < 64; q++ {
			s[n][q-c.qmfSplit+c.nHybrid] = x[n][q]
		}
	}

	// Decorrelation.
	var d [psSlots][psMaxBand]complex128
	for n := range psSlots {
		var pow [psMaxPar]float64
		for k := range nb {
			v := s[n][k]
			pow[c.kToI[k]] += real(v)*real(v) + imag(v)*imag(v)
		}
		var g [psMaxPar]float64
		for i := range c.nPar {
			p.peak[i] = math.Max(psAlphaDecay*p.peak[i], pow[i])
			p.smoothP[i] = (1-psAlphaSmooth)*p.smoothP[i] + psAlphaSmooth*pow[i]
			p.smoothPD[i] = (1-psAlphaSmooth)*p.smoothPD[i] + psAlphaSmooth*(p.peak[i]-pow[i])
			if psGamma*p.smoothPD[i] > p.smoothP[i] {
				g[i] = p.smoothP[i] / (psGamma * p.smoothPD[i])
			} else {
				g[i] = 1
			}
		}
		for k := range nb {
			in := s[n][k]
			var out complex128
			switch {
			case k < c.allpass:
				fc := c.fCenter[k]
				phi := cis(-math.Pi * psQPhi * fc)
				w := p.delay2[0][k] * phi
				p.delay2[0][k], p.delay2[1][k] = p.delay2[1][k], in
				decay := 1.0
				if k >= c.nHybrid {
					q := k - c.nHybrid + c.qmfSplit
					if q > c.decayCut {
						decay = math.Max(0, 1-psDecaySlope*float64(q-c.decayCut))
					}
				}
				for m := range 3 {
					a := psAllpassA[m] * decay
					qf := cis(-math.Pi * psAllpassQ[m] * fc)
					sOld := p.apRead(m, k, psAllpassDelay[m])
					sv := w + complex(a, 0)*qf*sOld
					y := qf*sOld - complex(a, 0)*sv
					p.apWrite(m, k, sv)
					w = y
				}
				out = w
			case k < c.short:
				out = p.longDel[p.longPos%14][k]
				p.longDel[p.longPos%14][k] = in
			default:
				out = p.delay2[1][k]
				p.delay2[1][k] = in
			}
			d[n][k] = out * complex(g[c.kToI[k]], 0)
		}
		p.longPos = (p.longPos + 1) % 14
		for m := range 3 {
			p.apPos[m] = (p.apPos[m] + 1) % psAPLen
		}
	}

	// Mixing, the coefficients interpolated from the last envelope's.
	var hl, hr [psSlots][psMaxBand]complex128
	prevB := -1
	for e := range p.numEnv {
		h := p.mixing(e)
		b := p.border[e]
		for n := prevB + 1; n <= b && n < psSlots; n++ {
			t := complex(float64(n-prevB)/float64(b-prevB), 0)
			for k := range nb {
				i := c.kToI[k]
				h11 := p.hPrev[0][i] + t*(h[0][i]-p.hPrev[0][i])
				h12 := p.hPrev[1][i] + t*(h[1][i]-p.hPrev[1][i])
				h21 := p.hPrev[2][i] + t*(h[2][i]-p.hPrev[2][i])
				h22 := p.hPrev[3][i] + t*(h[3][i]-p.hPrev[3][i])
				if c.fCenter[k] < 0 {
					// Negative frequencies take the conjugate phase.
					h11, h12, h21, h22 = conj(h11), conj(h12), conj(h21), conj(h22)
				}
				hl[n][k] = h11*s[n][k] + h21*d[n][k]
				hr[n][k] = h12*s[n][k] + h22*d[n][k]
			}
		}
		p.hPrev = h
		prevB = b
	}

	// Hybrid synthesis.
	l, r = new([sbrSlots][64]complex128), new([sbrSlots][64]complex128)
	for n := range psSlots {
		c.hybridSynthesis(hl[n][:nb], &l[n])
		c.hybridSynthesis(hr[n][:nb], &r[n])
	}
	return l, r
}

// The all-pass links' delay lines, 5 slots each.
func (p *psDecoder) apRead(m, k, dl int) complex128 {
	return p.apBuf[m][k][(p.apPos[m]+psAPLen-dl)%psAPLen]
}

func (p *psDecoder) apWrite(m, k int, v complex128) {
	p.apBuf[m][k][p.apPos[m]] = v
}

func cis(a float64) complex128 { return complex(math.Cos(a), math.Sin(a)) }

func conj(v complex128) complex128 { return complex(real(v), -imag(v)) }
