package vdisk

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"io"
	"strings"
)

// VHDX ([MS-VHDX] 2.2-2.6): a file type identifier ("vhdxfile") at 0, two
// 4 KiB headers at 64 and 128 KiB (the valid one of the higher sequence
// number is current), two 64 KiB region tables at 192 and 256 KiB naming
// the BAT and metadata regions, and in the metadata region the file
// parameters (block size, has-parent), the virtual disk size and the
// logical sector size. The BAT interleaves a sector bitmap entry after
// every chunk ratio payload entries; a payload entry's state says whether
// its block is in the file (FULLY_PRESENT, at its FileOffsetMB) or reads
// as zeros. Every field is little-endian; structures carry CRC-32C
// checksums.

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// guidBytes is a GUID as [MS-DTYP] lays it out on disk.
func guidBytes(s string) []byte {
	h, _ := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	b := make([]byte, 16)
	binary.LittleEndian.PutUint32(b[0:], binary.BigEndian.Uint32(h[0:]))
	binary.LittleEndian.PutUint16(b[4:], binary.BigEndian.Uint16(h[4:]))
	binary.LittleEndian.PutUint16(b[6:], binary.BigEndian.Uint16(h[6:]))
	copy(b[8:], h[8:16])
	return b
}

var (
	vhdxBATRegion      = guidBytes("2DC27766-F623-4200-9D64-115E9BFD4A08")
	vhdxMetadataRegion = guidBytes("8B7CA206-4790-4B9A-B8FE-575F050F886E")
	vhdxFileParameters = guidBytes("CAA16737-FA36-4D43-B3B6-33F0AA44E76B")
	vhdxVirtualSize    = guidBytes("2FA54224-CD1B-4876-B211-5DBED83BF4B8")
	vhdxLogicalSector  = guidBytes("8141BF1D-A96F-4709-BA47-F233A8FAAB5F")
	vhdxParentLocator  = guidBytes("A8D35F2D-B30B-454D-ABF7-D3D84834AB0C")
)

const (
	vhdxKiB = 1 << 10
	vhdxMiB = 1 << 20

	vhdxPayloadFullyPresent     = 6
	vhdxPayloadPartiallyPresent = 7
)

// vhdxChecked is whether b's CRC-32C, its field at 4 counted as zero,
// is that field.
func vhdxChecked(b []byte) bool {
	want := binary.LittleEndian.Uint32(b[4:])
	c := bytes.Clone(b)
	clear(c[4:8])
	return crc32.Checksum(c, castagnoli) == want
}

func openVHDX(r io.ReaderAt, size int64) (*Disk, error) {
	// The current header.
	var cur []byte
	var seq uint64
	for _, off := range []int64{64 * vhdxKiB, 128 * vhdxKiB} {
		h := make([]byte, 4*vhdxKiB)
		if readFull(r, size, h, off, "a VHDX header") != nil || !bytes.HasPrefix(h, []byte("head")) || !vhdxChecked(h) {
			continue
		}
		if s := binary.LittleEndian.Uint64(h[8:]); cur == nil || s > seq {
			cur, seq = h, s
		}
	}
	if cur == nil {
		return nil, malformed("no valid VHDX header")
	}
	if v := binary.LittleEndian.Uint16(cur[66:]); v != 1 {
		return nil, unsupported("VHDX version %d", v)
	}
	if !bytes.Equal(cur[48:64], make([]byte, 16)) {
		return nil, unsupported("a VHDX with a log to replay (it was not closed cleanly): open it in Hyper-V once, or convert it")
	}
	// The region table.
	var batOff, batLen, metaOff, metaLen int64
	found := false
	for _, off := range []int64{192 * vhdxKiB, 256 * vhdxKiB} {
		t := make([]byte, 64*vhdxKiB)
		if readFull(r, size, t, off, "a VHDX region table") != nil || !bytes.HasPrefix(t, []byte("regi")) || !vhdxChecked(t) {
			continue
		}
		n := int(binary.LittleEndian.Uint32(t[8:]))
		if n > 2047 {
			continue
		}
		for i := 0; i < n; i++ {
			e := t[16+32*i : 48+32*i]
			o, l, req := int64(binary.LittleEndian.Uint64(e[16:])), int64(binary.LittleEndian.Uint32(e[24:])), binary.LittleEndian.Uint32(e[28:])&1 == 1
			switch {
			case bytes.Equal(e[:16], vhdxBATRegion):
				batOff, batLen = o, l
			case bytes.Equal(e[:16], vhdxMetadataRegion):
				metaOff, metaLen = o, l
			case req:
				return nil, unsupported("a VHDX with a required region this reader does not know")
			}
		}
		found = true
		break
	}
	if !found || batLen == 0 || metaLen < 64*vhdxKiB {
		return nil, malformed("no valid VHDX region table with the BAT and metadata regions")
	}
	// The metadata.
	meta := make([]byte, metaLen)
	if err := readFull(r, size, meta, metaOff, "the VHDX metadata region"); err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(meta, []byte("metadata")) {
		return nil, malformed("no VHDX metadata table")
	}
	items := map[string][]byte{}
	n := int(binary.LittleEndian.Uint16(meta[10:]))
	if n > 2047 {
		return nil, malformed("%d VHDX metadata entries", n)
	}
	for i := 0; i < n; i++ {
		e := meta[32+32*i : 64+32*i]
		o, l := int64(binary.LittleEndian.Uint32(e[16:])), int64(binary.LittleEndian.Uint32(e[20:]))
		if o+l > metaLen || (l > 0 && o < 64*vhdxKiB) {
			return nil, malformed("a VHDX metadata item outside its region")
		}
		items[string(e[:16])] = meta[o : o+l]
	}
	fp, vs, ls := items[string(vhdxFileParameters)], items[string(vhdxVirtualSize)], items[string(vhdxLogicalSector)]
	if len(fp) < 8 || len(vs) < 8 || len(ls) < 4 {
		return nil, malformed("the VHDX metadata lacks the file parameters, virtual disk size or logical sector size")
	}
	block := int64(binary.LittleEndian.Uint32(fp))
	if binary.LittleEndian.Uint32(fp[4:])&2 != 0 || items[string(vhdxParentLocator)] != nil {
		return nil, unsupported("a differencing VHDX: its parent disk is another file")
	}
	if block < vhdxMiB || block > 256*vhdxMiB || block&(block-1) != 0 {
		return nil, malformed("a VHDX block size of %d bytes", block)
	}
	sector := int64(binary.LittleEndian.Uint32(ls))
	if sector != 512 && sector != 4096 {
		return nil, malformed("a VHDX logical sector size of %d", sector)
	}
	virt := int64(binary.LittleEndian.Uint64(vs))
	if virt <= 0 || virt > maxSize {
		return nil, malformed("a VHDX virtual size of %d bytes", virt)
	}
	ratio := (int64(1) << 23) * sector / block
	blocks := (virt + block - 1) / block
	entries := blocks + (blocks-1)/ratio
	if entries*8 > batLen {
		return nil, malformed("a VHDX BAT of %d bytes for %d blocks", batLen, blocks)
	}
	bat := make([]byte, entries*8)
	if err := readFull(r, size, bat, batOff, "the VHDX BAT"); err != nil {
		return nil, err
	}
	m := &vhdxMap{r: r, size: size, block: block, ratio: ratio, bat: make([]uint64, entries)}
	for i := range m.bat {
		m.bat[i] = binary.LittleEndian.Uint64(bat[8*i:])
	}
	return &Disk{Format: VHDX, Size: virt, m: m}, nil
}

type vhdxMap struct {
	r            io.ReaderAt
	size         int64
	block, ratio int64
	bat          []uint64
}

func (m *vhdxMap) chunkSize() int64 { return m.block }

func (m *vhdxMap) readChunk(i int64, b []byte) error {
	e := m.bat[i+i/m.ratio]
	switch e & 7 {
	case vhdxPayloadFullyPresent:
		return readFull(m.r, m.size, b, int64(e>>20)*vhdxMiB, "a VHDX payload block")
	case vhdxPayloadPartiallyPresent:
		return unsupported("a VHDX block partially present (a differencing disk's)")
	}
	// NOT_PRESENT, UNDEFINED, ZERO, UNMAPPED: zeros.
	return nil
}
