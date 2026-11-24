package compute

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// importVMs is fakeVMs that imports a disk.raw, recording what it was given.
type importVMs struct {
	fakeVMs
	keys    []string
	sizes   []int64
	release string
}

func (f *importVMs) ImportRaw(ctx context.Context, key string, sizeGb int64, open func(context.Context) (io.ReadCloser, error)) (string, error) {
	r, err := open(ctx)
	if err != nil {
		return "", err
	}
	_ = r.Close()
	f.keys, f.sizes = append(f.keys, key), append(f.sizes, sizeGb)
	return f.release, nil
}

func tarGz(t *testing.T, files map[string]int64) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for name, n := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: n, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	return b.Bytes()
}

func TestImageImportFromCloudStorage(t *testing.T) {
	objects := map[string][]byte{
		"b/ok.tar.gz":    tarGz(t, map[string]int64{"disk.raw": 3 << 20}),
		"b/two.tar.gz":   tarGz(t, map[string]int64{"disk.raw": 512, "extra": 1}),
		"b/named.tar.gz": tarGz(t, map[string]int64{"image.raw": 512}),
		"b/plain.raw":    make([]byte, 4096),
	}
	vms := &importVMs{release: "PRETTY_NAME=\"Ubuntu 24.04 LTS\"\nID=ubuntu\n"}
	s, err := New(Options{VMs: vms, ReadObject: func(_ context.Context, bucket, object string) (io.ReadCloser, int64, error) {
		b, ok := objects[bucket+"/"+object]
		if !ok {
			return nil, 0, fmt.Errorf("%s/%s: %w", bucket, object, ErrObjectNotFound)
		}
		return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	op, err := vmCall(t, s, "POST", "global/images", obj{"name": "imported", "family": "mine",
		"rawDisk": obj{"source": "https://storage.googleapis.com/b/ok.tar.gz"}})
	if err != nil {
		t.Fatal(err)
	}
	if op, _ = s.Call("GET", "p", "global/operations/"+lastSeg(str(op, "selfLink")), nil, nil); op["error"] != nil {
		t.Fatalf("operation: %v", op)
	}
	img, err := s.Call("GET", "p", "global/images/imported", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rd, _ := img["rawDisk"].(map[string]any)
	if img["status"] != "READY" || img["sourceType"] != "RAW" || str(rd, "containerType") != "TAR" ||
		str(rd, "source") != "https://storage.googleapis.com/b/ok.tar.gz" || img["diskSizeGb"] != "1" ||
		img["archiveSizeBytes"] != fmt.Sprint(len(objects["b/ok.tar.gz"])) {
		t.Fatalf("image: %v", img)
	}
	if len(vms.keys) != 1 || vms.sizes[0] != 1 {
		t.Fatalf("import: %v %v", vms.keys, vms.sizes)
	}
	// The imported system runs in its distribution's container: a disk made
	// from the image is seeded from the image's storage.
	if pub, co, err := s.imageSeedLocked("global/images/imported", "p"); err != nil || pub.Family != "ubuntu-2404-lts" || str(co, "_storageKey") != vms.keys[0] {
		t.Fatalf("seed: %v %v %v", pub.Family, co, err)
	}
	// gs:// as gcloud takes it.
	if _, err := vmCall(t, s, "POST", "global/images", obj{"name": "gs", "rawDisk": obj{"source": "gs://b/ok.tar.gz"}}); err != nil {
		t.Fatal(err)
	}
	// Google's errors: a malformed URL and two sources at once;
	// a missing object, a bad archive in the operation.
	for _, tc := range []struct {
		body   obj
		status int
		want   string
	}{
		{obj{"name": "x1", "rawDisk": obj{"source": "https://example.com/b/o.tar.gz"}}, http.StatusBadRequest, "rawDisk.source"},
		{obj{"name": "x2", "rawDisk": obj{}}, http.StatusBadRequest, "rawDisk.source"},
		{obj{"name": "x3", "rawDisk": obj{"source": "gs://b/ok.tar.gz", "containerType": "ZIP"}}, http.StatusBadRequest, "containerType"},
		{obj{"name": "x4", "rawDisk": obj{"source": "gs://b/ok.tar.gz"}, "sourceImage": "projects/debian-cloud/global/images/family/debian-12"}, http.StatusBadRequest, "Exactly one of"},
	} {
		if _, err := s.Call("POST", "p", "global/images", nil, tc.body); ErrorStatus(err) != tc.status || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v", tc.body, err)
		}
	}
	for name, want := range map[string]string{"gone.tar.gz": "was not found", "two.tar.gz": "not a valid image",
		"named.tar.gz": "not a valid image", "plain.raw": "not a valid image"} {
		img := strings.NewReplacer(".", "-", "_", "-").Replace(strings.TrimSuffix(name, ".tar.gz"))
		op, err := vmCall(t, s, "POST", "global/images", obj{"name": img, "rawDisk": obj{"source": "gs://b/" + name}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		op, _ = s.Call("GET", "p", "global/operations/"+lastSeg(str(op, "selfLink")), nil, nil)
		if !strings.Contains(fmt.Sprint(op["error"]), want) {
			t.Fatalf("%s: operation %v", name, op)
		}
		if _, err := s.Call("GET", "p", "global/images/"+img, nil, nil); ErrorStatus(err) != http.StatusNotFound {
			t.Fatalf("%s: a failed import left its image: %v", name, err)
		}
	}
	// Without Cloud Storage served, rawDisk stays 501.
	s2, _ := New(Options{VMs: vms})
	if _, err := s2.Call("POST", "p", "global/images", nil, obj{"name": "y", "rawDisk": obj{"source": "gs://b/ok.tar.gz"}}); ErrorStatus(err) != http.StatusNotImplemented {
		t.Fatalf("no storage: %v", err)
	}
}

func TestRawDiskObject(t *testing.T) {
	for src, want := range map[string]string{
		"https://storage.googleapis.com/b/dir/o.tar.gz": "b dir/o.tar.gz",
		"https://storage.cloud.google.com/b/o.tar.gz":   "b o.tar.gz",
		"gs://b/o.tar.gz":                                 "b o.tar.gz",
		"gs://b":                                          "",
		"https://storage.googleapis.com/b/":               "",
		"ftp://storage.googleapis.com/b/o":                "",
		"https://storage.googleapis.com.evil.example/b/o": "",
	} {
		b, o, ok := rawDiskObject(src)
		got := ""
		if ok {
			got = b + " " + o
		}
		if got != want {
			t.Errorf("%s: %q, want %q", src, got, want)
		}
	}
}
