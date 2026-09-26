//go:build oracle

package storageoracle

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

const chunk = 256 << 10 // the resumable chunk granularity

// TestOracleResumableHandshakes compares the resumable upload protocol: the
// session start, a chunk, the status query (308, and 200 with
// X-Http-Status-Code-Override when X-GUploader-No-308 is sent), a chunk
// under X-GUploader-No-308, the final chunk, a status query after completion, and a one-shot session.
func TestOracleResumableHandshakes(t *testing.T) {
	o := newOracle(t)
	b := "oracle-resumable"
	createBucket(o, b)
	data := payload(2*chunk + 1000)
	start := req{method: "POST", url: "/upload/storage/v1/b/" + b + "/o?uploadType=resumable&name=res.bin",
		header: map[string]string{"X-Upload-Content-Type": "application/octet-stream"},
		body:   jsonBody(map[string]any{"contentType": "application/octet-stream", "metadata": map[string]string{"k": "v"}})}
	tbStart, cbStart, _, _ := o.step("resumable start", start)
	tbURI, cbURI := tbStart.Header.Get("Location"), cbStart.Header.Get("Location")
	if tbURI == "" || cbURI == "" {
		o.check()
		t.Fatalf("no session URI: testbench %q, builtin %q", tbURI, cbURI)
	}
	put := func(uri, cr string, body []byte, extra map[string]string) req {
		h := map[string]string{"Content-Range": cr, "Content-Type": "application/octet-stream"}
		for k, v := range extra {
			h[k] = v
		}
		return req{method: "PUT", url: uri, header: h, body: body}
	}
	o.stepEach("resumable query before any bytes",
		put(tbURI, "bytes */*", []byte{}, nil), put(cbURI, "bytes */*", []byte{}, nil))
	first := fmt.Sprintf("bytes 0-%d/*", chunk-1)
	o.stepEach("resumable first chunk",
		put(tbURI, first, data[:chunk], nil), put(cbURI, first, data[:chunk], nil))
	o.stepEach("resumable query",
		put(tbURI, "bytes */*", []byte{}, nil), put(cbURI, "bytes */*", []byte{}, nil))
	no308 := map[string]string{"X-GUploader-No-308": "yes"}
	o.stepEach("resumable query with X-GUploader-No-308",
		put(tbURI, "bytes */*", []byte{}, no308), put(cbURI, "bytes */*", []byte{}, no308))
	second := fmt.Sprintf("bytes %d-%d/*", chunk, 2*chunk-1)
	o.stepEach("resumable chunk with X-GUploader-No-308",
		put(tbURI, second, data[chunk:2*chunk], no308), put(cbURI, second, data[chunk:2*chunk], no308))
	last := fmt.Sprintf("bytes %d-%d/%d", 2*chunk, len(data)-1, len(data))
	o.stepEach("resumable final chunk",
		put(tbURI, last, data[2*chunk:], nil), put(cbURI, last, data[2*chunk:], nil))
	done := fmt.Sprintf("bytes */%d", len(data))
	o.stepEach("resumable query after completion",
		put(tbURI, done, []byte{}, nil), put(cbURI, done, []byte{}, nil))

	one := req{method: "POST", url: "/upload/storage/v1/b/" + b + "/o?uploadType=resumable&name=one.bin",
		body: jsonBody(map[string]any{})}
	tbOne, cbOne, _, _ := o.step("resumable one-shot start", one)
	small := payload(10)
	o.stepEach("resumable one-shot upload",
		put(tbOne.Header.Get("Location"), "bytes 0-9/10", small, nil),
		put(cbOne.Header.Get("Location"), "bytes 0-9/10", small, nil))
	o.check()
}

// TestOracleSuccessShapes compares the success responses of object insert
// (multipart and media), get, patch and compose, and ranged downloads.
func TestOracleSuccessShapes(t *testing.T) {
	o := newOracle(t)
	b := "oracle-shapes"
	createBucket(o, b)

	boundary := "oracle_boundary"
	multipart := []byte("--" + boundary + "\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n" +
		`{"name":"multi.txt","contentType":"text/plain","metadata":{"k":"v"},"cacheControl":"no-cache"}` +
		"\r\n--" + boundary + "\r\nContent-Type: text/plain\r\n\r\nhello multipart\r\n--" + boundary + "--\r\n")
	o.step("insert multipart", req{method: "POST", url: "/upload/storage/v1/b/" + b + "/o?uploadType=multipart",
		header: map[string]string{"Content-Type": "multipart/related; boundary=" + boundary}, body: multipart})
	media := payload(1000)
	o.step("insert media", req{method: "POST", url: "/upload/storage/v1/b/" + b + "/o?uploadType=media&name=simple.bin",
		header: map[string]string{"Content-Type": "application/octet-stream"}, body: media})

	o.step("get", req{method: "GET", url: "/storage/v1/b/" + b + "/o/simple.bin"})
	o.step("get missing", req{method: "GET", url: "/storage/v1/b/" + b + "/o/missing.bin"})
	o.step("patch", req{method: "PATCH", url: "/storage/v1/b/" + b + "/o/multi.txt",
		body: jsonBody(map[string]any{"contentType": "text/csv", "metadata": map[string]any{"k": nil, "k2": "v2"}})})

	o.setup(req{method: "POST", url: "/upload/storage/v1/b/" + b + "/o?uploadType=media&name=part2.bin",
		header: map[string]string{"Content-Type": "application/octet-stream"}, body: payload(500)})
	o.step("compose", req{method: "POST", url: "/storage/v1/b/" + b + "/o/composed.bin/compose",
		body: jsonBody(map[string]any{
			"sourceObjects": []map[string]string{{"name": "simple.bin"}, {"name": "part2.bin"}},
			"destination":   map[string]string{"contentType": "application/octet-stream"},
		})})
	o.step("get composed", req{method: "GET", url: "/storage/v1/b/" + b + "/o/composed.bin"})

	dl := "/download/storage/v1/b/" + b + "/o/simple.bin?alt=media"
	for _, c := range []struct{ name, rng string }{
		{"download whole", ""},
		{"download range", "bytes=10-19"},
		{"download open range", "bytes=995-"},
		{"download suffix range", "bytes=-5"},
		{"download unsatisfiable range", "bytes=2000-"},
	} {
		h := map[string]string{}
		if c.rng != "" {
			h["Range"] = c.rng
		}
		o.step(c.name, req{method: "GET", url: dl, header: h})
	}
	o.step("xml download range", req{method: "GET", url: "/" + b + "/simple.bin",
		header: map[string]string{"Range": "bytes=0-3"}})
	o.check()
}

// TestOracleRewriteTokens compares a rewrite split by
// maxBytesRewrittenPerCall: every call's progress fields and the final
// resource.
func TestOracleRewriteTokens(t *testing.T) {
	o := newOracle(t)
	b := "oracle-rewrite"
	createBucket(o, b)
	o.setup(req{method: "POST", url: "/upload/storage/v1/b/" + b + "/o?uploadType=media&name=big.bin",
		header: map[string]string{"Content-Type": "application/octet-stream"}, body: payload(3<<20 + 7)})
	u := "/storage/v1/b/" + b + "/o/big.bin/rewriteTo/b/" + b + "/o/copy.bin?maxBytesRewrittenPerCall=1048576"
	tbResp, cbResp, tbBody, cbBody := o.step("rewrite call 1", req{method: "POST", url: u, body: jsonBody(map[string]any{})})
	for i := 2; i <= 6; i++ {
		if tbResp.StatusCode != http.StatusOK || cbResp.StatusCode != http.StatusOK {
			break
		}
		tbTok, cbTok := field(t, tbBody, "rewriteToken"), field(t, cbBody, "rewriteToken")
		if tbTok == "" && cbTok == "" {
			break
		}
		tbResp, cbResp, tbBody, cbBody = o.stepEach(fmt.Sprintf("rewrite call %d", i),
			req{method: "POST", url: u + "&rewriteToken=" + url.QueryEscape(tbTok), body: jsonBody(map[string]any{})},
			req{method: "POST", url: u + "&rewriteToken=" + url.QueryEscape(cbTok), body: jsonBody(map[string]any{})})
	}
	o.step("get rewritten", req{method: "GET", url: "/storage/v1/b/" + b + "/o/copy.bin"})
	o.check()
}
