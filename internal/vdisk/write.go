package vdisk

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Writers of the formats Open reads, for tests and fixtures: a VHD (fixed
// or dynamic) and a VMDK (monolithic sparse or stream-optimized) of a raw
// disk, laid out as the specifications describe. Blocks and grains of
// zeros are left unallocated.

// vhdGeometry is the CHS geometry the VHD specification's appendix
// computes for a disk of the given sectors.
func vhdGeometry(sectors int64) uint32 {
	sectors = min(sectors, 65535*16*255)
	var spt, heads, cth int64
	if sectors >= 65535*16*63 {
		spt, heads = 255, 16
		cth = sectors / spt
	} else {
		spt = 17
		cth = sectors / spt
		heads = max((cth+1023)/1024, 4)
		if cth >= heads*1024 || heads > 16 {
			spt, heads = 31, 16
			cth = sectors / spt
		}
		if cth >= heads*1024 {
			spt, heads = 63, 16
			cth = sectors / spt
		}
	}
	cyl := cth / heads
	return uint32(cyl)<<16 | uint32(heads)<<8 | uint32(spt)
}

func vhdFooter(size int64, dynamic bool) []byte {
	f := make([]byte, 512)
	copy(f, "conectix")
	be := binary.BigEndian
	be.PutUint32(f[8:], 2)
	be.PutUint32(f[12:], 0x00010000)
	if dynamic {
		be.PutUint64(f[16:], 512)
		be.PutUint32(f[60:], vhdTypeDynamic)
	} else {
		be.PutUint64(f[16:], 0xFFFFFFFFFFFFFFFF)
		be.PutUint32(f[60:], vhdTypeFixed)
	}
	// Seconds since 2000-01-01 UTC.
	be.PutUint32(f[24:], uint32(time.Now().Unix()-946684800))
	copy(f[28:], "cbrw")
	be.PutUint32(f[32:], 0x00010000)
	copy(f[36:], "Wi2k")
	be.PutUint64(f[40:], uint64(size))
	be.PutUint64(f[48:], uint64(size))
	be.PutUint32(f[56:], vhdGeometry(size/512))
	_, _ = rand.Read(f[68:84])
	be.PutUint32(f[64:], vhdChecksum(f, 64))
	return f
}

// WriteVHD writes raw (size bytes, a multiple of 512) as a VHD.
func WriteVHD(w io.Writer, raw io.ReaderAt, size int64, dynamic bool) error {
	if size <= 0 || size%512 != 0 {
		return fmt.Errorf("vdisk: a VHD's size must be a positive multiple of 512, not %d", size)
	}
	footer := vhdFooter(size, dynamic)
	if !dynamic {
		if _, err := io.Copy(w, io.NewSectionReader(raw, 0, size)); err != nil {
			return err
		}
		_, err := w.Write(footer)
		return err
	}
	const block = 2 << 20
	entries := (size + block - 1) / block
	batBytes := (4*entries + 511) / 512 * 512
	hdr := make([]byte, 1024)
	be := binary.BigEndian
	copy(hdr, "cxsparse")
	be.PutUint64(hdr[8:], 0xFFFFFFFFFFFFFFFF)
	be.PutUint64(hdr[16:], 1536)
	be.PutUint32(hdr[24:], 0x00010000)
	be.PutUint32(hdr[28:], uint32(entries))
	be.PutUint32(hdr[32:], block)
	be.PutUint32(hdr[36:], vhdChecksum(hdr, 36))
	bat := bytes.Repeat([]byte{0xff}, int(batBytes))
	buf := make([]byte, block)
	used := make([]bool, entries)
	next := int64(1536+batBytes) / 512
	for i := int64(0); i < entries; i++ {
		if nonZero(raw, size, i*block, buf) {
			used[i] = true
			be.PutUint32(bat[4*i:], uint32(next))
			next += 1 + block/512 // a 512-byte bitmap, then the block
		}
	}
	for _, b := range [][]byte{footer, hdr, bat} {
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	bitmap := bytes.Repeat([]byte{0xff}, 512)
	for i := int64(0); i < entries; i++ {
		if !used[i] {
			continue
		}
		clear(buf)
		readSome(raw, size, i*block, buf)
		if _, err := w.Write(bitmap); err != nil {
			return err
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	_, err := w.Write(footer)
	return err
}

// nonZero reads the chunk at off into buf and says whether any byte is set.
func nonZero(raw io.ReaderAt, size, off int64, buf []byte) bool {
	clear(buf)
	n := readSome(raw, size, off, buf)
	for _, c := range buf[:n] {
		if c != 0 {
			return true
		}
	}
	return false
}

func readSome(raw io.ReaderAt, size, off int64, buf []byte) int {
	n := min(int64(len(buf)), size-off)
	if n <= 0 {
		return 0
	}
	m, _ := raw.ReadAt(buf[:n], off)
	return m
}

func vmdkDescriptor(sectors int64, stream bool) []byte {
	t := "monolithicSparse"
	if stream {
		t = "streamOptimized"
	}
	return []byte(fmt.Sprintf("# Disk DescriptorFile\nversion=1\nCID=fffffffe\nparentCID=ffffffff\ncreateType=%q\n\n"+
		"# Extent description\nRW %d SPARSE \"disk.vmdk\"\n\n# The Disk Data Base\n#DDB\n\n"+
		"ddb.virtualHWVersion = \"4\"\nddb.adapterType = \"lsilogic\"\n", t, sectors))
}

func vmdkHeaderBytes(version, flags uint32, capacity, grain, descOff, descSize uint64, gtes uint32, gdOff, overhead uint64, compress uint16) []byte {
	b := make([]byte, 512)
	le := binary.LittleEndian
	copy(b, "KDMV")
	le.PutUint32(b[4:], version)
	le.PutUint32(b[8:], flags)
	le.PutUint64(b[12:], capacity)
	le.PutUint64(b[20:], grain)
	le.PutUint64(b[28:], descOff)
	le.PutUint64(b[36:], descSize)
	le.PutUint32(b[44:], gtes)
	le.PutUint64(b[48:], 0) // no redundant grain directory
	le.PutUint64(b[56:], gdOff)
	le.PutUint64(b[64:], overhead)
	copy(b[73:], "\n \r\n")
	le.PutUint16(b[77:], compress)
	return b
}

// WriteVMDK writes raw (size bytes, a multiple of 512) as a monolithic
// sparse VMDK, or a stream-optimized one with zlib-compressed grains.
func WriteVMDK(w io.Writer, raw io.ReaderAt, size int64, stream bool) error {
	if size <= 0 || size%512 != 0 {
		return fmt.Errorf("vdisk: a VMDK's size must be a positive multiple of 512, not %d", size)
	}
	const grain, gtes, descSectors = 128, 512, 20 // 64 KiB grains
	sectors := size / 512
	grains := (sectors + grain - 1) / grain
	tables := (grains + gtes - 1) / gtes
	desc := make([]byte, descSectors*512)
	copy(desc, vmdkDescriptor(sectors, stream))
	gt := make([]byte, 4*gtes*tables)
	gd := make([]byte, (4*tables+511)/512*512)
	buf := make([]byte, grain*512)
	le := binary.LittleEndian
	if !stream {
		gdSector := int64(1 + descSectors)
		gtSector := gdSector + int64(len(gd))/512
		overhead := (gtSector + int64(len(gt))/512 + grain - 1) / grain * grain
		next := overhead
		for t := int64(0); t < tables; t++ {
			le.PutUint32(gd[4*t:], uint32(gtSector+t*4*gtes/512))
		}
		var used []int64
		for g := int64(0); g < grains; g++ {
			if nonZero(raw, size, g*grain*512, buf) {
				le.PutUint32(gt[4*g:], uint32(next))
				used = append(used, g)
				next += grain
			}
		}
		pad := make([]byte, (overhead-gtSector)*512-int64(len(gt)))
		hdr := vmdkHeaderBytes(1, vmdkFlagNewline, uint64(sectors), grain, 1, descSectors, gtes, uint64(gdSector), uint64(overhead), 0)
		for _, b := range [][]byte{hdr, desc, gd, gt, pad} {
			if _, err := w.Write(b); err != nil {
				return err
			}
		}
		for _, g := range used {
			nonZero(raw, size, g*grain*512, buf)
			if _, err := w.Write(buf); err != nil {
				return err
			}
		}
		return nil
	}
	flags := uint32(vmdkFlagNewline | vmdkFlagCompress | vmdkFlagMarkers)
	hdr := vmdkHeaderBytes(3, flags, uint64(sectors), grain, 1, descSectors, gtes, vmdkGDAtEnd, uint64(1+descSectors), vmdkCompDeflate)
	cw := &countWriter{w: w}
	for _, b := range [][]byte{hdr, desc} {
		if _, err := cw.Write(b); err != nil {
			return err
		}
	}
	for g := int64(0); g < grains; g++ {
		if !nonZero(raw, size, g*grain*512, buf) {
			continue
		}
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		n := min(int64(len(buf)), size-g*grain*512)
		_, _ = zw.Write(buf[:n])
		_ = zw.Close()
		le.PutUint32(gt[4*g:], uint32(cw.n/512))
		m := make([]byte, 12)
		le.PutUint64(m, uint64(g*grain))
		le.PutUint32(m[8:], uint32(z.Len()))
		rec := append(m, z.Bytes()...)
		rec = append(rec, make([]byte, (512-len(rec)%512)%512)...)
		if _, err := cw.Write(rec); err != nil {
			return err
		}
	}
	marker := func(sectors uint64, typ uint32) []byte {
		m := make([]byte, 512)
		le.PutUint64(m, sectors)
		le.PutUint32(m[12:], typ)
		return m
	}
	for t := int64(0); t < tables; t++ {
		if _, err := cw.Write(marker(4*gtes/512, 1)); err != nil {
			return err
		}
		le.PutUint32(gd[4*t:], uint32(cw.n/512))
		if _, err := cw.Write(gt[t*4*gtes : (t+1)*4*gtes]); err != nil {
			return err
		}
	}
	if _, err := cw.Write(marker(uint64(len(gd)/512), 2)); err != nil {
		return err
	}
	gdSector := cw.n / 512
	if _, err := cw.Write(gd); err != nil {
		return err
	}
	footer := vmdkHeaderBytes(3, flags, uint64(sectors), grain, 1, descSectors, gtes, uint64(gdSector), uint64(1+descSectors), vmdkCompDeflate)
	for _, b := range [][]byte{marker(1, 3), footer, make([]byte, 512)} {
		if _, err := cw.Write(b); err != nil {
			return err
		}
	}
	return nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}
