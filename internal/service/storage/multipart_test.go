package storage

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func initiateMPU(t *testing.T, base, path string, h map[string]string) string {
	t.Helper()
	resp, body := xmlDo(t, "POST", base+path+"?uploads", "", h)
	var r struct {
		UploadID string `xml:"UploadId"`
	}
	if resp.StatusCode != 200 || xml.Unmarshal([]byte(body), &r) != nil || r.UploadID == "" || !strings.Contains(body, s3NS) {
		t.Fatalf("initiate = %d %s", resp.StatusCode, body)
	}
	return r.UploadID
}

func putPart(t *testing.T, base, path, id string, n int, data string) string {
	t.Helper()
	resp, body := xmlDo(t, "PUT", fmt.Sprintf("%s%s?partNumber=%d&uploadId=%s", base, path, n, id), data, nil)
	if resp.StatusCode != 200 || resp.Header.Get("ETag") == "" || !strings.Contains(resp.Header.Get("X-Goog-Hash"), "crc32c=") {
		t.Fatalf("part %d = %d %s %v", n, resp.StatusCode, body, resp.Header)
	}
	return resp.Header.Get("ETag")
}

func completeBody(etags map[int]string, order ...int) string {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for _, n := range order {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", n, etags[n])
	}
	b.WriteString("</CompleteMultipartUpload>")
	return b.String()
}

// The completed object has no MD5 (multipart-uploads docs), a CRC32C, and
// the parts' bytes in order.
func TestStorageXMLMultipartNoMD5(t *testing.T) {
	h := rawServer(t)
	path := "/raw/big.bin"
	id := initiateMPU(t, h.URL, path, map[string]string{"Content-Type": "application/x-test", "x-goog-meta-k": "v"})
	p1 := strings.Repeat("1", minPartSize)
	p2 := strings.Repeat("2", minPartSize)
	etags := map[int]string{1: putPart(t, h.URL, path, id, 1, p1), 2: putPart(t, h.URL, path, id, 2, p2), 3: putPart(t, h.URL, path, id, 3, "tail")}
	resp, body := xmlDo(t, "POST", h.URL+path+"?uploadId="+id, completeBody(etags, 1, 2, 3), nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "<CompleteMultipartUploadResult") || !strings.Contains(body, "-3&#34;") && !strings.Contains(body, `-3"`) {
		t.Fatalf("complete = %d %s", resp.StatusCode, body)
	}
	code, meta := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/big.bin?prettyPrint=false", "")
	if code != 200 || strings.Contains(meta, "md5Hash") || !strings.Contains(meta, `"crc32c"`) || !strings.Contains(meta, `"contentType":"application/x-test"`) ||
		!strings.Contains(meta, `"k":"v"`) || !strings.Contains(meta, fmt.Sprintf(`"size":"%d"`, 2*minPartSize+4)) {
		t.Errorf("the object = %d %s; want no md5Hash, a crc32c, the metadata and every byte", code, meta)
	}
	if _, got := xmlDo(t, "GET", h.URL+path, "", map[string]string{"Range": fmt.Sprintf("bytes=%d-", minPartSize-1)}); got != "1"+p2+"tail" {
		t.Errorf("the joined bytes are out of order")
	}
	if resp, _ := xmlDo(t, "GET", h.URL+path+"?uploadId="+id, "", nil); resp.StatusCode != 404 {
		t.Errorf("the upload after completion = %d; want gone", resp.StatusCode)
	}
}

// Abort is 204, and the upload is gone.
func TestStorageXMLMultipartAbort204InProcess(t *testing.T) {
	h := rawServer(t)
	id := initiateMPU(t, h.URL, "/raw/a.bin", nil)
	putPart(t, h.URL, "/raw/a.bin", id, 1, "x")
	if resp, _ := xmlDo(t, "DELETE", h.URL+"/raw/a.bin?uploadId="+id, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("abort = %d; want 204", resp.StatusCode)
	}
	if resp, body := xmlDo(t, "PUT", h.URL+"/raw/a.bin?partNumber=2&uploadId="+id, "x", nil); resp.StatusCode != 404 || xmlCode(body) != "NoSuchUpload" {
		t.Errorf("a part after abort = %d %s", resp.StatusCode, body)
	}
}

// List parts pages by max-parts and part-number-marker; list uploads shows
// what is in flight.
func TestStorageXMLMultipartListPartsInProcess(t *testing.T) {
	h := rawServer(t)
	id := initiateMPU(t, h.URL, "/raw/l.bin", nil)
	for n := 1; n <= 5; n++ {
		putPart(t, h.URL, "/raw/l.bin", id, n, fmt.Sprint(n))
	}
	putPart(t, h.URL, "/raw/l.bin", id, 3, "three")
	type parts struct {
		Parts []struct {
			PartNumber int   `xml:"PartNumber"`
			Size       int64 `xml:"Size"`
		} `xml:"Part"`
		IsTruncated bool `xml:"IsTruncated"`
		Next        int  `xml:"NextPartNumberMarker"`
	}
	_, body := xmlDo(t, "GET", h.URL+"/raw/l.bin?uploadId="+id+"&max-parts=2", "", nil)
	var p parts
	_ = xml.Unmarshal([]byte(body), &p)
	if len(p.Parts) != 2 || p.Parts[0].PartNumber != 1 || !p.IsTruncated || p.Next != 2 {
		t.Errorf("first page = %s", body)
	}
	_, body = xmlDo(t, "GET", h.URL+"/raw/l.bin?uploadId="+id+"&part-number-marker=2", "", nil)
	p = parts{}
	_ = xml.Unmarshal([]byte(body), &p)
	if len(p.Parts) != 3 || p.Parts[0].PartNumber != 3 || p.Parts[0].Size != 5 || p.IsTruncated {
		t.Errorf("after marker 2 = %s; want parts 3-5, part 3 replaced", body)
	}
	id2 := initiateMPU(t, h.URL, "/raw/m.bin", nil)
	_, body = xmlDo(t, "GET", h.URL+"/raw?uploads", "", nil)
	if !strings.Contains(body, "<ListMultipartUploadsResult") || !strings.Contains(body, id) || !strings.Contains(body, id2) {
		t.Errorf("list uploads = %s", body)
	}
	if _, body := xmlDo(t, "GET", h.URL+"/raw", "", nil); strings.Contains(body, "l.bin") {
		t.Errorf("an incomplete upload appears in the object listing: %s", body)
	}
}

// Preconditions are refused with 400 NotImplemented, and completion checks
// order, ETags and part sizes.
//
// unverified: storage.objects.insert 400: XML multipart part order, ETag and size errors (S3's InvalidPartOrder, InvalidPart and EntityTooSmall; no Cloud Storage page states them)
func TestStorageXMLMultipartPreconditionRefusedInProcess(t *testing.T) {
	h := rawServer(t)
	if resp, body := xmlDo(t, "POST", h.URL+"/raw/p.bin?uploads", "", map[string]string{"x-goog-if-generation-match": "0"}); resp.StatusCode != 400 || xmlCode(body) != "NotImplemented" {
		t.Errorf("initiate with a precondition = %d %s", resp.StatusCode, body)
	}
	id := initiateMPU(t, h.URL, "/raw/p.bin", nil)
	etags := map[int]string{1: putPart(t, h.URL, "/raw/p.bin", id, 1, "small"), 2: putPart(t, h.URL, "/raw/p.bin", id, 2, "last")}
	for want, body := range map[string]string{
		"InvalidPartOrder": completeBody(etags, 2, 1),
		"InvalidPart":      strings.Replace(completeBody(etags, 1, 2), etags[1], `"deadbeef"`, 1),
		"EntityTooSmall":   completeBody(etags, 1, 2),
		"MalformedXML":     "<nope",
	} {
		if resp, got := xmlDo(t, "POST", h.URL+"/raw/p.bin?uploadId="+id, body, nil); resp.StatusCode != 400 || xmlCode(got) != want {
			t.Errorf("%s: complete = %d %s", want, resp.StatusCode, got)
		}
	}
	if resp, _ := xmlDo(t, "PUT", h.URL+"/raw/p.bin?partNumber=10001&uploadId="+id, "x", nil); resp.StatusCode != 400 {
		t.Errorf("part 10001 = %d", resp.StatusCode)
	}
}

// Lifecycle's AbortIncompleteMultipartUpload removes old uploads.
func TestLifecycleAbortsIncompleteMultipartUploads(t *testing.T) {
	_, clock, base := clockServer(t, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC),
		`{"name":"mpu-lc","lifecycle":{"rule":[{"action":{"type":"AbortIncompleteMultipartUpload"},"condition":{"age":1,"matchesPrefix":["tmp/"]}}]}}`)
	initiateMPU(t, base, "/mpu-lc/tmp/a", nil)
	initiateMPU(t, base, "/mpu-lc/keep/b", nil)
	if res := runLifecycle(t, base); res.Aborted != 0 {
		t.Errorf("aborted before a day: %+v", res)
	}
	clock.Advance(25 * time.Hour)
	if res := runLifecycle(t, base); res.Aborted != 1 {
		t.Errorf("after a day: %+v; want the tmp/ upload only", res)
	}
	if _, body := xmlDo(t, "GET", base+"/mpu-lc?uploads", "", nil); strings.Contains(body, "tmp/a") || !strings.Contains(body, "keep/b") {
		t.Errorf("uploads left = %s", body)
	}
}
