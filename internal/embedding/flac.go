package embedding

import (
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"
)

// FLAC (#1651): a video's FLAC audio track (ISO/IEC 14496-12's fLaC sample
// entry, each sample one FLAC frame), decoded from the FLAC specification
// (RFC 9639): constant, verbatim, fixed and LPC subframes, wasted bits, the
// three stereo decorrelations and both Rice residual codings, 4 to 32 bits a
// sample and 1 to 8 channels. Checked sample for sample against ffmpeg 9's
// decodes (flac_test.go).

// bitReader reads big-endian bit fields.
type bitReader struct {
	b   []byte
	pos int // in bits
}

var errShort = errors.New("the data ends early")

func (r *bitReader) bits(n int) (uint64, error) {
	if n == 0 {
		return 0, nil
	}
	if r.pos+n > 8*len(r.b) {
		return 0, errShort
	}
	var v uint64
	for n > 0 {
		byt := r.b[r.pos>>3]
		off := r.pos & 7
		take := min(8-off, n)
		v = v<<take | uint64(byt>>(8-off-take))&(1<<take-1)
		r.pos += take
		n -= take
	}
	return v, nil
}

func (r *bitReader) signed(n int) (int64, error) {
	v, err := r.bits(n)
	if n == 0 || err != nil {
		return 0, err
	}
	return int64(v<<(64-n)) >> (64 - n), nil
}

// unary counts zero bits up to a one.
func (r *bitReader) unary() (int, error) {
	n := 0
	for {
		if r.pos >= 8*len(r.b) {
			return 0, errShort
		}
		if r.b[r.pos>>3]&(0x80>>(r.pos&7)) != 0 {
			r.pos++
			return n, nil
		}
		r.pos++
		n++
	}
}

// flacStreamInfo is what a frame header may defer to STREAMINFO.
type flacStreamInfo struct {
	rate, bits, channels int
}

func parseFLACStreamInfo(b []byte) (flacStreamInfo, error) {
	if len(b) < 18 {
		return flacStreamInfo{}, fmt.Errorf("a STREAMINFO block of %d bytes", len(b))
	}
	r := bitReader{b: b, pos: 80}
	rate, _ := r.bits(20)
	ch, _ := r.bits(3)
	bps, _ := r.bits(5)
	return flacStreamInfo{rate: int(rate), channels: int(ch) + 1, bits: int(bps) + 1}, nil
}

// flacFrame is one decoded frame: per-channel samples of bits bits, and
// its size in bytes, through its CRC-16.
type flacFrame struct {
	ch   [][]int64
	bits int
	rate int
	size int
}

// decodeFLACFrame decodes one FLAC frame.
func decodeFLACFrame(b []byte, si flacStreamInfo) (flacFrame, error) {
	r := &bitReader{b: b}
	sync, err := r.bits(15)
	if err != nil || sync != 0x3FFE<<1 {
		return flacFrame{}, fmt.Errorf("a FLAC frame with no frame sync")
	}
	r.bits(1) // blocking strategy
	bsCode, _ := r.bits(4)
	rateCode, _ := r.bits(4)
	chCode, _ := r.bits(4)
	bpsCode, _ := r.bits(3)
	if res, _ := r.bits(1); res != 0 {
		return flacFrame{}, fmt.Errorf("a FLAC frame header's reserved bit is set")
	}
	// The coded frame or sample number, UTF-8 style.
	first, err := r.bits(8)
	if err != nil {
		return flacFrame{}, err
	}
	for m := uint64(0x40); first&0x80 != 0 && first&m != 0; m >>= 1 {
		if m == 1 {
			return flacFrame{}, fmt.Errorf("a FLAC frame number of more than 7 bytes")
		}
		if _, err := r.bits(8); err != nil {
			return flacFrame{}, err
		}
	}
	var blockSize int
	switch {
	case bsCode == 0:
		return flacFrame{}, fmt.Errorf("a FLAC frame with the reserved block size code")
	case bsCode == 1:
		blockSize = 192
	case bsCode <= 5:
		blockSize = 576 << (bsCode - 2)
	case bsCode == 6:
		v, err := r.bits(8)
		if err != nil {
			return flacFrame{}, err
		}
		blockSize = int(v) + 1
	case bsCode == 7:
		v, err := r.bits(16)
		if err != nil {
			return flacFrame{}, err
		}
		blockSize = int(v) + 1
	default:
		blockSize = 256 << (bsCode - 8)
	}
	rate := si.rate
	rates := [12]int{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}
	switch {
	case rateCode == 15:
		return flacFrame{}, fmt.Errorf("a FLAC frame with the forbidden sample rate code")
	case rateCode > 0 && rateCode < 12:
		rate = rates[rateCode]
	case rateCode == 12:
		v, err := r.bits(8)
		if err != nil {
			return flacFrame{}, err
		}
		rate = int(v) * 1000
	case rateCode == 13 || rateCode == 14:
		v, err := r.bits(16)
		if err != nil {
			return flacFrame{}, err
		}
		rate = int(v)
		if rateCode == 14 {
			rate *= 10
		}
	}
	bps := si.bits
	switch bpsCode {
	case 0:
	case 3:
		return flacFrame{}, fmt.Errorf("a FLAC frame with the reserved sample size code")
	default:
		bps = [8]int{0, 8, 12, 0, 16, 20, 24, 32}[bpsCode]
	}
	if bps < 4 || bps > 32 {
		return flacFrame{}, fmt.Errorf("a FLAC frame of %d bits a sample", bps)
	}
	r.bits(8) // the header's CRC-8
	channels := int(chCode) + 1
	if chCode >= 8 {
		if chCode > 10 {
			return flacFrame{}, fmt.Errorf("a FLAC frame with the reserved channel assignment %d", chCode)
		}
		channels = 2
	}
	out := flacFrame{ch: make([][]int64, channels), bits: bps, rate: rate}
	for c := range channels {
		sb := bps
		// The side channel has a bit more.
		if (chCode == 8 && c == 1) || (chCode == 9 && c == 0) || (chCode == 10 && c == 1) {
			sb++
		}
		if out.ch[c], err = decodeFLACSubframe(r, blockSize, sb); err != nil {
			return flacFrame{}, fmt.Errorf("channel %d: %w", c, err)
		}
	}
	a, s := out.ch[0], []int64(nil)
	if channels == 2 {
		s = out.ch[1]
	}
	switch chCode {
	case 8: // left, side
		for i := range s {
			s[i] = a[i] - s[i]
		}
	case 9: // side, right
		for i := range a {
			a[i] += s[i]
		}
	case 10: // mid, side
		for i := range a {
			m := a[i]<<1 | s[i]&1
			a[i], s[i] = (m+s[i])>>1, (m-s[i])>>1
		}
	}
	// Zero padding to a byte, then the frame's CRC-16.
	out.size = (r.pos+7)/8 + 2
	return out, nil
}

// DecodeFLACFile decodes a FLAC file (RFC 9639: "fLaC", its metadata
// blocks, then its frames back to back) to mono samples, its channels
// averaged, at the stream's rate, for Speech-to-Text's FLAC (#1799,
// internal/speech). maxSeconds bounds it: longer audio is a tooLongError,
// which IsTooLong reports. Frame CRCs are not checked; a stream that does
// not parse is an error, never partial audio.
func DecodeFLACFile(data []byte, maxSeconds int) (mono []float32, rate int, err error) {
	return decodeFLACFile(data, maxSeconds, false)
}

// DecodeFLACChannels is DecodeFLACFile with the channels kept apart:
// interleaved samples in [-1, 1], their count, and the rate (#1799,
// Speech-to-Text's per-channel recognition).
func DecodeFLACChannels(data []byte, maxSeconds int) (interleaved []float32, channels, rate int, err error) {
	s, rate, err := decodeFLACFile(data, maxSeconds, true)
	if err != nil {
		return nil, 0, 0, err
	}
	return s, flacChannels(data), rate, nil
}

// flacChannels is the channel count of a FLAC file DecodeFLACFile read.
func flacChannels(data []byte) int {
	if len(data) >= 8+18 {
		if si, err := parseFLACStreamInfo(data[8:]); err == nil {
			return si.channels
		}
	}
	return 1
}

func decodeFLACFile(data []byte, maxSeconds int, keep bool) (mono []float32, rate int, err error) {
	if len(data) < 4 || string(data[:4]) != "fLaC" {
		return nil, 0, errors.New("the data is not a FLAC stream")
	}
	pos := 4
	var si *flacStreamInfo
	for last := false; !last; {
		if pos+4 > len(data) {
			return nil, 0, fmt.Errorf("the FLAC metadata: %w", errShort)
		}
		last = data[pos]&0x80 != 0
		typ, n := data[pos]&0x7f, int(data[pos+1])<<16|int(data[pos+2])<<8|int(data[pos+3])
		pos += 4
		if pos+n > len(data) {
			return nil, 0, fmt.Errorf("the FLAC metadata: %w", errShort)
		}
		if typ == 0 {
			v, err := parseFLACStreamInfo(data[pos : pos+n])
			if err != nil {
				return nil, 0, err
			}
			si = &v
		}
		pos += n
	}
	if si == nil || si.rate == 0 {
		return nil, 0, errors.New("the FLAC stream has no STREAMINFO")
	}
	for i := 0; pos < len(data); i++ {
		f, err := decodeFLACFrame(data[pos:], *si)
		if err != nil {
			return nil, 0, fmt.Errorf("the FLAC stream's frame %d: %v", i, err)
		}
		if f.rate != si.rate {
			return nil, 0, notDecodedf("its FLAC frames change from %d Hz to %d Hz, which is not decoded", si.rate, f.rate)
		}
		if keep {
			scale := float32(int64(1) << (f.bits - 1))
			for j := range f.ch[0] {
				for c := range f.ch {
					mono = append(mono, float32(f.ch[c][j])/scale)
				}
			}
			if len(mono) > maxSeconds*si.rate*len(f.ch) {
				return nil, 0, &tooLongError{float64(len(mono)) / float64(si.rate*len(f.ch))}
			}
			pos += f.size
			continue
		}
		scale := float32(int64(1)<<(f.bits-1)) * float32(len(f.ch))
		for j := range f.ch[0] {
			var sum int64
			for c := range f.ch {
				sum += f.ch[c][j]
			}
			mono = append(mono, float32(sum)/scale)
		}
		if len(mono) > maxSeconds*si.rate {
			return nil, 0, &tooLongError{float64(len(mono)) / float64(si.rate)}
		}
		pos += f.size
	}
	if len(mono) == 0 {
		return nil, 0, errors.New("the FLAC stream has no audio frames")
	}
	return mono, si.rate, nil
}

func decodeFLACSubframe(r *bitReader, n, bps int) ([]int64, error) {
	pad, err := r.bits(1)
	if err != nil {
		return nil, err
	}
	if pad != 0 {
		return nil, fmt.Errorf("a subframe's padding bit is set")
	}
	typ, _ := r.bits(6)
	wasted := 0
	if w, _ := r.bits(1); w != 0 {
		k, err := r.unary()
		if err != nil {
			return nil, err
		}
		wasted = k + 1
		if wasted >= bps {
			return nil, fmt.Errorf("%d wasted bits of %d", wasted, bps)
		}
		bps -= wasted
	}
	s := make([]int64, n)
	switch {
	case typ == 0: // constant
		v, err := r.signed(bps)
		if err != nil {
			return nil, err
		}
		for i := range s {
			s[i] = v
		}
	case typ == 1: // verbatim
		for i := range s {
			if s[i], err = r.signed(bps); err != nil {
				return nil, err
			}
		}
	case typ >= 8 && typ <= 12: // fixed
		order := int(typ - 8)
		if order > n {
			return nil, fmt.Errorf("a fixed predictor of order %d for %d samples", order, n)
		}
		for i := range order {
			if s[i], err = r.signed(bps); err != nil {
				return nil, err
			}
		}
		if err := flacResidual(r, s, order); err != nil {
			return nil, err
		}
		fixed := [5][]int64{{}, {1}, {2, -1}, {3, -3, 1}, {4, -6, 4, -1}}[order]
		for i := order; i < n; i++ {
			var p int64
			for j, c := range fixed {
				p += c * s[i-1-j]
			}
			s[i] += p
		}
	case typ >= 32: // LPC
		order := int(typ-32) + 1
		if order > n {
			return nil, fmt.Errorf("an LPC predictor of order %d for %d samples", order, n)
		}
		for i := range order {
			if s[i], err = r.signed(bps); err != nil {
				return nil, err
			}
		}
		prec, err := r.bits(4)
		if err != nil {
			return nil, err
		}
		if prec == 15 {
			return nil, fmt.Errorf("an LPC coefficient precision of 16")
		}
		shift, err := r.signed(5)
		if err != nil {
			return nil, err
		}
		if shift < 0 {
			return nil, fmt.Errorf("a negative LPC shift")
		}
		coef := make([]int64, order)
		for i := range coef {
			if coef[i], err = r.signed(int(prec) + 1); err != nil {
				return nil, err
			}
		}
		if err := flacResidual(r, s, order); err != nil {
			return nil, err
		}
		for i := order; i < n; i++ {
			var p int64
			for j, c := range coef {
				p += c * s[i-1-j]
			}
			s[i] += p >> shift
		}
	default:
		return nil, fmt.Errorf("the reserved subframe type %d", typ)
	}
	if wasted > 0 {
		for i := range s {
			s[i] <<= wasted
		}
	}
	return s, nil
}

// flacResidual adds the Rice-coded residual to s[order:].
func flacResidual(r *bitReader, s []int64, order int) error {
	method, err := r.bits(2)
	if err != nil {
		return err
	}
	if method > 1 {
		return fmt.Errorf("the reserved residual coding method %d", method)
	}
	pbits, escape := 4, uint64(15)
	if method == 1 {
		pbits, escape = 5, 31
	}
	po, err := r.bits(4)
	if err != nil {
		return err
	}
	parts := 1 << po
	if len(s)%parts != 0 || len(s)>>po < order {
		return fmt.Errorf("a residual partition order of %d for %d samples", po, len(s))
	}
	i := order
	for p := range parts {
		count := len(s) >> po
		if p == 0 {
			count -= order
		}
		k, err := r.bits(pbits)
		if err != nil {
			return err
		}
		if k == escape {
			raw, err := r.bits(5)
			if err != nil {
				return err
			}
			for range count {
				if s[i], err = r.signed(int(raw)); err != nil {
					return err
				}
				i++
			}
			continue
		}
		for range count {
			q, err := r.unary()
			if err != nil {
				return err
			}
			lo, err := r.bits(int(k))
			if err != nil {
				return err
			}
			u := uint64(q)<<k | lo
			s[i] = int64(u>>1) ^ -int64(u&1)
			i++
		}
	}
	return nil
}

// flacChannelLayouts are the channels ffmpeg's FLAC decoder assigns a
// stream of n channels (RFC 9639's assignments).
var flacChannelLayouts = [9][]string{
	1: {"FC"},
	2: {"FL", "FR"},
	3: {"FL", "FR", "FC"},
	4: {"FL", "FR", "BL", "BR"},
	5: {"FL", "FR", "FC", "BL", "BR"},
	6: {"FL", "FR", "FC", "LFE", "BL", "BR"},
	7: {"FL", "FR", "FC", "LFE", "BC", "SL", "SR"},
	8: {"FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"},
}

// decodeFLACTrack decodes a video's FLAC track to interleaved samples, as
// ffmpeg's flac decoder outputs them (s16 for up to 16 bits, else s32,
// then float).
func decodeFLACTrack(tr *soundTrack, e *mp4.AudioSampleEntryBox) (pcm []float32, channels, rate int, layout []string, err error) {
	si, err := flacTrackInfo(e)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	for i, au := range tr.samples {
		f, err := decodeFLACFrame(au, si)
		if err != nil {
			return nil, 0, 0, nil, fmt.Errorf("the FLAC track's frame %d: %v", i, err)
		}
		if channels == 0 {
			channels, rate = len(f.ch), f.rate
		} else if len(f.ch) != channels {
			return nil, 0, 0, nil, notDecodedf("its FLAC frames change from %d to %d channels, which is not decoded", channels, len(f.ch))
		}
		if len(pcm)/channels > (MaxAudioSeconds+60)*max(rate, 1) {
			return nil, 0, 0, nil, &tooLongError{float64(len(pcm)/channels) / float64(rate)}
		}
		scale, shift := float32(1.0/32768), 16-f.bits
		if f.bits > 16 {
			scale, shift = 1.0/(1<<31), 32-f.bits
		}
		for j := range f.ch[0] {
			for c := range channels {
				pcm = append(pcm, float32(int32(f.ch[c][j]<<shift))*scale)
			}
		}
	}
	if rate == 0 {
		return nil, 0, 0, nil, fmt.Errorf("the FLAC track has no sample rate")
	}
	return pcm, channels, rate, flacChannelLayouts[channels], nil
}

// flacTrackInfo reads the STREAMINFO of a fLaC sample entry's dfLa box.
func flacTrackInfo(e *mp4.AudioSampleEntryBox) (flacStreamInfo, error) {
	if e == nil || e.DfLa == nil {
		return flacStreamInfo{}, fmt.Errorf("the fLaC track has no dfLa box")
	}
	for _, b := range e.DfLa.MetadataBlocks {
		if b.BlockType == 0 {
			return parseFLACStreamInfo(b.BlockData)
		}
	}
	return flacStreamInfo{}, fmt.Errorf("the fLaC track's dfLa box has no STREAMINFO")
}
