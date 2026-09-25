package storage

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func xmlDo(t *testing.T, method, url, body string, h map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range h {
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

func xmlCode(body string) string {
	var e struct {
		Code string `xml:"Code"`
	}
	_ = xml.Unmarshal([]byte(body), &e)
	return e.Code
}

// PUT with metadata, preconditions and Content-MD5; GET it back; DELETE.
func TestStorageXMLPutGetDeleteInProcess(t *testing.T) {
	h := rawServer(t)
	sum := md5.Sum([]byte("hello xml"))
	resp, body := xmlDo(t, "PUT", h.URL+"/raw/dir/x.txt", "hello xml", map[string]string{
		"Content-Type": "text/plain", "Content-MD5": base64.StdEncoding.EncodeToString(sum[:]),
		"x-goog-meta-owner": "me", "x-goog-if-generation-match": "0", "Cache-Control": "no-store",
	})
	if resp.StatusCode != 200 || resp.Header.Get("X-Goog-Generation") == "" || resp.Header.Get("ETag") != fmt.Sprintf(`"%x"`, sum) || body != "" {
		t.Fatalf("PUT = %d %v %q", resp.StatusCode, resp.Header, body)
	}
	resp, body = xmlDo(t, "GET", h.URL+"/raw/dir/x.txt", "", nil)
	if resp.StatusCode != 200 || body != "hello xml" || resp.Header.Get("X-Goog-Meta-Owner") != "me" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("GET = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, body := xmlDo(t, "PUT", h.URL+"/raw/dir/x.txt", "again", map[string]string{"x-goog-if-generation-match": "0"}); resp.StatusCode != 412 || xmlCode(body) != "PreconditionFailed" {
		t.Errorf("PUT if absent over an object = %d %s", resp.StatusCode, body)
	}
	if resp, body := xmlDo(t, "PUT", h.URL+"/raw/bad", "x", map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(make([]byte, 16))}); resp.StatusCode != 400 || xmlCode(body) != "InvalidArgument" {
		t.Errorf("a bad Content-MD5 = %d %s", resp.StatusCode, body)
	}
	if resp, _ := xmlDo(t, "DELETE", h.URL+"/raw/dir/x.txt", "", nil); resp.StatusCode != 204 {
		t.Errorf("DELETE = %d", resp.StatusCode)
	}
	if resp, body := xmlDo(t, "GET", h.URL+"/raw/dir/x.txt", "", nil); resp.StatusCode != 404 || xmlCode(body) != "NoSuchKey" {
		t.Errorf("GET after DELETE = %d %s", resp.StatusCode, body)
	}
}

// GET /{bucket} lists with prefix, delimiter, marker and max-keys.
func TestStorageXMLListBucketInProcess(t *testing.T) {
	h := rawServer(t)
	for _, n := range []string{"a.txt", "d/1", "d/2", "e/1", "z.txt"} {
		xmlDo(t, "PUT", h.URL+"/raw/"+n, "x", nil)
	}
	type listing struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
		Prefixes []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
		IsTruncated bool   `xml:"IsTruncated"`
		NextMarker  string `xml:"NextMarker"`
	}
	list := func(q string) (listing, string) {
		resp, body := xmlDo(t, "GET", h.URL+"/raw?"+q, "", nil)
		if resp.StatusCode != 200 || !strings.Contains(body, "<ListBucketResult") {
			t.Fatalf("list %s = %d %s", q, resp.StatusCode, body)
		}
		var l listing
		_ = xml.Unmarshal([]byte(body), &l)
		var keys []string
		for _, c := range l.Contents {
			keys = append(keys, c.Key)
		}
		for _, p := range l.Prefixes {
			keys = append(keys, p.Prefix)
		}
		return l, strings.Join(keys, ",")
	}
	if _, got := list(""); got != "a.txt,d/1,d/2,e/1,z.txt" {
		t.Errorf("all = %s", got)
	}
	if _, got := list("delimiter=/"); got != "a.txt,z.txt,d/,e/" {
		t.Errorf("with delimiter = %s", got)
	}
	if _, got := list("prefix=d/"); got != "d/1,d/2" {
		t.Errorf("prefix = %s", got)
	}
	l, got := list("delimiter=/&max-keys=2")
	if got != "a.txt,d/" || !l.IsTruncated || l.NextMarker != "d/" {
		t.Errorf("first page = %s truncated=%v next=%q", got, l.IsTruncated, l.NextMarker)
	}
	if _, got := list("delimiter=/&marker=" + l.NextMarker); got != "z.txt,e/" {
		t.Errorf("after the marker = %s", got)
	}
}

// XML resumable: start is 201 with Location, chunks are PUT, a status query
// is 308, and a cancel is 204.
func TestStorageXMLResumable201And204InProcess(t *testing.T) {
	h := rawServer(t)
	resp, _ := xmlDo(t, "POST", h.URL+"/raw/big.bin", "", map[string]string{"x-goog-resumable": "start", "Content-Type": "application/octet-stream", "x-goog-meta-k": "v"})
	uri := resp.Header.Get("Location")
	if resp.StatusCode != 201 || !strings.Contains(uri, "/raw/big.bin?upload_id=") {
		t.Fatalf("start = %d %q", resp.StatusCode, uri)
	}
	data := strings.Repeat("a", 256<<10) + "tail"
	if resp, _ := xmlDo(t, "PUT", uri, data[:256<<10], map[string]string{"Content-Range": "bytes 0-262143/*"}); resp.StatusCode != 308 || resp.Header.Get("Range") != "bytes=0-262143" {
		t.Errorf("first chunk = %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := xmlDo(t, "PUT", uri, "", map[string]string{"Content-Range": "bytes */*"}); resp.StatusCode != 308 {
		t.Errorf("status query = %d", resp.StatusCode)
	}
	resp, body := xmlDo(t, "PUT", uri, data[256<<10:], map[string]string{"Content-Range": fmt.Sprintf("bytes 262144-%d/%d", len(data)-1, len(data))})
	if resp.StatusCode != 200 || resp.Header.Get("X-Goog-Generation") == "" || body != "" {
		t.Fatalf("final chunk = %d %v %q", resp.StatusCode, resp.Header, body)
	}
	if resp, body := xmlDo(t, "GET", h.URL+"/raw/big.bin", "", nil); len(body) != len(data) || resp.Header.Get("X-Goog-Meta-K") != "v" {
		t.Errorf("the uploaded object = %d bytes, %v", len(body), resp.Header)
	}
	resp, _ = xmlDo(t, "POST", h.URL+"/raw/cancelled", "", map[string]string{"x-goog-resumable": "start"})
	if resp, _ := xmlDo(t, "DELETE", resp.Header.Get("Location"), "", nil); resp.StatusCode != 204 {
		t.Errorf("cancel = %d; want 204", resp.StatusCode)
	}
}

// Errors are XML documents with the XML API's codes.
func TestStorageXMLErrorCodesInProcess(t *testing.T) {
	h := rawServer(t)
	xmlDo(t, "PUT", h.URL+"/raw/o", "x", nil)
	for name, c := range map[string]struct {
		method, path string
		h            map[string]string
		status       int
		code         string
	}{
		"NoSuchBucket":       {"GET", "/nope/o", nil, 404, "NoSuchBucket"},
		"NoSuchBucket list":  {"GET", "/nope", nil, 404, "NoSuchBucket"},
		"NoSuchKey":          {"GET", "/raw/missing", nil, 404, "NoSuchKey"},
		"NoSuchKey delete":   {"DELETE", "/raw/missing", nil, 404, "NoSuchKey"},
		"PreconditionFailed": {"DELETE", "/raw/o", map[string]string{"x-goog-if-generation-match": "1"}, 412, "PreconditionFailed"},
		"BucketNotEmpty":     {"DELETE", "/raw", nil, 409, "BucketNotEmpty"},
		"InvalidArgument":    {"GET", "/raw?max-keys=-1", nil, 400, "InvalidArgument"},
	} {
		resp, body := xmlDo(t, c.method, h.URL+c.path, "", c.h)
		if resp.StatusCode != c.status || xmlCode(body) != c.code || !strings.Contains(resp.Header.Get("Content-Type"), "xml") {
			t.Errorf("%s = %d %s; want %d %s", name, resp.StatusCode, body, c.status, c.code)
		}
	}
	xmlDo(t, "DELETE", h.URL+"/raw/o", "", nil)
	if resp, _ := xmlDo(t, "DELETE", h.URL+"/raw", "", nil); resp.StatusCode != 204 {
		t.Errorf("DELETE an empty bucket = %d", resp.StatusCode)
	}
}

// {bucket}.{host} addressing reaches the same bucket as the path.
func TestStorageXMLHostStyleInProcess(t *testing.T) {
	s, err := NewServer(Options{Hosts: []string{"storage.local"}})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"vhost"}`)
	if resp, _ := xmlDo(t, "PUT", h.URL+"/dir/v.txt", "virtual", map[string]string{"Host": "vhost.storage.local"}); resp.StatusCode != 200 {
		t.Fatalf("virtual-hosted PUT = %d", resp.StatusCode)
	}
	if _, body := xmlDo(t, "GET", h.URL+"/vhost/dir/v.txt", "", nil); body != "virtual" {
		t.Errorf("path-style GET of the virtual-hosted PUT = %q", body)
	}
	if _, body := xmlDo(t, "GET", h.URL+"/", "", map[string]string{"Host": "vhost.storage.local"}); !strings.Contains(body, "<Key>dir/v.txt</Key>") {
		t.Errorf("virtual-hosted listing = %s", body)
	}
}
