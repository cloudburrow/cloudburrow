package bigqueryfront

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	gproto "github.com/golang/protobuf/proto" //nolint:staticcheck // the ORC module's messages are golang/protobuf's
	"github.com/golang/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/scritchley/orc"
	orcproto "github.com/scritchley/orc/proto"
)

// Reading an ORC file (#1713, orcload.go).
//
// The file's layout is the Apache ORC specification's
// (https://orc.apache.org/specification/ORCv1/): the "ORC" magic, the
// stripes, then the footer and metadata, compressed by the file's codec,
// and last the postscript, uncompressed, its length the file's last
// byte. github.com/scritchley/orc (MIT, pure Go; docs/upstream-evaluation.md)
// gives the protocol buffer messages and the column decoders (the run
// length integer, byte and boolean encodings, v1 and v2, direct and
// dictionary strings, decimals, timestamps, floats, binaries). The front
// reads the file's tail, decompresses each stripe's streams and puts the
// column readers together itself, because the module's own reader:
//
//   - reads NONE, ZLIB and SNAPPY files only, where BigQuery reads "Zlib,
//     Snappy, LZO, LZ4, and ZSTD" (Loading ORC data, Compression): the
//     front decompresses each stream itself (orcDecompress), with
//     klauspost/compress for ZSTD, pierrec/lz4 for LZ4 and a decoder of its
//     own for LZO (lzo1xDecompress), modules CloudBurrow already built with;
//   - reads a NULL DATE as 1970-01-01 (its DateTreeReader does not look at
//     the PRESENT stream), so a DATE column is read as its days, an integer;
//   - reads a DECIMAL's digits past the 63rd bit wrongly (it shifts each
//     varint group as an int64), so decimals are read here (orcDecimals);
//   - reads an empty BINARY at the end of its stream as an error (EOF), so
//     binaries are read here (orcBinary);
//   - reads a TIMESTAMP from 2015-01-01 UTC, where the file's seconds count
//     from 2015-01-01 in the writer's zone (orcWallClock);
//   - reads a MAP's keys and values one entry ahead after the first row
//     (MapTreeReader moves on once more than the entries it reads), so maps,
//     lists, structs and unions are put together here.

// orcFile is an ORC file's tail, read.
type orcFile struct {
	ra     io.ReaderAt
	size   int64
	ps     *orcproto.PostScript
	footer *orcproto.Footer
	types  []*orcproto.Type
}

// errNotORC is a file that is not one ORC file BigQuery reads.
var errNotORC = errors.New("the data is not an ORC file")

// orcMaxTail bounds the footer CloudBurrow reads (a postscript says how
// long it is; a corrupt one could say anything).
const orcMaxTail = 256 << 20

// openORC reads the file's postscript and footer.
func openORC(ra io.ReaderAt, size int64) (o *orcFile, err error) {
	defer orcRecover(&err)
	if size < 4 {
		return nil, errNotORC
	}
	head := make([]byte, 3)
	if _, err := ra.ReadAt(head, 0); err != nil || string(head) != "ORC" {
		return nil, errNotORC
	}
	last := make([]byte, 1)
	if _, err := ra.ReadAt(last, size-1); err != nil {
		return nil, err
	}
	psLen := int64(last[0])
	if psLen == 0 || psLen+1 > size {
		return nil, errNotORC
	}
	psBytes := make([]byte, psLen)
	if _, err := ra.ReadAt(psBytes, size-1-psLen); err != nil {
		return nil, err
	}
	ps := &orcproto.PostScript{}
	if err := gproto.Unmarshal(psBytes, ps); err != nil || ps.GetMagic() != "ORC" {
		return nil, errNotORC
	}
	o = &orcFile{ra: ra, size: size, ps: ps}
	footLen := int64(ps.GetFooterLength())
	if footLen <= 0 || footLen > orcMaxTail || footLen+psLen+1 > size {
		return nil, fmt.Errorf("%w: its footer length %d is not within the file", errNotORC, footLen)
	}
	raw := make([]byte, footLen)
	if _, err := ra.ReadAt(raw, size-1-psLen-footLen); err != nil {
		return nil, err
	}
	data, err := o.decompress(raw)
	if err != nil {
		return nil, err
	}
	o.footer = &orcproto.Footer{}
	if err := gproto.Unmarshal(data, o.footer); err != nil {
		return nil, fmt.Errorf("%w: its footer does not parse: %v", errNotORC, err)
	}
	o.types = o.footer.GetTypes()
	if len(o.types) == 0 || o.types[0].GetKind() != orcproto.Type_STRUCT {
		return nil, fmt.Errorf("%w: its root type is not a struct", errNotORC)
	}
	for i, t := range o.types {
		for _, s := range t.GetSubtypes() {
			if int(s) <= i || int(s) >= len(o.types) {
				return nil, fmt.Errorf("%w: its type %d names a subtype %d out of order", errNotORC, i, s)
			}
		}
	}
	return o, nil
}

// orcRecover turns a panic of the decoders on a corrupt file into an error.
func orcRecover(err *error) {
	if p := recover(); p != nil {
		*err = fmt.Errorf("%w: it is corrupt (%v)", errNotORC, p)
	}
}

// orcCodecError is a compression kind CloudBurrow does not read.
type orcCodecError struct{ kind orcproto.CompressionKind }

func (e *orcCodecError) Error() string {
	return fmt.Sprintf("its compression kind %s is not one ORC defines", e.kind)
}

// decompress reads a stream of the file: chunks, each a three-byte
// header (its length times two, plus one when it is stored uncompressed)
// and its bytes, unless the file is not compressed.
func (o *orcFile) decompress(raw []byte) ([]byte, error) {
	kind := o.ps.GetCompression()
	if kind == orcproto.CompressionKind_NONE {
		return raw, nil
	}
	block := int(o.ps.GetCompressionBlockSize())
	if block <= 0 || block > 1<<24 {
		block = 256 << 10
	}
	var out []byte
	for len(raw) > 0 {
		if len(raw) < 3 {
			return nil, fmt.Errorf("%w: a compressed chunk's header is cut short", errNotORC)
		}
		h := int(raw[0]) | int(raw[1])<<8 | int(raw[2])<<16
		n := h >> 1
		raw = raw[3:]
		if n > len(raw) {
			return nil, fmt.Errorf("%w: a compressed chunk of %d bytes runs past its stream", errNotORC, n)
		}
		chunk := raw[:n]
		raw = raw[n:]
		if h&1 == 1 {
			out = append(out, chunk...)
			continue
		}
		got, err := orcDecompress(kind, chunk, block)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

var zstdChunks, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))

// orcDecompress decompresses one chunk of kind; block is the file's
// compressionBlockSize, the most a chunk holds.
func orcDecompress(kind orcproto.CompressionKind, chunk []byte, block int) ([]byte, error) {
	bad := func(err error) ([]byte, error) {
		return nil, fmt.Errorf("%w: a %s chunk does not decompress: %v", errNotORC, kind, err)
	}
	switch kind {
	case orcproto.CompressionKind_ZLIB:
		got, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(chunk)), int64(block)+1))
		if err != nil {
			return bad(err)
		}
		return got, nil
	case orcproto.CompressionKind_SNAPPY:
		got, err := snappy.Decode(nil, chunk)
		if err != nil {
			return bad(err)
		}
		return got, nil
	case orcproto.CompressionKind_ZSTD:
		got, err := zstdChunks.DecodeAll(chunk, nil)
		if err != nil {
			return bad(err)
		}
		return got, nil
	case orcproto.CompressionKind_LZ4:
		dst := make([]byte, block)
		n, err := lz4.UncompressBlock(chunk, dst)
		if err != nil {
			return bad(err)
		}
		return dst[:n], nil
	case orcproto.CompressionKind_LZO:
		got, err := lzo1xDecompress(chunk, block)
		if err != nil {
			return bad(err)
		}
		return got, nil
	}
	return nil, &orcCodecError{kind: kind}
}

// orcStreamKey names a stream of a stripe: its column and kind.
type orcStreamKey struct {
	column uint32
	kind   orcproto.Stream_Kind
}

// orcStripe is one stripe's streams, decompressed, and its columns'
// encodings.
type orcStripe struct {
	rows      int64
	streams   map[orcStreamKey][]byte
	encodings []*orcproto.ColumnEncoding
	// zone is the writer's time zone, a TIMESTAMP's (orcWallClock).
	zone *time.Location
}

// stripe reads stripe i.
func (o *orcFile) stripe(i int) (s *orcStripe, err error) {
	defer orcRecover(&err)
	info := o.footer.GetStripes()[i]
	off := int64(info.GetOffset())
	index, data, foot := int64(info.GetIndexLength()), int64(info.GetDataLength()), int64(info.GetFooterLength())
	total := index + data + foot
	if off < 3 || index < 0 || data < 0 || foot <= 0 || total > o.size || off+total > o.size {
		return nil, fmt.Errorf("%w: its stripe %d is not within the file", errNotORC, i)
	}
	buf := make([]byte, total)
	if _, err := o.ra.ReadAt(buf, off); err != nil {
		return nil, err
	}
	sfBytes, err := o.decompress(buf[index+data:])
	if err != nil {
		return nil, err
	}
	sf := &orcproto.StripeFooter{}
	if err := gproto.Unmarshal(sfBytes, sf); err != nil {
		return nil, fmt.Errorf("%w: stripe %d's footer does not parse: %v", errNotORC, i, err)
	}
	s = &orcStripe{rows: int64(info.GetNumberOfRows()), streams: map[orcStreamKey][]byte{}, encodings: sf.GetColumns(),
		zone: time.UTC}
	if tz := sf.GetWriterTimezone(); tz != "" && tz != "UTC" && tz != "GMT" {
		if s.zone, err = time.LoadLocation(tz); err != nil {
			return nil, &orcZoneError{zone: tz, err: err}
		}
	}
	var at int64
	for _, st := range sf.GetStreams() {
		n := int64(st.GetLength())
		if n < 0 || at+n > index+data {
			return nil, fmt.Errorf("%w: stripe %d's stream runs past its data", errNotORC, i)
		}
		raw := buf[at : at+n]
		at += n
		switch st.GetKind() {
		case orcproto.Stream_ROW_INDEX, orcproto.Stream_BLOOM_FILTER, orcproto.Stream_BLOOM_FILTER_UTF8:
			continue // indexes: the front reads every row
		}
		got, err := o.decompress(raw)
		if err != nil {
			return nil, err
		}
		s.streams[orcStreamKey{st.GetColumn(), st.GetKind()}] = got
	}
	return s, nil
}

// orcValues reads one value of a column a row at a time: nil for NULL.
type orcValues interface {
	next() (any, error)
}

// orcUnion is a union's value: the tag of its type, and the value.
type orcUnion struct {
	tag   int
	value any
}

// orcMapEntry is one entry of a map's value.
type orcMapEntry struct{ key, value any }

// reader puts together the reader of column id of stripe s.
func (s *orcStripe) reader(types []*orcproto.Type, id uint32) (orcValues, error) {
	if int(id) >= len(types) {
		return nil, fmt.Errorf("%w: column %d has no type", errNotORC, id)
	}
	if int(id) >= len(s.encodings) {
		return nil, fmt.Errorf("%w: column %d has no encoding", errNotORC, id)
	}
	t, enc := types[id], s.encodings[id]
	get := func(k orcproto.Stream_Kind) io.Reader {
		return bytes.NewReader(s.streams[orcStreamKey{id, k}])
	}
	var present io.Reader
	if b, ok := s.streams[orcStreamKey{id, orcproto.Stream_PRESENT}]; ok {
		present = bytes.NewReader(b)
	}
	leaf := func(r orc.TreeReader, err error) (orcValues, error) {
		if err != nil {
			return nil, err
		}
		return &orcLeaf{r: r, id: id}, nil
	}
	children := func() ([]orcValues, error) {
		out := make([]orcValues, len(t.GetSubtypes()))
		for i, c := range t.GetSubtypes() {
			r, err := s.reader(types, c)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	switch k := t.GetKind(); k {
	case orcproto.Type_BOOLEAN:
		return leaf(orc.NewBooleanTreeReader(present, get(orcproto.Stream_DATA), enc))
	case orcproto.Type_BYTE:
		return leaf(orc.NewByteTreeReader(present, get(orcproto.Stream_DATA), enc))
	case orcproto.Type_SHORT, orcproto.Type_INT, orcproto.Type_LONG, orcproto.Type_DATE:
		// A DATE's days since the epoch, read as an integer (above).
		return leaf(orc.NewIntegerTreeReader(present, get(orcproto.Stream_DATA), enc))
	case orcproto.Type_FLOAT:
		return leaf(orc.NewFloatTreeReader(4, present, get(orcproto.Stream_DATA), enc))
	case orcproto.Type_DOUBLE:
		return leaf(orc.NewFloatTreeReader(8, present, get(orcproto.Stream_DATA), enc))
	case orcproto.Type_STRING, orcproto.Type_VARCHAR, orcproto.Type_CHAR:
		return leaf(orc.NewStringTreeReader(present, get(orcproto.Stream_DATA), get(orcproto.Stream_LENGTH),
			get(orcproto.Stream_DICTIONARY_DATA), enc))
	case orcproto.Type_BINARY:
		return &orcBinary{present: orcPresent(present), lengths: orcUnsigned(get(orcproto.Stream_LENGTH), enc),
			data: get(orcproto.Stream_DATA)}, nil
	case orcproto.Type_TIMESTAMP, orcTimestampInstant:
		r, err := orc.NewTimestampTreeReader(present, get(orcproto.Stream_DATA), get(orcproto.Stream_SECONDARY), enc)
		if err != nil {
			return nil, err
		}
		zone := s.zone
		if t.GetKind() == orcTimestampInstant {
			zone = time.UTC // an instant counts from 2015-01-01 UTC
		}
		return &orcLeaf{r: r, id: id, conv: func(v any) any {
			if t, ok := v.(time.Time); ok {
				return orcWallClock(t, zone)
			}
			return v
		}}, nil
	case orcproto.Type_DECIMAL:
		scales, err := orc.NewIntegerTreeReader(nil, get(orcproto.Stream_SECONDARY), enc)
		if err != nil {
			return nil, err
		}
		return &orcDecimals{present: orcPresent(present), data: bufio.NewReader(get(orcproto.Stream_DATA)), scales: scales}, nil
	case orcproto.Type_STRUCT:
		kids, err := children()
		if err != nil {
			return nil, err
		}
		return &orcStruct{present: orcPresent(present), names: t.GetFieldNames(), kids: kids}, nil
	case orcproto.Type_LIST, orcproto.Type_MAP:
		kids, err := children()
		if err != nil {
			return nil, err
		}
		if (k == orcproto.Type_LIST && len(kids) != 1) || (k == orcproto.Type_MAP && len(kids) != 2) {
			return nil, fmt.Errorf("%w: column %d's %s type has %d subtypes", errNotORC, id, k, len(kids))
		}
		return &orcRepeated{present: orcPresent(present), lengths: orcUnsigned(get(orcproto.Stream_LENGTH), enc), kids: kids}, nil
	case orcproto.Type_UNION:
		kids, err := children()
		if err != nil {
			return nil, err
		}
		return &orcUnionValues{present: orcPresent(present), tags: orc.NewRunLengthByteReader(bufio.NewReader(get(orcproto.Stream_DATA))),
			kids: kids}, nil
	default:
		return nil, fmt.Errorf("%w: column %d's type kind %d is not one ORC defines", errNotORC, id, k)
	}
}

// orcTimestampInstant is ORC's TIMESTAMP_INSTANT (a timestamp with local
// time zone), a kind the module's protocol buffers predate.
const orcTimestampInstant orcproto.Type_Kind = 18

func orcPresent(r io.Reader) *orc.BooleanReader {
	if r == nil {
		return nil
	}
	return orc.NewBooleanReader(bufio.NewReader(r))
}

// isPresent reads the next bit of a present stream (nil: every value is
// present).
func isPresent(p *orc.BooleanReader) (bool, error) {
	if p == nil {
		return true, nil
	}
	if !p.Next() {
		if err := p.Err(); err != nil {
			return false, err
		}
		return false, fmt.Errorf("%w: a PRESENT stream ends before its column", errNotORC)
	}
	return p.Bool(), nil
}

// orcUnsigned reads a LENGTH stream: unsigned integers, by the column's
// encoding (v1 or v2).
func orcUnsigned(r io.Reader, enc *orcproto.ColumnEncoding) orc.IntegerReader {
	br := bufio.NewReader(r)
	switch enc.GetKind() {
	case orcproto.ColumnEncoding_DIRECT_V2, orcproto.ColumnEncoding_DICTIONARY_V2:
		return orc.NewRunLengthIntegerReaderV2(br, false, false)
	}
	return orc.NewRunLengthIntegerReader(br, false)
}

// orcLeaf reads a primitive column through the module's TreeReader.
type orcLeaf struct {
	r    orc.TreeReader
	id   uint32
	conv func(any) any // or nil
}

func (l *orcLeaf) next() (any, error) {
	if !l.r.Next() {
		if err := l.r.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: column %d has fewer values than its rows", errNotORC, l.id)
	}
	v := l.r.Value()
	if err := l.r.Err(); err != nil {
		return nil, err
	}
	if l.conv != nil {
		v = l.conv(v)
	}
	return v, nil
}

// orcZoneError is a writer's time zone CloudBurrow cannot read.
type orcZoneError struct {
	zone string
	err  error
}

func (e *orcZoneError) Error() string {
	return fmt.Sprintf("its writer's time zone %s, which this machine's time zone database does not have (%v)", e.zone, e.err)
}

// orcWallClock is a TIMESTAMP as the module reads it (2015-01-01 00:00:00
// UTC plus the file's seconds, and its nanoseconds) as ORC's own reader
// gives it: the file's seconds count from 2015-01-01 00:00:00 in the
// writer's time zone (the stripe's writerTimezone), and the value is the
// wall-clock time there, here as UTC. The Java writer truncates a
// negative time's seconds toward zero, and its reader
// (TimestampTreeReader) takes a second off again.
func orcWallClock(t time.Time, zone *time.Location) time.Time {
	secs, nanos := t.Unix(), int64(t.Nanosecond())
	if secs < 0 && nanos > 999999 {
		secs--
	}
	if zone == time.UTC {
		return time.Unix(secs, nanos).UTC()
	}
	base := time.Date(2015, 1, 1, 0, 0, 0, 0, zone).Unix() - orcBaseUTC
	w := time.Unix(secs+base, nanos).In(zone)
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
}

// orcBaseUTC is 2015-01-01 00:00:00 UTC, ORC's timestamp epoch.
const orcBaseUTC = 1420070400

// orcBinary reads a BINARY column: a LENGTH stream of each value's length
// and its bytes in the DATA stream.
type orcBinary struct {
	present *orc.BooleanReader
	lengths orc.IntegerReader
	data    io.Reader
}

func (b *orcBinary) next() (any, error) {
	ok, err := isPresent(b.present)
	if err != nil || !ok {
		return nil, err
	}
	if !b.lengths.Next() {
		if err := b.lengths.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: a BINARY's LENGTH stream ends before its column", errNotORC)
	}
	n := b.lengths.Int()
	if n < 0 || n > 1<<30 {
		return nil, fmt.Errorf("%w: a BINARY value of %d bytes", errNotORC, n)
	}
	out := make([]byte, n)
	if _, err := io.ReadFull(b.data, out); err != nil {
		return nil, fmt.Errorf("%w: a BINARY's DATA stream ends before its column", errNotORC)
	}
	return out, nil
}

// orcDecimal is a decimal's value: unscaled × 10^-scale.
type orcDecimal struct {
	unscaled *big.Int
	scale    int64
}

// orcDecimals reads a DECIMAL column: its DATA stream is each value's
// unscaled integer, an unbounded zigzag base-128 varint, and its SECONDARY
// stream each value's scale, a signed integer.
type orcDecimals struct {
	present *orc.BooleanReader
	data    *bufio.Reader
	scales  orc.TreeReader
}

func (d *orcDecimals) next() (any, error) {
	ok, err := isPresent(d.present)
	if err != nil || !ok {
		return nil, err
	}
	u := new(big.Int)
	for shift := uint(0); ; shift += 7 {
		b, err := d.data.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: a DECIMAL's DATA stream ends before its column", errNotORC)
		}
		if shift > 8*64 {
			return nil, fmt.Errorf("%w: a DECIMAL's value runs past 512 bits", errNotORC)
		}
		u.Or(u, new(big.Int).Lsh(big.NewInt(int64(b&0x7f)), shift))
		if b&0x80 == 0 {
			break
		}
	}
	// zigzag: (u >> 1) XOR -(u & 1)
	neg := u.Bit(0) == 1
	u.Rsh(u, 1)
	if neg {
		u.Neg(u).Sub(u, big.NewInt(1))
	}
	if !d.scales.Next() {
		if err := d.scales.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: a DECIMAL's SECONDARY stream ends before its column", errNotORC)
	}
	scale, _ := d.scales.Value().(int64)
	return orcDecimal{unscaled: u, scale: scale}, nil
}

type orcStruct struct {
	present *orc.BooleanReader
	names   []string
	kids    []orcValues
}

func (s *orcStruct) next() (any, error) {
	ok, err := isPresent(s.present)
	if err != nil || !ok {
		return nil, err
	}
	out := make([]any, len(s.kids))
	for i, k := range s.kids {
		if out[i], err = k.next(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// orcRepeated reads a list ([]any of its elements) or a map
// ([]orcMapEntry).
type orcRepeated struct {
	present *orc.BooleanReader
	lengths orc.IntegerReader
	kids    []orcValues
}

func (r *orcRepeated) next() (any, error) {
	ok, err := isPresent(r.present)
	if err != nil || !ok {
		return nil, err
	}
	if !r.lengths.Next() {
		if err := r.lengths.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: a LENGTH stream ends before its column", errNotORC)
	}
	n := r.lengths.Int()
	if n < 0 || n > 1<<28 {
		return nil, fmt.Errorf("%w: a list or map of %d entries", errNotORC, n)
	}
	if len(r.kids) == 1 {
		out := make([]any, n)
		for i := range out {
			if out[i], err = r.kids[0].next(); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	out := make([]orcMapEntry, n)
	for i := range out {
		if out[i].key, err = r.kids[0].next(); err != nil {
			return nil, err
		}
		if out[i].value, err = r.kids[1].next(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type orcUnionValues struct {
	present *orc.BooleanReader
	tags    *orc.RunLengthByteReader
	kids    []orcValues
}

func (u *orcUnionValues) next() (any, error) {
	ok, err := isPresent(u.present)
	if err != nil || !ok {
		return nil, err
	}
	if !u.tags.Next() {
		if err := u.tags.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: a union's tags end before its column", errNotORC)
	}
	tag := int(u.tags.Byte())
	if tag >= len(u.kids) {
		return nil, fmt.Errorf("%w: a union's tag %d names none of its %d types", errNotORC, tag, len(u.kids))
	}
	v, err := u.kids[tag].next()
	if err != nil {
		return nil, err
	}
	return orcUnion{tag: tag, value: v}, nil
}

// rows reads every row of the file, each as the root struct's values in
// its fields' order, and calls fn with each.
func (o *orcFile) rows(fn func([]any) error) (n int64, err error) {
	defer orcRecover(&err)
	for i := range o.footer.GetStripes() {
		s, err := o.stripe(i)
		if err != nil {
			return n, err
		}
		root, err := s.reader(o.types, 0)
		if err != nil {
			return n, err
		}
		for j := int64(0); j < s.rows; j++ {
			v, err := root.next()
			if err != nil {
				return n, err
			}
			vals, _ := v.([]any)
			if vals == nil {
				return n, fmt.Errorf("%w: a row is NULL", errNotORC)
			}
			if err := fn(vals); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// lzo1xDecompress decodes one LZO1X block (the LZO format of ORC's LZO
// codec, Apache aircompressor's), of at most limit bytes. It follows the
// format as the Linux kernel's lzo1x_decompress_safe reads it
// (Documentation/staging/lzo.rst): each instruction copies literals or a
// match, and the two low bits of a match's instruction are the literals
// (0 to 3) that follow it.
func lzo1xDecompress(in []byte, limit int) ([]byte, error) {
	out := make([]byte, 0, limit)
	ip := 0
	short := errors.New("the block is cut short")
	byteAt := func() (int, error) {
		if ip >= len(in) {
			return 0, short
		}
		b := int(in[ip])
		ip++
		return b, nil
	}
	literals := func(n int) error {
		if n < 0 || ip+n > len(in) {
			return short
		}
		if len(out)+n > limit {
			return errors.New("the block decompresses past its limit")
		}
		out = append(out, in[ip:ip+n]...)
		ip += n
		return nil
	}
	match := func(dist, n int) error {
		if dist <= 0 || dist > len(out) {
			return fmt.Errorf("a match reaches %d bytes back of %d", dist, len(out))
		}
		if len(out)+n > limit {
			return errors.New("the block decompresses past its limit")
		}
		from := len(out) - dist
		for k := 0; k < n; k++ {
			out = append(out, out[from+k])
		}
		return nil
	}
	// extended reads a length's run of zero bytes and its last byte.
	extended := func(base int) (int, error) {
		n := 0
		for {
			b, err := byteAt()
			if err != nil {
				return 0, err
			}
			if b != 0 {
				return n + base + b, nil
			}
			n += 255
			if n > 1<<26 {
				return 0, errors.New("a length runs too long")
			}
		}
	}
	le16 := func() (int, error) {
		if ip+2 > len(in) {
			return 0, short
		}
		v := int(binary.LittleEndian.Uint16(in[ip:]))
		ip += 2
		return v, nil
	}
	state := 0
	if len(in) > 0 && in[0] > 17 {
		t := int(in[0]) - 17
		ip = 1
		if err := literals(t); err != nil {
			return nil, err
		}
		state = t
		if t >= 4 {
			state = 4
		}
	}
	for {
		t, err := byteAt()
		if err != nil {
			return nil, err
		}
		var dist, n, next int
		switch {
		case t < 16 && state == 0:
			n = t
			if n == 0 {
				if n, err = extended(15); err != nil {
					return nil, err
				}
			}
			if err := literals(n + 3); err != nil {
				return nil, err
			}
			state = 4
			continue
		case t < 16:
			h, err := byteAt()
			if err != nil {
				return nil, err
			}
			next = t & 3
			if state == 4 {
				dist, n = 1+0x800+(t>>2)+(h<<2), 3
			} else {
				dist, n = 1+(t>>2)+(h<<2), 2
			}
		case t >= 64:
			h, err := byteAt()
			if err != nil {
				return nil, err
			}
			next = t & 3
			dist, n = 1+((t>>2)&7)+(h<<3), (t>>5)+1
		case t >= 32:
			n = t & 31
			if n == 0 {
				if n, err = extended(31); err != nil {
					return nil, err
				}
			}
			n += 2
			v, err := le16()
			if err != nil {
				return nil, err
			}
			dist, next = 1+(v>>2), v&3
		default: // 16 to 31
			n = t & 7
			if n == 0 {
				if n, err = extended(7); err != nil {
					return nil, err
				}
			}
			n += 2
			v, err := le16()
			if err != nil {
				return nil, err
			}
			dist, next = ((t&8)<<11)+(v>>2), v&3
			if dist == 0 {
				return out, nil // the end of the block
			}
			dist += 0x4000
		}
		if err := match(dist, n); err != nil {
			return nil, err
		}
		if err := literals(next); err != nil {
			return nil, err
		}
		state = next
	}
}
