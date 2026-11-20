package speech

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	opus "github.com/darui3018823/opus"
	"github.com/darui3018823/opus/oggopus"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
)

// Opus audio for Speech-to-Text's OGG_OPUS and WEBM_OPUS encodings (#1799):
// Opus in Ogg (RFC 7845) and Opus in WebM (Matroska, its A_OPUS codec
// mapping), decoded with the pure-Go darui3018823/opus decoder (BSD-2-Clause,
// already linked for MP4 Opus tracks, #1739) at 48 kHz, as libopus decodes
// it: the header's output gain applied, the pre-skip (Ogg's OpusHead or
// WebM's CodecDelay) and the end trimming (Ogg's final granule position or
// WebM's DiscardPadding) dropped, then mixed to mono and resampled to
// SampleRate like every other format here. Mapping families 0 (mono and
// stereo) and 1 (surround, up to 8 channels) are decoded; the ambisonic
// families 2 and 3 are refused 501.

const opusRate = 48000

// opusStream decodes one Opus stream's packets to interleaved samples.
type opusStream interface {
	DecodeFloat32(packet []byte) ([]float32, error)
}

// opusHead is what a decoder needs of an OpusHead.
type opusHead struct {
	channels, family, streams, coupled int
	mapping                            []byte
	preSkip                            int
	gain                               float32
}

func headOf(h oggopus.Head) opusHead {
	return opusHead{channels: int(h.Channels), family: int(h.MappingFamily), streams: int(h.StreamCount), coupled: int(h.CoupledCount),
		mapping: h.ChannelMapping, preSkip: int(h.PreSkip), gain: float32(math.Pow(10, float64(h.OutputGain)/(20*256)))}
}

func (h opusHead) decoder() (opusStream, error) {
	switch {
	case h.family == 0 && h.channels >= 1 && h.channels <= 2:
		return opus.NewDecoder(opusRate, h.channels)
	case h.family == 1 && h.channels >= 1 && h.channels <= 8:
		if len(h.mapping) != h.channels || h.streams < 1 || h.coupled > h.streams {
			return nil, fmt.Errorf("the Opus channel mapping (%d streams, %d coupled, %d entries) does not fit its %d channels",
				h.streams, h.coupled, len(h.mapping), h.channels)
		}
		return opus.NewMultistreamDecoder(opusRate, h.channels, h.streams, h.coupled, h.mapping)
	}
	return nil, fmt.Errorf("%w: Opus of channel mapping family %d (%d channels) has no local decoder in CloudBurrow; "+
		"families 0 and 1 (mono to 7.1) are decoded", ErrUnsupported, h.family, h.channels)
}

// opusPCM collects one stream's decoded samples, interleaved at 48 kHz.
type opusPCM struct {
	h   opusHead
	d   opusStream
	pcm []float32
	max int // samples per channel
}

func newOpusPCM(h opusHead, maxSeconds int) (*opusPCM, error) {
	d, err := h.decoder()
	if err != nil {
		return nil, err
	}
	return &opusPCM{h: h, d: d, max: (maxSeconds + 1) * opusRate}, nil
}

// add decodes a packet, dropping skip samples per channel from its start
// and trim from its end: the packet's samples per channel.
func (p *opusPCM) add(packet []byte, skip, trim int) (int, error) {
	out, err := p.d.DecodeFloat32(packet)
	if err != nil {
		return 0, err
	}
	ch := p.h.channels
	n := len(out) / ch
	skip, trim = min(max(skip, 0), n), min(max(trim, 0), n)
	if skip+trim >= n {
		return n, nil
	}
	for _, v := range out[skip*ch : (n-trim)*ch] {
		p.pcm = append(p.pcm, v*p.h.gain)
	}
	if len(p.pcm)/ch > p.max {
		return n, ErrTooLong
	}
	return n, nil
}

// mono is the samples mixed to one channel at SampleRate.
func (p *opusPCM) mono() []float32 {
	return embedding.Resample(embedding.Average(p.pcm, p.h.channels), opusRate, SampleRate)
}

// DecodeOggOpus decodes Opus in Ogg to mono samples at SampleRate, chained
// streams one after another.
func DecodeOggOpus(data []byte, maxSeconds int) ([]float32, error) {
	r, err := oggopus.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the Ogg Opus stream: %v", err)
	}
	var (
		mono []float32
		p    *opusPCM
		link = -1
	)
	for {
		pk, err := r.NextPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the Ogg Opus stream: %v", err)
		}
		if pk.LinkIndex != link {
			if p != nil {
				mono = append(mono, p.mono()...)
			}
			link = pk.LinkIndex
			if p, err = newOpusPCM(headOf(r.Head), maxSeconds); err != nil {
				return nil, err
			}
		}
		if _, err := p.add(pk.Data, pk.DiscardStart, pk.DiscardEnd); err != nil {
			if errors.Is(err, ErrTooLong) {
				return nil, err
			}
			return nil, fmt.Errorf("the Ogg Opus stream's packet: %v", err)
		}
	}
	if p == nil {
		return nil, errors.New("the Ogg Opus stream has no audio")
	}
	return append(mono, p.mono()...), nil
}

// Matroska element IDs (https://www.matroska.org/technical/elements.html)
// read by DecodeWebMOpus.
const (
	mkvEBML           = 0x1A45DFA3
	mkvDocType        = 0x4282
	mkvSegment        = 0x18538067
	mkvTracks         = 0x1654AE6B
	mkvTrackEntry     = 0xAE
	mkvTrackNumber    = 0xD7
	mkvTrackType      = 0x83
	mkvCodecID        = 0x86
	mkvCodecPrivate   = 0x63A2
	mkvCodecDelay     = 0x56AA
	mkvCluster        = 0x1F43B675
	mkvSimpleBlock    = 0xA3
	mkvBlockGroup     = 0xA0 // read whole: Block and DiscardPadding
	mkvBlock          = 0xA1
	mkvDiscardPadding = 0x75A2
)

// mkvMasters are the master elements whose children are read in place.
var mkvMasters = map[uint64]bool{mkvSegment: true, mkvTracks: true, mkvTrackEntry: true, mkvCluster: true}

// vint reads an EBML variable-length integer: its value with the length
// marker removed (or kept, for IDs), its length, and whether every value
// bit is 1 (an unknown size).
func vint(b []byte, keepMarker bool) (v uint64, n int, unknown bool, err error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false, errors.New("invalid EBML variable-length integer")
	}
	for n = 1; b[0]&(0x80>>(n-1)) == 0; n++ {
	}
	if len(b) < n {
		return 0, 0, false, io.ErrUnexpectedEOF
	}
	v = uint64(b[0])
	if !keepMarker {
		v &= uint64(0xFF >> n)
	}
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	all := uint64(1)<<(7*n) - 1
	return v, n, !keepMarker && v == all, nil
}

func mkvUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func mkvInt(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	v := int64(int8(b[0]))
	for _, c := range b[1:] {
		v = v<<8 | int64(c)
	}
	return v
}

// mkvFrames splits a Block's or SimpleBlock's payload into its track
// number and frames, undoing Xiph, fixed-size or EBML lacing.
func mkvFrames(b []byte) (uint64, [][]byte, error) {
	track, n, _, err := vint(b, false)
	if err != nil || len(b) < n+3 {
		return 0, nil, errors.New("a WebM block is truncated")
	}
	flags := b[n+2]
	b = b[n+3:]
	lacing := flags >> 1 & 3
	if lacing == 0 {
		return track, [][]byte{b}, nil
	}
	if len(b) < 1 {
		return 0, nil, errors.New("a WebM block's lacing is truncated")
	}
	count := int(b[0]) + 1
	b = b[1:]
	sizes := make([]int, count-1)
	switch lacing {
	case 1: // Xiph
		for i := range sizes {
			for {
				if len(b) == 0 {
					return 0, nil, errors.New("a WebM block's lacing is truncated")
				}
				c := b[0]
				sizes[i] += int(c)
				b = b[1:]
				if c != 255 {
					break
				}
			}
		}
	case 2: // fixed
		if len(b)%count != 0 {
			return 0, nil, errors.New("a WebM block's fixed lacing does not divide its frames")
		}
		for i := range sizes {
			sizes[i] = len(b) / count
		}
	case 3: // EBML
		first, n, _, err := vint(b, false)
		if err != nil {
			return 0, nil, err
		}
		b = b[n:]
		if count > 1 {
			sizes[0] = int(first)
		}
		for i := 1; i < len(sizes); i++ {
			raw, n, _, err := vint(b, false)
			if err != nil {
				return 0, nil, err
			}
			b = b[n:]
			bias := int64(1)<<(7*n-1) - 1
			sizes[i] = sizes[i-1] + int(int64(raw)-bias)
		}
	}
	frames := make([][]byte, 0, count)
	for _, s := range sizes {
		if s < 0 || s > len(b) {
			return 0, nil, errors.New("a WebM block's laced frame sizes exceed the block")
		}
		frames = append(frames, b[:s])
		b = b[s:]
	}
	return track, append(frames, b), nil
}

// mkvTrack is a TrackEntry as DecodeWebMOpus reads it.
type mkvTrack struct {
	number, kind uint64
	codec        string
	private      []byte
	delay        int64 // CodecDelay, ns
}

// mkvElement is one element: its ID, its body (nil for a master read in
// place) and the rest of the data after its header (a master) or body.
func mkvElement(b []byte) (id uint64, body, rest []byte, master bool, err error) {
	id, n, _, err := vint(b, true)
	if err != nil {
		return 0, nil, nil, false, err
	}
	size, m, unknown, err := vint(b[n:], false)
	if err != nil {
		return 0, nil, nil, false, err
	}
	b = b[n+m:]
	if mkvMasters[id] {
		return id, nil, b, true, nil
	}
	if unknown || size > uint64(len(b)) {
		return id, nil, nil, false, io.ErrUnexpectedEOF
	}
	return id, b[:size], b[size:], false, nil
}

// DecodeWebMOpus decodes the first Opus audio track of a WebM (Matroska)
// file to mono samples at SampleRate. Its elements are read in order, the
// master elements Segment, Tracks, TrackEntry and Cluster in place, so
// live files with unknown-size Segments and Clusters read too; each
// SimpleBlock's or BlockGroup's frames are decoded in file order, a
// BlockGroup's DiscardPadding trimming its last frame.
func DecodeWebMOpus(data []byte, maxSeconds int) ([]float32, error) {
	var (
		tracks  []*mkvTrack
		cur     *mkvTrack
		p       *opusPCM
		opusNum uint64
		skip    int // pre-skip samples per channel still to drop
	)
	start := func() error {
		for _, t := range tracks {
			if t.kind != 2 || t.codec != "A_OPUS" {
				continue
			}
			h, err := oggopus.ParseHead(t.private)
			if err != nil {
				return fmt.Errorf("the WebM Opus track's CodecPrivate is not an OpusHead: %v", err)
			}
			oh := headOf(h)
			if t.delay > 0 {
				oh.preSkip = int(t.delay * opusRate / 1e9)
			}
			if p, err = newOpusPCM(oh, maxSeconds); err != nil {
				return err
			}
			opusNum, skip = t.number, oh.preSkip
			return nil
		}
		return errors.New("the WebM file has no Opus audio track")
	}
	block := func(body []byte, padding int64) error {
		if p == nil {
			if err := start(); err != nil {
				return err
			}
		}
		track, frames, err := mkvFrames(body)
		if err != nil {
			return err
		}
		if track != opusNum {
			return nil
		}
		for i, f := range frames {
			trim := 0
			if i == len(frames)-1 && padding > 0 {
				trim = int(padding * opusRate / 1e9)
			}
			n, err := p.add(f, skip, trim)
			if err != nil {
				if errors.Is(err, ErrTooLong) {
					return err
				}
				return fmt.Errorf("the WebM Opus track's frame: %v", err)
			}
			skip = max(skip-n, 0)
		}
		return nil
	}
	b := data
	first := true
	for len(b) > 0 {
		id, body, rest, master, err := mkvElement(b)
		if err != nil {
			if p != nil && errors.Is(err, io.ErrUnexpectedEOF) && id != mkvSimpleBlock && id != mkvBlockGroup {
				break // a truncated trailing element (Cues, Tags...)
			}
			return nil, fmt.Errorf("the WebM file is truncated or malformed: %v", err)
		}
		b = rest
		if first {
			if id != mkvEBML {
				return nil, errors.New("the WebM file does not start with an EBML header")
			}
			first = false
			docType := ""
			for e := body; len(e) > 0; {
				cid, cb, crest, _, err := mkvElement(e)
				if err != nil {
					break
				}
				if cid == mkvDocType {
					docType = string(bytes.TrimRight(cb, "\x00"))
				}
				e = crest
			}
			if docType != "webm" && docType != "matroska" {
				return nil, fmt.Errorf("the EBML file's DocType is %q, not webm", docType)
			}
			continue
		}
		if master {
			if id == mkvTrackEntry {
				cur = &mkvTrack{}
				tracks = append(tracks, cur)
			}
			continue // its children follow in place
		}
		switch id {
		case mkvTrackNumber, mkvTrackType, mkvCodecID, mkvCodecPrivate, mkvCodecDelay:
			if cur == nil {
				continue
			}
			switch id {
			case mkvTrackNumber:
				cur.number = mkvUint(body)
			case mkvTrackType:
				cur.kind = mkvUint(body)
			case mkvCodecID:
				cur.codec = string(bytes.TrimRight(body, "\x00"))
			case mkvCodecPrivate:
				cur.private = body
			case mkvCodecDelay:
				cur.delay = int64(mkvUint(body))
			}
		case mkvSimpleBlock:
			if err := block(body, 0); err != nil {
				return nil, err
			}
		case mkvBlockGroup:
			var blk []byte
			var padding int64
			for e := body; len(e) > 0; {
				cid, cb, crest, _, err := mkvElement(e)
				if err != nil {
					return nil, fmt.Errorf("the WebM file's BlockGroup is malformed: %v", err)
				}
				switch cid {
				case mkvBlock:
					blk = cb
				case mkvDiscardPadding:
					padding = mkvInt(cb)
				}
				e = crest
			}
			if blk != nil {
				if err := block(blk, padding); err != nil {
					return nil, err
				}
			}
		}
	}
	if p == nil {
		if err := start(); err != nil {
			return nil, err
		}
	}
	return p.mono(), nil
}
