package ac3

import (
	"errors"
	"fmt"
	"math"
)

// Channel indexes into a frame's per-channel arrays: the full bandwidth
// channels 0 to 4 in coded order, then the coupling channel and the LFE.
const (
	cplCh  = 5
	lfeCh  = 6
	nChArr = 7
)

// Exponent strategies (Table 6.4).
const (
	expReuse = 0
	expD15   = 1
	expD25   = 2
	expD45   = 3
)

// Delta bit allocation modes (clause 4.4.3.49).
const (
	dbaReuse    = 0
	dbaNew      = 1
	dbaNone     = 2
	dbaReserved = 3
)

// header is a syncframe's syncinfo and bsi.
type header struct {
	eac3        bool
	bsid        int
	strmtyp     int // 0 independent, 1 dependent, 2 converted from AC-3
	substreamid int
	frameBytes  int
	fscod       int
	rate        int
	halfRate    bool // E-AC-3 fscod2: 24, 22.05 or 16 kHz
	numblks     int
	acmod       int
	nfchans     int
	lfeon       bool

	cmixlev, surmixlev   float64 // AC-3 cmixlev and surmixlev
	hasCmix, hasSurmix   bool
	ltrtCmix, loroCmix   float64 // Annex D and E-AC-3 3-bit levels
	ltrtSurmix, loroSurm float64
	hasXmix              bool
	chanmap              int // -1: none
	compr                [2]int
	hasCompr             [2]bool
}

var acmodChannels = [8]int{2, 1, 2, 3, 3, 4, 4, 5}

// acmodNames are the coded channels of each acmod as ffmpeg names them.
var acmodNames = [8][]string{
	{"FL", "FR"}, {"FC"}, {"FL", "FR"}, {"FL", "FC", "FR"},
	{"FL", "FR", "BC"}, {"FL", "FC", "FR", "BC"}, {"FL", "FR", "SL", "SR"}, {"FL", "FC", "FR", "SL", "SR"},
}

// parseHeader reads syncinfo and bsi.
func parseHeader(b []byte) (*header, *bitReader, error) {
	if len(b) < 7 || b[0] != 0x0b || b[1] != 0x77 {
		return nil, nil, errors.New("ac3: no syncword")
	}
	h := &header{bsid: int(b[5] >> 3), chanmap: -1}
	switch {
	case h.bsid <= 8:
		return parseAC3Header(b, h)
	case h.bsid >= 11 && h.bsid <= 16:
		h.eac3 = true
		return parseEAC3Header(b, h)
	default:
		return nil, nil, fmt.Errorf("ac3: bsid %d is not decodable", h.bsid)
	}
}

func parseAC3Header(b []byte, h *header) (*header, *bitReader, error) {
	r := &bitReader{b: b}
	r.skip(32) // syncword, crc1
	h.fscod = r.u(2)
	frmsizecod := r.u(6)
	if h.fscod == 3 || frmsizecod > 37 {
		return nil, nil, errors.New("ac3: reserved sample rate or frame size code")
	}
	words := frmSizeTab[frmsizecod>>1][h.fscod]
	if h.fscod == 1 {
		words += frmsizecod & 1
	}
	h.frameBytes = words * 2
	h.rate = sampleRates[h.fscod]
	h.numblks = 6
	h.bsid = r.u(5)
	r.skip(3) // bsmod
	h.acmod = r.u(3)
	h.nfchans = acmodChannels[h.acmod]
	if h.acmod&1 != 0 && h.acmod != 1 {
		h.cmixlev, h.hasCmix = cmixLevels[r.u(2)], true
	}
	if h.acmod&4 != 0 {
		h.surmixlev, h.hasSurmix = surMixLevels[r.u(2)], true
	}
	if h.acmod == 2 {
		r.skip(2) // dsurmod
	}
	h.lfeon = r.flag()
	for i := range 1 + btoi(h.acmod == 0) {
		r.skip(5) // dialnorm
		if r.flag() {
			h.compr[i], h.hasCompr[i] = r.u(8), true
		}
		if r.flag() {
			r.skip(8) // langcod
		}
		if r.flag() {
			r.skip(7) // mixlevel, roomtyp
		}
	}
	r.skip(2) // copyrightb, origbs
	if h.bsid == 6 {
		// Annex D's alternate syntax.
		if r.flag() {
			r.skip(2) // dmixmod
			h.ltrtCmix, h.ltrtSurmix = mixLevels3[r.u(3)], surLevel3(r.u(3))
			h.loroCmix, h.loroSurm = mixLevels3[r.u(3)], surLevel3(r.u(3))
			h.hasXmix = true
		}
		if r.flag() {
			r.skip(14)
		}
	} else {
		if r.flag() {
			r.skip(14) // timecod1
		}
		if r.flag() {
			r.skip(14) // timecod2
		}
	}
	if r.flag() {
		r.skip((r.u(6) + 1) * 8) // addbsi
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	return h, r, nil
}

// surLevel3 is a 3-bit surround mix level; the reserved codes stand for
// 0.841 (Table D.1.3).
func surLevel3(c int) float64 {
	if c < 3 {
		return mixLevels3[3]
	}
	return mixLevels3[c]
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseEAC3Header(b []byte, h *header) (*header, *bitReader, error) {
	r := &bitReader{b: b}
	r.skip(16)
	h.strmtyp = r.u(2)
	h.substreamid = r.u(3)
	h.frameBytes = (r.u(11) + 1) * 2
	h.fscod = r.u(2)
	if h.fscod == 3 {
		fscod2 := r.u(2)
		if fscod2 == 3 {
			return nil, nil, errors.New("ac3: reserved E-AC-3 sample rate code")
		}
		h.halfRate = true
		h.rate = sampleRates[fscod2] / 2
		h.numblks = 6
		h.fscod = fscod2 // the bit allocation's hearing threshold follows it
	} else {
		h.rate = sampleRates[h.fscod]
		h.numblks = [4]int{1, 2, 3, 6}[r.u(2)]
	}
	h.acmod = r.u(3)
	h.nfchans = acmodChannels[h.acmod]
	h.lfeon = r.flag()
	h.bsid = r.u(5)
	if h.strmtyp == 3 {
		return nil, nil, errors.New("ac3: reserved E-AC-3 stream type")
	}
	r.skip(5) // dialnorm
	if r.flag() {
		h.compr[0], h.hasCompr[0] = r.u(8), true
	}
	if h.acmod == 0 {
		r.skip(5)
		if r.flag() {
			h.compr[1], h.hasCompr[1] = r.u(8), true
		}
	}
	if h.strmtyp == 1 && r.flag() {
		h.chanmap = r.u(16)
	}
	if r.flag() { // mixmdate
		if h.acmod > 2 {
			r.skip(2) // dmixmod
		}
		if h.acmod&1 != 0 && h.acmod > 2 {
			h.ltrtCmix, h.loroCmix = mixLevels3[r.u(3)], mixLevels3[r.u(3)]
			h.hasXmix = true
		}
		if h.acmod&4 != 0 {
			h.ltrtSurmix, h.loroSurm = surLevel3(r.u(3)), surLevel3(r.u(3))
			h.hasXmix = true
		}
		if h.lfeon && r.flag() {
			r.skip(5) // lfemixlevcod
		}
		if h.strmtyp == 0 {
			if r.flag() {
				r.skip(6) // pgmscl
			}
			if h.acmod == 0 && r.flag() {
				r.skip(6)
			}
			if r.flag() {
				r.skip(6) // extpgmscl
			}
			switch r.u(2) { // mixdef
			case 1:
				r.skip(5)
			case 2:
				r.skip(12)
			case 3:
				n := r.u(5)
				r.skip(8 * (n + 2))
			}
			if h.acmod < 2 {
				if r.flag() {
					r.skip(14) // panmean, paninfo
				}
				if h.acmod == 0 && r.flag() {
					r.skip(14)
				}
			}
			if r.flag() { // frmmixcfginfoe
				if h.numblks == 1 {
					r.skip(5)
				} else {
					for range h.numblks {
						if r.flag() {
							r.skip(5)
						}
					}
				}
			}
		}
	}
	if r.flag() { // infomdate
		r.skip(5) // bsmod, copyrightb, origbs
		if h.acmod == 2 {
			r.skip(4)
		}
		if h.acmod >= 6 {
			r.skip(2)
		}
		if r.flag() {
			r.skip(8)
		}
		if h.acmod == 0 && r.flag() {
			r.skip(8)
		}
		if !h.halfRate {
			r.skip(1) // sourcefscod
		}
	}
	if h.strmtyp == 0 && h.numblks != 6 {
		r.skip(1) // convsync
	}
	if h.strmtyp == 2 {
		blkid := h.numblks == 6 || r.flag()
		if blkid {
			r.skip(6) // frmsizecod
		}
	}
	if r.flag() {
		r.skip((r.u(6) + 1) * 8) // addbsi
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	return h, r, nil
}

// dba is a channel's delta bit allocation.
type dba struct {
	mode            int
	offst, ln, deba [8]int
	nseg            int
}

// readDBA reads one channel's delta bit allocation for its mode
// (cpldeltbae or deltbae): new segments, none, or the previous block's
// (reuse). The reserved mode 3 is an error: A/52 clause 5.4.3.48 says a
// decoder receiving it should not decode the audio, and mute.
func readDBA(r *bitReader, d *dba, mode int) error {
	switch mode {
	case dbaNew:
		d.mode = dbaNew
		d.nseg = r.u(3) + 1
		for s := range d.nseg {
			d.offst[s], d.ln[s], d.deba[s] = r.u(5), r.u(4), r.u(3)
		}
	case dbaNone:
		d.mode = dbaNone
	case dbaReserved:
		return errors.New("ac3: reserved delta bit allocation mode")
	}
	return nil
}

// frame is the state of one syncframe's decoding: what its blocks carry
// over from block to block.
type frame struct {
	h   *header
	r   *bitReader
	sub *substream

	// audfrm (E-AC-3); for AC-3 each block sets its own.
	expstre, ahte                              bool
	snroffststr                                int
	transproce, blkswe, dithflage, bamode      bool
	frmfgaincode, dbaflde, skipflde, spxattene bool
	cplstreB, cplinuB                          [6]bool
	cplexpstrB                                 [6]int
	chexpstrB                                  [6][5]int
	lfeexpstrB                                 [6]int
	chahtinu                                   [nChArr]int // 0, 1, -1 (read)
	frmcsnroffst, frmfsnroffst                 int
	chinspxatten                               [5]bool
	spxattencod                                [5]int
	firstspxcos, firstcplcos                   [5]bool
	firstcplleak                               bool

	blksw, dithflag [5]bool
	dynrng          [2]float64

	// coupling
	cplinu, phsflginu    bool
	chincpl              [5]bool
	cplbegf, cplendf     int
	cplbndstrc           [18]bool
	ncplsubnd            int
	cplco                [5][18]float64 // by sub-band
	phsflg               [18]bool
	cplstrtmant, cplendm int
	ecplinu              bool
	ecplbegf             int
	ecplBegin, ecplEnd   int
	ecplbndstrc          [22]bool
	ecplamp              [5][23]float64 // by sub-band

	// spectral extension
	spxinu                     bool
	chinspx                    [5]bool
	spxstrtf, spxBegin, spxEnd int
	spxbegf                    int
	spxbndstrc                 [17]bool
	nspxbnds                   int
	spxbndsz                   [17]int
	spxco, nblend, sblend      [5][17]float64

	rematflg [4]bool

	exps    [nChArr][256]int
	endmant [nChArr]int
	bap     [nChArr][256]int

	sdcycod, fdcycod, sgaincod, dbpbcod, floorcod int
	csnroffst                                     int
	fsnroffst, fgaincod                           [nChArr]int
	cplfleak, cplsleak                            int
	dbas                                          [nChArr]dba

	coef [nChArr][256]float64

	// AHT: each channel's six blocks of mantissas, read in its first block.
	ahtMant              [nChArr][6][256]float64
	gaqmod               [nChArr]int
	cplahtinu, lfeahtinu int
}

// substream is what a substream keeps from frame to frame.
type substream struct {
	delay [nChArr][256]float64
	rng   uint32
}

func (s *substream) dither() float64 {
	s.rng = s.rng*1664525 + 1013904223
	return float64(int32(s.rng)) / (1 << 31) * ditherLevel
}

// ditherLevel scales the uniform dither (clause 6.3.4: 0.707 is optimum).
// Tests turn it off.
var ditherLevel = 0.707106781

// noise is zero-mean, unit-variance noise for spectral extension.
func (s *substream) noise() float64 {
	s.rng = s.rng*1664525 + 1013904223
	return float64(int32(s.rng)) / (1 << 31) * math.Sqrt(3)
}

// decodeFrame decodes a syncframe whose header is parsed, to numblks*256
// samples of each coded channel (fbw in coded order, then LFE).
func (sub *substream) decodeFrame(h *header, r *bitReader) ([][]float64, error) {
	f := &frame{h: h, r: r, sub: sub}
	nout := h.nfchans + btoi(h.lfeon)
	out := make([][]float64, nout)
	for i := range out {
		out[i] = make([]float64, h.numblks*256)
	}
	f.dynrng = [2]float64{1, 1}
	f.cplbndstrc = defCplBndStrc
	f.spxbndstrc = defSpxBndStrc
	f.ecplbndstrc = defEcplBndStrc
	for i := range f.dbas {
		f.dbas[i].mode = dbaNone
	}
	if h.eac3 {
		if err := f.audfrm(); err != nil {
			return nil, err
		}
	}
	for blk := range h.numblks {
		if err := f.audblk(blk); err != nil {
			return nil, fmt.Errorf("ac3: block %d: %w", blk, err)
		}
		if r.err != nil {
			return nil, r.err
		}
		if tally != nil {
			f.tallyBlock()
		}
		f.reconstruct(blk)
		for ch := range nout {
			c := ch
			if h.lfeon && ch == h.nfchans {
				c = lfeCh
			}
			short := c < 5 && f.blksw[c]
			imdct(&f.coef[c], short, &sub.delay[c], out[ch][blk*256:(blk+1)*256])
		}
	}
	return out, nil
}

func (f *frame) audfrm() error {
	h, r := f.h, f.r
	if h.numblks == 6 {
		f.expstre, f.ahte = r.flag(), r.flag()
	} else {
		f.expstre = true
	}
	f.snroffststr = r.u(2)
	if f.snroffststr == 3 {
		return errors.New("ac3: reserved SNR offset strategy")
	}
	f.transproce, f.blkswe, f.dithflage, f.bamode = r.flag(), r.flag(), r.flag(), r.flag()
	f.frmfgaincode, f.dbaflde, f.skipflde, f.spxattene = r.flag(), r.flag(), r.flag(), r.flag()
	ncplblks := 0
	if h.acmod > 1 {
		f.cplstreB[0] = true
		f.cplinuB[0] = r.flag()
		for blk := 1; blk < h.numblks; blk++ {
			f.cplstreB[blk] = r.flag()
			if f.cplstreB[blk] {
				f.cplinuB[blk] = r.flag()
			} else {
				f.cplinuB[blk] = f.cplinuB[blk-1]
			}
		}
	}
	for blk := range h.numblks {
		ncplblks += btoi(f.cplinuB[blk])
	}
	if f.expstre {
		for blk := range h.numblks {
			if f.cplinuB[blk] {
				f.cplexpstrB[blk] = r.u(2)
			}
			for ch := range h.nfchans {
				f.chexpstrB[blk][ch] = r.u(2)
			}
		}
	} else {
		if h.acmod > 1 && ncplblks > 0 {
			s := frmExpStr[r.u(5)]
			for blk := range 6 {
				f.cplexpstrB[blk] = s[blk]
			}
		}
		for ch := range h.nfchans {
			s := frmExpStr[r.u(5)]
			for blk := range 6 {
				f.chexpstrB[blk][ch] = s[blk]
			}
		}
	}
	if h.lfeon {
		for blk := range h.numblks {
			f.lfeexpstrB[blk] = r.u(1)
		}
	}
	if h.strmtyp == 0 {
		if h.numblks != 6 && !r.flag() {
			// no converter exponent strategies
		} else {
			r.skip(5 * h.nfchans)
		}
	}
	if f.ahte {
		ncplregs := 0
		for blk := range 6 {
			if f.cplstreB[blk] || (f.cplinuB[blk] && f.cplexpstrB[blk] != expReuse) {
				ncplregs++
			}
		}
		if ncplblks == 6 && ncplregs == 1 {
			f.cplahtinu = r.u(1)
		}
		for ch := range h.nfchans {
			n := 0
			for blk := range 6 {
				if f.chexpstrB[blk][ch] != expReuse {
					n++
				}
			}
			if n == 1 {
				f.chahtinu[ch] = r.u(1)
			}
		}
		if h.lfeon {
			n := 0
			for blk := range 6 {
				if f.lfeexpstrB[blk] != expReuse {
					n++
				}
			}
			if n == 1 {
				f.lfeahtinu = r.u(1)
			}
		}
	}
	f.chahtinu[cplCh], f.chahtinu[lfeCh] = f.cplahtinu, f.lfeahtinu
	if f.snroffststr == 0 {
		f.frmcsnroffst, f.frmfsnroffst = r.u(6), r.u(4)
	}
	if f.transproce {
		// Transient pre-noise processing (E.2.7) is optional decoder
		// post-processing, which ffmpeg does not apply either: read and
		// ignored.
		for range h.nfchans {
			if r.flag() {
				r.skip(18)
			}
		}
	}
	if f.spxattene {
		for ch := range h.nfchans {
			if f.chinspxatten[ch] = r.flag(); f.chinspxatten[ch] {
				f.spxattencod[ch] = r.u(5)
			}
		}
	}
	if h.numblks != 1 && r.flag() {
		words := h.frameBytes / 2
		r.skip((h.numblks - 1) * (4 + int(math.Ceil(math.Log2(float64(words))))))
	}
	for ch := range 5 {
		f.firstspxcos[ch], f.firstcplcos[ch] = true, true
	}
	f.firstcplleak = true
	return r.err
}

// audblk reads one audio block and leaves its transform coefficients in
// f.coef (before decoupling, rematrixing and spectral extension).
func (f *frame) audblk(blk int) error {
	h, r := f.h, f.r
	nf := h.nfchans
	eac3 := h.eac3
	// block switch and dither flags
	for ch := range nf {
		if !eac3 || f.blkswe {
			f.blksw[ch] = r.flag()
		} else {
			f.blksw[ch] = false
		}
	}
	for ch := range nf {
		if !eac3 || f.dithflage {
			f.dithflag[ch] = r.flag()
		} else {
			f.dithflag[ch] = true
		}
	}
	// dynamic range
	for i := range 1 + btoi(h.acmod == 0) {
		if r.flag() {
			f.dynrng[i] = dynrngGain(r.u(8))
		} else if blk == 0 {
			f.dynrng[i] = 1
		}
	}
	// spectral extension strategy
	if eac3 {
		if blk == 0 || r.flag() {
			f.spxinu = r.flag()
			if f.spxinu {
				if h.acmod == 1 {
					f.chinspx[0] = true
				} else {
					for ch := range nf {
						f.chinspx[ch] = r.flag()
					}
				}
				f.spxstrtf = r.u(2)
				f.spxbegf = r.u(3)
				spxendf := r.u(3)
				if f.spxbegf < 6 {
					f.spxBegin = f.spxbegf + 2
				} else {
					f.spxBegin = f.spxbegf*2 - 3
				}
				if spxendf < 3 {
					f.spxEnd = spxendf + 5
				} else {
					f.spxEnd = spxendf*2 + 3
				}
				if f.spxstrtf >= f.spxBegin || f.spxBegin >= f.spxEnd {
					return errors.New("invalid spectral extension range")
				}
				if r.flag() {
					for bnd := f.spxBegin + 1; bnd < f.spxEnd; bnd++ {
						f.spxbndstrc[bnd] = r.flag()
					}
				}
				f.nspxbnds = 1
				f.spxbndsz[0] = 12
				for bnd := f.spxBegin + 1; bnd < f.spxEnd; bnd++ {
					if !f.spxbndstrc[bnd] {
						f.spxbndsz[f.nspxbnds] = 12
						f.nspxbnds++
					} else {
						f.spxbndsz[f.nspxbnds-1] += 12
					}
				}
			} else {
				for ch := range nf {
					f.chinspx[ch] = false
					f.firstspxcos[ch] = true
				}
			}
		}
		if f.spxinu {
			for ch := range nf {
				if !f.chinspx[ch] {
					f.firstspxcos[ch] = true
					continue
				}
				spxcoe := true
				if f.firstspxcos[ch] {
					f.firstspxcos[ch] = false
				} else {
					spxcoe = r.flag()
				}
				if !spxcoe {
					continue
				}
				spxblnd := r.u(5)
				mstr := r.u(2)
				noffset := float64(spxblnd) / 32
				spxmant := spxBandTable(f.spxBegin)
				for bnd := range f.nspxbnds {
					e, m := r.u(4), r.u(2)
					var t float64
					if e == 15 {
						t = float64(m) / 4
					} else {
						t = float64(m+4) / 8
					}
					f.spxco[ch][bnd] = math.Ldexp(t, -(e + 3*mstr))
					bs := f.spxbndsz[bnd]
					nratio := (float64(spxmant)+0.5*float64(bs))/float64(spxBandTable(f.spxEnd)) - noffset
					nratio = min(1, max(0, nratio))
					f.nblend[ch][bnd] = math.Sqrt(nratio)
					f.sblend[ch][bnd] = math.Sqrt(1 - nratio)
					spxmant += bs
				}
			}
		}
	}
	// coupling strategy
	cplstre := false
	if eac3 {
		cplstre = f.cplstreB[blk]
		if cplstre {
			f.cplinu = f.cplinuB[blk]
		}
	} else {
		cplstre = r.flag()
		if cplstre {
			f.cplinu = r.flag()
		} else if blk == 0 {
			return errors.New("no coupling strategy in block 0")
		}
	}
	if cplstre {
		if f.cplinu {
			f.ecplinu = eac3 && r.flag()
			if eac3 && h.acmod == 2 {
				f.chincpl[0], f.chincpl[1] = true, true
			} else {
				for ch := range nf {
					f.chincpl[ch] = r.flag()
				}
			}
			if !f.ecplinu {
				if h.acmod == 2 {
					f.phsflginu = r.flag()
				}
				f.cplbegf = r.u(4)
				if !f.spxinu {
					f.cplendf = r.u(4)
				} else if f.spxbegf < 6 {
					f.cplendf = f.spxbegf - 2
				} else {
					f.cplendf = f.spxbegf*2 - 7
				}
				f.ncplsubnd = 3 + f.cplendf - f.cplbegf
				if f.ncplsubnd < 1 {
					return errors.New("invalid coupling range")
				}
				if !eac3 || r.flag() {
					for bnd := 1; bnd < f.ncplsubnd; bnd++ {
						f.cplbndstrc[f.cplbegf+bnd] = r.flag()
					}
				}
				f.cplstrtmant = 37 + 12*f.cplbegf
				f.cplendm = 37 + 12*(f.cplendf+3)
			} else {
				ecplbegf := r.u(4)
				f.ecplbegf = ecplbegf
				switch {
				case ecplbegf < 3:
					f.ecplBegin = ecplbegf * 2
				case ecplbegf < 13:
					f.ecplBegin = ecplbegf + 2
				default:
					f.ecplBegin = ecplbegf*2 - 10
				}
				if !f.spxinu {
					f.ecplEnd = r.u(4) + 7
				} else if f.spxbegf < 6 {
					f.ecplEnd = f.spxbegf + 5
				} else {
					f.ecplEnd = f.spxbegf * 2
				}
				if f.ecplBegin >= f.ecplEnd || f.ecplEnd > 22 {
					return errors.New("invalid enhanced coupling range")
				}
				if r.flag() {
					for sbnd := max(9, f.ecplBegin+1); sbnd < f.ecplEnd; sbnd++ {
						f.ecplbndstrc[sbnd] = r.flag()
					}
				}
				f.cplstrtmant = ecplSubbndTab[f.ecplBegin]
				f.cplendm = ecplSubbndTab[f.ecplEnd]
			}
		} else {
			for ch := range nf {
				f.chincpl[ch] = false
				f.firstcplcos[ch] = true
			}
			f.firstcplleak = true
			f.phsflginu = false
			f.ecplinu = false
		}
	}
	// coupling coordinates
	if f.cplinu {
		if !f.ecplinu {
			ncplbnd := 0
			for bnd := 0; bnd < f.ncplsubnd; bnd++ {
				if bnd == 0 || !f.cplbndstrc[f.cplbegf+bnd] {
					ncplbnd++
				}
			}
			anyCoe := false
			var cplcoe [5]bool
			for ch := range nf {
				if !f.chincpl[ch] {
					f.firstcplcos[ch] = true
					continue
				}
				if eac3 && f.firstcplcos[ch] {
					cplcoe[ch] = true
					f.firstcplcos[ch] = false
				} else {
					cplcoe[ch] = r.flag()
				}
				if !cplcoe[ch] {
					continue
				}
				anyCoe = true
				mstr := r.u(2)
				var bandco [18]float64
				for bnd := range ncplbnd {
					e, m := r.u(4), r.u(4)
					var t float64
					if e == 15 {
						t = float64(m) / 16
					} else {
						t = float64(m+16) / 32
					}
					bandco[bnd] = math.Ldexp(t, -(e + 3*mstr))
				}
				bnd := -1
				for s := 0; s < f.ncplsubnd; s++ {
					if s == 0 || !f.cplbndstrc[f.cplbegf+s] {
						bnd++
					}
					f.cplco[ch][f.cplbegf+s] = bandco[bnd]
				}
			}
			if h.acmod == 2 && f.phsflginu && (cplcoe[0] || cplcoe[1]) {
				bnd := -1
				var flags [18]bool
				for b := range ncplbnd {
					flags[b] = r.flag()
				}
				for s := 0; s < f.ncplsubnd; s++ {
					if s == 0 || !f.cplbndstrc[f.cplbegf+s] {
						bnd++
					}
					f.phsflg[f.cplbegf+s] = flags[bnd]
				}
			} else if !f.phsflginu {
				f.phsflg = [18]bool{}
			}
			_ = anyCoe
		} else {
			necplbnd := 0
			for sbnd := f.ecplBegin; sbnd < f.ecplEnd; sbnd++ {
				if sbnd == f.ecplBegin || !f.ecplbndstrc[sbnd] {
					necplbnd++
				}
			}
			first := -1
			r.skip(1) // reserved
			for ch := range nf {
				if !f.chincpl[ch] {
					f.firstcplcos[ch] = true
					continue
				}
				if first == -1 {
					first = ch
				}
				var param1e, rsvd bool
				if f.firstcplcos[ch] {
					param1e, rsvd = true, ch > first
					f.firstcplcos[ch] = false
				} else {
					param1e = r.flag()
					if ch > first {
						rsvd = r.flag()
					}
				}
				if param1e {
					var amp [22]float64
					for bnd := range necplbnd {
						a := r.u(5)
						if a != 31 {
							amp[bnd] = math.Ldexp(float64(ecplAmpMantTab[a])/32, -ecplAmpExpTab[a])
						}
					}
					bnd := -1
					for sbnd := f.ecplBegin; sbnd < f.ecplEnd; sbnd++ {
						if sbnd == f.ecplBegin || !f.ecplbndstrc[sbnd] {
							bnd++
						}
						f.ecplamp[ch][sbnd] = amp[bnd]
					}
				}
				if rsvd {
					r.skip(9 * (necplbnd - 1))
				}
				if ch > first {
					r.skip(1)
				}
			}
		}
	}
	// rematrixing
	if h.acmod == 2 {
		if (eac3 && blk == 0) || r.flag() {
			n := 4
			switch {
			case f.cplinu && f.ecplinu:
				n = [16]int{0, 1, 2, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4}[f.ecplbegf]
			case f.cplinu:
				switch {
				case f.cplbegf == 0:
					n = 2
				case f.cplbegf < 3:
					n = 3
				}
			case f.spxinu && f.spxbegf < 2:
				n = 3
			}
			f.rematflg = [4]bool{}
			for bnd := range n {
				f.rematflg[bnd] = r.flag()
			}
		}
	} else if blk == 0 && !eac3 && h.acmod == 2 {
		return errors.New("no rematrixing strategy in block 0")
	}
	// exponent strategies
	var cplexpstr int
	var chexpstr [5]int
	lfeexpstr := expReuse
	if eac3 {
		cplexpstr = f.cplexpstrB[blk]
		chexpstr = f.chexpstrB[blk]
		lfeexpstr = f.lfeexpstrB[blk]
	} else {
		if f.cplinu {
			cplexpstr = r.u(2)
		}
		for ch := range nf {
			chexpstr[ch] = r.u(2)
		}
		if h.lfeon {
			lfeexpstr = r.u(1)
		}
	}
	if blk == 0 {
		for ch := range nf {
			if chexpstr[ch] == expReuse {
				return errors.New("exponents reused in block 0")
			}
		}
	}
	var chbwcod [5]int
	for ch := range nf {
		if chexpstr[ch] != expReuse && !f.chincpl[ch] && !f.chinspx[ch] {
			if chbwcod[ch] = r.u(6); chbwcod[ch] > 60 {
				return errors.New("invalid channel bandwidth code")
			}
		}
	}
	// exponents
	if f.cplinu && cplexpstr != expReuse {
		abs := r.u(4) << 1
		n := f.cplendm - f.cplstrtmant
		grp := [4]int{0, 3, 6, 12}[cplexpstr]
		if err := f.decodeExps(cplexpstr, abs, n/grp, f.exps[cplCh][f.cplstrtmant-1:]); err != nil {
			return err
		}
		f.endmant[cplCh] = f.cplendm
	}
	for ch := range nf {
		if chexpstr[ch] == expReuse {
			continue
		}
		var end int
		switch {
		case f.chincpl[ch]:
			end = f.cplstrtmant
		case f.chinspx[ch] && f.spxinu:
			end = spxBandTable(f.spxBegin)
		default:
			end = 37 + 3*(chbwcod[ch]+12)
		}
		f.endmant[ch] = end
		abs := r.u(4)
		var ngrps int
		switch chexpstr[ch] {
		case expD15:
			ngrps = (end - 1) / 3
		case expD25:
			ngrps = (end - 1 + 3) / 6
		case expD45:
			ngrps = (end - 1 + 9) / 12
		}
		if err := f.decodeExps(chexpstr[ch], abs, ngrps, f.exps[ch][:]); err != nil {
			return err
		}
		r.skip(2) // gainrng
	}
	if h.lfeon && lfeexpstr != expReuse {
		f.endmant[lfeCh] = 7
		if err := f.decodeExps(expD15, r.u(4), 2, f.exps[lfeCh][:]); err != nil {
			return err
		}
	}
	// bit allocation parameters
	if !eac3 || f.bamode {
		if r.flag() {
			f.sdcycod, f.fdcycod, f.sgaincod, f.dbpbcod, f.floorcod = r.u(2), r.u(2), r.u(2), r.u(2), r.u(3)
		} else if blk == 0 {
			return errors.New("no bit allocation parameters in block 0")
		}
	} else {
		f.sdcycod, f.fdcycod, f.sgaincod, f.dbpbcod, f.floorcod = 2, 1, 1, 2, 7
	}
	if eac3 && f.snroffststr == 0 {
		f.csnroffst = f.frmcsnroffst
		for i := range f.fsnroffst {
			f.fsnroffst[i] = f.frmfsnroffst
		}
	} else if (eac3 && blk == 0) || r.flag() {
		f.csnroffst = r.u(6)
		switch {
		case eac3 && f.snroffststr == 1:
			v := r.u(4)
			for i := range f.fsnroffst {
				f.fsnroffst[i] = v
			}
		case eac3:
			if f.cplinu {
				f.fsnroffst[cplCh] = r.u(4)
			}
			for ch := range nf {
				f.fsnroffst[ch] = r.u(4)
			}
			if h.lfeon {
				f.fsnroffst[lfeCh] = r.u(4)
			}
		default:
			if f.cplinu {
				f.fsnroffst[cplCh], f.fgaincod[cplCh] = r.u(4), r.u(3)
			}
			for ch := range nf {
				f.fsnroffst[ch], f.fgaincod[ch] = r.u(4), r.u(3)
			}
			if h.lfeon {
				f.fsnroffst[lfeCh], f.fgaincod[lfeCh] = r.u(4), r.u(3)
			}
		}
	}
	if eac3 {
		if f.frmfgaincode && r.flag() {
			if f.cplinu {
				f.fgaincod[cplCh] = r.u(3)
			}
			for ch := range nf {
				f.fgaincod[ch] = r.u(3)
			}
			if h.lfeon {
				f.fgaincod[lfeCh] = r.u(3)
			}
		} else {
			for i := range f.fgaincod {
				f.fgaincod[i] = 4
			}
		}
		if h.strmtyp == 0 && r.flag() {
			r.skip(10) // convsnroffst
		}
	}
	if f.cplinu {
		leak := false
		if eac3 && f.firstcplleak {
			leak = true
			f.firstcplleak = false
		} else {
			leak = r.flag()
		}
		if leak {
			f.cplfleak, f.cplsleak = r.u(3), r.u(3)
		}
	}
	// delta bit allocation
	if !eac3 || f.dbaflde {
		if r.flag() {
			var modes [nChArr]int
			if f.cplinu {
				modes[cplCh] = r.u(2)
			}
			for ch := range nf {
				modes[ch] = r.u(2)
			}
			if f.cplinu {
				if err := readDBA(r, &f.dbas[cplCh], modes[cplCh]); err != nil {
					return err
				}
			}
			for ch := range nf {
				if err := readDBA(r, &f.dbas[ch], modes[ch]); err != nil {
					return err
				}
			}
		}
	}
	// skip field
	if !eac3 || f.skipflde {
		if r.flag() {
			r.skip(r.u(9) * 8)
		}
	}
	if r.err != nil {
		return r.err
	}
	// bit allocation
	if f.cplinu {
		f.allocate(cplCh, f.cplstrtmant, f.cplendm)
	}
	for ch := range nf {
		f.allocate(ch, 0, f.endmant[ch])
	}
	if h.lfeon {
		f.allocate(lfeCh, 0, 7)
	}
	// mantissas
	var g groups
	gotCpl := false
	for ch := range nf {
		if err := f.mantissas(ch, 0, f.endmant[ch], blk, &g); err != nil {
			return err
		}
		if f.cplinu && f.chincpl[ch] && !gotCpl {
			if err := f.mantissas(cplCh, f.cplstrtmant, f.cplendm, blk, &g); err != nil {
				return err
			}
			gotCpl = true
		}
	}
	if h.lfeon {
		if err := f.mantissas(lfeCh, 0, 7, blk, &g); err != nil {
			return err
		}
	}
	return r.err
}

// dynrngGain is the gain a dynrng word gives (clause 6.7.2.2).
func dynrngGain(v int) float64 {
	x := v >> 5
	if x >= 4 {
		x -= 8
	}
	return math.Ldexp(float64(32+v&31)/64, x+1)
}

// decodeExps decodes a channel's exponents into out, out[0] the absolute
// exponent (clause 6.1.3).
func (f *frame) decodeExps(str, abs, ngrps int, out []int) error {
	r := f.r
	grpsize := [4]int{0, 1, 2, 4}[str]
	out[0] = abs
	prev := abs
	k := 1
	for range ngrps {
		gexp := r.u(7)
		if gexp >= 125 {
			return errors.New("invalid grouped exponent")
		}
		d := [3]int{gexp / 25, gexp % 25 / 5, gexp % 5}
		for _, m := range d {
			prev += m - 2
			if prev < 0 || prev > 24 {
				return errors.New("exponent out of range")
			}
			for range grpsize {
				if k < len(out) {
					out[k] = prev
				}
				k++
			}
		}
	}
	return nil
}

// allocate runs the parametric bit allocation (clause 6.2.2) for one
// channel, leaving bap (or hebap for an AHT channel) in f.bap[ch].
func (f *frame) allocate(ch, start, end int) {
	if f.chahtinu[ch] == -1 {
		return // an AHT channel's allocation is its first block's
	}
	h := f.h
	bap := &f.bap[ch]
	zero := f.csnroffst == 0
	if zero {
		for i := range f.fsnroffst {
			if f.fsnroffst[i] != 0 {
				zero = false
			}
		}
	}
	if zero || end <= start {
		for i := start; i < end; i++ {
			bap[i] = 0
		}
		return
	}
	sdecay, fdecay := slowdec[f.sdcycod], fastdec[f.fdcycod]
	sgain, dbknee, floor := slowgain[f.sgaincod], dbpbtab[f.dbpbcod], floortab[f.floorcod]
	fgain := fastgain[f.fgaincod[ch]]
	snroffset := (((f.csnroffst - 15) << 4) + f.fsnroffst[ch]) << 2
	var psd [256]int
	exps := &f.exps[ch]
	for bin := start; bin < end; bin++ {
		psd[bin] = 3072 - exps[bin]<<7
	}
	var bndpsd, excite, mask [50]int
	j, k := start, masktab[start]
	for {
		lastbin := min(bndtab[k]+bndsz[k], end)
		bndpsd[k] = psd[j]
		j++
		for ; j < lastbin; j++ {
			bndpsd[k] = logadd(bndpsd[k], psd[j])
		}
		k++
		if end <= lastbin {
			break
		}
	}
	bndstrt := masktab[start]
	bndend := masktab[end-1] + 1
	var fastleak, slowleak, begin int
	if bndstrt == 0 {
		lowcomp := 0
		lowcomp = calcLowcomp(lowcomp, bndpsd[0], bndpsd[1], 0)
		excite[0] = bndpsd[0] - fgain - lowcomp
		lowcomp = calcLowcomp(lowcomp, bndpsd[1], bndpsd[2], 1)
		excite[1] = bndpsd[1] - fgain - lowcomp
		begin = 7
		for bin := 2; bin < 7; bin++ {
			if bndend != 7 || bin != 6 {
				lowcomp = calcLowcomp(lowcomp, bndpsd[bin], bndpsd[bin+1], bin)
			}
			fastleak = bndpsd[bin] - fgain
			slowleak = bndpsd[bin] - sgain
			excite[bin] = fastleak - lowcomp
			if bndend != 7 || bin != 6 {
				if bndpsd[bin] <= bndpsd[bin+1] {
					begin = bin + 1
					break
				}
			}
		}
		for bin := begin; bin < min(bndend, 22); bin++ {
			if bndend != 7 || bin != 6 {
				lowcomp = calcLowcomp(lowcomp, bndpsd[bin], bndpsd[bin+1], bin)
			}
			fastleak -= fdecay
			fastleak = max(fastleak, bndpsd[bin]-fgain)
			slowleak -= sdecay
			slowleak = max(slowleak, bndpsd[bin]-sgain)
			excite[bin] = max(fastleak-lowcomp, slowleak)
		}
		begin = 22
	} else {
		begin = bndstrt
		fastleak = (f.cplfleak << 8) + 768
		slowleak = (f.cplsleak << 8) + 768
	}
	for bin := begin; bin < bndend; bin++ {
		fastleak -= fdecay
		fastleak = max(fastleak, bndpsd[bin]-fgain)
		slowleak -= sdecay
		slowleak = max(slowleak, bndpsd[bin]-sgain)
		excite[bin] = max(fastleak, slowleak)
	}
	for bin := bndstrt; bin < bndend; bin++ {
		if bndpsd[bin] < dbknee {
			excite[bin] += (dbknee - bndpsd[bin]) >> 2
		}
		mask[bin] = max(excite[bin], hth[h.fscod][bin])
	}
	if d := &f.dbas[ch]; ch != lfeCh && d.mode != dbaNone {
		band := 0
		for s := range d.nseg {
			band += d.offst[s]
			delta := (d.deba[s] - 4) << 7
			if d.deba[s] >= 4 {
				delta = (d.deba[s] - 3) << 7
			}
			for range d.ln[s] {
				if band < 50 {
					mask[band] += delta
				}
				band++
			}
		}
	}
	tab := &baptab
	if f.chahtinu[ch] == 1 {
		tab = &hebaptab
	}
	i, j := start, masktab[start]
	for {
		lastbin := min(bndtab[j]+bndsz[j], end)
		m := mask[j] - snroffset - floor
		if m < 0 {
			m = 0
		}
		m &= 0x1fe0
		m += floor
		for ; i < lastbin; i++ {
			addr := (psd[i] - m) >> 5
			bap[i] = tab[min(63, max(0, addr))]
		}
		j++
		if end <= lastbin {
			break
		}
	}
}

func logadd(a, b int) int {
	c := a - b
	addr := min(abs(c)>>1, 255)
	if c >= 0 {
		return a + latab[addr]
	}
	return b + latab[addr]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func calcLowcomp(a, b0, b1, bin int) int {
	switch {
	case bin < 7:
		if b0+256 == b1 {
			a = 384
		} else if b0 > b1 {
			a = max(0, a-64)
		}
	case bin < 20:
		if b0+256 == b1 {
			a = 320
		} else if b0 > b1 {
			a = max(0, a-64)
		}
	default:
		a = max(0, a-128)
	}
	return a
}

// groups holds the partly used grouped mantissas of a block (clause 6.3.5).
type groups struct {
	b1, b2, b4 []float64
}

// mantissas reads one channel's mantissas and sets its coefficients:
// mantissa x 2^-exponent.
func (f *frame) mantissas(ch, start, end, blk int, g *groups) error {
	r := f.r
	coef := &f.coef[ch]
	for i := range coef {
		coef[i] = 0
	}
	switch f.chahtinu[ch] {
	case 1:
		if err := f.ahtMantissas(ch, start, end); err != nil {
			return err
		}
		f.chahtinu[ch] = -1
		fallthrough
	case -1:
		for bin := start; bin < end; bin++ {
			coef[bin] = math.Ldexp(f.ahtCoef(ch, bin, blk), -f.exps[ch][bin])
		}
		return nil
	}
	bap := &f.bap[ch]
	dith := ch < 5 && f.dithflag[ch]
	for bin := start; bin < end; bin++ {
		var m float64
		switch b := bap[bin]; b {
		case 0:
			if dith {
				m = f.sub.dither()
			}
		case 1:
			if len(g.b1) == 0 {
				c := r.u(5)
				if c >= 27 {
					return errors.New("invalid grouped mantissa")
				}
				g.b1 = []float64{sym(c/9, 3), sym(c%9/3, 3), sym(c%3, 3)}
			}
			m, g.b1 = g.b1[0], g.b1[1:]
		case 2:
			if len(g.b2) == 0 {
				c := r.u(7)
				if c >= 125 {
					return errors.New("invalid grouped mantissa")
				}
				g.b2 = []float64{sym(c/25, 5), sym(c%25/5, 5), sym(c%5, 5)}
			}
			m, g.b2 = g.b2[0], g.b2[1:]
		case 3:
			c := r.u(3)
			if c == 7 {
				return errors.New("invalid mantissa")
			}
			m = sym(c, 7)
		case 4:
			if len(g.b4) == 0 {
				c := r.u(7)
				if c >= 121 {
					return errors.New("invalid grouped mantissa")
				}
				g.b4 = []float64{sym(c/11, 11), sym(c%11, 11)}
			}
			m, g.b4 = g.b4[0], g.b4[1:]
		case 5:
			c := r.u(4)
			if c == 15 {
				return errors.New("invalid mantissa")
			}
			m = sym(c, 15)
		default:
			n := qntztab[b]
			m = math.Ldexp(float64(r.s(n)), 1-n)
		}
		coef[bin] = math.Ldexp(m, -f.exps[ch][bin])
	}
	return nil
}

// sym is a symmetric quantizer's value (Tables 6.19 to 6.23).
func sym(code, levels int) float64 {
	return float64(2*code-(levels-1)) / float64(levels)
}

// ahtMantissas reads the six blocks' mantissas of an AHT channel
// (E.2.4.4), leaving the AHT spectrum in f.ahtMant.
func (f *frame) ahtMantissas(ch, start, end int) error {
	r := f.r
	gaqmod := r.u(2)
	f.gaqmod[ch] = gaqmod
	endbap := 12
	if gaqmod >= 2 {
		endbap = 17
	}
	bap := &f.bap[ch]
	active := 0
	for bin := start; bin < end; bin++ {
		if bap[bin] > 7 && bap[bin] < endbap {
			active++
		}
	}
	var gains []int // 1, 2 or 4 by active bin
	switch gaqmod {
	case 1, 2:
		hi := 2
		if gaqmod == 2 {
			hi = 4
		}
		for range active {
			if r.flag() {
				gains = append(gains, hi)
			} else {
				gains = append(gains, 1)
			}
		}
	case 3:
		for range (active + 2) / 3 {
			c := r.u(5)
			if c >= 27 {
				return errors.New("invalid GAQ gain")
			}
			for _, m := range [3]int{c / 9, c % 9 / 3, c % 3} {
				gains = append(gains, 1<<m)
			}
		}
	}
	gi := 0
	for bin := start; bin < end; bin++ {
		hb := bap[bin]
		switch {
		case hb == 0:
			for n := range 6 {
				f.ahtMant[ch][n][bin] = 0
			}
		case hb < 8:
			idx := r.u(hebapBits[hb])
			v := vqTables[hb][idx]
			for n := range 6 {
				f.ahtMant[ch][n][bin] = float64(v[n]) / 32768
			}
		default:
			gain := 1
			if hb < endbap && gaqmod != 0 {
				gain = gains[gi]
				gi++
			}
			m := hebapBits[hb]
			for n := range 6 {
				f.ahtMant[ch][n][bin] = gaqMantissa(r, hb, m, gain)
			}
		}
	}
	return r.err
}

// gaqMantissa reads one mantissa of hebap 8 or more (Tables E.2.5, E.2.6).
func gaqMantissa(r *bitReader, hebap, m, gain int) float64 {
	remap := func(x float64, g int) float64 {
		row := gaqRemap[hebap-8][g]
		a := float64(row[0]) / 32768
		b := float64(row[1]) / 32768
		if x < 0 {
			b = float64(row[2]) / 32768
		}
		return x + a*x + b
	}
	switch gain {
	case 1:
		x := math.Ldexp(float64(r.s(m)), 1-m)
		return remap(x, 0)
	case 2, 4:
		gi, small, large := 1, m-1, m-1
		if gain == 4 {
			gi, small, large = 2, m-2, m
		}
		c := r.s(small)
		if c == -(1 << (small - 1)) { // the tag: a large mantissa follows
			x := math.Ldexp(float64(r.s(large)), 1-large)
			return remap(x, gi)
		}
		return math.Ldexp(float64(c), 1-small) / float64(gain)
	}
	return 0
}

// ahtCoef is block blk's mantissa of an AHT channel's bin: the inverse
// DCT of E.2.4.5 over its six AHT coefficients,
// C(k,m) = sqrt(2) sum_j R_j X(k,j) cos(j(2m+1)pi/12), R_0 = 1/sqrt(2).
func (f *frame) ahtCoef(ch, bin, blk int) float64 {
	s := 0.0
	for j := range 6 {
		rj := 1.0
		if j == 0 {
			rj = math.Sqrt2 / 2
		}
		s += rj * f.ahtMant[ch][j][bin] * math.Cos(float64(j*(2*blk+1))*math.Pi/12)
	}
	return math.Sqrt2 * s
}

// reconstruct decouples, rematrixes, synthesizes the spectral extension
// and applies dynamic range, leaving each channel's final coefficients.
func (f *frame) reconstruct(blk int) {
	h := f.h
	nf := h.nfchans
	sub := f.sub
	if f.cplinu {
		cpl := &f.coef[cplCh]
		for ch := range nf {
			if !f.chincpl[ch] {
				continue
			}
			c := &f.coef[ch]
			if f.ecplinu {
				for sbnd := f.ecplBegin; sbnd < f.ecplEnd; sbnd++ {
					for bin := ecplSubbndTab[sbnd]; bin < ecplSubbndTab[sbnd+1]; bin++ {
						c[bin] = cpl[bin] * f.ecplamp[ch][sbnd]
					}
				}
				continue
			}
			for s := f.cplbegf; s < 3+f.cplendf; s++ {
				co := f.cplco[ch][s] * 8
				if ch == 1 && h.acmod == 2 && f.phsflg[s] {
					co = -co
				}
				for bin := 37 + 12*s; bin < 49+12*s; bin++ {
					v := cpl[bin]
					if f.bap[cplCh][bin] == 0 && f.dithflag[ch] && f.chahtinu[cplCh] == 0 {
						v = math.Ldexp(sub.dither(), -f.exps[cplCh][bin])
					}
					c[bin] = v * co
				}
			}
		}
	}
	if h.acmod == 2 {
		end := min(f.endmant[0], f.endmant[1])
		if f.cplinu {
			end = f.cplstrtmant
		}
		l, rr := &f.coef[0], &f.coef[1]
		for bnd := range 4 {
			if !f.rematflg[bnd] {
				continue
			}
			for bin := rematBands[bnd]; bin < min(rematBands[bnd+1], end); bin++ {
				a, b := l[bin], rr[bin]
				l[bin], rr[bin] = a+b, a-b
			}
		}
	}
	if f.spxinu {
		for ch := range nf {
			if f.chinspx[ch] {
				f.spectralExtension(ch)
			}
		}
	}
	for ch := range nf {
		g := f.dynrng[0]
		if h.acmod == 0 && ch == 1 {
			g = f.dynrng[1]
		}
		for i := range f.coef[ch] {
			f.coef[ch][i] *= g
		}
	}
	if h.lfeon {
		for i := range f.coef[lfeCh] {
			f.coef[lfeCh][i] *= f.dynrng[0]
		}
	}
}

// spectralExtension synthesizes a channel's high frequencies (E.2.6.4).
func (f *frame) spectralExtension(ch int) {
	tc := &f.coef[ch]
	copystart := spxBandTable(f.spxstrtf)
	copyend := spxBandTable(f.spxBegin)
	copyidx, insert := copystart, copyend
	var wrap [17]bool
	for bnd := range f.nspxbnds {
		bs := f.spxbndsz[bnd]
		if copyidx+bs > copyend {
			copyidx = copystart
			wrap[bnd] = true
		}
		for range bs {
			if copyidx == copyend {
				copyidx = copystart
			}
			tc[insert] = tc[copyidx]
			insert++
			copyidx++
		}
	}
	var rms [17]float64
	m := copyend
	for bnd := range f.nspxbnds {
		bs := f.spxbndsz[bnd]
		acc := 0.0
		for range bs {
			acc += tc[m] * tc[m]
			m++
		}
		rms[bnd] = math.Sqrt(acc / float64(bs))
	}
	if f.chinspxatten[ch] {
		notch := func(center int) {
			for i := -2; i <= 2; i++ {
				b := center + i
				if b >= 0 && b < 256 {
					tc[b] *= spxAtten(f.spxattencod[ch], 2-abs(i))
				}
			}
		}
		notch(copyend)
		at := copyend + f.spxbndsz[0]
		for bnd := 1; bnd < f.nspxbnds; bnd++ {
			if wrap[bnd] {
				notch(at)
			}
			at += f.spxbndsz[bnd]
		}
	}
	m = copyend
	for bnd := range f.nspxbnds {
		bs := f.spxbndsz[bnd]
		nscale := rms[bnd] * f.nblend[ch][bnd]
		sscale := f.sblend[ch][bnd]
		co := f.spxco[ch][bnd] * 32
		for range bs {
			tc[m] = (tc[m]*sscale + f.sub.noise()*nscale) * co
			m++
		}
	}
}

// tally, when set (by tests), counts the coding tools the decoded blocks
// use.
var tally map[string]int

func (f *frame) tallyBlock() {
	h := f.h
	for ch := range h.nfchans {
		if f.blksw[ch] {
			tally["blksw"]++
		}
		if f.chincpl[ch] && f.cplinu {
			tally["coupled channel"]++
		}
		if f.chahtinu[ch] != 0 {
			tally["aht"]++
		}
		if f.chinspx[ch] && f.spxinu {
			tally["spx"]++
		}
		if f.dbas[ch].mode == dbaNew {
			tally["dba"]++
		}
	}
	if f.cplinu && h.acmod == 2 && f.phsflginu {
		tally["phsflginu"]++
	}
	if f.ecplinu && f.cplinu {
		tally["ecpl"]++
	}
	for _, v := range f.rematflg {
		if v && h.acmod == 2 {
			tally["rematrix band"]++
		}
	}
	if h.lfeon {
		tally["lfe"]++
	}
	if f.dynrng[0] != 1 {
		tally["dynrng"]++
	}
	tally["blocks"]++
	tally[fmt.Sprintf("%d-block frames", h.numblks)]++
	if !f.expstre && h.eac3 {
		tally["frame exponent strategy"]++
	}
}
