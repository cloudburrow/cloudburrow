// Package vdisk reads virtual disk files as the raw disks they hold, for
// Compute Engine's image import of a virtual disk (#2139): VHD (fixed and
// dynamic), VHDX (fixed and dynamic) and VMDK (monolithic sparse and
// stream-optimized). It is written from the formats' published
// specifications, with nothing linked from another implementation:
//
//   - Microsoft's Virtual Hard Disk Image Format Specification, version 1.0
//     (the footer, the dynamic disk header, the block allocation table and
//     the per-block sector bitmaps);
//   - Microsoft's [MS-VHDX] Virtual Hard Disk v2 (VHDX) File Format (the
//     file type identifier, headers, region table, metadata region and the
//     BAT's payload and sector bitmap blocks);
//   - VMware's Virtual Disk Format 5.0 (the hosted sparse extent header,
//     grain directories and tables, the embedded descriptor, and the
//     stream-optimized format's compressed grains, markers and footer).
//
// A Disk is an io.ReaderAt of the raw disk, Size bytes long; unallocated
// blocks and grains read as zeros. Differencing disks (VHD type 4, VHDX
// with a parent), VHDX files with a log to replay, and VMDKs whose data is
// in other files (descriptor-only, split and flat extents) are refused with
// ErrUnsupported, as one Cloud Storage object can hold neither a parent
// nor another extent.
package vdisk

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Format names a virtual disk format.
type Format string

const (
	VHDFixed   Format = "VHD (fixed)"
	VHDDynamic Format = "VHD (dynamic)"
	VHDX       Format = "VHDX"
	VMDKSparse Format = "VMDK (monolithicSparse)"
	VMDKStream Format = "VMDK (streamOptimized)"
)

var (
	// ErrUnknown is a file that is none of the formats read here.
	ErrUnknown = errors.New("not a VMDK, VHD or VHDX file")
	// ErrUnsupported is a file of a known format whose kind is not read.
	ErrUnsupported = errors.New("unsupported virtual disk")
	// ErrMalformed is a file of a known format that breaks its
	// specification.
	ErrMalformed = errors.New("malformed virtual disk")
)

// maxSize is the largest virtual disk read: 64 TiB, VHDX's own limit and
// above Compute Engine's image size limit.
const maxSize = 64 << 40

func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, a...))
}

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, a...))
}

// mapper reads a disk in chunks of a fixed size.
type mapper interface {
	chunkSize() int64
	// readChunk fills b, chunkSize long and zeroed, with chunk i.
	readChunk(i int64, b []byte) error
}

// Disk is a virtual disk file read as its raw disk.
type Disk struct {
	Format Format
	// Size is the raw disk's size in bytes.
	Size int64

	m        mapper
	mu       sync.Mutex
	cacheIdx int64
	cache    []byte
}

// ReadAt reads the raw disk's bytes at off.
func (d *Disk) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("vdisk: negative offset")
	}
	if off >= d.Size {
		return 0, io.EOF
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cs := d.m.chunkSize()
	n := 0
	for n < len(p) && off < d.Size {
		i := off / cs
		if d.cache == nil || d.cacheIdx != i {
			if d.cache == nil {
				d.cache = make([]byte, cs)
			} else {
				clear(d.cache)
			}
			d.cacheIdx = -1
			if err := d.m.readChunk(i, d.cache); err != nil {
				return n, err
			}
			d.cacheIdx = i
		}
		within := off - i*cs
		end := min(cs, d.Size-i*cs)
		c := copy(p[n:], d.cache[within:end])
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Reader reads the whole raw disk from its start.
func (d *Disk) Reader() io.Reader { return io.NewSectionReader(d, 0, d.Size) }

// Detect names the format of a file from its first and last bytes (head
// at least 512 and tail the last 512, or fewer for a smaller file), or
// ErrUnknown.
func Detect(head, tail []byte) (Format, error) {
	switch {
	case bytes.HasPrefix(head, []byte("vhdxfile")):
		return VHDX, nil
	case bytes.HasPrefix(head, []byte("KDMV")):
		return VMDKSparse, nil
	case bytes.HasPrefix(head, []byte("# Disk DescriptorFile")):
		return "", unsupported("a VMDK descriptor file: its extents are other files, and only monolithic VMDK files are supported; rename the -flat.vmdk extent to disk.raw and import it in a .tar.gz")
	case bytes.HasPrefix(head, []byte("COWD")):
		return "", unsupported("an ESX Server sparse (COWD) VMDK extent")
	case len(tail) >= 512 && bytes.HasPrefix(tail[len(tail)-512:], []byte("conectix")),
		len(tail) >= 511 && bytes.HasPrefix(tail[len(tail)-511:], []byte("conectix")):
		return VHDFixed, nil // or dynamic: the footer says which
	case bytes.HasPrefix(head, []byte("conectix")):
		return VHDDynamic, nil
	}
	return "", ErrUnknown
}

// Open reads the virtual disk file r, size bytes long.
func Open(r io.ReaderAt, size int64) (*Disk, error) {
	head := make([]byte, min(size, 512))
	if _, err := r.ReadAt(head, 0); err != nil && err != io.EOF {
		return nil, err
	}
	tail := make([]byte, min(size, 512))
	if _, err := r.ReadAt(tail, size-int64(len(tail))); err != nil && err != io.EOF {
		return nil, err
	}
	f, err := Detect(head, tail)
	if err != nil {
		return nil, err
	}
	var d *Disk
	switch f {
	case VHDFixed, VHDDynamic:
		d, err = openVHD(r, size)
	case VHDX:
		d, err = openVHDX(r, size)
	case VMDKSparse:
		d, err = openVMDK(r, size)
	}
	if err != nil {
		return nil, err
	}
	if d.Size <= 0 || d.Size > maxSize {
		return nil, malformed("a virtual size of %d bytes", d.Size)
	}
	d.cacheIdx = -1
	return d, nil
}

// readFull reads len(b) bytes of r at off, every one of them inside size.
func readFull(r io.ReaderAt, size int64, b []byte, off int64, what string) error {
	if off < 0 || off+int64(len(b)) > size {
		return malformed("%s at %d (%d bytes) is outside the file", what, off, len(b))
	}
	n, err := r.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = malformed("%s at %d is truncated", what, off)
	}
	return err
}
