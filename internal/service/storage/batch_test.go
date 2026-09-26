package storage

import (
	"bufio"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// batchBody builds a batch as the Python client does: multipart/mixed parts
// of application/http, each with a Content-ID.
func batchBody(parts ...string) (string, string) {
	var b strings.Builder
	for i, p := range parts {
		fmt.Fprintf(&b, "--BATCH\r\nContent-Type: application/http\r\nContent-Transfer-Encoding: binary\r\nContent-ID: <id+%d>\r\n\r\n%s\r\n", i, p)
	}
	b.WriteString("--BATCH--\r\n")
	return b.String(), "multipart/mixed; boundary=BATCH"
}

type batchPart struct {
	id     string
	status int
	body   string
}

func sendBatch(t *testing.T, url, body, ctype string) []batchPart {
	t.Helper()
	resp, err := http.Post(url, ctype, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("batch = %d %s", resp.StatusCode, b)
	}
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	var out []batchPart
	mr := multipart.NewReader(resp.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		inner, err := http.ReadResponse(bufio.NewReader(p), nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(inner.Body)
		out = append(out, batchPart{id: p.Header.Get("Content-ID"), status: inner.StatusCode, body: string(b)})
	}
}

// Each part is answered in order with Content-ID <response-id>, dispatched as
// the direct call would be: three deletes and a patch, the Python client's case.
func TestStorageBatchContentIDs(t *testing.T) {
	h := rawServer(t)
	for _, n := range []string{"a", "b", "c", "keep"} {
		upload(t, h, "uploadType=media&name="+n, "text/plain", n, nil)
	}
	body, ctype := batchBody(
		"DELETE /storage/v1/b/raw/o/a HTTP/1.1\r\n",
		"DELETE /storage/v1/b/raw/o/b HTTP/1.1\r\n",
		"DELETE /storage/v1/b/raw/o/c HTTP/1.1\r\n",
		"PATCH /storage/v1/b/raw/o/keep HTTP/1.1\r\nContent-Type: application/json\r\nContent-Length: 30\r\n\r\n{\"metadata\":{\"batched\":\"yes\"}}",
		"GET /storage/v1/b/raw/o/absent HTTP/1.1\r\n",
	)
	parts := sendBatch(t, h.URL+"/batch/storage/v1", body, ctype)
	if len(parts) != 5 {
		t.Fatalf("%d parts answered, want 5", len(parts))
	}
	for i, want := range []int{204, 204, 204, 200, 404} {
		if parts[i].id != fmt.Sprintf("<response-id+%d>", i) || parts[i].status != want {
			t.Errorf("part %d = %s %d, want <response-id+%d> %d", i, parts[i].id, parts[i].status, i, want)
		}
	}
	if !strings.Contains(parts[3].body, `"batched": "yes"`) {
		t.Errorf("the patch's response = %s", parts[3].body)
	}
	for _, n := range []string{"a", "b", "c"} {
		if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/"+n, ""); code != 404 {
			t.Errorf("%s survived its batched delete", n)
		}
	}
}

// The bytes google-cloud-storage 3.14.1 sends (captured on #516): LF line
// ends, an absolute URL, and a body with no Content-Length, which is the
// rest of the part.
func TestStorageBatchPythonPartWithoutContentLength(t *testing.T) {
	h := rawServer(t)
	upload(t, h, "uploadType=media&name=keep", "text/plain", "k", nil)
	body := "--===1==\nContent-Type: application/http\nMIME-Version: 1.0\n\n" +
		"PATCH http://127.0.0.1:4460/storage/v1/b/raw/o/keep?projection=full&prettyPrint=false HTTP/1.1\n" +
		"Content-Type: application/json\n\n{\"metadata\": {\"batched\": \"yes\"}}\n--===1==--\n"
	parts := sendBatch(t, h.URL+"/batch/storage/v1", body, `multipart/mixed; boundary="===1=="`)
	if len(parts) != 1 || parts[0].status != 200 {
		t.Fatalf("parts = %+v, want one 200", parts)
	}
	if _, got := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/keep", ""); !strings.Contains(got, `"batched":"yes"`) && !strings.Contains(got, `"batched": "yes"`) {
		t.Errorf("the batched patch was not applied: %s", got)
	}
}

// An upload or a download inside a batch is refused, per part; more than 100
// calls refuse the whole batch.
func TestStorageBatchRejectsMedia(t *testing.T) {
	h := rawServer(t)
	upload(t, h, "uploadType=media&name=o", "text/plain", "x", nil)
	body, ctype := batchBody(
		"POST /upload/storage/v1/b/raw/o?uploadType=media&name=u HTTP/1.1\r\nContent-Length: 1\r\n\r\nx",
		"GET /storage/v1/b/raw/o/o?alt=media HTTP/1.1\r\n",
		"GET /download/storage/v1/b/raw/o/o HTTP/1.1\r\n",
		"GET /storage/v1/b/raw/o/o HTTP/1.1\r\n",
	)
	parts := sendBatch(t, h.URL+"/batch/storage/v1", body, ctype)
	for i, want := range []int{400, 400, 400, 200} {
		if parts[i].status != want {
			t.Errorf("part %d = %d %s, want %d", i, parts[i].status, parts[i].body, want)
		}
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/u", ""); code != 404 {
		t.Error("an upload inside a batch was stored")
	}
	var many []string
	for i := 0; i <= maxBatchCalls; i++ {
		many = append(many, "GET /storage/v1/b/raw/o/o HTTP/1.1\r\n")
	}
	body, ctype = batchBody(many...)
	resp, err := http.Post(h.URL+"/batch/storage/v1", ctype, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("%d calls = %d, want 400", maxBatchCalls+1, resp.StatusCode)
	}
	if resp, _ := http.Post(h.URL+"/batch/storage/v1", "application/json", strings.NewReader("{}")); resp.StatusCode != 400 {
		t.Errorf("a non-multipart batch = %d", resp.StatusCode)
	}
}
