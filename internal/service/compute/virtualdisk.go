package compute

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cloudburrow/cloudburrow/internal/vdisk"
)

// Image import from a virtual disk (#2139). Google imports a VMDK, VHD or
// VHDX from Cloud Storage with its image import tools, which convert the
// file to a raw disk and then create the image from it
// (https://docs.cloud.google.com/compute/docs/import/importing-virtual-disks,
// read 2026-11-22: "Only monolithic VMDK files are supported"; a flat
// VMDK's extent is renamed to disk.raw and imported in a .tar.gz). Here
// images.insert's rawDisk.source may name such a file as well as a
// .tar.gz: an object that is not a gzip archive is read as a virtual disk
// by internal/vdisk (written from the formats' published specifications,
// no qemu-img), spooled to a file beside the state, and its raw disk is
// handed to the backend as the disk.raw of a .tar.gz made on the fly, so
// it is imported as an archive's disk.raw is (imageimport.go). A file of
// none of the formats keeps Google's answer for a bad archive; a
// differencing disk, a VHDX with a log to replay, or a VMDK whose data is
// in other files (descriptor-only, split, delta) is refused with the
// reason.

// openVirtualDisk spools r (n bytes) to a temporary file and opens it as a
// virtual disk; done removes the file.
func (s *Server) openVirtualDisk(r io.Reader, n int64) (*vdisk.Disk, func(), error) {
	dir := ""
	if s.opts.State != "" {
		dir = filepath.Dir(s.opts.State)
	}
	f, err := os.CreateTemp(dir, "cloudburrow-vdisk-*")
	if err != nil {
		return nil, nil, err
	}
	done := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	if _, err := io.Copy(f, r); err != nil {
		done()
		return nil, nil, err
	}
	d, err := vdisk.Open(f, n)
	if err != nil {
		done()
		switch {
		case errors.Is(err, vdisk.ErrUnknown):
			return nil, nil, errf(http.StatusBadRequest, "invalid", "The tar archive is not a valid image.")
		case errors.Is(err, vdisk.ErrUnsupported), errors.Is(err, vdisk.ErrMalformed):
			return nil, nil, errf(http.StatusBadRequest, "invalid", "The virtual disk is not a valid image: %v.", err)
		}
		return nil, nil, err
	}
	return d, done, nil
}

// rawTarGz is d's raw disk as a .tar.gz of one disk.raw, made as it is
// read. A grain or block that cannot be read ends the stream with its
// error.
func rawTarGz(d *vdisk.Disk) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		zw, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
		tw := tar.NewWriter(zw)
		err := tw.WriteHeader(&tar.Header{Name: "disk.raw", Mode: 0o644, Size: d.Size, Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.Copy(tw, d.Reader())
		}
		if err == nil {
			err = tw.Close()
		}
		if err == nil {
			err = zw.Close()
		}
		if err != nil {
			err = errf(http.StatusBadRequest, "invalid", "The virtual disk is not a valid image: %v.", err)
		}
		_ = pw.CloseWithError(err)
	}()
	return pr
}
