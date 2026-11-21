package embedding

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/Eyevinn/mp4ff/avc"
	mp4hevc "github.com/Eyevinn/mp4ff/hevc"
)

// A Matroska (MKV) and WebM demuxer (#1651), for multimodalembedding@001,
// which takes both containers. It reads the first video track's frames,
// written from the Matroska specification (RFC 9559) and WebM's container
// guidelines, into the videoTrack the MP4 reader makes, so the same H.264,
// H.265, VP9 and AV1 decoders read them. Laced blocks (which video tracks do
// not use) and compressed tracks (ContentEncoding) are ErrNotDecoded.

// MatroskaMIMETypes are the Matroska containers demuxed here, for a caller
// that sets VideoOptions.Matroska.
var MatroskaMIMETypes = []string{"video/webm", "video/x-matroska"}

var matroskaMIMETypes = map[string]bool{"video/webm": true, "video/x-matroska": true, "video/mkv": true}

// isMatroska reports whether data starts with an EBML header.
func isMatroska(data []byte) bool {
	return len(data) >= 4 && binary.BigEndian.Uint32(data) == mkvEBML
}

const (
	mkvEBML            = 0x1A45DFA3
	mkvSegment         = 0x18538067
	mkvInfo            = 0x1549A966
	mkvTimecodeScale   = 0x2AD7B1
	mkvDuration        = 0x4489
	mkvTracks          = 0x1654AE6B
	mkvTrackEntry      = 0xAE
	mkvTrackNumber     = 0xD7
	mkvTrackType       = 0x83
	mkvCodecID         = 0x86
	mkvCodecPrivate    = 0x63A2
	mkvDefaultDuration = 0x23E383
	mkvContentEncs     = 0x6D80
	mkvCluster         = 0x1F43B675
	mkvTimecode        = 0xE7
	mkvSimpleBlock     = 0xA3
	mkvBlockGroup      = 0xA0
	mkvBlock           = 0xA1
	mkvBlockDuration   = 0x9B
	mkvReferenceBlock  = 0xFB
)

// mkvTopLevel are the Segment's children: an unknown-size Cluster ends at
// the next of them.
var mkvTopLevel = map[uint32]bool{
	0x114D9B74: true, mkvInfo: true, mkvTracks: true, mkvCluster: true, 0x1C53BB6B: true,
	0x1043A770: true, 0x1941A469: true, 0x1254C367: true,
}

var errMKVTruncated = errors.New("the data is not a valid Matroska or WebM video: it is truncated")

// mkvID reads an element ID, its length marker kept.
func mkvID(b []byte) (uint32, int, error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, errMKVTruncated
	}
	n := 1
	for m := byte(0x80); b[0]&m == 0; m >>= 1 {
		n++
	}
	if n > 4 || len(b) < n {
		return 0, 0, errMKVTruncated
	}
	var id uint32
	for _, c := range b[:n] {
		id = id<<8 | uint32(c)
	}
	return id, n, nil
}

// mkvVint reads a variable-length integer; unknown is the all-ones value
// (an unknown size).
func mkvVint(b []byte) (v uint64, n int, unknown bool, err error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false, errMKVTruncated
	}
	n = 1
	m := byte(0x80)
	for ; b[0]&m == 0; m >>= 1 {
		n++
	}
	if len(b) < n {
		return 0, 0, false, errMKVTruncated
	}
	v = uint64(b[0] & (m - 1))
	ones := v == uint64(m-1)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
		ones = ones && c == 0xFF
	}
	return v, n, ones, nil
}

// mkvElement is one element: its ID and its data (to its parent's end for
// an unknown size).
type mkvElement struct {
	id      uint32
	data    []byte
	unknown bool
}

// mkvNext reads the element at b's start; next is the offset after it.
func mkvNext(b []byte) (e mkvElement, next int, err error) {
	id, n, err := mkvID(b)
	if err != nil {
		return e, 0, err
	}
	size, m, unknown, err := mkvVint(b[n:])
	if err != nil {
		return e, 0, err
	}
	start := n + m
	if unknown {
		return mkvElement{id: id, data: b[start:], unknown: true}, len(b), nil
	}
	if size > uint64(len(b)-start) {
		return e, 0, errMKVTruncated
	}
	end := start + int(size)
	return mkvElement{id: id, data: b[start:end]}, end, nil
}

// mkvChildren calls f for each child element of data.
func mkvChildren(data []byte, f func(mkvElement) error) error {
	for len(data) > 0 {
		e, next, err := mkvNext(data)
		if err != nil {
			return err
		}
		if err := f(e); err != nil {
			return err
		}
		data = data[next:]
	}
	return nil
}

func mkvUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func mkvFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return 0
}

type mkvTrack struct {
	number, kind    uint64
	codec           string
	private         []byte
	defaultDuration uint64 // ns
	encoded         bool
}

// readMatroskaTrack reads the first video track of a Matroska or WebM file.
// Times are in nanoseconds (timescale 1e9).
func readMatroskaTrack(data []byte) (*videoTrack, error) {
	hdr, next, err := mkvNext(data)
	if err != nil || hdr.id != mkvEBML {
		return nil, fmt.Errorf("the data is not a valid Matroska or WebM video: it has no EBML header")
	}
	data = data[next:]
	var seg []byte
	for len(data) > 0 && seg == nil {
		e, n, err := mkvNext(data)
		if err != nil {
			return nil, err
		}
		if e.id == mkvSegment {
			seg = e.data
		}
		data = data[n:]
	}
	if seg == nil {
		return nil, fmt.Errorf("the data is not a valid Matroska or WebM video: it has no Segment")
	}

	scale := uint64(1000000)
	var duration float64
	var video *mkvTrack
	tr := &videoTrack{timescale: 1000000000}
	type block struct {
		tc       int64
		dur      int64
		key      bool
		payload  []byte
		hasDur   bool
		trackNum uint64
	}
	var blocks []block
	// parseBlock reads a (Simple)Block's header.
	parseBlock := func(b []byte, clusterTC int64, simple bool) (block, error) {
		num, n, _, err := mkvVint(b)
		if err != nil || len(b) < n+3 {
			return block{}, errMKVTruncated
		}
		rel := int16(binary.BigEndian.Uint16(b[n:]))
		flags := b[n+2]
		if flags&0x06 != 0 {
			return block{trackNum: num, payload: nil, tc: -1}, nil // laced: checked below for the video track
		}
		return block{trackNum: num, tc: clusterTC + int64(rel), key: simple && flags&0x80 != 0, payload: b[n+3:]}, nil
	}
	laced := false

	for len(seg) > 0 {
		e, n, err := mkvNext(seg)
		if err != nil {
			return nil, err
		}
		if e.unknown && e.id == mkvCluster {
			// An unknown-size Cluster ends at the next top-level element.
			end := 0
			for end < len(e.data) {
				c, m, err := mkvNext(e.data[end:])
				if err != nil {
					return nil, err
				}
				if mkvTopLevel[c.id] {
					break
				}
				end += m
			}
			n = len(seg) - len(e.data) + end
			e.data = e.data[:end]
		}
		switch e.id {
		case mkvInfo:
			if err := mkvChildren(e.data, func(c mkvElement) error {
				switch c.id {
				case mkvTimecodeScale:
					if v := mkvUint(c.data); v > 0 {
						scale = v
					}
				case mkvDuration:
					duration = mkvFloat(c.data)
				}
				return nil
			}); err != nil {
				return nil, err
			}
		case mkvTracks:
			if err := mkvChildren(e.data, func(c mkvElement) error {
				if c.id != mkvTrackEntry {
					return nil
				}
				t := &mkvTrack{}
				if err := mkvChildren(c.data, func(f mkvElement) error {
					switch f.id {
					case mkvTrackNumber:
						t.number = mkvUint(f.data)
					case mkvTrackType:
						t.kind = mkvUint(f.data)
					case mkvCodecID:
						t.codec = string(f.data)
					case mkvCodecPrivate:
						t.private = f.data
					case mkvDefaultDuration:
						t.defaultDuration = mkvUint(f.data)
					case mkvContentEncs:
						t.encoded = true
					}
					return nil
				}); err != nil {
					return err
				}
				if t.kind == 1 && video == nil {
					video = t
				}
				return nil
			}); err != nil {
				return nil, err
			}
		case mkvCluster:
			var clusterTC int64
			if err := mkvChildren(e.data, func(c mkvElement) error {
				switch c.id {
				case mkvTimecode:
					clusterTC = int64(mkvUint(c.data))
				case mkvSimpleBlock:
					b, err := parseBlock(c.data, clusterTC, true)
					if err != nil {
						return err
					}
					if b.tc == -1 {
						laced = laced || video == nil || b.trackNum == video.number
						return nil
					}
					blocks = append(blocks, b)
				case mkvBlockGroup:
					var b block
					ref, have := false, false
					var dur int64
					hasDur := false
					if err := mkvChildren(c.data, func(g mkvElement) error {
						switch g.id {
						case mkvBlock:
							var err error
							if b, err = parseBlock(g.data, clusterTC, false); err != nil {
								return err
							}
							have = true
						case mkvBlockDuration:
							dur, hasDur = int64(mkvUint(g.data)), true
						case mkvReferenceBlock:
							ref = true
						}
						return nil
					}); err != nil {
						return err
					}
					if !have {
						return nil
					}
					if b.tc == -1 {
						laced = laced || video == nil || b.trackNum == video.number
						return nil
					}
					b.key, b.dur, b.hasDur = !ref, dur, hasDur
					blocks = append(blocks, b)
				}
				return nil
			}); err != nil {
				return nil, err
			}
		}
		seg = seg[n:]
	}
	if video == nil {
		return nil, fmt.Errorf("the video has no video track")
	}
	if video.encoded {
		return nil, notDecodedf("the Matroska video track uses ContentEncoding (compressed or encrypted frames), which this build does not read")
	}
	if laced {
		return nil, notDecodedf("the Matroska video track uses laced blocks, which this build does not read")
	}
	if err := matroskaCodec(tr, video); err != nil {
		return nil, err
	}
	sc := int64(scale)
	for _, b := range blocks {
		if b.trackNum != video.number {
			continue
		}
		pts := b.tc * sc
		dur := int64(video.defaultDuration)
		if b.hasDur {
			dur = b.dur * sc
		}
		tr.samples = append(tr.samples, b.payload)
		tr.pts = append(tr.pts, pts)
		tr.sync = append(tr.sync, b.key)
		tr.end = max(tr.end, pts+dur)
	}
	if len(tr.samples) == 0 {
		return nil, fmt.Errorf("the video track has no samples")
	}
	if d := int64(duration * float64(sc)); d > tr.end {
		tr.end = d
	}
	if tr.end <= 0 {
		return nil, fmt.Errorf("the video has no duration")
	}
	return tr, nil
}

// matroskaCodec sets tr's codec and configuration from a Matroska track's
// CodecID and CodecPrivate.
func matroskaCodec(tr *videoTrack, t *mkvTrack) error {
	switch t.codec {
	case "V_MPEG4/ISO/AVC":
		rec, err := avc.DecodeAVCDecConfRec(t.private)
		if err != nil {
			return fmt.Errorf("the H.264 track's CodecPrivate is not an AVC configuration: %v", err)
		}
		if len(t.private) < 5 || t.private[4]&3 != 3 {
			return notDecodedf("the H.264 track uses NAL lengths of other than 4 bytes, which this build does not read")
		}
		tr.codec, tr.avcC, tr.nalLengthSize = "avc1", &rec, 4
	case "V_MPEGH/ISO/HEVC":
		rec, err := mp4hevc.DecodeHEVCDecConfRec(t.private)
		if err != nil {
			return fmt.Errorf("the H.265 track's CodecPrivate is not an HEVC configuration: %v", err)
		}
		tr.codec = "hvc1"
		for _, a := range rec.NaluArrays {
			tr.hvcC = append(tr.hvcC, a.Nalus...)
		}
		tr.nalLengthSize = int(rec.LengthSizeMinusOne) + 1
	case "V_VP9":
		tr.codec = "vp09"
	case "V_AV1":
		tr.codec = "av01"
		if len(t.private) > 4 {
			tr.av1Config = t.private[4:] // after the av1C header: configOBUs
		}
	default:
		return fmt.Errorf("%w %q; Google takes H.264, H.265, VP9 and AV1, and this build decodes those", ErrVideoCodec, t.codec)
	}
	return nil
}
