package vdisk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math/rand"
	"testing"
)

// sample is a raw disk of size bytes: random data in a few places, the
// rest zeros, so both allocated and unallocated blocks are read.
func sample(size int64) []byte {
	b := make([]byte, size)
	r := rand.New(rand.NewSource(1))
	for _, off := range []int64{0, 70 << 10, 3 << 20, size - 1500} {
		if off >= 0 && off < size {
			r.Read(b[off:min(off+4096, size)])
		}
	}
	return b
}

func open(t *testing.T, file []byte) *Disk {
	t.Helper()
	d, err := Open(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func roundTrip(t *testing.T, raw []byte, file []byte, want Format) {
	t.Helper()
	d := open(t, file)
	if d.Format != want || d.Size != int64(len(raw)) {
		t.Fatalf("Open: %s of %d bytes; want %s of %d", d.Format, d.Size, want, len(raw))
	}
	got, err := io.ReadAll(d.Reader())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		for i := range got {
			if got[i] != raw[i] {
				t.Fatalf("raw disk differs at byte %d", i)
			}
		}
		t.Fatalf("raw disk of %d bytes, want %d", len(got), len(raw))
	}
	// Random access across chunk boundaries.
	p := make([]byte, 5000)
	for _, off := range []int64{1, 65530, 2<<20 - 100, int64(len(raw)) - 5000} {
		if n, err := d.ReadAt(p, off); n != len(p) || err != nil || !bytes.Equal(p, raw[off:off+5000]) {
			t.Fatalf("ReadAt(%d): %d %v", off, n, err)
		}
	}
	if _, err := d.ReadAt(p, d.Size); err != io.EOF {
		t.Fatalf("ReadAt past the end: %v", err)
	}
}

func TestVHDFixedAndDynamicRoundTrip(t *testing.T) {
	raw := sample(9<<20 + 512*7)
	for _, dynamic := range []bool{false, true} {
		var f bytes.Buffer
		if err := WriteVHD(&f, bytes.NewReader(raw), int64(len(raw)), dynamic); err != nil {
			t.Fatal(err)
		}
		want := VHDFixed
		if dynamic {
			want = VHDDynamic
			// Zero blocks are not in the file: the dynamic disk is smaller.
			if f.Len() >= len(raw) {
				t.Fatalf("a dynamic VHD of %d bytes for %d", f.Len(), len(raw))
			}
		}
		roundTrip(t, raw, f.Bytes(), want)
	}
}

func TestVHDDynamicSectorBitmap(t *testing.T) {
	raw := sample(4 << 20)
	var f bytes.Buffer
	if err := WriteVHD(&f, bytes.NewReader(raw), int64(len(raw)), true); err != nil {
		t.Fatal(err)
	}
	file := f.Bytes()
	// Block 0's bitmap is right after the BAT (sector 3 + 1); clearing
	// sector 0's bit makes it read as zeros, as the specification says.
	bat := binary.BigEndian.Uint32(file[1536:])
	file[int64(bat)*512] &^= 0x80
	d := open(t, file)
	p := make([]byte, 1024)
	if _, err := d.ReadAt(p, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p[:512], make([]byte, 512)) || !bytes.Equal(p[512:], raw[512:1024]) {
		t.Fatal("a sector whose bitmap bit is clear does not read as zeros")
	}
}

func TestVMDKSparseAndStreamOptimizedRoundTrip(t *testing.T) {
	raw := sample(3<<20 + 512*3)
	for _, stream := range []bool{false, true} {
		var f bytes.Buffer
		if err := WriteVMDK(&f, bytes.NewReader(raw), int64(len(raw)), stream); err != nil {
			t.Fatal(err)
		}
		want := VMDKSparse
		if stream {
			want = VMDKStream
		}
		if f.Len() >= len(raw) {
			t.Fatalf("%s: %d bytes for a mostly empty disk of %d", want, f.Len(), len(raw))
		}
		roundTrip(t, raw, f.Bytes(), want)
	}
}

// vhdx builds a VHDX by hand ([MS-VHDX]): 1 MiB blocks, 512-byte logical
// sectors, every non-zero block FULLY_PRESENT and the rest NOT_PRESENT.
func vhdx(raw []byte, mutate func(meta []byte)) []byte {
	const mib = 1 << 20
	size := int64(len(raw))
	blocks := (size + mib - 1) / mib
	le := binary.LittleEndian
	f := make([]byte, 3*mib)
	copy(f, "vhdxfile")
	crc := func(b []byte) { clear(b[4:8]); le.PutUint32(b[4:], crc32.Checksum(b, castagnoli)) }
	for i, off := range []int{64 << 10, 128 << 10} {
		h := f[off : off+4096]
		copy(h, "head")
		le.PutUint64(h[8:], uint64(i+1))
		le.PutUint16(h[66:], 1)
		le.PutUint32(h[68:], mib)
		le.PutUint64(h[72:], mib)
		crc(h)
	}
	for _, off := range []int{192 << 10, 256 << 10} {
		r := f[off : off+64<<10]
		copy(r, "regi")
		le.PutUint32(r[8:], 2)
		copy(r[16:], vhdxBATRegion)
		le.PutUint64(r[32:], 2*mib)
		le.PutUint32(r[40:], mib)
		le.PutUint32(r[44:], 1)
		copy(r[48:], vhdxMetadataRegion)
		le.PutUint64(r[64:], mib)
		le.PutUint32(r[72:], mib)
		le.PutUint32(r[76:], 1)
		crc(r)
	}
	meta := f[mib : 2*mib]
	copy(meta, "metadata")
	le.PutUint16(meta[10:], 3)
	item := func(i int, id []byte, off, n uint32) {
		e := meta[32+32*i:]
		copy(e, id)
		le.PutUint32(e[16:], off)
		le.PutUint32(e[20:], n)
		le.PutUint32(e[24:], 4)
	}
	item(0, vhdxFileParameters, 64<<10, 8)
	le.PutUint32(meta[64<<10:], mib)
	item(1, vhdxVirtualSize, 64<<10+8, 8)
	le.PutUint64(meta[64<<10+8:], uint64(size))
	item(2, vhdxLogicalSector, 64<<10+16, 4)
	le.PutUint32(meta[64<<10+16:], 512)
	if mutate != nil {
		mutate(meta)
	}
	bat := f[2*mib : 3*mib]
	for b := int64(0); b < blocks; b++ {
		chunk := raw[b*mib : min((b+1)*mib, size)]
		if bytes.Equal(chunk, make([]byte, len(chunk))) {
			continue
		}
		at := int64(len(f))
		f = append(f, chunk...)
		f = append(f, make([]byte, mib-len(chunk))...)
		le.PutUint64(f[2*mib+8*b:], uint64(at/mib)<<20|vhdxPayloadFullyPresent)
	}
	_ = bat
	return f
}

func TestVHDXRoundTrip(t *testing.T) {
	raw := sample(5<<20 + 512)
	roundTrip(t, raw, vhdx(raw, nil), VHDX)
}

func TestRefused(t *testing.T) {
	raw := sample(2 << 20)
	var vhd bytes.Buffer
	_ = WriteVHD(&vhd, bytes.NewReader(raw), int64(len(raw)), false)
	var sparse bytes.Buffer
	_ = WriteVMDK(&sparse, bytes.NewReader(raw), int64(len(raw)), false)

	differencing := bytes.Clone(vhd.Bytes())
	foot := differencing[len(differencing)-512:]
	binary.BigEndian.PutUint32(foot[60:], vhdTypeDifferencing)
	binary.BigEndian.PutUint32(foot[64:], vhdChecksum(foot, 64))

	badSum := bytes.Clone(vhd.Bytes())
	badSum[len(badSum)-512+50] ^= 1

	split := bytes.Clone(sparse.Bytes())
	i := bytes.Index(split, []byte(`"monolithicSparse"`))
	copy(split[i:], `"twoGbMaxExtentSparse" `)

	textMode := bytes.Clone(sparse.Bytes())
	copy(textMode[73:], "\n \n\n")

	truncated := sparse.Bytes()[:len(sparse.Bytes())-70000]

	diffVHDX := vhdx(raw, func(meta []byte) { binary.LittleEndian.PutUint32(meta[64<<10+4:], 2) })
	noSize := vhdx(raw, func(meta []byte) { binary.LittleEndian.PutUint16(meta[10:], 1) })

	for name, c := range map[string]struct {
		file []byte
		want error
	}{
		"a raw disk":                   {raw, ErrUnknown},
		"a gzip file":                  {[]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0}, ErrUnknown},
		"a descriptor-only VMDK":       {[]byte("# Disk DescriptorFile\nversion=1\ncreateType=\"monolithicFlat\"\nRW 4096 FLAT \"d-flat.vmdk\" 0\n"), ErrUnsupported},
		"a differencing VHD":           {differencing, ErrUnsupported},
		"a VHD footer checksum":        {badSum, ErrMalformed},
		"a split VMDK":                 {split, ErrUnsupported},
		"a VMDK sent in text mode":     {textMode, ErrMalformed},
		"a truncated VMDK":             {truncated, ErrMalformed},
		"a differencing VHDX":          {diffVHDX, ErrUnsupported},
		"a VHDX without its disk size": {noSize, ErrMalformed},
	} {
		d, err := Open(bytes.NewReader(c.file), int64(len(c.file)))
		if err == nil && d != nil {
			// A truncation shows when the missing grains are read.
			_, err = io.ReadAll(d.Reader())
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v; want %v", name, err, c.want)
		}
	}
}
