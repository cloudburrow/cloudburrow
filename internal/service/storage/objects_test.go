package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

func sdkBucket(t *testing.T, name string) (*gcs.Client, *gcs.BucketHandle, *httptest.Server) {
	t.Helper()
	c, h := sdk(t)
	bh := c.Bucket(name)
	if err := bh.Create(context.Background(), "demo-project", nil); err != nil {
		t.Fatal(err)
	}
	return c, bh, h
}

func write(t *testing.T, o *gcs.ObjectHandle, body []byte, set func(*gcs.Writer)) *gcs.ObjectAttrs {
	t.Helper()
	w := o.NewWriter(context.Background())
	if set != nil {
		set(w)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return w.Attrs()
}

func read(t *testing.T, o *gcs.ObjectHandle, off, n int64) []byte {
	t.Helper()
	r, err := o.NewRangeReader(context.Background(), off, n)
	if err != nil {
		t.Fatalf("NewRangeReader(%d, %d): %v", off, n, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read (%d, %d): %v", off, n, err)
	}
	return b
}

// Upload, metadata, download and delete through the official client, over
// its default XML reads.
func TestStorageObjectRoundTripInProcess(t *testing.T) {
	_, bh, _ := sdkBucket(t, "round-trip")
	o := bh.Object("dir/round trip.txt")
	payload := []byte("cloudburrow round trip")
	a := write(t, o, payload, nil)
	if a.Size != int64(len(payload)) || a.Generation == 0 || a.Metageneration != 1 || a.CRC32C == 0 || len(a.MD5) != 16 {
		t.Errorf("written attrs = %+v", a)
	}
	got, err := o.Attrs(context.Background())
	// The client sniffs the content type from the bytes.
	if err != nil || got.Size != int64(len(payload)) || !strings.HasPrefix(got.ContentType, "text/plain") {
		t.Errorf("Attrs = %+v, %v", got, err)
	}
	if b := read(t, o, 0, -1); !bytes.Equal(b, payload) {
		t.Errorf("download = %q", b)
	}
	second := write(t, o, []byte("second"), nil)
	if second.Generation <= a.Generation {
		t.Errorf("an overwrite kept generation %d (was %d)", second.Generation, a.Generation)
	}
	if err := o.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Attrs(context.Background()); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Errorf("Attrs after delete = %v", err)
	}
	if _, err := o.NewReader(context.Background()); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Errorf("NewReader after delete = %v", err)
	}
}

// Checksums are returned and verified on read, and custom metadata written
// with the content survives (a multipart upload).
func TestStorageChecksumsAndMultipartMetadata(t *testing.T) {
	_, bh, _ := sdkBucket(t, "sums")
	o := bh.Object("with-metadata.txt")
	body := []byte("multipart body")
	sum := md5.Sum(body)
	write(t, o, body, func(w *gcs.Writer) {
		w.ContentType = "text/plain; charset=utf-8"
		w.Metadata = map[string]string{"origin": "test", "purpose": "multipart"}
		w.CacheControl = "no-store"
		w.MD5 = sum[:] // sent in the metadata part, and checked by the server
	})
	a, err := o.Attrs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Metadata["origin"] != "test" || !strings.HasPrefix(a.ContentType, "text/plain") || a.CacheControl != "no-store" ||
		!bytes.Equal(a.MD5, sum[:]) || a.CRC32C == 0 {
		t.Errorf("attrs = %+v", a)
	}
	if b := read(t, o, 0, -1); !bytes.Equal(b, body) {
		t.Errorf("content = %q; the metadata part may have been stored as content", b)
	}
}

// Ranges are byte-exact over the default XML reads and over JSON reads.
func TestStorageRangedReads(t *testing.T) {
	c, bh, h := sdkBucket(t, "ranges")
	body := []byte("0123456789abcdefghij")
	write(t, bh.Object("r"), body, nil)
	jsonClient, err := gcs.NewClient(context.Background(), option.WithEndpoint(h.URL+"/storage/v1/"), option.WithoutAuthentication(), gcs.WithJSONReads())
	if err != nil {
		t.Fatal(err)
	}
	defer jsonClient.Close()
	for name, o := range map[string]*gcs.ObjectHandle{"xml": c.Bucket("ranges").Object("r"), "json": jsonClient.Bucket("ranges").Object("r")} {
		for _, rg := range []struct {
			off, n int64
			want   string
		}{{0, 10, "0123456789"}, {10, -1, "abcdefghij"}, {-5, -1, "fghij"}, {3, 4, "3456"}, {15, 100, "fghij"}} {
			if got := read(t, o, rg.off, rg.n); string(got) != rg.want {
				t.Errorf("%s: range (%d, %d) = %q, want %q", name, rg.off, rg.n, got, rg.want)
			}
		}
	}
}

func upload(t *testing.T, h *httptest.Server, query, contentType, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", h.URL+"/upload/storage/v1/b/raw/o?"+query, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func rawServer(t *testing.T) *httptest.Server {
	t.Helper()
	_, h := sdk(t)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"raw"}`); code != 200 {
		t.Fatal(body)
	}
	return h
}

// A range starting past the end is 416 with Content-Range bytes */size.
func TestStorageUnsatisfiableRange416(t *testing.T) {
	h := rawServer(t)
	upload(t, h, "uploadType=media&name=o", "text/plain", "0123456789", nil)
	for _, path := range []string{"/raw/o", "/storage/v1/b/raw/o/o?alt=media", "/download/storage/v1/b/raw/o/o?alt=media"} {
		req, _ := http.NewRequest("GET", h.URL+path, nil)
		req.Header.Set("Range", "bytes=50-")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 416 || resp.Header.Get("Content-Range") != "bytes */10" {
			t.Errorf("%s bytes=50- = %d, Content-Range %q", path, resp.StatusCode, resp.Header.Get("Content-Range"))
		}
	}
	req, _ := http.NewRequest("GET", h.URL+"/raw/o", nil)
	req.Header.Set("Range", "bytes=2-4")
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != "234" || resp.Header.Get("Content-Range") != "bytes 2-4/10" {
		t.Errorf("bytes=2-4 = %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Range"))
	}
	for _, k := range []string{"X-Goog-Hash", "X-Goog-Generation", "X-Goog-Metageneration", "X-Goog-Stored-Content-Length", "X-Goog-Stored-Content-Encoding", "X-Goog-Storage-Class", "Last-Modified"} {
		if resp.Header.Get(k) == "" {
			t.Errorf("an XML read has no %s", k)
		}
	}
}

// A body whose MD5 does not match the one the client sent is refused, and
// nothing is stored.
func TestStorageBadMD5Rejected(t *testing.T) {
	h := rawServer(t)
	wrong := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))
	if code, body := upload(t, h, "uploadType=media&name=bad", "text/plain", "hello", map[string]string{"Content-MD5": wrong}); code != 400 || !strings.Contains(body, "MD5") {
		t.Errorf("a wrong Content-MD5 = %d %s", code, body)
	}
	if code, body := upload(t, h, "uploadType=media&name=bad", "text/plain", "hello", map[string]string{"X-Goog-Hash": "md5=" + wrong}); code != 400 || !strings.Contains(body, "MD5") {
		t.Errorf("a wrong x-goog-hash md5 = %d %s", code, body)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/bad", ""); code != 404 {
		t.Errorf("a refused upload left an object (%d)", code)
	}
	sum := md5.Sum([]byte("hello"))
	if code, body := upload(t, h, "uploadType=media&name=good", "text/plain", "hello", map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:])}); code != 200 {
		t.Errorf("a right Content-MD5 = %d %s", code, body)
	}
}

// An upload sent to the metadata URL is 400 wrongUrlForUpload.
func TestStorageMetadataOnlyPostIsWrongUrlForUpload(t *testing.T) {
	h := rawServer(t)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/raw/o", `{"name":"x"}`); code != 400 || !strings.Contains(body, "wrongUrlForUpload") {
		t.Errorf("POST /storage/v1/b/raw/o = %d %s", code, body)
	}
}

// ifGenerationMatch=0 creates only when absent; a stale generation is 412;
// an unkept field is refused.
func TestStorageInsertPreconditions(t *testing.T) {
	h := rawServer(t)
	if code, body := upload(t, h, "uploadType=media&name=once&ifGenerationMatch=0", "text/plain", "a", nil); code != 200 {
		t.Fatalf("first create-if-absent = %d %s", code, body)
	}
	if code, _ := upload(t, h, "uploadType=media&name=once&ifGenerationMatch=0", "text/plain", "b", nil); code != 412 {
		t.Errorf("second create-if-absent = %d, want 412", code)
	}
	if code, _ := upload(t, h, "uploadType=media&name=once&ifGenerationMatch=1", "text/plain", "b", nil); code != 412 {
		t.Errorf("a stale ifGenerationMatch = %d, want 412", code)
	}
	multipart := "--B\r\nContent-Type: application/json\r\n\r\n{\"name\":\"held\",\"temporaryHold\":true}\r\n--B\r\nContent-Type: text/plain\r\n\r\nx\r\n--B--\r\n"
	if code, body := upload(t, h, "uploadType=multipart", "multipart/related; boundary=B", multipart, nil); code != 400 || !strings.Contains(body, "temporaryHold") {
		t.Errorf("an unkept object field = %d %s", code, body)
	}
	if code, body := upload(t, h, "uploadType=media&name=%0A", "text/plain", "x", nil); code != 400 {
		t.Errorf("a name with a newline = %d %s", code, body)
	}
	if code, body := upload(t, h, "name=x", "text/plain", "x", nil); code != 400 || !strings.Contains(body, "uploadType") {
		t.Errorf("no uploadType = %d %s", code, body)
	}
	if code, _ := upload(t, h, "uploadType=media&name=x", "text/plain", "x", nil); code != 200 {
		t.Errorf("a plain media upload = %d", code)
	}
	req, _ := http.NewRequest("POST", h.URL+"/upload/storage/v1/b/absent/o?uploadType=media&name=x", strings.NewReader("x"))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("an upload into a missing bucket = %d", resp.StatusCode)
	}
}
