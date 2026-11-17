// Package ac3 decodes AC-3 and Enhanced AC-3 (E-AC-3) audio.
//
// It is written for CloudBurrow from the specification alone: ATSC A/52,
// "Digital Audio Compression (AC-3, E-AC-3) Standard"
// (https://www.atsc.org/atsc-documents/a522012-digital-audio-compression-ac-3-e-ac-3-standard-12172012/),
// in the text ETSI publishes as TS 102 366 V1.4.1 (2017-09)
// (https://www.etsi.org/deliver/etsi_ts/102300_102399/102366/01.04.01_60/ts_102366v010401p.pdf),
// whose clauses and tables the code names. No other decoder's source was
// read.
//
// AC-3 (bsid 0 to 8, with Annex D's alternate bit stream syntax) and E-AC-3
// (bsid 11 to 16, Annex E) are decoded: every audio coding mode and the
// LFE, exponent strategies, the parametric bit allocation with delta bit
// allocation, grouped and asymmetric mantissas with dither, coupling with
// phase flags, rematrixing, dynamic range control (dynrng, applied at full
// scale as ffmpeg's decoder does by default), block switching and the
// IMDCT; and for E-AC-3 the audio frame's strategies, 1, 2, 3 or 6 blocks a
// syncframe, the reduced sample rates, the adaptive hybrid transform with
// vector and gain adaptive quantization, spectral extension with its
// attenuation, enhanced coupling, and dependent substreams, whose channels
// replace or extend independent substream 0's. Other independent
// substreams (other programmes) are skipped, as E.2.8.3 allows. Transient
// pre-noise processing (E.2.7) is read and not applied, as ffmpeg does not.
package ac3

import (
	"errors"
	"fmt"
	"sort"
)

// Decoder decodes an AC-3 or E-AC-3 stream, one MP4 sample at a time.
type Decoder struct {
	subs   map[int]*substream // by strmtyp<<3 | substreamid
	layout []string
	cmix   float64
	surmix float64
}

// NewDecoder returns a decoder.
func NewDecoder() *Decoder {
	return &Decoder{subs: map[int]*substream{}, cmix: cmixLevels[1], surmix: surMixLevels[1]}
}

// Layout names the channels of the last decoded output, in their order:
// FL FR FC LFE BL BR FLC FRC BC SL SR and so on, ffmpeg's names and order.
func (d *Decoder) Layout() []string { return d.layout }

// MixLevels returns the centre and surround downmix levels (linear gains)
// the stream carries, as ffmpeg's decoder exports them: the Lo/Ro levels of
// E-AC-3's mixing metadata or AC-3's Annex D extended bsi when present,
// else AC-3's cmixlev and surmixlev, else -4.5 dB and -6 dB.
func (d *Decoder) MixLevels() (center, surround float64) { return d.cmix, d.surmix }

// order is ffmpeg's native channel order.
var order = []string{"FL", "FR", "FC", "LFE", "BL", "BR", "FLC", "FRC", "BC", "SL", "SR",
	"TC", "TFL", "TFC", "TFR", "TBL", "TBC", "TBR", "WL", "WR", "SDL", "SDR", "LFE2", "TSL", "TSR"}

// chanmapNames are the locations of Table E.1.4's bits, MSB first.
var chanmapNames = [16][]string{
	{"FL"}, {"FC"}, {"FR"}, {"SL"}, {"SR"}, {"FLC", "FRC"}, {"BL", "BR"}, {"BC"},
	{"TC"}, {"SDL", "SDR"}, {"WL", "WR"}, {"TFL", "TFR"}, {"TFC"}, {"TSL", "TSR"}, {"LFE2"}, {"LFE"},
}

// Decode decodes one MP4 sample (one or more syncframes: an AC-3
// syncframe, or E-AC-3 independent and dependent substreams) to
// interleaved PCM, full scale 1.0, in Layout's order.
func (d *Decoder) Decode(sample []byte) (pcm []float32, channels, rate int, err error) {
	var (
		cur      map[string][]float64 // the programme being assembled
		curRate  int
		curNames []string
		skipping bool
	)
	flush := func() error {
		if cur == nil {
			return nil
		}
		names := append([]string(nil), curNames...)
		sort.SliceStable(names, func(i, j int) bool { return rank(names[i]) < rank(names[j]) })
		if channels != 0 && (len(names) != channels || curRate != rate) {
			return errors.New("ac3: the channel layout changes within a sample")
		}
		channels, rate = len(names), curRate
		d.layout = names
		n := len(cur[names[0]])
		start := len(pcm)
		pcm = append(pcm, make([]float32, n*len(names))...)
		for c, name := range names {
			s := cur[name]
			for i := range n {
				pcm[start+i*len(names)+c] = float32(s[i])
			}
		}
		cur = nil
		return nil
	}
	for len(sample) > 0 {
		h, r, err := parseHeader(sample)
		if err != nil {
			return nil, 0, 0, err
		}
		if h.frameBytes > len(sample) {
			return nil, 0, 0, fmt.Errorf("ac3: a %d-byte syncframe in %d bytes", h.frameBytes, len(sample))
		}
		r.b = sample[:h.frameBytes]
		sample = sample[h.frameBytes:]
		dependent := h.eac3 && h.strmtyp == 1
		if testAsDependent != nil {
			dependent = dependent || testAsDependent(h)
		}
		independent := !dependent
		if independent {
			if err := flush(); err != nil {
				return nil, 0, 0, err
			}
			skipping = h.eac3 && h.substreamid != 0
			if skipping {
				continue
			}
		} else if skipping || cur == nil {
			continue // a dependent substream of a skipped programme
		}
		key := btoi(dependent)<<3 | h.substreamid
		if !h.eac3 {
			key = 0
		}
		sub := d.subs[key]
		if sub == nil {
			sub = &substream{rng: 1}
			d.subs[key] = sub
		}
		out, err := sub.decodeFrame(h, r)
		if err != nil {
			return nil, 0, 0, err
		}
		names := append([]string(nil), acmodNames[h.acmod]...)
		if h.lfeon {
			names = append(names, "LFE")
		}
		if !independent && h.chanmap >= 0 {
			names = names[:0]
			for bit := range 16 {
				if h.chanmap&(1<<(15-bit)) != 0 {
					names = append(names, chanmapNames[bit]...)
				}
			}
			if len(names) != len(out) {
				return nil, 0, 0, errors.New("ac3: a dependent substream's channel map does not match its channels")
			}
		}
		if independent {
			cur, curRate, curNames = map[string][]float64{}, h.rate, nil
			d.setMixLevels(h)
		} else if h.rate != curRate || len(out[0]) != len(cur[curNames[0]]) {
			return nil, 0, 0, errors.New("ac3: a dependent substream differs from its independent substream")
		}
		for i, name := range names {
			if _, ok := cur[name]; !ok {
				curNames = append(curNames, name)
			}
			cur[name] = out[i]
		}
	}
	if err := flush(); err != nil {
		return nil, 0, 0, err
	}
	if channels == 0 {
		return nil, 0, 0, errors.New("ac3: no syncframe")
	}
	return pcm, channels, rate, nil
}

func (d *Decoder) setMixLevels(h *header) {
	d.cmix, d.surmix = cmixLevels[1], surMixLevels[1]
	if h.hasCmix {
		d.cmix = h.cmixlev
	}
	if h.hasSurmix {
		d.surmix = h.surmixlev
	}
	// The Lo/Ro levels, when given (E-AC-3's mixing metadata, AC-3's
	// Annex D xbsi1), are the ones ffmpeg's decoder exports and downmixes
	// with.
	if h.hasXmix {
		if h.acmod&1 != 0 && h.acmod > 2 {
			d.cmix = h.loroCmix
		}
		if h.acmod&4 != 0 {
			d.surmix = h.loroSurm
		}
	}
}

func rank(name string) int {
	for i, n := range order {
		if n == name {
			return i
		}
	}
	return len(order)
}

// testAsDependent, set by tests, makes Decode treat a syncframe as a
// dependent substream (it may set the header's chanmap).
var testAsDependent func(h *header) bool
