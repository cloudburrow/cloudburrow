//go:build compat

package compat

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"cloud.google.com/go/storage"
)

// TestConsoleStorageObjects.
//
// Console upload, download and preview (#295), each checked through the
// official SDK against the CI instance: an upload reads back through the
// SDK with the same bytes and CRC32C, a 10 MiB download is byte-identical,
// an HTML object previews as text/plain with a CSP and nosniff, and an
// upload over the limit is refused and leaves no object.
func TestConsoleStorageObjects(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	bkt := bh.BucketName()
	q := func(project bool, segs ...string) string {
		v := url.Values{}
		if project {
			v.Set("project", h.Project())
		}
		for _, s := range segs {
			v.Add("name", s)
		}
		return v.Encode()
	}

	consoleUpload := func(prefix []string, filename string, data []byte) (int, string) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", filename)
		_, _ = part.Write(data)
		_ = mw.Close()
		resp, err := http.Post("http://"+addr+"/api/objects/storage/upload?"+q(true, prefix...),
			mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Upload through the console; read back through the SDK.
	data := make([]byte, 3<<20+17)
	_, _ = rand.Read(data)
	if code, body := consoleUpload([]string{bkt, "uploads"}, "blob.bin", data); code != http.StatusOK {
		t.Fatalf("console upload = %d: %s", code, body)
	}
	obj := bh.Object("uploads/blob.bin")
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatalf("the SDK cannot see the console upload: %v", err)
	}
	if want := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)); attrs.CRC32C != want {
		t.Errorf("CRC32C = %08x, want %08x", attrs.CRC32C, want)
	}
	rd, err := obj.NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rd)
	_ = rd.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("the SDK read %d bytes that differ from the %d uploaded", len(got), len(data))
	}

	// A 10 MiB object written by the SDK downloads byte-identical.
	big := make([]byte, 10<<20)
	_, _ = rand.Read(big)
	w := bh.Object("big.bin").NewWriter(ctx)
	if _, err := w.Write(big); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/api/objects/storage/download?" + q(true, bkt, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	n, _ := io.Copy(sum, resp.Body)
	_ = resp.Body.Close()
	want := sha256.Sum256(big)
	if resp.StatusCode != http.StatusOK || n != int64(len(big)) || !bytes.Equal(sum.Sum(nil), want[:]) {
		t.Errorf("download = %d, %d bytes, sha %x; want 200, %d bytes, %x", resp.StatusCode, n, sum.Sum(nil), len(big), want)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "attachment; filename=big.bin" {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(big)) {
		t.Errorf("Content-Length = %q", cl)
	}

	// An HTML object is previewed as its text, never as a page.
	const page = "<html><script>document.title='ran'</script></html>"
	hw := bh.Object("page.html").NewWriter(ctx)
	hw.ContentType = "text/html"
	_, _ = hw.Write([]byte(page))
	if err := hw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get("http://" + addr + "/api/objects/storage/preview?" + q(true, bkt, "page.html"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || string(body) != page {
		t.Errorf("HTML preview = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("HTML preview has no nosniff")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp == "" || !bytes.Contains([]byte(csp), []byte("default-src 'none'")) {
		t.Errorf("HTML preview CSP = %q", csp)
	}

	// Over the limit: refused with the limit named, and no object is left.
	if code, body := consoleDo(t, addr, http.MethodPut, "/api/settings", `{"uploadLimitBytes":1048576}`); code != http.StatusOK {
		t.Fatalf("set the upload limit = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, addr, http.MethodPut, "/api/settings", fmt.Sprintf(`{"uploadLimitBytes":%d}`, 32<<20))
	})
	code, body2 := consoleUpload([]string{bkt}, "too-big.bin", make([]byte, 2<<20))
	if code != http.StatusRequestEntityTooLarge || !bytes.Contains([]byte(body2), []byte("upload limit of 1 MiB")) {
		t.Errorf("over-limit upload = %d: %s", code, body2)
	}
	if _, err := bh.Object("too-big.bin").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("an over-limit upload left an object (%v)", err)
	}

	// Delete through the console removes the real object.
	if code, body := consoleDo(t, addr, http.MethodDelete, "/api/objects/storage?"+q(true, bkt, "uploads/blob.bin"), ""); code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, body)
	}
	if _, err := obj.Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the object survived a console delete (%v)", err)
	}
}
