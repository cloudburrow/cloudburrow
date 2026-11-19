package aac

// Spectral Band Replication (HE-AAC v1) — ISO/IEC 14496-3 §4.6.18. Written
// for CloudBurrow from the standard's text (#1739): the bitstream syntax
// (§4.4.2.8, Tables 4.62 to 4.74), the frequency band tables (§4.6.18.3),
// the QMF banks (§4.6.18.4), HF generation (§4.6.18.6) and HF adjustment
// (§4.6.18.7), in floating point at the 16-bit PCM scale the standard uses.

import (
	"errors"
	"math"
	"sort"

	"github.com/cloudburrow/cloudburrow/third_party/aac-go/internal/bits"
)

const (
	sbrTimeSlots = 16 // numTimeSlots for a 1024-sample frame
	sbrRate      = 2  // QMF slots per SBR time slot
	sbrSlots     = sbrTimeSlots * sbrRate
	sbrHFGen     = 8 // t_HFGen
	sbrHFAdj     = 2 // t_HFAdj
	sbrMaxEnv    = 5
	sbrMaxNoise  = 2
)

const (
	frameFIXFIX = 0
	frameFIXVAR = 1
	frameVARFIX = 2
	frameVARVAR = 3
)

// bitr is a reader whose first error sticks, so the SBR syntax reads like
// the standard's tables.
type bitr struct {
	r   *bits.BitReader
	err error
}

func (b *bitr) u(n int) int {
	if b.err != nil || n == 0 {
		return 0
	}
	v, err := b.r.ReadBits(n)
	if err != nil {
		b.err = err
		return 0
	}
	return int(v)
}

func (b *bitr) pos() int { return b.r.Position() }

// sbrHeader is sbr_header() — Table 4.63.
type sbrHeader struct {
	ampRes, startFreq, stopFreq, xoverBand           int
	freqScale, alterScale, noiseBands                int
	limiterBands, limiterGains, interpolFreq, smooth int
}

// sbrChannel is one channel's frame data and the state SBR keeps between
// frames.
type sbrChannel struct {
	// sbr_grid
	frameClass int
	numEnv     int
	numNoise   int
	freqRes    [sbrMaxEnv]int
	tE         [sbrMaxEnv + 1]int
	tQ         [sbrMaxNoise + 1]int
	lA         int
	ampRes     int
	// sbr_dtdf, sbr_invf
	dfEnv   [sbrMaxEnv]int
	dfNoise [sbrMaxNoise]int
	invf    [5]int
	// envelope and noise floor, in the coded units (absolute after delta
	// decoding)
	env   [sbrMaxEnv][48]int
	noise [sbrMaxNoise][5]int
	// sinusoidal coding
	addHarmonicFlag bool
	addHarmonic     [48]int

	// Between frames.
	envPrev     [48]int
	freqResPrev int
	noisePrev   [5]int
	invfPrev    [5]int
	bwPrev      [5]float64
	lAPrevIsEnd bool // the previous frame's l_A was its last envelope
	harmPrev    [48]int
	sIdxPrev    [64]int // S_IndexMapped of the previous frame's last envelope
	indexNoise  int
	indexSine   int
	gTemp       [][64]float64 // G_lim_boost and Q_M_lim_boost history (h_SL slots)
	qTemp       [][64]float64
	smoothReset bool

	// Dequantised, for the current frame.
	eOrig [sbrMaxEnv][48]float64
	qOrig [sbrMaxNoise][5]float64

	// QMF state.
	anaX   [320]float64
	synV   [1280]float64
	synV32 [640]float64                        // downsampled synthesis
	xLow   [sbrSlots + sbrHFGen][32]complex128 // X_Low; index 8 is this frame's first slot
	yBuf   [sbrSlots + sbrHFGen + 2][64]complex128
	yPrev  [sbrSlots + sbrHFGen + 2][64]complex128
}

// sbrElement holds a channel element's SBR state.
type sbrElement struct {
	rate       int // the SBR (output) sampling rate
	haveHeader bool
	hdr        sbrHeader
	tablesOK   bool
	stereo     bool
	coupling   bool
	ch         [2]sbrChannel
	// frequency tables
	k0, k2, kx, m      int
	fMaster            []int
	fHigh, fLow        []int
	fNoise             []int
	fLim               []int
	patchNum, patchSrc []int
	nHigh, nLow, nQ    int
	// parametric stereo, carried in an SCE's extension
	ps    *psDecoder
	psHit bool // this frame carried PS data
}

func newSBRElement(rate int, stereo bool) *sbrElement {
	e := &sbrElement{rate: rate, stereo: stereo}
	for i := range e.ch {
		e.ch[i].smoothReset = true
	}
	return e
}

var errSBR = errors.New("aac: invalid SBR data")

// parseExtension reads sbr_extension_data() — Table 4.62 — from a fill
// element's extension_payload of cnt bytes, the 4-bit extension_type
// already read.
func (e *sbrElement) parseExtension(r *bits.BitReader, crc bool, payloadBits int) error {
	b := &bitr{r: r}
	start := b.pos()
	if crc {
		b.u(10)
	}
	if b.u(1) == 1 { // bs_header_flag
		var h sbrHeader
		h.ampRes = b.u(1)
		h.startFreq = b.u(4)
		h.stopFreq = b.u(4)
		h.xoverBand = b.u(3)
		b.u(2)
		extra1, extra2 := b.u(1), b.u(1)
		h.freqScale, h.alterScale, h.noiseBands = 2, 1, 2
		if extra1 == 1 {
			h.freqScale, h.alterScale, h.noiseBands = b.u(2), b.u(1), b.u(2)
		}
		h.limiterBands, h.limiterGains, h.interpolFreq, h.smooth = 2, 2, 1, 1
		if extra2 == 1 {
			h.limiterBands, h.limiterGains, h.interpolFreq, h.smooth = b.u(2), b.u(2), b.u(1), b.u(1)
		}
		if b.err != nil {
			return b.err
		}
		reset := !e.haveHeader || h.startFreq != e.hdr.startFreq || h.stopFreq != e.hdr.stopFreq ||
			h.xoverBand != e.hdr.xoverBand || h.freqScale != e.hdr.freqScale ||
			h.alterScale != e.hdr.alterScale || h.noiseBands != e.hdr.noiseBands
		limChange := e.haveHeader && h.limiterBands != e.hdr.limiterBands
		e.hdr = h
		e.haveHeader = true
		if reset {
			if err := e.makeTables(); err != nil {
				e.tablesOK = false
				return err
			}
			e.tablesOK = true
			e.reset()
		} else if limChange {
			e.makeLimiterTable()
		}
	}
	if !e.haveHeader || !e.tablesOK {
		// No header yet: the frame's SBR data is skipped (a stream may
		// start without one; #1754's conformance streams do).
		for range payloadBits - (b.pos() - start) {
			b.u(1)
		}
		return b.err
	}
	e.psHit = false
	var err error
	if e.stereo {
		err = e.parseCPE(b)
	} else {
		err = e.parseSCE(b)
	}
	if err == nil {
		err = b.err
	}
	if err != nil {
		return err
	}
	used := b.pos() - start
	if used > payloadBits {
		return errSBR
	}
	for range payloadBits - used {
		b.u(1)
	}
	return b.err
}

func (e *sbrElement) reset() {
	for i := range e.ch {
		c := &e.ch[i]
		c.smoothReset = true
		c.freqResPrev = -1
		c.envPrev = [48]int{}
		c.noisePrev = [5]int{}
		c.sIdxPrev = [64]int{}
		c.harmPrev = [48]int{}
	}
}

func (e *sbrElement) parseSCE(b *bitr) error {
	c := &e.ch[0]
	if b.u(1) == 1 {
		b.u(4)
	}
	if err := e.parseGrid(b, c); err != nil {
		return err
	}
	e.parseDTDF(b, c)
	e.parseInvf(b, c)
	if err := e.parseEnvelope(b, c, 0, false); err != nil {
		return err
	}
	if err := e.parseNoise(b, c, 0, false); err != nil {
		return err
	}
	e.parseSinusoidal(b, c)
	return e.parseExtended(b)
}

func (e *sbrElement) parseCPE(b *bitr) error {
	c0, c1 := &e.ch[0], &e.ch[1]
	if b.u(1) == 1 {
		b.u(8)
	}
	e.coupling = b.u(1) == 1
	if e.coupling {
		if err := e.parseGrid(b, c0); err != nil {
			return err
		}
		e.copyGrid(c1, c0)
		e.parseDTDF(b, c0)
		e.parseDTDF(b, c1)
		e.parseInvf(b, c0)
		c1.invf = c0.invf
		if err := e.parseEnvelope(b, c0, 0, true); err != nil {
			return err
		}
		if err := e.parseNoise(b, c0, 0, true); err != nil {
			return err
		}
		if err := e.parseEnvelope(b, c1, 1, true); err != nil {
			return err
		}
		if err := e.parseNoise(b, c1, 1, true); err != nil {
			return err
		}
	} else {
		if err := e.parseGrid(b, c0); err != nil {
			return err
		}
		if err := e.parseGrid(b, c1); err != nil {
			return err
		}
		e.parseDTDF(b, c0)
		e.parseDTDF(b, c1)
		e.parseInvf(b, c0)
		e.parseInvf(b, c1)
		if err := e.parseEnvelope(b, c0, 0, false); err != nil {
			return err
		}
		if err := e.parseEnvelope(b, c1, 1, false); err != nil {
			return err
		}
		if err := e.parseNoise(b, c0, 0, false); err != nil {
			return err
		}
		if err := e.parseNoise(b, c1, 1, false); err != nil {
			return err
		}
	}
	e.parseSinusoidal(b, c0)
	e.parseSinusoidal(b, c1)
	return e.parseExtended(b)
}

func (e *sbrElement) copyGrid(dst, src *sbrChannel) {
	dst.frameClass, dst.numEnv, dst.numNoise = src.frameClass, src.numEnv, src.numNoise
	dst.freqRes, dst.tE, dst.tQ, dst.lA, dst.ampRes = src.freqRes, src.tE, src.tQ, src.lA, src.ampRes
}

// parseGrid reads sbr_grid() — Table 4.69 — and derives the envelope and
// noise floor time borders (§4.6.18.3.3).
func (e *sbrElement) parseGrid(b *bitr, c *sbrChannel) error {
	c.ampRes = e.hdr.ampRes
	c.frameClass = b.u(2)
	var (
		lead, trail       int
		relLead, relTrail []int
		pointer           int
	)
	ptrBits := func(n int) int { return bitsFor(n + 1) }
	switch c.frameClass {
	case frameFIXFIX:
		c.numEnv = 1 << b.u(2)
		if c.numEnv == 1 {
			c.ampRes = 0
		}
		fr := b.u(1)
		for i := range min(c.numEnv, sbrMaxEnv) {
			c.freqRes[i] = fr
		}
		lead, trail = 0, sbrTimeSlots
		rel := int(math.Round(float64(sbrTimeSlots) / float64(c.numEnv)))
		for range c.numEnv - 1 {
			relLead = append(relLead, rel)
		}
	case frameFIXVAR:
		trail = b.u(2) + sbrTimeSlots
		n := b.u(2)
		c.numEnv = n + 1
		for range n {
			relTrail = append(relTrail, 2*b.u(2)+2)
		}
		pointer = b.u(ptrBits(c.numEnv))
		for i := range c.numEnv {
			c.freqRes[c.numEnv-1-i] = b.u(1)
		}
	case frameVARFIX:
		lead = b.u(2)
		trail = sbrTimeSlots
		n := b.u(2)
		c.numEnv = n + 1
		for range n {
			relLead = append(relLead, 2*b.u(2)+2)
		}
		pointer = b.u(ptrBits(c.numEnv))
		for i := range c.numEnv {
			c.freqRes[i] = b.u(1)
		}
	case frameVARVAR:
		lead = b.u(2)
		trail = b.u(2) + sbrTimeSlots
		n0, n1 := b.u(2), b.u(2)
		c.numEnv = n0 + n1 + 1
		if c.numEnv > sbrMaxEnv {
			return errSBR
		}
		for range n0 {
			relLead = append(relLead, 2*b.u(2)+2)
		}
		for range n1 {
			relTrail = append(relTrail, 2*b.u(2)+2)
		}
		pointer = b.u(ptrBits(c.numEnv))
		for i := range c.numEnv {
			c.freqRes[i] = b.u(1)
		}
	}
	if b.err != nil {
		return b.err
	}
	if c.numEnv > sbrMaxEnv {
		return errSBR
	}
	c.tE[0] = lead
	c.tE[c.numEnv] = trail
	for l := 1; l <= len(relLead); l++ {
		c.tE[l] = c.tE[l-1] + relLead[l-1]
	}
	for i, l := 0, c.numEnv-1; i < len(relTrail); i, l = i+1, l-1 {
		c.tE[l] = c.tE[l+1] - relTrail[i]
	}
	for l := range c.numEnv {
		if c.tE[l] >= c.tE[l+1] {
			return errSBR
		}
	}
	// l_A and the middle noise border.
	le := c.numEnv
	c.lA = -1
	switch c.frameClass {
	case frameVARFIX:
		if pointer > 1 {
			c.lA = pointer - 1
		}
	case frameFIXVAR, frameVARVAR:
		if pointer > 0 {
			c.lA = le + 1 - pointer
		}
	}
	if le > 1 {
		c.numNoise = 2
	} else {
		c.numNoise = 1
	}
	c.tQ[0] = c.tE[0]
	c.tQ[c.numNoise] = c.tE[le]
	if c.numNoise == 2 {
		var mid int
		switch c.frameClass {
		case frameFIXFIX:
			mid = le / 2
		case frameVARFIX:
			switch pointer {
			case 0:
				mid = 1
			case 1:
				mid = le - 1
			default:
				mid = pointer - 1
			}
		default:
			if pointer > 1 {
				mid = le + 1 - pointer
			} else {
				mid = le - 1
			}
		}
		if mid < 0 || mid > le {
			return errSBR
		}
		c.tQ[1] = c.tE[mid]
	}
	return nil
}

func bitsFor(n int) int { // ceil(log2(n))
	b := 0
	for (1 << b) < n {
		b++
	}
	return b
}

func (e *sbrElement) parseDTDF(b *bitr, c *sbrChannel) {
	for i := range c.numEnv {
		c.dfEnv[i] = b.u(1)
	}
	for i := range c.numNoise {
		c.dfNoise[i] = b.u(1)
	}
}

func (e *sbrElement) parseInvf(b *bitr, c *sbrChannel) {
	for i := range e.nQ {
		c.invf[i] = b.u(2)
	}
}

func (e *sbrElement) numBands(res int) int {
	if res == 1 {
		return e.nHigh
	}
	return e.nLow
}

// parseEnvelope reads sbr_envelope() — Table 4.71 — and delta-decodes it
// (§4.6.18.3.5).
func (e *sbrElement) parseEnvelope(b *bitr, c *sbrChannel, ch int, coupling bool) error {
	bal := coupling && ch == 1
	// The balance channel's values step by two (delta = 2,
	// §4.6.18.3.2): they are kept at full scale.
	step := 1
	if bal {
		step = 2
	}
	var tHuff, fHuff *sbrHuffman
	startBits := 7
	switch {
	case bal && c.ampRes == 1:
		tHuff, fHuff, startBits = &huffTEnvBal30, &huffFEnvBal30, 5
	case bal:
		tHuff, fHuff, startBits = &huffTEnvBal15, &huffFEnvBal15, 6
	case c.ampRes == 1:
		tHuff, fHuff, startBits = &huffTEnv30, &huffFEnv30, 6
	default:
		tHuff, fHuff = &huffTEnv15, &huffFEnv15
	}
	prev := c.envPrev
	prevRes := c.freqResPrev
	for l := range c.numEnv {
		res := c.freqRes[l]
		n := e.numBands(res)
		cur := &c.env[l]
		if c.dfEnv[l] == 0 {
			cur[0] = step * b.u(startBits)
			for k := 1; k < n; k++ {
				d, err := fHuff.decode(b)
				if err != nil {
					return err
				}
				cur[k] = cur[k-1] + step*d
			}
		} else {
			if prevRes < 0 {
				prevRes = res // after a reset the previous envelope is zero
			}
			for k := range n {
				d, err := tHuff.decode(b)
				if err != nil {
					return err
				}
				var p int
				switch {
				case res == prevRes:
					p = prev[k]
				case res == 1: // high from low
					p = prev[e.lowIndexOfHigh(k)]
				default: // low from high
					p = prev[e.highIndexOfLow(k)]
				}
				cur[k] = p + step*d
			}
		}
		if b.err != nil {
			return b.err
		}
		prev = *cur
		prevRes = res
	}
	c.envPrev = prev
	c.freqResPrev = prevRes
	return nil
}

// lowIndexOfHigh is i with f_low[i] <= f_high[k] < f_low[i+1].
func (e *sbrElement) lowIndexOfHigh(k int) int {
	f := e.fHigh[k]
	for i := range e.nLow {
		if e.fLow[i] <= f && f < e.fLow[i+1] {
			return i
		}
	}
	return e.nLow - 1
}

// highIndexOfLow is i with f_high[i] == f_low[k].
func (e *sbrElement) highIndexOfLow(k int) int {
	f := e.fLow[k]
	for i := range e.nHigh {
		if e.fHigh[i] == f {
			return i
		}
	}
	return 0
}

// parseNoise reads sbr_noise() — Table 4.72.
func (e *sbrElement) parseNoise(b *bitr, c *sbrChannel, ch int, coupling bool) error {
	bal := coupling && ch == 1
	tHuff, fHuff := &huffTNoise30, &huffFEnv30
	step := 1
	if bal {
		tHuff, fHuff, step = &huffTNoiseBal30, &huffFEnvBal30, 2
	}
	prev := c.noisePrev
	for l := range c.numNoise {
		cur := &c.noise[l]
		if c.dfNoise[l] == 0 {
			cur[0] = step * b.u(5)
			for k := 1; k < e.nQ; k++ {
				d, err := fHuff.decode(b)
				if err != nil {
					return err
				}
				cur[k] = cur[k-1] + step*d
			}
		} else {
			for k := range e.nQ {
				d, err := tHuff.decode(b)
				if err != nil {
					return err
				}
				cur[k] = prev[k] + step*d
			}
		}
		prev = *cur
	}
	c.noisePrev = prev
	return b.err
}

func (e *sbrElement) parseSinusoidal(b *bitr, c *sbrChannel) {
	c.addHarmonicFlag = b.u(1) == 1
	c.addHarmonic = [48]int{}
	if c.addHarmonicFlag {
		for i := range e.nHigh {
			c.addHarmonic[i] = b.u(1)
		}
	}
}

// parseExtended reads bs_extended_data and its sbr_extension()s; PS is
// extension 2.
func (e *sbrElement) parseExtended(b *bitr) error {
	if b.u(1) == 0 {
		return b.err
	}
	cnt := b.u(4)
	if cnt == 15 {
		cnt += b.u(8)
	}
	left := 8 * cnt
	for left > 7 && b.err == nil {
		id := b.u(2)
		left -= 2
		start := b.pos()
		if id == 2 && !e.stereo {
			if e.ps == nil {
				e.ps = newPSDecoder()
			}
			if err := e.ps.parse(b, left); err != nil {
				return err
			}
			e.psHit = true
		}
		used := b.pos() - start
		if used > left {
			return errSBR
		}
		// The rest of this extension is skipped.
		for range left - used {
			b.u(1)
		}
		left = 0
	}
	for range left {
		b.u(1)
	}
	return b.err
}

// makeTables derives the frequency band tables — §4.6.18.3.2.
func (e *sbrElement) makeTables() error {
	fs := e.rate
	h := &e.hdr
	var startMin, stopMin int
	switch {
	case fs < 32000:
		startMin, stopMin = 3000, 6000
	case fs < 64000:
		startMin, stopMin = 4000, 8000
	default:
		startMin, stopMin = 5000, 10000
	}
	startMin = int(math.Round(float64(startMin) * 128 / float64(fs)))
	stopMin = int(math.Round(float64(stopMin) * 128 / float64(fs)))
	var offs []int
	switch {
	case fs <= 16000:
		offs = []int{-8, -7, -6, -5, -4, -3, -2, -1, 0, 1, 2, 3, 4, 5, 6, 7}
	case fs <= 22050:
		offs = []int{-5, -4, -3, -2, -1, 0, 1, 2, 3, 4, 5, 6, 7, 9, 11, 13}
	case fs <= 24000:
		offs = []int{-5, -3, -2, -1, 0, 1, 2, 3, 4, 5, 6, 7, 9, 11, 13, 16}
	case fs <= 32000:
		offs = []int{-6, -4, -2, -1, 0, 1, 2, 3, 4, 5, 6, 7, 9, 11, 13, 16}
	case fs <= 64000:
		offs = []int{-4, -2, -1, 0, 1, 2, 3, 4, 5, 6, 7, 9, 11, 13, 16, 20}
	default:
		offs = []int{-2, -1, 0, 1, 2, 3, 4, 5, 6, 7, 9, 11, 13, 16, 20, 24}
	}
	k0 := startMin + offs[h.startFreq]
	var k2 int
	switch h.stopFreq {
	case 14:
		k2 = 2 * k0
	case 15:
		k2 = 3 * k0
	default:
		dk := make([]int, 13)
		for i := range 13 {
			a := math.Round(float64(stopMin) * math.Pow(64/float64(stopMin), float64(i+1)/13))
			c := math.Round(float64(stopMin) * math.Pow(64/float64(stopMin), float64(i)/13))
			dk[i] = int(a - c)
		}
		sort.Ints(dk)
		k2 = stopMin
		for i := range h.stopFreq {
			k2 += dk[i]
		}
	}
	k2 = min(k2, 64)
	if k0 >= k2 || k0 < 1 {
		return errSBR
	}
	e.k0, e.k2 = k0, k2

	// f_master
	var master []int
	if h.freqScale == 0 {
		dk, nb := 1, 0
		if h.alterScale == 1 {
			dk = 2
			nb = 2 * int(math.Round(float64(k2-k0)/4))
		} else {
			nb = 2 * ((k2 - k0) / 2)
		}
		if nb <= 0 {
			return errSBR
		}
		vdk := make([]int, nb)
		for i := range vdk {
			vdk[i] = dk
		}
		diff := (k2 - k0) - nb*dk
		k, incr := 0, 1
		if diff > 0 {
			k, incr = nb-1, -1
		}
		for diff != 0 {
			vdk[k] -= incr
			k += incr
			diff += incr
		}
		master = append(master, k0)
		for i := range nb {
			master = append(master, master[i]+vdk[i])
		}
	} else {
		bands := []float64{0, 12, 10, 8}[h.freqScale]
		warp := 1.0
		if h.alterScale == 1 {
			warp = 1.3
		}
		twoRegions := float64(k2)/float64(k0) > 2.2449
		k1 := k2
		if twoRegions {
			k1 = 2 * k0
		}
		nb0 := 2 * int(math.Round(bands*math.Log(float64(k1)/float64(k0))/(2*math.Log(2))))
		if nb0 <= 0 {
			return errSBR
		}
		vdk0 := geomDiffs(k0, k1, nb0)
		sort.Ints(vdk0)
		master = append(master, k0)
		for i := range nb0 {
			master = append(master, master[i]+vdk0[i])
		}
		if twoRegions {
			nb1 := 2 * int(math.Round(bands*math.Log(float64(k2)/float64(k1))/(2*math.Log(2)*warp)))
			if nb1 > 0 {
				vdk1 := geomDiffs(k1, k2, nb1)
				sort.Ints(vdk1)
				if vdk1[0] < vdk0[nb0-1] {
					change := vdk0[nb0-1] - vdk1[0]
					vdk1[0] += change
					vdk1[nb1-1] -= change
					sort.Ints(vdk1)
				}
				for i := range nb1 {
					master = append(master, master[len(master)-1]+vdk1[i])
				}
			}
		}
	}
	nMaster := len(master) - 1
	if h.xoverBand >= nMaster {
		return errSBR
	}
	e.fMaster = master
	e.nHigh = nMaster - h.xoverBand
	e.fHigh = master[h.xoverBand:]
	e.nLow = e.nHigh/2 + e.nHigh%2
	e.fLow = make([]int, e.nLow+1)
	e.fLow[0] = e.fHigh[0]
	for k := 1; k <= e.nLow; k++ {
		e.fLow[k] = e.fHigh[2*k-e.nHigh%2]
	}
	e.kx = e.fHigh[0]
	e.m = e.fHigh[e.nHigh] - e.kx
	if e.kx > 32 || e.kx+e.m > 64 || e.m <= 0 || e.nHigh > 48 {
		return errSBR
	}
	nq := int(math.Round(float64(h.noiseBands) * math.Log2(float64(k2)/float64(e.kx))))
	e.nQ = min(max(1, nq), 5)
	e.fNoise = make([]int, e.nQ+1)
	idx := 0
	e.fNoise[0] = e.fLow[0]
	for k := 1; k <= e.nQ; k++ {
		idx += (e.nLow - idx) / (e.nQ + 1 - k)
		e.fNoise[k] = e.fLow[idx]
	}
	e.makePatches()
	e.makeLimiterTable()
	return nil
}

func geomDiffs(a, b, n int) []int {
	d := make([]int, n)
	r := float64(b) / float64(a)
	for k := range n {
		hi := math.Round(float64(a) * math.Pow(r, float64(k+1)/float64(n)))
		lo := math.Round(float64(a) * math.Pow(r, float64(k)/float64(n)))
		d[k] = int(hi - lo)
	}
	return d
}

// makePatches builds the HF generator's patches — §4.6.18.6.3.
func (e *sbrElement) makePatches() {
	e.patchNum, e.patchSrc = nil, nil
	nMaster := len(e.fMaster) - 1
	msb, usb := e.k0, e.kx
	goalSb := int(math.Round(2.048e6 / float64(e.rate)))
	k := nMaster
	if goalSb < e.kx+e.m {
		k = 0
		for i := 0; e.fMaster[i] < goalSb; i++ {
			k = i + 1
		}
	}
	for guard := 0; guard < 16; guard++ {
		j := k + 1
		var sb, odd int
		for {
			j--
			sb = e.fMaster[j]
			odd = (sb - 2 + e.k0) % 2
			if sb <= e.k0-1+msb-odd {
				break
			}
		}
		n := max(sb-usb, 0)
		if n > 0 {
			e.patchNum = append(e.patchNum, n)
			e.patchSrc = append(e.patchSrc, e.k0-odd-n)
			usb, msb = sb, sb
		} else {
			msb = e.kx
		}
		if e.fMaster[k]-sb < 3 {
			k = nMaster
		}
		if sb == e.kx+e.m {
			break
		}
	}
	if n := len(e.patchNum); n > 1 && e.patchNum[n-1] < 3 {
		e.patchNum = e.patchNum[:n-1]
		e.patchSrc = e.patchSrc[:n-1]
	}
}

// makeLimiterTable builds f_tableLim — §4.6.18.3.2.4.
func (e *sbrElement) makeLimiterTable() {
	if e.hdr.limiterBands == 0 {
		e.fLim = []int{e.fLow[0] - e.kx, e.fLow[e.nLow] - e.kx}
		return
	}
	perOctave := []float64{0, 1.2, 2, 3}[e.hdr.limiterBands]
	borders := []int{e.kx}
	for _, n := range e.patchNum {
		borders = append(borders, borders[len(borders)-1]+n)
	}
	isBorder := func(v int) bool {
		for _, b := range borders {
			if b == v {
				return true
			}
		}
		return false
	}
	lim := append([]int(nil), e.fLow...)
	lim = append(lim, borders[1:len(borders)-1]...)
	sort.Ints(lim)
	k := 1
	for k < len(lim) {
		oct := math.Log2(float64(lim[k])/float64(lim[k-1])) * perOctave
		if oct < 0.49 {
			if lim[k] == lim[k-1] {
				lim = append(lim[:k], lim[k+1:]...)
				continue
			}
			if isBorder(lim[k]) {
				if isBorder(lim[k-1]) {
					k++
					continue
				}
				lim = append(lim[:k-1], lim[k:]...)
				continue
			}
			lim = append(lim[:k], lim[k+1:]...)
			continue
		}
		k++
	}
	e.fLim = make([]int, len(lim))
	for i, v := range lim {
		e.fLim[i] = v - e.kx
	}
}
