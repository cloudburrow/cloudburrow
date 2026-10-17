//go:build compat

package compat

import (
	"bytes"
	"encoding/xml"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// The XML API's bucket PUT, service GET and form POST (#1258), to
// docs.cloud.google.com/storage/docs/xml-api/put-bucket-create,
// .../xml-api/get-service and .../xml-api/post-object-form. No official
// client speaks the XML API's bucket and service calls over plain HTTP
// (gsutil needs HTTPS, see TestGsutilJSONAndHMACXML), so those are raw HTTP
// to the docs; a form POST is what a browser sends, with a policy the
// official Go client signs.

// TestStorageXMLBucketPutAndServiceGet: PUT /{bucket} with a
// CreateBucketConfiguration creates a bucket the official client reads back
// with its location and class; GET / with x-goog-project-id lists it in a
// ListAllMyBucketsResult; a second PUT is 409 BucketAlreadyOwnedByYou.
// covers: storage.buckets.insert
// covers: storage.buckets.list
func TestStorageXMLBucketPutAndServiceGet(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	name := h.Project() + "-xmlcreate"
	resp, body := xmlCall(t, h, "PUT", "/"+name,
		`<CreateBucketConfiguration><LocationConstraint>EU</LocationConstraint><StorageClass>NEARLINE</StorageClass></CreateBucketConfiguration>`,
		map[string]string{"x-goog-project-id": h.Project()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT bucket = %d %s", resp.StatusCode, body)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, c.Bucket(name)) })
	attrs, err := c.Bucket(name).Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Location != "EU" || attrs.StorageClass != "NEARLINE" {
		t.Errorf("the XML-made bucket: location %q, class %q", attrs.Location, attrs.StorageClass)
	}
	if resp, body := xmlCall(t, h, "PUT", "/"+name, "", map[string]string{"x-goog-project-id": h.Project()}); resp.StatusCode != http.StatusConflict || xmlErrorCode(body) != "BucketAlreadyOwnedByYou" {
		t.Errorf("PUT it again = %d %s; want 409 BucketAlreadyOwnedByYou", resp.StatusCode, body)
	}
	resp, body = xmlCall(t, h, "GET", "/", "", map[string]string{"x-goog-project-id": h.Project()})
	var res struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Buckets []struct {
			Name, CreationDate string
		} `xml:"Buckets>Bucket"`
	}
	if err := xml.Unmarshal([]byte(body), &res); resp.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("GET service = %d %s (%v)", resp.StatusCode, body, err)
	}
	found := false
	for _, b := range res.Buckets {
		if b.Name == name {
			found = true
			if _, err := time.Parse(time.RFC3339, b.CreationDate); err != nil {
				t.Errorf("CreationDate %q: %v", b.CreationDate, err)
			}
		}
	}
	if !found {
		t.Errorf("GET service does not list %s: %s", name, body)
	}
	if resp, body := xmlCall(t, h, "GET", "/", "", nil); resp.StatusCode != http.StatusBadRequest || xmlErrorCode(body) != "MissingSecurityHeader" {
		t.Errorf("GET service with no project = %d %s; want 400 MissingSecurityHeader", resp.StatusCode, body)
	}
}

// submitForm posts a policy's fields and a file as a browser form does,
// file last, without following a redirect.
func submitForm(t *testing.T, h *Harness, url string, fields map[string]string, file string) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	var names []string
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		_ = mw.WriteField(k, fields[k])
	}
	fw, _ := mw.CreateFormFile("file", "photo.txt")
	_, _ = io.WriteString(fw, file)
	_ = mw.Close()
	req, err := http.NewRequestWithContext(h.Context(), "POST", url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestStorageXMLFormPostPolicyV4: a POST policy from the official client's
// GenerateSignedPostPolicyV4 uploads the form's file under the key, with
// the content type and metadata, and answers success_action_status 201; a
// file outside content-length-range, a changed eq field, an extra field and
// a tampered signature are refused; success_action_redirect is 303.
// covers: storage.objects.insert
func TestStorageXMLFormPostPolicyV4(t *testing.T) {
	pk, email := signingIdentity(t)
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, c)
	host := strings.TrimPrefix(h.Endpoint(EnvStorage), "http://")
	sign := func(opts storage.PostPolicyV4Options) *storage.PostPolicyV4 {
		opts.GoogleAccessID, opts.PrivateKey, opts.Insecure, opts.Hostname = email, pk, true, host
		opts.Expires = time.Now().Add(time.Minute)
		p, err := storage.GenerateSignedPostPolicyV4(bh.BucketName(), "uploads/${filename}", &opts)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := sign(storage.PostPolicyV4Options{
		Fields:     &storage.PolicyV4Fields{ContentType: "text/plain", StatusCodeOnSuccess: 201, Metadata: map[string]string{"x-goog-meta-origin": "browser"}},
		Conditions: []storage.PostPolicyV4Condition{storage.ConditionContentLengthRange(1, 64)},
	})
	resp, body := submitForm(t, h, p.URL, p.Fields, "hello from a form")
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, "<Key>uploads/photo.txt</Key>") {
		t.Fatalf("form POST = %d %s; want 201 with the key", resp.StatusCode, body)
	}
	attrs, err := bh.Object("uploads/photo.txt").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.ContentType != "text/plain" || attrs.Metadata["origin"] != "browser" || attrs.Size != int64(len("hello from a form")) {
		t.Errorf("the posted object: %q %v %d", attrs.ContentType, attrs.Metadata, attrs.Size)
	}
	if resp, body := submitForm(t, h, p.URL, p.Fields, strings.Repeat("x", 65)); resp.StatusCode != http.StatusBadRequest || xmlErrorCode(body) != "EntityTooLarge" {
		t.Errorf("a file over content-length-range = %d %s", resp.StatusCode, body)
	}
	changed := map[string]string{}
	for k, v := range p.Fields {
		changed[k] = v
	}
	changed["content-type"] = "text/html"
	if resp, body := submitForm(t, h, p.URL, changed, "x"); resp.StatusCode != http.StatusForbidden || xmlErrorCode(body) != "AccessDenied" {
		t.Errorf("a field off its eq condition = %d %s; want 403", resp.StatusCode, body)
	}
	delete(changed, "content-type")
	changed["content-type"], changed["x-goog-meta-unsigned"] = "text/plain", "1"
	if resp, body := submitForm(t, h, p.URL, changed, "x"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Extra input fields") {
		t.Errorf("a field the policy does not name = %d %s; want 403", resp.StatusCode, body)
	}
	delete(changed, "x-goog-meta-unsigned")
	// Change the first byte whatever it is ("00" was a no-op 1 run in 256).
	if sig := p.Fields["x-goog-signature"]; strings.HasPrefix(sig, "00") {
		changed["x-goog-signature"] = "ff" + sig[2:]
	} else {
		changed["x-goog-signature"] = "00" + sig[2:]
	}
	if resp, body := submitForm(t, h, p.URL, changed, "x"); resp.StatusCode != http.StatusForbidden || xmlErrorCode(body) != "SignatureDoesNotMatch" {
		t.Errorf("a tampered signature = %d %s; want 403 SignatureDoesNotMatch", resp.StatusCode, body)
	}
	rp := sign(storage.PostPolicyV4Options{Fields: &storage.PolicyV4Fields{RedirectToURLOnSuccess: "https://app.test/uploaded"},
		Conditions: []storage.PostPolicyV4Condition{storage.ConditionStartsWith("$key", "uploads/")}})
	resp, body = submitForm(t, h, rp.URL, rp.Fields, "redirected")
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "https://app.test/uploaded?") || !strings.Contains(loc, "key=uploads%2Fphoto.txt") {
		t.Errorf("success_action_redirect = %d %q %s", resp.StatusCode, loc, body)
	}
}
