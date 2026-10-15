package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
)

// formPost sends an HTML form upload; fields go in order, file last.
func formPost(t *testing.T, url string, fields [][2]string, file string) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		_ = mw.WriteField(f[0], f[1])
	}
	fw, _ := mw.CreateFormFile("file", "local.txt")
	_, _ = io.WriteString(fw, file)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// Service GET lists the x-goog-project-id project's buckets; without a
// project it is 400. Bucket PUT creates, with a location and class.
func TestStorageXMLServiceAndBucketPutInProcess(t *testing.T) {
	h := rawServer(t)
	resp, body := xmlDo(t, "PUT", h.URL+"/made-by-xml", `<CreateBucketConfiguration><LocationConstraint>EU</LocationConstraint><StorageClass>NEARLINE</StorageClass></CreateBucketConfiguration>`,
		map[string]string{"x-goog-project-id": "p"})
	if resp.StatusCode != 200 {
		t.Fatalf("PUT bucket = %d %s", resp.StatusCode, body)
	}
	code, js := raw(t, "GET", h.URL+"/storage/v1/b/made-by-xml", "")
	if code != 200 || !strings.Contains(js, `"location": "EU"`) && !strings.Contains(js, `"location":"EU"`) || !strings.Contains(js, "NEARLINE") {
		t.Errorf("the XML-made bucket = %d %s", code, js)
	}
	if resp, body := xmlDo(t, "PUT", h.URL+"/made-by-xml", "", map[string]string{"x-goog-project-id": "p"}); resp.StatusCode != 409 || xmlCode(body) != "BucketAlreadyOwnedByYou" {
		t.Errorf("PUT it again = %d %s", resp.StatusCode, body)
	}
	if resp, body := xmlDo(t, "PUT", h.URL+"/made-by-xml", "", map[string]string{"x-goog-project-id": "q"}); resp.StatusCode != 409 || xmlCode(body) != "BucketAlreadyExists" {
		t.Errorf("PUT it from another project = %d %s", resp.StatusCode, body)
	}
	if resp, body := xmlDo(t, "PUT", h.URL+"/other-bucket", "<CreateBucketConfiguration><Rpo>X</Rpo></CreateBucketConfiguration>", map[string]string{"x-goog-project-id": "p"}); resp.StatusCode != 400 {
		t.Errorf("an unknown element = %d %s", resp.StatusCode, body)
	}
	if resp, body := xmlDo(t, "PUT", h.URL+"/Bad_Name", "", map[string]string{"x-goog-project-id": "p"}); resp.StatusCode != 400 || xmlCode(body) != "InvalidArgument" {
		t.Errorf("a bad name = %d %s", resp.StatusCode, body)
	}
	if resp, body := xmlDo(t, "PUT", h.URL+"/no-project", "", nil); resp.StatusCode != 400 || xmlCode(body) != "MissingSecurityHeader" {
		t.Errorf("PUT without a project = %d %s", resp.StatusCode, body)
	}
	resp, body = xmlDo(t, "GET", h.URL+"/", "", map[string]string{"x-goog-project-id": "p"})
	var res struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Owner   struct{ ID string }
		Buckets []struct {
			Name, CreationDate string
		} `xml:"Buckets>Bucket"`
	}
	if err := xml.Unmarshal([]byte(body), &res); resp.StatusCode != 200 || err != nil || len(res.Buckets) != 2 ||
		res.Buckets[0].Name != "made-by-xml" || res.Buckets[1].Name != "raw" || res.Buckets[0].CreationDate == "" || res.Owner.ID == "" {
		t.Errorf("GET service = %d %s (%v)", resp.StatusCode, body, err)
	}
	if resp, body := xmlDo(t, "GET", h.URL+"/", "", nil); resp.StatusCode != 400 || xmlCode(body) != "MissingSecurityHeader" {
		t.Errorf("GET service without a project = %d %s", resp.StatusCode, body)
	}
	// The HMAC key in an Authorization header names the default project.
	id, _ := hmacKeyFor(t, h.URL)
	resp, body = xmlDo(t, "GET", h.URL+"/", "", map[string]string{"Authorization": "GOOG4-HMAC-SHA256 Credential=" + id + "/20260101/auto/storage/goog4_request, SignedHeaders=host, Signature=00"})
	if resp.StatusCode != 200 || !strings.Contains(body, "<Name>raw</Name>") {
		t.Errorf("GET service with an HMAC key's project = %d %s", resp.StatusCode, body)
	}
}

// A form POST with the official client's V4 policy stores the file under
// the key with the fields' metadata, honouring success_action_status; the
// policy's conditions, expiry and signature are checked.
func TestStorageXMLFormPostPolicyInProcess(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	const email = "app@p.iam.gserviceaccount.com"
	_, base := signedServer(t, Options{SigningKeys: map[string]*rsa.PublicKey{email: &key.PublicKey}})
	pk := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	policy := func(t *testing.T, opts gcs.PostPolicyV4Options) *gcs.PostPolicyV4 {
		opts.GoogleAccessID, opts.PrivateKey, opts.Insecure = email, pk, true
		opts.Hostname = strings.TrimPrefix(base, "http://")
		if opts.Expires.IsZero() {
			opts.Expires = time.Now().Add(time.Minute)
		}
		p, err := gcs.GenerateSignedPostPolicyV4("signed", "up/${filename}", &opts)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ordered := func(p *gcs.PostPolicyV4, extra ...[2]string) [][2]string {
		var f [][2]string
		for k, v := range p.Fields {
			f = append(f, [2]string{k, v})
		}
		return append(f, extra...)
	}
	p := policy(t, gcs.PostPolicyV4Options{Fields: &gcs.PolicyV4Fields{ContentType: "text/plain", StatusCodeOnSuccess: 201,
		Metadata: map[string]string{"x-goog-meta-owner": "me"}}, Conditions: []gcs.PostPolicyV4Condition{gcs.ConditionContentLengthRange(1, 10)}})
	if !strings.HasPrefix(p.URL, base+"/signed/") {
		t.Fatalf("policy URL = %s", p.URL)
	}
	resp, body := formPost(t, p.URL, ordered(p), "posted!")
	if resp.StatusCode != 201 || !strings.Contains(body, "<Key>up/local.txt</Key>") {
		t.Fatalf("form POST = %d %s", resp.StatusCode, body)
	}
	resp, body = xmlDo(t, "GET", base+"/signed/up/local.txt", "", nil)
	if resp.StatusCode != 200 || body != "posted!" || resp.Header.Get("Content-Type") != "text/plain" || resp.Header.Get("X-Goog-Meta-Owner") != "me" {
		t.Errorf("GET posted = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, body := formPost(t, p.URL, ordered(p), "far too long for the range"); resp.StatusCode != 400 || xmlCode(body) != "EntityTooLarge" {
		t.Errorf("too large = %d %s", resp.StatusCode, body)
	}
	var f [][2]string
	for _, kv := range ordered(p) {
		if kv[0] == "content-type" {
			kv[1] = "text/html"
		}
		f = append(f, kv)
	}
	if resp, body := formPost(t, p.URL, f, "x"); resp.StatusCode != 403 || xmlCode(body) != "AccessDenied" {
		t.Errorf("a failed eq condition = %d %s", resp.StatusCode, body)
	}
	if resp, body := formPost(t, p.URL, ordered(p, [2]string{"x-goog-meta-extra", "1"}), "x"); resp.StatusCode != 403 || !strings.Contains(body, "Extra input fields") {
		t.Errorf("an extra field = %d %s", resp.StatusCode, body)
	}
	if resp, body := formPost(t, p.URL, ordered(p, [2]string{"x-ignore-me", "1"}), "x"); resp.StatusCode != 201 {
		t.Errorf("an x-ignore- field = %d %s", resp.StatusCode, body)
	}
	f = nil
	for _, kv := range ordered(p) {
		if kv[0] == "x-goog-signature" {
			kv[1] = "00" + kv[1][2:]
		}
		f = append(f, kv)
	}
	if resp, body := formPost(t, p.URL, f, "x"); resp.StatusCode != 403 || xmlCode(body) != "SignatureDoesNotMatch" {
		t.Errorf("a tampered signature = %d %s", resp.StatusCode, body)
	}
	// Expiry is judged on the server's clock: sign a policy already past.
	past := policy(t, gcs.PostPolicyV4Options{Expires: time.Now().Add(time.Second)})
	time.Sleep(1100 * time.Millisecond)
	if resp, body := formPost(t, past.URL, ordered(past), "x"); resp.StatusCode != 403 || !strings.Contains(body, "expired") {
		t.Errorf("an expired policy = %d %s", resp.StatusCode, body)
	}
	// starts-with and success_action_redirect, the redirect winning.
	rp := policy(t, gcs.PostPolicyV4Options{Fields: &gcs.PolicyV4Fields{RedirectToURLOnSuccess: "https://app.test/done"},
		Conditions: []gcs.PostPolicyV4Condition{gcs.ConditionStartsWith("$key", "up/")}})
	resp, body = formPost(t, rp.URL, ordered(rp), "r")
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "https://app.test/done?") || !strings.Contains(resp.Header.Get("Location"), "key=up%2Flocal.txt") {
		t.Errorf("redirect = %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

// A V4 HMAC-signed policy, signed by hand to the docs; an unsigned form
// (no policy) uploads with 204; an unknown field is refused.
func TestStorageXMLFormPostHMACAndUnsignedInProcess(t *testing.T) {
	_, base := signedServer(t, Options{})
	id, secret := hmacKeyFor(t, base)
	now := time.Now().UTC()
	date, day := now.Format("20060102T150405Z"), now.Format("20060102")
	cred := id + "/" + day + "/auto/storage/goog4_request"
	doc, _ := json.Marshal(map[string]any{"expiration": now.Add(time.Minute).Format(time.RFC3339), "conditions": []any{
		map[string]string{"bucket": "signed"}, []any{"eq", "$key", "h.txt"}, map[string]string{"x-goog-date": date},
		map[string]string{"x-goog-credential": cred}, map[string]string{"x-goog-algorithm": "GOOG4-HMAC-SHA256"}}})
	pol := base64.StdEncoding.EncodeToString(doc)
	mac := func(k []byte, d string) []byte { m := hmac.New(sha256.New, k); m.Write([]byte(d)); return m.Sum(nil) }
	k := mac([]byte("GOOG4"+secret), day)
	for _, p := range []string{"auto", "storage", "goog4_request"} {
		k = mac(k, p)
	}
	fields := [][2]string{{"key", "h.txt"}, {"x-goog-date", date}, {"x-goog-credential", cred}, {"x-goog-algorithm", "GOOG4-HMAC-SHA256"},
		{"policy", pol}, {"x-goog-signature", hex.EncodeToString(mac(k, pol))}}
	if resp, body := formPost(t, base+"/signed", fields, "hmac"); resp.StatusCode != 204 {
		t.Fatalf("HMAC form POST = %d %s", resp.StatusCode, body)
	}
	if resp, body := xmlDo(t, "GET", base+"/signed/h.txt", "", nil); body != "hmac" {
		t.Errorf("GET = %d %s", resp.StatusCode, body)
	}
	if resp, body := formPost(t, base+"/signed", [][2]string{{"key", "plain.txt"}, {"success_action_status", "200"}}, "plain"); resp.StatusCode != 200 || resp.Header.Get("X-Goog-Generation") == "" {
		t.Errorf("unsigned form POST = %d %s", resp.StatusCode, body)
	}
	if resp, body := formPost(t, base+"/signed", [][2]string{{"key", "x"}, {"x-goog-storage-class", "COLDLINE"}}, "x"); resp.StatusCode != 400 {
		t.Errorf("an unsupported field = %d %s", resp.StatusCode, body)
	}
	if resp, body := formPost(t, base+"/no-such-bucket", [][2]string{{"key", "x"}}, "x"); resp.StatusCode != 404 || xmlCode(body) != "NoSuchBucket" {
		t.Errorf("a missing bucket = %d %s", resp.StatusCode, body)
	}
	if resp, body := formPost(t, base+"/signed", [][2]string{{"success_action_status", "201"}}, "x"); resp.StatusCode != 400 {
		t.Errorf("no key = %d %s", resp.StatusCode, body)
	}
}
