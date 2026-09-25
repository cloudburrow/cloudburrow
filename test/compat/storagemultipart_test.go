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
// docs.cloud.google.com/storage/docs/multipart-uploads. fake-gcs-server
// serves none of it, so these run against the builtin server.

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
	builtinOnly(t, "XML multipart uploads are not served")
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
	builtinOnly(t, "XML multipart uploads are not served")
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
	builtinOnly(t, "XML multipart uploads are not served")
	h := New(t)
	b := xmlBucket(t, h, "mpupre")
	t.Cleanup(func() { xmlCall(t, h, "DELETE", "/"+b, "", nil) })
	resp, body := xmlCall(t, h, "POST", "/"+b+"/p.bin?uploads", "", map[string]string{"x-goog-if-generation-match": "0"})
	if resp.StatusCode != http.StatusBadRequest || xmlErrorCode(body) != "NotImplemented" {
		t.Errorf("initiate with a precondition = %d %s; want 400 NotImplemented", resp.StatusCode, body)
	}
}
