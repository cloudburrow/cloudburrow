package compute

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/vdisk"
)

// rawVMs is fakeVMs that unpacks the disk.raw it is given.
type rawVMs struct {
	fakeVMs
	raws  [][]byte
	sizes []int64
}

func (f *rawVMs) ImportRaw(ctx context.Context, key string, sizeGb int64, open func(context.Context) (io.ReadCloser, error)) (string, error) {
	r, err := open(ctx)
	if err != nil {
		return "", err
	}
	defer r.Close()
	zr, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(zr)
	h, err := tr.Next()
	if err != nil || h.Name != "disk.raw" {
		return "", fmt.Errorf("archive: %v %v", h, err)
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		return "", err
	}
	if _, err := tr.Next(); err != io.EOF {
		return "", fmt.Errorf("more than disk.raw: %v", err)
	}
	f.raws, f.sizes = append(f.raws, b), append(f.sizes, sizeGb)
	return "ID=debian\n", nil
}

func TestImageImportFromAVirtualDisk(t *testing.T) {
	raw := make([]byte, 6<<20)
	copy(raw[1024+56:], []byte{0x53, 0xef}) // an ext4 superblock's magic
	copy(raw[5<<20:], "the end of the disk")
	objects := map[string][]byte{}
	put := func(name string, write func(io.Writer) error) {
		var b bytes.Buffer
		if err := write(&b); err != nil {
			t.Fatal(err)
		}
		objects["b/"+name] = b.Bytes()
	}
	ra := bytes.NewReader(raw)
	n := int64(len(raw))
	put("disk.vmdk", func(w io.Writer) error { return vdisk.WriteVMDK(w, ra, n, false) })
	put("stream.vmdk", func(w io.Writer) error { return vdisk.WriteVMDK(w, ra, n, true) })
	put("fixed.vhd", func(w io.Writer) error { return vdisk.WriteVHD(w, ra, n, false) })
	put("dynamic.vhd", func(w io.Writer) error { return vdisk.WriteVHD(w, ra, n, true) })
	objects["b/desc.vmdk"] = []byte("# Disk DescriptorFile\nversion=1\ncreateType=\"monolithicFlat\"\nRW 12288 FLAT \"d-flat.vmdk\" 0\n")
	diff := bytes.Clone(objects["b/fixed.vhd"])
	diff[len(diff)-512+60+3] = 4 // a differencing disk; its checksum now fails
	objects["b/diff.vhd"] = diff

	vms := &rawVMs{}
	dir := t.TempDir()
	s, err := New(Options{VMs: vms, State: dir + "/state.json", ReadObject: func(_ context.Context, bucket, object string) (io.ReadCloser, int64, error) {
		b, ok := objects[bucket+"/"+object]
		if !ok {
			return nil, 0, fmt.Errorf("%s/%s: %w", bucket, object, ErrObjectNotFound)
		}
		return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"disk.vmdk", "stream.vmdk", "fixed.vhd", "dynamic.vhd"} {
		img := fmt.Sprintf("img-%d", i)
		op, err := vmCall(t, s, "POST", "global/images", obj{"name": img, "rawDisk": obj{"source": "gs://b/" + name}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if op, _ = s.Call("GET", "p", "global/operations/"+lastSeg(str(op, "selfLink")), nil, nil); op["error"] != nil {
			t.Fatalf("%s: operation %v", name, op)
		}
		got, err := s.Call("GET", "p", "global/images/"+img, nil, nil)
		if err != nil || got["status"] != "READY" || got["diskSizeGb"] != "1" || got["archiveSizeBytes"] != fmt.Sprint(len(objects["b/"+name])) {
			t.Fatalf("%s: image %v %v", name, got, err)
		}
		if !bytes.Equal(vms.raws[i], raw) {
			t.Fatalf("%s: the imported disk.raw differs from the disk", name)
		}
	}
	for name, want := range map[string]string{"desc.vmdk": "only monolithic", "diff.vhd": "not a valid image"} {
		img := strings.TrimSuffix(strings.ReplaceAll(name, ".", "-"), "-")
		op, err := vmCall(t, s, "POST", "global/images", obj{"name": img, "rawDisk": obj{"source": "gs://b/" + name}})
		if err != nil {
			t.Fatal(err)
		}
		op, _ = s.Call("GET", "p", "global/operations/"+lastSeg(str(op, "selfLink")), nil, nil)
		if !strings.Contains(fmt.Sprint(op["error"]), want) {
			t.Fatalf("%s: operation %v", name, op)
		}
	}
}
