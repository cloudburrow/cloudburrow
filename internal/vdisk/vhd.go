package vdisk

import (
	"bytes"
	"encoding/binary"
	"io"
)

// VHD (Virtual Hard Disk Image Format Specification 1.0): every file ends
// in a 512-byte footer (511 for files of Virtual PC before 2004); a
// dynamic disk also starts with a copy of it, followed at the footer's data
// offset by a 1024-byte dynamic disk header naming the block allocation
// table (BAT). Each BAT entry is the sector of its block's sector bitmap,
// which the block's data follows, or 0xFFFFFFFF for a block never written.
// Every field is big-endian.

const (
	vhdTypeFixed        = 2
	vhdTypeDynamic      = 3
	vhdTypeDifferencing = 4
	vhdUnused           = 0xFFFFFFFF
)

// vhdChecksum is the one's complement of the sum of b's bytes, the
// checksum field (at sum) counted as zero.
func vhdChecksum(b []byte, sum int) uint32 {
	var s uint32
	for i, c := range b {
		if i >= sum && i < sum+4 {
			continue
		}
		s += uint32(c)
	}
	return ^s
}

func openVHD(r io.ReaderAt, size int64) (*Disk, error) {
	footer := make([]byte, 512)
	at := size - 512
	if err := readFull(r, size, footer, at, "the VHD footer"); err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(footer, []byte("conectix")) {
		// A 511-byte footer.
		at = size - 511
		clear(footer)
		if err := readFull(r, size, footer[:511], at, "the VHD footer"); err != nil {
			return nil, err
		}
	}
	if !bytes.HasPrefix(footer, []byte("conectix")) {
		// Only the copy at the start (a footer damaged at the end).
		at = 0
		if err := readFull(r, size, footer, 0, "the VHD footer"); err != nil {
			return nil, err
		}
	}
	if got, want := binary.BigEndian.Uint32(footer[64:]), vhdChecksum(footer, 64); got != want {
		return nil, malformed("the VHD footer's checksum is %#x, not %#x", got, want)
	}
	if v := binary.BigEndian.Uint32(footer[12:]); v>>16 != 1 {
		return nil, unsupported("VHD file format version %#x", v)
	}
	current := int64(binary.BigEndian.Uint64(footer[48:]))
	switch t := binary.BigEndian.Uint32(footer[60:]); t {
	case vhdTypeFixed:
		if current > at {
			return nil, malformed("a fixed VHD of %d bytes holds only %d", current, at)
		}
		return &Disk{Format: VHDFixed, Size: current, m: &rawMap{r: r, size: current}}, nil
	case vhdTypeDynamic:
	case vhdTypeDifferencing:
		return nil, unsupported("a differencing VHD: its parent disk is another file")
	default:
		return nil, malformed("VHD disk type %d", t)
	}
	hdrOff := int64(binary.BigEndian.Uint64(footer[16:]))
	hdr := make([]byte, 1024)
	if err := readFull(r, size, hdr, hdrOff, "the VHD dynamic disk header"); err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(hdr, []byte("cxsparse")) {
		return nil, malformed("no dynamic disk header at %d", hdrOff)
	}
	if got, want := binary.BigEndian.Uint32(hdr[36:]), vhdChecksum(hdr, 36); got != want {
		return nil, malformed("the VHD dynamic disk header's checksum is %#x, not %#x", got, want)
	}
	batOff := int64(binary.BigEndian.Uint64(hdr[16:]))
	entries := int64(binary.BigEndian.Uint32(hdr[28:]))
	block := int64(binary.BigEndian.Uint32(hdr[32:]))
	if block < 512 || block > 256<<20 || block&(block-1) != 0 {
		return nil, malformed("a VHD block size of %d bytes", block)
	}
	if current <= 0 || current > maxSize || entries < (current+block-1)/block {
		return nil, malformed("a VHD of %d bytes in %d blocks of %d", current, entries, block)
	}
	bat := make([]byte, 4*entries)
	if err := readFull(r, size, bat, batOff, "the VHD block allocation table"); err != nil {
		return nil, err
	}
	m := &vhdMap{r: r, size: size, block: block, bat: make([]uint32, entries)}
	for i := range m.bat {
		m.bat[i] = binary.BigEndian.Uint32(bat[4*i:])
	}
	// The sector bitmap: a bit per sector, padded to a sector boundary.
	m.bitmap = ((block/512+7)/8 + 511) / 512 * 512
	return &Disk{Format: VHDDynamic, Size: current, m: m}, nil
}

type vhdMap struct {
	r             io.ReaderAt
	size          int64
	block, bitmap int64
	bat           []uint32
}

func (m *vhdMap) chunkSize() int64 { return m.block }

func (m *vhdMap) readChunk(i int64, b []byte) error {
	if i >= int64(len(m.bat)) || m.bat[i] == vhdUnused {
		return nil
	}
	off := int64(m.bat[i]) * 512
	bm := make([]byte, m.bitmap)
	if err := readFull(m.r, m.size, bm, off, "a VHD sector bitmap"); err != nil {
		return err
	}
	if err := readFull(m.r, m.size, b, off+m.bitmap, "a VHD block"); err != nil {
		return err
	}
	// A sector whose bit is clear was never written: zeros.
	for s := int64(0); s < m.block/512; s++ {
		if bm[s/8]&(0x80>>(s%8)) == 0 {
			clear(b[s*512 : (s+1)*512])
		}
	}
	return nil
}

// rawMap is a raw disk at the start of a file (a fixed VHD).
type rawMap struct {
	r    io.ReaderAt
	size int64
}

func (m *rawMap) chunkSize() int64 { return 1 << 20 }

func (m *rawMap) readChunk(i int64, b []byte) error {
	off := i << 20
	n := min(int64(len(b)), m.size-off)
	return readFull(m.r, m.size, b[:n], off, "a fixed VHD's data")
}
