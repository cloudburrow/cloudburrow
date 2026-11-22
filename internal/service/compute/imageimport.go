package compute

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Image import from a Cloud Storage file (#2139,
// https://docs.cloud.google.com/compute/docs/import/import-existing-image
// and the images.insert reference, read 2026-11-22): images.insert with
// rawDisk.source, the Cloud Storage URL of a gzip-compressed tar archive
// holding a single file named disk.raw, the disk's bytes. The archive is
// read from this instance's own Cloud Storage (Options.ReadObject) and
// checked as Google checks it; the backend (RawImporter) then extracts the
// disk's root file system (its Linux partition's /etc, /usr, /var, /home,
// /root, /opt and /srv, what a boot disk holds here) into the image's
// storage, so a disk made from the image boots the imported system's files.

// ErrObjectNotFound is what Options.ReadObject wraps for a missing bucket
// or object.
var ErrObjectNotFound = errors.New("object not found")

// RawImporter is the optional VMBackend method that fills an image's
// storage from an imported disk.raw. open reopens the .tar.gz archive
// (which holds disk.raw) as often as the backend needs it. It returns the
// imported system's /etc/os-release, "" when it has none.
type RawImporter interface {
	ImportRaw(ctx context.Context, key string, sizeGb int64, open func(context.Context) (io.ReadCloser, error)) (osRelease string, err error)
}

// rawDiskObject parses rawDisk.source: https://storage.googleapis.com/B/O
// (the form the API documents), its storage.cloud.google.com twin, or
// gs://B/O as gcloud takes it.
func rawDiskObject(src string) (bucket, object string, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(src, "gs://"):
		rest = strings.TrimPrefix(src, "gs://")
	default:
		u, err := url.Parse(src)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
			(u.Host != "storage.googleapis.com" && u.Host != "storage.cloud.google.com" && u.Host != "commondatastorage.googleapis.com") {
			return "", "", false
		}
		rest = strings.TrimPrefix(u.Path, "/")
	}
	bucket, object, found := strings.Cut(rest, "/")
	if !found || bucket == "" || object == "" {
		return "", "", false
	}
	return bucket, object, true
}

// checkRawTar reads a .tar.gz archive as Google does: gzip-compressed, a tar
// archive of a single regular file named disk.raw. It returns the disk's
// size in bytes.
func checkRawTar(r io.Reader) (int64, error) {
	bad := errf(http.StatusBadRequest, "invalid", "The tar archive is not a valid image.")
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, bad
	}
	tr := tar.NewReader(zr)
	var size int64 = -1
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, bad
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir && (name == "" || name == ".") {
			continue
		}
		if name != "disk.raw" || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeGNUSparse) || size >= 0 {
			return 0, bad
		}
		size = h.Size
	}
	if size <= 0 {
		return 0, bad
	}
	return size, nil
}

// osReleaseImage is the public image whose container runs an imported
// system: the one of its distribution (os-release ID), else Debian's.
func osReleaseImage(osRelease string) PublicImage {
	id := ""
	for _, l := range strings.Split(osRelease, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "ID="); ok {
			id = strings.Trim(v, `"'`)
		}
	}
	for _, p := range PublicImages {
		if id != "" && strings.HasPrefix(p.Family, id) {
			return p
		}
	}
	return PublicImages[0]
}

// insertRawImageLocked is images.insert's rawDisk branch; o is the image
// so far (name, labels, family, ...).
func (s *Server) insertRawImageLocked(c call, rel string, o obj) (obj, error) {
	rd, _ := c.body["rawDisk"].(map[string]any)
	src := str(rd, "source")
	if src == "" {
		return nil, invalid("Invalid value for field 'resource.rawDisk.source': ''. The source must be specified.")
	}
	if ct := str(rd, "containerType"); ct != "" && ct != "TAR" {
		return nil, invalid("Invalid value for field 'resource.rawDisk.containerType': '%s'. Must be TAR.", ct)
	}
	bucket, object, ok := rawDiskObject(src)
	if !ok {
		return nil, invalid("Invalid value for field 'resource.rawDisk.source': '%s'. The URL is malformed.", src)
	}
	imp, isImp := s.opts.VMs.(RawImporter)
	if s.opts.ReadObject == nil || !isImp {
		return nil, unimplemented("images imported from a Cloud Storage file need Cloud Storage served on this instance")
	}
	if err := s.checkDiskBudget(); err != nil {
		return nil, err
	}
	o["rawDisk"] = obj{"source": src, "containerType": "TAR"}
	o["sourceType"] = "RAW"
	locs := strsAny(c.body["storageLocations"])
	if len(locs) == 0 {
		locs = []string{"us"}
	}
	o["storageLocations"] = toAny(locs)
	key := vmKey("img", o)
	o["_storageKey"] = key
	s.res[rel] = norm(o)
	read := s.opts.ReadObject
	open := func(ctx context.Context) (io.ReadCloser, error) {
		r, _, err := read(ctx, bucket, object)
		return r, err
	}
	var archive, size int64
	var osRelease string
	return s.runOpLocked(c.project, "global", "insert", rel, func(ctx context.Context) error {
		r, n, err := read(ctx, bucket, object)
		if errors.Is(err, ErrObjectNotFound) {
			return errf(http.StatusNotFound, "notFound", "The resource '%s' was not found", src)
		}
		if err != nil {
			return err
		}
		archive = n
		br := bufio.NewReader(r)
		if magic, _ := br.Peek(2); len(magic) < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
			// Not a gzip archive: a virtual disk (VMDK, VHD, VHDX),
			// converted to its raw disk here (virtualdisk.go).
			d, done, err := s.openVirtualDisk(br, n)
			_ = r.Close()
			if err != nil {
				return err
			}
			defer done()
			size = d.Size
			gb := (size + 1<<30 - 1) >> 30
			osRelease, err = imp.ImportRaw(ctx, key, gb, func(context.Context) (io.ReadCloser, error) { return rawTarGz(d), nil })
			return err
		}
		size, err = checkRawTar(br)
		_ = r.Close()
		if err != nil {
			return err
		}
		gb := (size + 1<<30 - 1) >> 30
		osRelease, err = imp.ImportRaw(ctx, key, gb, open)
		return err
	}, func(err error) {
		if err != nil {
			delete(s.res, rel)
			// Storage the importer made before failing.
			go func() { _ = s.opts.VMs.DeleteSnapshot(context.Background(), key) }()
			return
		}
		if cur, ok := s.res[rel]; ok {
			img := osReleaseImage(osRelease)
			cur["status"] = "READY"
			cur["archiveSizeBytes"] = itoa64(archive)
			cur["diskSizeGb"] = itoa64((size + 1<<30 - 1) >> 30)
			cur["_sourceImage"] = imageRel(img)
		}
	})
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
