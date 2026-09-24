package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// objectProvider is an in-memory ObjectStore.
type objectProvider struct {
	fakeProvider
	mu      *sync.Mutex
	objects map[string]fakeObject
	// open, when set, supplies the reader for every object, so a test can
	// control when bytes become available.
	open func() io.ReadCloser
}

type fakeObject struct {
	data        []byte
	contentType string
}

func newObjectProvider() objectProvider {
	return objectProvider{fakeProvider: fakeProvider{id: "storage", title: "Cloud Storage"},
		mu: &sync.Mutex{}, objects: map[string]fakeObject{}}
}

func (p objectProvider) Upload(ctx context.Context, _ string, prefix []string, name, ct string, r io.Reader) (string, error) {
	full := strings.Join(append(append([]string{}, prefix...), name), "/")
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.objects[full] = fakeObject{data, ct}
	return full, nil
}

func (p objectProvider) OpenObject(_ context.Context, _ string, path []string) (ObjectReader, error) {
	p.mu.Lock()
	o, ok := p.objects[strings.Join(path, "/")]
	p.mu.Unlock()
	if !ok {
		return ObjectReader{}, errors.New("no such object")
	}
	rc := io.NopCloser(bytes.NewReader(o.data))
	if p.open != nil {
		rc = p.open()
	}
	return ObjectReader{ReadCloser: rc, Name: path[len(path)-1], Size: int64(len(o.data)), ContentType: o.contentType}, nil
}

func (p objectProvider) DeleteObject(_ context.Context, _ string, path []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.objects, strings.Join(path, "/"))
	return nil
}

func upload(t *testing.T, url, filename, ct string, data []byte) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename=%q`, filename)}
	h["Content-Type"] = []string{ct}
	part, _ := mw.CreatePart(h)
	_, _ = part.Write(data)
	_ = mw.Close()
	resp, err := http.Post(url, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestUploadStoresTheFileAndRecordsIt(t *testing.T) {
	p := newObjectProvider()
	srv := serve(t, p)
	resp, body := upload(t, srv.URL+"/api/objects/storage/upload?project=p&name=bkt&name=dir",
		"../../evil/notes.txt", "text/plain", []byte("hello"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload = %d: %s", resp.StatusCode, body)
	}
	// The browser's filename is reduced to its last segment: "../" in it
	// cannot climb out of the prefix.
	if o, ok := p.objects["bkt/dir/notes.txt"]; !ok || string(o.data) != "hello" || o.contentType != "text/plain" {
		t.Fatalf("stored = %v", p.objects)
	}
	_, ops := get(t, srv, "/api/operations?project=p", nil)
	if !strings.Contains(ops, `"kind":"upload"`) || !strings.Contains(ops, "bkt/dir/notes.txt") {
		t.Errorf("the upload is not in the ledger: %s", ops)
	}
}

func TestUploadOverTheLimitIsRefused(t *testing.T) {
	p := newObjectProvider()
	srv := serve(t, p)
	code, body := sendBody(t, srv, http.MethodPut, "/api/settings", `{"uploadLimitBytes":1024}`)
	if code != http.StatusOK || !strings.Contains(body, `"uploadLimitBytes":1024`) {
		t.Fatalf("settings = %d: %s", code, body)
	}
	// Past the limit but within the framing allowance, so the refusal comes
	// from counting the stream, not from Content-Length.
	resp, body := upload(t, srv.URL+"/api/objects/storage/upload?project=p&name=bkt",
		"big.bin", "application/octet-stream", bytes.Repeat([]byte("x"), 2048))
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(body, "upload limit of 1.0 KiB") {
		t.Fatalf("over-limit upload = %d: %s", resp.StatusCode, body)
	}
	if len(p.objects) != 0 {
		t.Errorf("an over-limit upload left an object: %v", p.objects)
	}
	// And refused before reading when the request declares its size.
	resp, body = upload(t, srv.URL+"/api/objects/storage/upload?project=p&name=bkt",
		"huge.bin", "application/octet-stream", bytes.Repeat([]byte("x"), 200<<10))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared over-limit upload = %d: %s", resp.StatusCode, body)
	}
	if code, _ := sendBody(t, srv, http.MethodPut, "/api/settings", `{"uploadLimitBytes":0}`); code != http.StatusBadRequest {
		t.Errorf("a zero limit was accepted: %d", code)
	}
}

func TestDefaultUploadLimit(t *testing.T) {
	srv := serve(t, newObjectProvider())
	_, body := get(t, srv, "/api/settings", nil)
	if !strings.Contains(body, fmt.Sprintf(`"uploadLimitBytes":%d`, 32<<20)) {
		t.Errorf("default settings = %s", body)
	}
}

// The download is streamed: the first bytes reach the client while the
// provider's reader has not finished, which a handler that buffered the
// object could never do.
func TestDownloadIsStreamed(t *testing.T) {
	p := newObjectProvider()
	p.objects["bkt/big.bin"] = fakeObject{data: make([]byte, 2<<20)}
	release := make(chan struct{})
	p.open = func() io.ReadCloser {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(bytes.Repeat([]byte("a"), 1<<20))
			<-release
			_, _ = pw.Write(bytes.Repeat([]byte("b"), 1<<20))
			_ = pw.Close()
		}()
		return pr
	}
	srv := serve(t, p)
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/objects/storage/download?name=bkt&name=big.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the response did not start before the reader finished: %v", err)
	}
	defer resp.Body.Close()
	first := make([]byte, 1<<20)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("the first MiB did not arrive while the rest was held back: %v", err)
	}
	if first[0] != 'a' || first[len(first)-1] != 'a' {
		t.Fatal("wrong first MiB")
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename=big.bin` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("download headers = %v", resp.Header)
	}
}

func TestPreviewNeverServesHTMLAsHTML(t *testing.T) {
	p := newObjectProvider()
	p.objects["bkt/page.html"] = fakeObject{[]byte("<script>alert(1)</script>"), "text/html"}
	p.objects["bkt/pic.svg"] = fakeObject{[]byte("<svg onload=alert(1)/>"), "image/svg+xml"}
	p.objects["bkt/pic.png"] = fakeObject{[]byte("\x89PNG"), "image/png"}
	p.objects["bkt/app.bin"] = fakeObject{[]byte{0, 1}, "application/octet-stream"}
	p.objects["bkt/big.txt"] = fakeObject{make([]byte, PreviewLimit+1), "text/plain"}
	srv := serve(t, p)
	check := func(name, wantType string, wantCode int) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/objects/storage/preview?name=bkt&name=" + name)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != wantCode {
			t.Errorf("%s: status %d, want %d: %s", name, resp.StatusCode, wantCode, body)
		}
		if wantType != "" && resp.Header.Get("Content-Type") != wantType {
			t.Errorf("%s: Content-Type %q, want %q", name, resp.Header.Get("Content-Type"), wantType)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", name)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
			t.Errorf("%s: CSP %q", name, csp)
		}
	}
	check("page.html", "text/plain; charset=utf-8", http.StatusOK)
	check("pic.svg", "text/plain; charset=utf-8", http.StatusOK)
	check("pic.png", "image/png", http.StatusOK)
	check("app.bin", "", http.StatusUnsupportedMediaType)
	check("big.txt", "", http.StatusRequestEntityTooLarge)
}

func TestDeleteObjectIsRecorded(t *testing.T) {
	p := newObjectProvider()
	p.objects["bkt/a.txt"] = fakeObject{[]byte("a"), "text/plain"}
	srv := serve(t, p)
	code, body := sendBody(t, srv, http.MethodDelete, "/api/objects/storage?project=p&name=bkt&name=a.txt", "")
	if code != http.StatusOK {
		t.Fatalf("delete = %d: %s", code, body)
	}
	if len(p.objects) != 0 {
		t.Error("the object survived")
	}
	_, ops := get(t, srv, "/api/operations?project=p", nil)
	if !strings.Contains(ops, `"kind":"delete"`) || !strings.Contains(ops, "bkt/a.txt") {
		t.Errorf("the delete is not in the ledger: %s", ops)
	}
	if code, _ := sendBody(t, srv, http.MethodDelete, "/api/objects/storage?name=bkt", ""); code != http.StatusBadRequest {
		t.Errorf("a delete naming only the bucket = %d, want 400", code)
	}
}

func TestDownloadName(t *testing.T) {
	for in, want := range map[string]string{
		"dir/report.csv": "report.csv", `we"ird:na*me.txt`: "we_ird_na_me.txt", "..": "download", "a\nb": "a_b",
	} {
		if got := downloadName(in); got != want {
			t.Errorf("downloadName(%q) = %q, want %q", in, got, want)
		}
	}
}
