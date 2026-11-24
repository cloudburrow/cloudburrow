package vdisk

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"io"
	"regexp"
	"strings"
)

// VMDK (VMware Virtual Disk Format 5.0): a hosted sparse extent starts with
// a 512-byte little-endian header (magic "KDMV"); its embedded descriptor
// names the disk's type and extents; the grain directory (GD) lists the
// sectors of the grain tables (GTs), whose entries are the sectors of
// grains (0: never written; 1: a zero grain). A stream-optimized disk
// writes its GD at the end, named by a footer (a copy of the header one
// sector before the end-of-stream marker), and compresses each grain with
// DEFLATE behind a 12-byte grain marker (its LBA and compressed size).

const (
	vmdkSector       = 512
	vmdkGDAtEnd      = 0xFFFFFFFFFFFFFFFF
	vmdkFlagNewline  = 1 << 0
	vmdkFlagCompress = 1 << 16
	vmdkFlagMarkers  = 1 << 17
	vmdkCompDeflate  = 1
)

var (
	vmdkCreateType = regexp.MustCompile(`(?m)^\s*createType\s*=\s*"([^"]*)"`)
	vmdkExtent     = regexp.MustCompile(`(?m)^\s*(RW|RDONLY|NOACCESS)\s+\d+\s+(\w+)`)
	vmdkParent     = regexp.MustCompile(`(?m)^\s*parentCID\s*=\s*([0-9a-fA-F]+)`)
)

type vmdkHeader struct {
	version, flags    uint32
	capacity, grain   uint64
	descOff, descSize uint64
	gtes              uint32
	gdOff, overhead   uint64
	newlineChars      [4]byte
	compress          uint16
}

func parseVMDKHeader(b []byte) (vmdkHeader, error) {
	var h vmdkHeader
	if !bytes.HasPrefix(b, []byte("KDMV")) {
		return h, malformed("no VMDK sparse extent header")
	}
	le := binary.LittleEndian
	h.version, h.flags = le.Uint32(b[4:]), le.Uint32(b[8:])
	h.capacity, h.grain = le.Uint64(b[12:]), le.Uint64(b[20:])
	h.descOff, h.descSize = le.Uint64(b[28:]), le.Uint64(b[36:])
	h.gtes = le.Uint32(b[44:])
	h.gdOff = le.Uint64(b[56:])
	h.overhead = le.Uint64(b[64:])
	copy(h.newlineChars[:], b[73:77])
	h.compress = le.Uint16(b[77:])
	return h, nil
}

func openVMDK(r io.ReaderAt, size int64) (*Disk, error) {
	b := make([]byte, vmdkSector)
	if err := readFull(r, size, b, 0, "the VMDK header"); err != nil {
		return nil, err
	}
	h, err := parseVMDKHeader(b)
	if err != nil {
		return nil, err
	}
	if h.version < 1 || h.version > 3 {
		return nil, unsupported("VMDK sparse extent version %d", h.version)
	}
	if h.flags&vmdkFlagNewline != 0 && h.newlineChars != [4]byte{'\n', ' ', '\r', '\n'} {
		return nil, malformed("the VMDK header's newline characters are corrupted (transferred in text mode?)")
	}
	format := VMDKSparse
	if h.flags&vmdkFlagCompress != 0 {
		format = VMDKStream
	}
	desc := ""
	if h.descOff > 0 && h.descSize > 0 {
		if h.descSize > 2048 {
			return nil, malformed("a VMDK descriptor of %d sectors", h.descSize)
		}
		d := make([]byte, h.descSize*vmdkSector)
		if err := readFull(r, size, d, int64(h.descOff)*vmdkSector, "the VMDK descriptor"); err != nil {
			return nil, err
		}
		desc = string(bytes.TrimRight(d, "\x00"))
	}
	if desc != "" {
		if m := vmdkCreateType.FindStringSubmatch(desc); m != nil {
			switch m[1] {
			case "monolithicSparse", "streamOptimized":
			default:
				return nil, unsupported("a %s VMDK: its data is in other files; only monolithic VMDK files are supported", m[1])
			}
		}
		if n := len(vmdkExtent.FindAllStringSubmatch(desc, -1)); n > 1 {
			return nil, unsupported("a VMDK of %d extents: only monolithic VMDK files are supported", n)
		}
		if m := vmdkParent.FindStringSubmatch(desc); m != nil && !strings.EqualFold(m[1], "ffffffff") {
			return nil, unsupported("a VMDK delta disk (snapshot): its parent disk is another file")
		}
	}
	if h.gdOff == vmdkGDAtEnd {
		// Stream-optimized: the footer, one sector before the
		// end-of-stream marker, carries the real GD offset.
		if size < 3*vmdkSector {
			return nil, malformed("a stream-optimized VMDK with no footer")
		}
		f := make([]byte, vmdkSector)
		if err := readFull(r, size, f, size-2*vmdkSector, "the VMDK footer"); err != nil {
			return nil, err
		}
		fh, err := parseVMDKHeader(f)
		if err != nil {
			return nil, malformed("a stream-optimized VMDK with no footer")
		}
		h.gdOff = fh.gdOff
		if fh.capacity != 0 {
			h.capacity = fh.capacity
		}
	}
	if h.compress != 0 && h.compress != vmdkCompDeflate {
		return nil, unsupported("VMDK compression algorithm %d", h.compress)
	}
	if h.grain < 1 || h.grain > 1<<16 || h.grain&(h.grain-1) != 0 {
		return nil, malformed("a VMDK grain of %d sectors", h.grain)
	}
	if h.gtes == 0 || h.gtes > 1<<16 {
		return nil, malformed("%d entries per VMDK grain table", h.gtes)
	}
	if h.capacity == 0 || h.capacity > maxSize/vmdkSector {
		return nil, malformed("a VMDK capacity of %d sectors", h.capacity)
	}
	grains := (h.capacity + h.grain - 1) / h.grain
	tables := (grains + uint64(h.gtes) - 1) / uint64(h.gtes)
	gd := make([]byte, 4*tables)
	if err := readFull(r, size, gd, int64(h.gdOff)*vmdkSector, "the VMDK grain directory"); err != nil {
		return nil, err
	}
	m := &vmdkMap{r: r, size: size, grain: int64(h.grain) * vmdkSector, gtes: int64(h.gtes),
		compressed: h.flags&vmdkFlagCompress != 0, gd: make([]uint32, tables), gts: map[int64][]uint32{}}
	for i := range m.gd {
		m.gd[i] = binary.LittleEndian.Uint32(gd[4*i:])
	}
	return &Disk{Format: format, Size: int64(h.capacity) * vmdkSector, m: m}, nil
}

type vmdkMap struct {
	r          io.ReaderAt
	size       int64
	grain      int64 // bytes
	gtes       int64
	compressed bool
	gd         []uint32
	gts        map[int64][]uint32
}

func (m *vmdkMap) chunkSize() int64 { return m.grain }

func (m *vmdkMap) table(t int64) ([]uint32, error) {
	if gt, ok := m.gts[t]; ok {
		return gt, nil
	}
	var gt []uint32
	if m.gd[t] != 0 {
		b := make([]byte, 4*m.gtes)
		if err := readFull(m.r, m.size, b, int64(m.gd[t])*vmdkSector, "a VMDK grain table"); err != nil {
			return nil, err
		}
		gt = make([]uint32, m.gtes)
		for i := range gt {
			gt[i] = binary.LittleEndian.Uint32(b[4*i:])
		}
	}
	if len(m.gts) > 4096 {
		clear(m.gts)
	}
	m.gts[t] = gt
	return gt, nil
}

func (m *vmdkMap) readChunk(i int64, b []byte) error {
	t := i / m.gtes
	if t >= int64(len(m.gd)) {
		return nil
	}
	gt, err := m.table(t)
	if err != nil || gt == nil {
		return err
	}
	e := int64(gt[i%m.gtes])
	if e <= 1 {
		return nil // never written, or a zero grain
	}
	off := e * vmdkSector
	if !m.compressed {
		n := min(m.grain, m.size-off)
		if n <= 0 {
			return malformed("a VMDK grain at sector %d is outside the file", e)
		}
		// The last grain of a file may be cut short; the rest is zeros.
		return readFull(m.r, m.size, b[:n], off, "a VMDK grain")
	}
	hdr := make([]byte, 12)
	if err := readFull(m.r, m.size, hdr, off, "a VMDK grain marker"); err != nil {
		return err
	}
	if lba := int64(binary.LittleEndian.Uint64(hdr)); lba != i*(m.grain/vmdkSector) {
		return malformed("the VMDK grain for sector %d is marked %d", i*(m.grain/vmdkSector), lba)
	}
	n := int64(binary.LittleEndian.Uint32(hdr[8:]))
	if n <= 0 || n > 2*m.grain+4096 {
		return malformed("a VMDK compressed grain of %d bytes", n)
	}
	data := make([]byte, n)
	if err := readFull(m.r, m.size, data, off+12, "a VMDK compressed grain"); err != nil {
		return err
	}
	return inflate(data, b)
}

// inflate decompresses a grain: RFC 1950 (zlib), as VMware writes it, or a
// bare RFC 1951 stream.
func inflate(data, b []byte) error {
	var zr io.ReadCloser
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		zr = flate.NewReader(bytes.NewReader(data))
	}
	defer zr.Close()
	n, err := io.ReadFull(zr, b)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		// A short final grain: the rest stays zero.
		if n == 0 {
			return malformed("an empty VMDK compressed grain")
		}
		return nil
	}
	if err != nil {
		return malformed("a VMDK compressed grain: %v", err)
	}
	return nil
}
