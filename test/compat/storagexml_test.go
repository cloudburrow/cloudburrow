//go:build compat

package compat

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The XML API subset (#507), raw HTTP, to docs.cloud.google.com/storage/docs/xml-api.
// TestStorageXMLHostStyle needs the server started with
// --host storage.localhost, as CI starts it.

const xmlHostBase = "storage.localhost"

func xmlCall(t *testing.T, h *Harness, method, pathOrURL, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	u := pathOrURL
	if strings.HasPrefix(u, "/") {
		u = h.Endpoint(EnvStorage) + u
	}
	req, err := http.NewRequestWithContext(h.Context(), method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func xmlErrorCode(body string) string {
	var e struct {
		Code string `xml:"Code"`
	}
	_ = xml.Unmarshal([]byte(body), &e)
	return e.Code
}

func xmlBucket(t *testing.T, h *Harness, suffix string) string {
	t.Helper()
	name := h.Project() + "-" + suffix
	if code, body := rawStorage(t, h, "POST", "/storage/v1/b?project="+h.Project(), `{"name":"`+name+`"}`); code != http.StatusOK {
		t.Fatalf("create bucket = %d %s", code, body)
	}
	return name
}

// TestStorageXMLPutGetDelete: PUT with x-goog-meta-*, a precondition and
// Content-MD5, GET it back, DELETE it.
func TestStorageXMLPutGetDelete(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "xmlput")
	sum := md5.Sum([]byte("hello xml"))
	resp, _ := xmlCall(t, h, "PUT", "/"+b+"/x.txt", "hello xml", map[string]string{
		"Content-Type": "text/plain", "Content-MD5": base64.StdEncoding.EncodeToString(sum[:]),
		"x-goog-meta-owner": "me", "x-goog-if-generation-match": "0",
	})
	if resp.StatusCode != 200 || resp.Header.Get("ETag") != fmt.Sprintf(`"%x"`, sum) {
		t.Fatalf("PUT = %d %v", resp.StatusCode, resp.Header)
	}
	resp, body := xmlCall(t, h, "GET", "/"+b+"/x.txt", "", nil)
	if resp.StatusCode != 200 || body != "hello xml" || resp.Header.Get("X-Goog-Meta-Owner") != "me" {
		t.Errorf("GET = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ := xmlCall(t, h, "DELETE", "/"+b+"/x.txt", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE = %d", resp.StatusCode)
	}
	xmlCall(t, h, "DELETE", "/"+b, "", nil)
}

// TestStorageXMLListBucket: GET /{bucket} with a delimiter and max-keys.
func TestStorageXMLListBucket(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "xmllist")
	for _, n := range []string{"a.txt", "d/1", "d/2", "z.txt"} {
		xmlCall(t, h, "PUT", "/"+b+"/"+n, "x", nil)
	}
	t.Cleanup(func() {
		for _, n := range []string{"a.txt", "d/1", "d/2", "z.txt"} {
			xmlCall(t, h, "DELETE", "/"+b+"/"+n, "", nil)
		}
		xmlCall(t, h, "DELETE", "/"+b, "", nil)
	})
	resp, body := xmlCall(t, h, "GET", "/"+b+"?delimiter=/&max-keys=2", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "<Key>a.txt</Key>") || !strings.Contains(body, "<Prefix>d/</Prefix>") ||
		!strings.Contains(body, "<IsTruncated>true</IsTruncated>") || strings.Contains(body, "z.txt") {
		t.Errorf("first page = %d %s", resp.StatusCode, body)
	}
	if _, body := xmlCall(t, h, "GET", "/"+b+"?delimiter=/&marker=d/", "", nil); !strings.Contains(body, "<Key>z.txt</Key>") || strings.Contains(body, "a.txt") {
		t.Errorf("after the marker = %s", body)
	}
}

// TestStorageXMLResumable201And204: a session starts with 201 and
// Location, a chunk PUT completes it, and a cancel is 204.
func TestStorageXMLResumable201And204(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "xmlres")
	resp, _ := xmlCall(t, h, "POST", "/"+b+"/r.bin", "", map[string]string{"x-goog-resumable": "start"})
	uri := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusCreated || uri == "" {
		t.Fatalf("start = %d %q; want 201 with Location", resp.StatusCode, uri)
	}
	if resp, _ := xmlCall(t, h, "PUT", uri, "0123456789", map[string]string{"Content-Range": "bytes 0-9/10"}); resp.StatusCode != 200 {
		t.Errorf("the whole upload in one chunk = %d", resp.StatusCode)
	}
	resp, _ = xmlCall(t, h, "POST", "/"+b+"/c.bin", "", map[string]string{"x-goog-resumable": "start"})
	if resp, _ := xmlCall(t, h, "DELETE", resp.Header.Get("Location"), "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("cancel = %d; want 204", resp.StatusCode)
	}
	xmlCall(t, h, "DELETE", "/"+b+"/r.bin", "", nil)
	xmlCall(t, h, "DELETE", "/"+b, "", nil)
}

// TestStorageXMLErrorCodes: NoSuchBucket, NoSuchKey, PreconditionFailed,
// BucketNotEmpty and InvalidArgument, as <Error> documents.
func TestStorageXMLErrorCodes(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "xmlerr")
	xmlCall(t, h, "PUT", "/"+b+"/o", "x", nil)
	t.Cleanup(func() { xmlCall(t, h, "DELETE", "/"+b+"/o", "", nil); xmlCall(t, h, "DELETE", "/"+b, "", nil) })
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
		status       int
		code         string
	}{
		{"GET", "/" + h.Project() + "-nope/o", nil, 404, "NoSuchBucket"},
		{"GET", "/" + b + "/missing", nil, 404, "NoSuchKey"},
		{"DELETE", "/" + b + "/o", map[string]string{"x-goog-if-generation-match": "1"}, 412, "PreconditionFailed"},
		{"DELETE", "/" + b, nil, 409, "BucketNotEmpty"},
		{"GET", "/" + b + "?max-keys=-1", nil, 400, "InvalidArgument"},
	} {
		resp, body := xmlCall(t, h, c.method, c.path, "", c.hdr)
		if resp.StatusCode != c.status || xmlErrorCode(body) != c.code {
			t.Errorf("%s %s = %d %s; want %d %s", c.method, c.path, resp.StatusCode, body, c.status, c.code)
		}
	}
}

// TestStorageXMLHostStyle: {bucket}.{host} reaches the same bucket as the
// path.
func TestStorageXMLHostStyle(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "xmlhost")
	if resp, _ := xmlCall(t, h, "PUT", "/v.txt", "virtual", map[string]string{"Host": b + "." + xmlHostBase}); resp.StatusCode != 200 {
		t.Fatalf("virtual-hosted PUT = %d", resp.StatusCode)
	}
	if _, body := xmlCall(t, h, "GET", "/"+b+"/v.txt", "", nil); body != "virtual" {
		t.Errorf("path-style GET of a virtual-hosted PUT = %q", body)
	}
	xmlCall(t, h, "DELETE", "/"+b+"/v.txt", "", nil)
	xmlCall(t, h, "DELETE", "/"+b, "", nil)
}
