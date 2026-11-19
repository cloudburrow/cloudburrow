package aac

import "math"

// The SBR signal path for one channel — ISO/IEC 14496-3 §4.6.18.4 to
// §4.6.18.8.

var (
	anaCos, anaSin [32][64]float64  // 2·exp(iπ/64·(k+½)(2n−½))
	synCos, synSin [128][64]float64 // exp(iπ/128·(k+½)(2n−255))/64
	dsCos, dsSin   [64][32]float64  // exp(iπ/128·(k+½)(4n−255))/64
)

func init() {
	for k := range 32 {
		for n := range 64 {
			a := math.Pi / 64 * (float64(k) + 0.5) * (2*float64(n) - 0.5)
			anaCos[k][n], anaSin[k][n] = 2*math.Cos(a), 2*math.Sin(a)
		}
	}
	for n := range 128 {
		for k := range 64 {
			a := math.Pi / 128 * (float64(k) + 0.5) * (2*float64(n) - 255)
			synCos[n][k], synSin[n][k] = math.Cos(a)/64, math.Sin(a)/64
		}
	}
	for n := range 64 {
		for k := range 32 {
			// The 64-band bank's modulation at every other sample.
			a := math.Pi / 128 * (float64(k) + 0.5) * (4*float64(n) - 255)
			dsCos[n][k], dsSin[n][k] = math.Cos(a)/64, math.Sin(a)/64
		}
	}
}

// analysis runs the 32-band analysis QMF bank over 1024 core samples into
// X_Low's slots 8 to 39, after moving the last 8 slots to the front.
func (c *sbrChannel) analysis(in []float64) {
	copy(c.xLow[:sbrHFGen], c.xLow[sbrSlots:])
	var u [64]float64
	for l := range sbrSlots {
		copy(c.anaX[32:], c.anaX[:288])
		for n := range 32 {
			c.anaX[31-n] = in[32*l+n]
		}
		for n := range 64 {
			var s float64
			for j := range 5 {
				s += c.anaX[n+64*j] * sbrQMFWindow[2*(n+64*j)]
			}
			u[n] = s
		}
		row := &c.xLow[l+sbrHFGen]
		for k := range 32 {
			var re, im float64
			cs, sn := &anaCos[k], &anaSin[k]
			for n := range 64 {
				re += u[n] * cs[n]
				im += u[n] * sn[n]
			}
			row[k] = complex(re, im)
		}
	}
}

// synthesis runs the 64-band synthesis QMF bank over 32 slots of X to 2048
// samples.
func (c *sbrChannel) synthesis(x *[sbrSlots][64]complex128, out []float64) {
	var w [640]float64
	for l := range sbrSlots {
		copy(c.synV[128:], c.synV[:1152])
		xs := &x[l]
		for n := range 128 {
			var s float64
			cs, sn := &synCos[n], &synSin[n]
			for k := range 64 {
				s += real(xs[k])*cs[k] - imag(xs[k])*sn[k]
			}
			c.synV[n] = s
		}
		for i := range 5 {
			for j := range 64 {
				w[128*i+j] = c.synV[256*i+j] * sbrQMFWindow[128*i+j]
				w[128*i+64+j] = c.synV[256*i+192+j] * sbrQMFWindow[128*i+64+j]
			}
		}
		for j := range 64 {
			var s float64
			for i := range 10 {
				s += w[64*i+j]
			}
			out[64*l+j] = s
		}
	}
}

// synthesis32 runs the downsampled (32-band) synthesis QMF bank over
// bands 0 to 31 of 32 slots of X to 1024 samples, the window being every
// other coefficient of c.
func (c *sbrChannel) synthesis32(x *[sbrSlots][64]complex128, out []float64) {
	var w [320]float64
	for l := range sbrSlots {
		copy(c.synV32[64:], c.synV32[:576])
		xs := &x[l]
		for n := range 64 {
			var s float64
			cs, sn := &dsCos[n], &dsSin[n]
			for k := range 32 {
				s += real(xs[k])*cs[k] - imag(xs[k])*sn[k]
			}
			c.synV32[n] = s
		}
		for i := range 5 {
			for j := range 32 {
				w[64*i+j] = c.synV32[128*i+j] * sbrQMFWindow[2*(64*i+j)]
				w[64*i+32+j] = c.synV32[128*i+96+j] * sbrQMFWindow[2*(64*i+32+j)]
			}
		}
		for j := range 32 {
			var s float64
			for i := range 10 {
				s += w[32*i+j]
			}
			out[32*l+j] = s
		}
	}
}

// process runs SBR on one channel's 1024 core samples and returns the QMF
// matrix X to synthesise (§4.6.18.8). With no SBR data for this frame, the
// high band carried over from the last frame is kept and the rest is empty.
func (e *sbrElement) process(ch int, in []float64, haveData bool) *[sbrSlots][64]complex128 {
	c := &e.ch[ch]
	c.analysis(in)
	x := new([sbrSlots][64]complex128)
	kx := 32
	if e.tablesOK {
		kx = e.kx
	}
	if e.tablesOK {
		if haveData {
			e.dequant(ch)
			e.hfGenerate(c)
			e.hfAdjust(c)
		} else {
			var y [len(c.yBuf)][64]complex128
			copy(y[:], c.yPrev[sbrSlots:])
			c.yBuf, c.yPrev = y, y
		}
	}
	for l := range sbrSlots {
		for k := range kx {
			x[l][k] = c.xLow[l+sbrHFAdj][k]
		}
		if e.tablesOK {
			for k := e.kx; k < e.kx+e.m; k++ {
				x[l][k] = c.yBuf[l+sbrHFAdj][k]
			}
		}
	}
	return x
}

// ahead returns QMF bands 0 to 2 of the six slots after the SBR matrix
// process returned (its slots 32 to 37), which parametric stereo's hybrid
// filters look ahead to.
func (c *sbrChannel) ahead() *[6][5]complex128 {
	var a [6][5]complex128
	for n := range 6 {
		copy(a[n][:], c.xLow[sbrSlots+sbrHFAdj+n][:5])
	}
	return &a
}

// dequant turns the coded envelope and noise floor into energies —
// §4.6.18.3.5.
func (e *sbrElement) dequant(ch int) {
	c := &e.ch[ch]
	a := 1.0
	if c.ampRes == 0 {
		a = 0.5
	}
	if e.stereo && e.coupling {
		c0, c1 := &e.ch[0], &e.ch[1]
		panOffset := 12
		if c0.ampRes == 0 {
			panOffset = 24
		}
		for l := range c0.numEnv {
			n := e.numBands(c0.freqRes[l])
			for k := range n {
				lev := 64 * math.Pow(2, a*float64(c0.env[l][k])+1)
				pan := a * float64(c1.env[l][k]-panOffset)
				if ch == 0 {
					c.eOrig[l][k] = lev / (1 + math.Pow(2, -pan))
				} else {
					c.eOrig[l][k] = lev / (1 + math.Pow(2, pan))
				}
			}
		}
		for l := range c0.numNoise {
			for k := range e.nQ {
				lev := math.Pow(2, float64(6-c0.noise[l][k]+1))
				pan := float64(c1.noise[l][k] - 12)
				if ch == 0 {
					c.qOrig[l][k] = lev / (1 + math.Pow(2, -pan))
				} else {
					c.qOrig[l][k] = lev / (1 + math.Pow(2, pan))
				}
			}
		}
		return
	}
	for l := range c.numEnv {
		for k := range e.numBands(c.freqRes[l]) {
			c.eOrig[l][k] = 64 * math.Pow(2, a*float64(c.env[l][k]))
		}
	}
	for l := range c.numNoise {
		for k := range e.nQ {
			c.qOrig[l][k] = math.Pow(2, float64(6-c.noise[l][k]))
		}
	}
}

// hfGenerate fills the high band from the low band — §4.6.18.6.
func (e *sbrElement) hfGenerate(c *sbrChannel) {
	// chirp factors per noise band
	var bw [5]float64
	for i := range e.nQ {
		var t float64
		switch c.invf[i] {
		case 0:
			if c.invfPrev[i] == 1 {
				t = 0.6
			}
		case 1:
			if c.invfPrev[i] == 0 {
				t = 0.6
			} else {
				t = 0.75
			}
		case 2:
			t = 0.9
		case 3:
			t = 0.98
		}
		if t < c.bwPrev[i] {
			t = 0.75*t + 0.25*c.bwPrev[i]
		} else {
			t = 0.90625*t + 0.09375*c.bwPrev[i]
		}
		if t < 0.015625 {
			t = 0
		}
		bw[i] = t
	}
	c.bwPrev = bw
	c.invfPrev = c.invf

	// LPC by the covariance method on each source subband.
	var a0, a1 [32]complex128
	for p := range e.k0 {
		var phi [3][3]complex128
		for i := range 3 {
			for j := 1; j < 3; j++ {
				var s complex128
				for n := range sbrSlots + 6 {
					x := c.xLow[n-i+sbrHFAdj][p]
					y := c.xLow[n-j+sbrHFAdj][p]
					s += x * complex(real(y), -imag(y))
				}
				phi[i][j] = s
			}
		}
		p01, p02, p11, p12, p22 := phi[0][1], phi[0][2], phi[1][1], phi[1][2], phi[2][2]
		d := real(p22)*real(p11) - (real(p12)*real(p12)+imag(p12)*imag(p12))/(1+1e-6)
		var al0, al1 complex128
		if d != 0 {
			al1 = (p01*p12 - p02*p11) / complex(d, 0)
		}
		if real(p11) != 0 {
			al0 = -(p01 + al1*complex(real(p12), -imag(p12))) / p11
		}
		if cmplxAbs(al0) >= 4 || cmplxAbs(al1) >= 4 {
			al0, al1 = 0, 0
		}
		a0[p], a1[p] = al0, al1
	}

	// X_High, reusing yBuf as its storage.
	c.yBuf = [len(c.yBuf)][64]complex128{}
	lo, hi := sbrRate*c.tE[0], sbrRate*c.tE[c.numEnv]
	k := e.kx
	for i, n := range e.patchNum {
		for x := range n {
			p := e.patchSrc[i] + x
			g := 0
			for g < e.nQ-1 && k >= e.fNoise[g+1] {
				g++
			}
			b := complex(bw[g], 0)
			b2 := complex(bw[g]*bw[g], 0)
			for l := lo; l < hi; l++ {
				m := l + sbrHFAdj
				c.yBuf[m][k] = c.xLow[m][p] + b*a0[p]*c.xLow[m-1][p] + b2*a1[p]*c.xLow[m-2][p]
			}
			k++
		}
	}
}

func cmplxAbs(z complex128) float64 { return math.Hypot(real(z), imag(z)) }

var (
	limGains     = [4]float64{0.70795, 1.0, 1.41254, 1e10}
	smoothFilter = [5]float64{0.33333333333333, 0.30150283239582, 0.21816949906249, 0.11516383427084, 0.03183050093751}
)

// hfAdjust shapes the high band to the transmitted envelope and adds noise
// and sinusoids — §4.6.18.7. On entry yBuf holds X_High; on return it holds
// Y for this frame's slots, with the slots before t_E(0) carried over.
func (e *sbrElement) hfAdjust(c *sbrChannel) {
	const eps, eps0 = 1.0, 1e-12
	M, kx := e.m, e.kx
	le := c.numEnv
	xHigh := c.yBuf
	var y [len(c.yBuf)][64]complex128
	// Slots before this frame's first envelope come from the last frame.
	copy(y[:], c.yPrev[sbrSlots:])

	// S_IndexMapped per envelope
	var sIdx [sbrMaxEnv][64]int
	for l := range le {
		for i := range e.nHigh {
			if c.addHarmonic[i] == 0 {
				continue
			}
			mid := (e.fHigh[i+1] + e.fHigh[i]) / 2
			if l >= c.lA || c.sIdxPrev[mid-kx] == 1 {
				sIdx[l][mid-kx] = 1
			}
		}
	}

	hSL := 4
	if e.hdr.smooth == 1 {
		hSL = 0
	}
	if len(c.gTemp) != hSL+1 || c.smoothReset {
		c.gTemp = make([][64]float64, hSL+1)
		c.qTemp = make([][64]float64, hSL+1)
	}

	slot := 0 // counts the frame's slots for the noise index
	for l := range le {
		res := c.freqRes[l]
		ft := e.fLow
		if res == 1 {
			ft = e.fHigh
		}
		nb := e.numBands(res)
		q := 0
		for q < c.numNoise-1 && c.tE[l] >= c.tQ[q+1] {
			q++
		}
		delta := !(l == c.lA || (l == 0 && c.lAPrevIsEnd))
		var eOrig, qMap, eCurr, sMapped, gain, qm, sm [64]float64
		for b := range nb {
			for k := ft[b]; k < ft[b+1]; k++ {
				eOrig[k-kx] = c.eOrig[l][b]
			}
		}
		for b := range e.nQ {
			for k := e.fNoise[b]; k < e.fNoise[b+1]; k++ {
				qMap[k-kx] = c.qOrig[q][b]
			}
		}
		for b := range nb {
			s := 0
			for k := ft[b]; k < ft[b+1]; k++ {
				s |= sIdx[l][k-kx]
			}
			for k := ft[b]; k < ft[b+1]; k++ {
				sMapped[k-kx] = float64(s)
			}
		}
		t0, t1 := sbrRate*c.tE[l]+sbrHFAdj, sbrRate*c.tE[l+1]+sbrHFAdj
		span := float64(t1 - t0)
		if e.hdr.interpolFreq == 1 {
			for m := range M {
				var s float64
				for i := t0; i < t1; i++ {
					v := xHigh[i][m+kx]
					s += real(v)*real(v) + imag(v)*imag(v)
				}
				eCurr[m] = s / span
			}
		} else {
			for b := range nb {
				var s float64
				for k := ft[b]; k < ft[b+1]; k++ {
					for i := t0; i < t1; i++ {
						v := xHigh[i][k]
						s += real(v)*real(v) + imag(v)*imag(v)
					}
				}
				s /= span * float64(ft[b+1]-ft[b])
				for k := ft[b]; k < ft[b+1]; k++ {
					eCurr[k-kx] = s
				}
			}
		}
		for m := range M {
			qm[m] = math.Sqrt(eOrig[m] * qMap[m] / (1 + qMap[m]))
			sm[m] = math.Sqrt(eOrig[m] * float64(sIdx[l][m]) / (1 + qMap[m]))
			if sMapped[m] == 0 {
				d := 1.0
				if !delta {
					d = 0
				}
				gain[m] = math.Sqrt(eOrig[m] / ((eps + eCurr[m]) * (1 + d*qMap[m])))
			} else {
				gain[m] = math.Sqrt(eOrig[m] / (eps + eCurr[m]) * qMap[m] / (1 + qMap[m]))
			}
		}
		// limiter
		for b := 0; b+1 < len(e.fLim); b++ {
			lo, hi := e.fLim[b], e.fLim[b+1]
			var so, sc float64
			for m := lo; m < hi; m++ {
				so += eOrig[m]
				sc += eCurr[m]
			}
			gMax := math.Min(math.Sqrt((eps0+so)/(eps0+sc))*limGains[e.hdr.limiterGains], 1e5)
			var den float64
			for m := lo; m < hi; m++ {
				if gain[m] > gMax {
					qm[m] = qm[m] * gMax / gain[m]
					gain[m] = gMax
				}
				den += eCurr[m] * gain[m] * gain[m]
				den += sm[m] * sm[m]
				if delta && sm[m] == 0 {
					den += qm[m] * qm[m]
				}
			}
			boost := math.Min(math.Sqrt((eps0+so)/(eps0+den)), 1.584893192)
			for m := lo; m < hi; m++ {
				gain[m] *= boost
				qm[m] *= boost
				sm[m] *= boost
			}
		}
		// assembly
		for i := t0; i < t1; i++ {
			copy(c.gTemp, c.gTemp[1:])
			copy(c.qTemp, c.qTemp[1:])
			copy(c.gTemp[hSL][:], gain[:])
			copy(c.qTemp[hSL][:], qm[:])
			if c.smoothReset {
				for j := range hSL {
					c.gTemp[j] = c.gTemp[hSL]
					c.qTemp[j] = c.qTemp[hSL]
				}
				c.smoothReset = false
			}
			for m := range M {
				var g, qf float64
				if hSL == 0 || !delta {
					g, qf = c.gTemp[hSL][m], c.qTemp[hSL][m]
				} else {
					for j := 0; j <= hSL; j++ {
						g += c.gTemp[hSL-j][m] * smoothFilter[j]
						qf += c.qTemp[hSL-j][m] * smoothFilter[j]
					}
				}
				if sm[m] != 0 || !delta {
					qf = 0
				}
				c.indexNoise = (c.indexNoise + 1) & 511
				v := xHigh[i][m+kx] * complex(g, 0)
				v += complex(qf*sbrNoise[c.indexNoise][0], qf*sbrNoise[c.indexNoise][1])
				if sm[m] != 0 {
					s := sm[m]
					switch c.indexSine {
					case 0:
						v += complex(s, 0)
					case 2:
						v -= complex(s, 0)
					case 1, 3:
						sign := 1.0
						if (m+kx)&1 == 1 {
							sign = -1
						}
						if c.indexSine == 3 {
							sign = -sign
						}
						v += complex(0, s*sign)
					}
				}
				y[i][m+kx] = v
			}
			c.indexSine = (c.indexSine + 1) & 3
			slot++
		}
	}
	c.yBuf = y
	c.yPrev = y
	// carry-over state
	c.lAPrevIsEnd = c.lA == le
	c.sIdxPrev = sIdx[le-1]
	c.harmPrev = c.addHarmonic
}
