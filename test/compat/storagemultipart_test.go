//go:build compat

package compat

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// XML API multipart uploads (#508), raw HTTP, to
// docs.cloud.google.com/storage/docs/multipart-uploads.

func mpuStart(t *testing.T, h *Harness, path string, hdr map[string]string) string {
	t.Helper()
	resp, body := xmlCall(t, h, "POST", path+"?uploads", "", hdr)
	var r struct {
		UploadID string `xml:"UploadId"`
	}
	if resp.StatusCode != 200 || xml.Unmarshal([]byte(body), &r) != nil || r.UploadID == "" {
		t.Fatalf("initiate = %d %s", resp.StatusCode, body)
	}
	return r.UploadID
}

// TestStorageXMLMultipartAbort204: an abort is 204, and the upload is gone.
func TestStorageXMLMultipartAbort204(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "mpuabort")
	t.Cleanup(func() { xmlCall(t, h, "DELETE", "/"+b, "", nil) })
	id := mpuStart(t, h, "/"+b+"/a.bin", nil)
	xmlCall(t, h, "PUT", fmt.Sprintf("/%s/a.bin?partNumber=1&uploadId=%s", b, id), "x", nil)
	if resp, _ := xmlCall(t, h, "DELETE", "/"+b+"/a.bin?uploadId="+id, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("abort = %d; want 204", resp.StatusCode)
	}
	if resp, _ := xmlCall(t, h, "GET", "/"+b+"/a.bin?uploadId="+id, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("list parts after abort = %d; want 404", resp.StatusCode)
	}
}

// TestStorageXMLMultipartListParts: parts page by max-parts and
// part-number-marker, and the upload is listed in the bucket's uploads.
func TestStorageXMLMultipartListParts(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "mpulist")
	id := mpuStart(t, h, "/"+b+"/l.bin", nil)
	t.Cleanup(func() {
		xmlCall(t, h, "DELETE", "/"+b+"/l.bin?uploadId="+id, "", nil)
		xmlCall(t, h, "DELETE", "/"+b, "", nil)
	})
	for n := 1; n <= 3; n++ {
		if resp, _ := xmlCall(t, h, "PUT", fmt.Sprintf("/%s/l.bin?partNumber=%d&uploadId=%s", b, n, id), fmt.Sprint(n), nil); resp.StatusCode != 200 || resp.Header.Get("ETag") == "" {
			t.Fatalf("part %d = %d", n, resp.StatusCode)
		}
	}
	_, body := xmlCall(t, h, "GET", "/"+b+"/l.bin?uploadId="+id+"&max-parts=2", "", nil)
	if strings.Count(body, "<Part>") != 2 || !strings.Contains(body, "<IsTruncated>true</IsTruncated>") || !strings.Contains(body, "<NextPartNumberMarker>2</NextPartNumberMarker>") {
		t.Errorf("first page = %s", body)
	}
	if _, body := xmlCall(t, h, "GET", "/"+b+"/l.bin?uploadId="+id+"&part-number-marker=2", "", nil); strings.Count(body, "<Part>") != 1 || !strings.Contains(body, "<PartNumber>3</PartNumber>") {
		t.Errorf("after the marker = %s", body)
	}
	if _, body := xmlCall(t, h, "GET", "/"+b+"?uploads", "", nil); !strings.Contains(body, id) {
		t.Errorf("list uploads = %s", body)
	}
}

// TestStorageXMLMultipartPreconditionRefused: "Preconditions are not
// supported" (multipart-uploads docs): 400 NotImplemented.
func TestStorageXMLMultipartPreconditionRefused(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "mpupre")
	t.Cleanup(func() { xmlCall(t, h, "DELETE", "/"+b, "", nil) })
	resp, body := xmlCall(t, h, "POST", "/"+b+"/p.bin?uploads", "", map[string]string{"x-goog-if-generation-match": "0"})
	if resp.StatusCode != http.StatusBadRequest || xmlErrorCode(body) != "NotImplemented" {
		t.Errorf("initiate with a precondition = %d %s; want 400 NotImplemented", resp.StatusCode, body)
	}
}

// TestStorageXMLMultipartCompleteRefusals: completing an upload whose
// parts are out of order is 400 InvalidPartOrder, with an ETag that does
// not match is 400 InvalidPart, and with a part under 5 MiB that is not the
// last is 400 InvalidArgument, the codes post-object-complete documents
// (docs.cloud.google.com/storage/docs/xml-api/post-object-complete, read
// 2026-10-17).
//
// covers: storage.objects.insert
func TestStorageXMLMultipartCompleteRefusals(t *testing.T) {
	h := New(t)
	b := xmlBucket(t, h, "mpucomplete")
	id := mpuStart(t, h, "/"+b+"/c.bin", nil)
	t.Cleanup(func() {
		xmlCall(t, h, "DELETE", "/"+b+"/c.bin?uploadId="+id, "", nil)
		xmlCall(t, h, "DELETE", "/"+b, "", nil)
	})
	etags := map[int]string{}
	for n := 1; n <= 2; n++ {
		resp, _ := xmlCall(t, h, "PUT", fmt.Sprintf("/%s/c.bin?partNumber=%d&uploadId=%s", b, n, id), fmt.Sprint("part", n), nil)
		if resp.StatusCode != 200 || resp.Header.Get("ETag") == "" {
			t.Fatalf("part %d = %d", n, resp.StatusCode)
		}
		etags[n] = resp.Header.Get("ETag")
	}
	complete := func(parts ...[2]string) string {
		var s strings.Builder
		s.WriteString("<CompleteMultipartUpload>")
		for _, p := range parts {
			fmt.Fprintf(&s, "<Part><PartNumber>%s</PartNumber><ETag>%s</ETag></Part>", p[0], p[1])
		}
		s.WriteString("</CompleteMultipartUpload>")
		return s.String()
	}
	for want, body := range map[string]string{
		"InvalidPartOrder": complete([2]string{"2", etags[2]}, [2]string{"1", etags[1]}),
		"InvalidPart":      complete([2]string{"1", `"deadbeef"`}, [2]string{"2", etags[2]}),
		"InvalidArgument":  complete([2]string{"1", etags[1]}, [2]string{"2", etags[2]}),
	} {
		if resp, got := xmlCall(t, h, "POST", "/"+b+"/c.bin?uploadId="+id, body, nil); resp.StatusCode != http.StatusBadRequest || xmlErrorCode(got) != want {
			t.Errorf("complete, want %s: %d %s", want, resp.StatusCode, got)
		}
	}
}
