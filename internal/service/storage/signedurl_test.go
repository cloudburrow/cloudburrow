package storage

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// signHMACV4 signs a GET URL with GOOG4-HMAC-SHA256, written here from the
// signatures docs rather than shared with the server, so the test does not
// check the server against itself.
func signHMACV4(t *testing.T, base, path, accessID, secret string, at time.Time, expires int) string {
	t.Helper()
	u, _ := url.Parse(base)
	date := at.UTC().Format("20060102T150405Z")
	scope := at.UTC().Format("20060102") + "/auto/storage/goog4_request"
	q := url.Values{
		"X-Goog-Algorithm": {"GOOG4-HMAC-SHA256"}, "X-Goog-Credential": {accessID + "/" + scope},
		"X-Goog-Date": {date}, "X-Goog-Expires": {fmt.Sprint(expires)}, "X-Goog-SignedHeaders": {"host"},
	}
	var segs []string
	for _, s := range strings.Split(path, "/") {
		segs = append(segs, strings.ReplaceAll(url.QueryEscape(s), "+", "%20"))
	}
	canonical := strings.Join([]string{"GET", strings.Join(segs, "/"), strings.ReplaceAll(q.Encode(), "+", "%20"),
		"host:" + u.Host, "", "host", "UNSIGNED-PAYLOAD"}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	sts := "GOOG4-HMAC-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	mac := func(k []byte, d string) []byte { m := hmac.New(sha256.New, k); m.Write([]byte(d)); return m.Sum(nil) }
	k := mac([]byte("GOOG4"+secret), at.UTC().Format("20060102"))
	for _, p := range []string{"auto", "storage", "goog4_request"} {
		k = mac(k, p)
	}
	q.Set("X-Goog-Signature", hex.EncodeToString(mac(k, sts)))
	return base + strings.Join(segs, "/") + "?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

func hmacKeyFor(t *testing.T, base string) (id, secret string) {
	t.Helper()
	code, body := raw(t, "POST", base+"/storage/v1/projects/p/hmacKeys?serviceAccountEmail=signer@p.iam.gserviceaccount.com", "")
	var k struct {
		Metadata struct {
			AccessID string `json:"accessId"`
		} `json:"metadata"`
		Secret string `json:"secret"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &k) != nil {
		t.Fatalf("create HMAC key = %d %s", code, body)
	}
	return k.Metadata.AccessID, k.Secret
}

func signedServer(t *testing.T, o Options) (*Server, string) {
	t.Helper()
	s, err := NewServer(o)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"signed"}`)
	xmlDo(t, "PUT", h.URL+"/signed/dir/secret.txt", "classified", nil)
	return s, h.URL
}

// A V4 HMAC URL signed with a #505 key reads the object; a tampered one
// is 403 SignatureDoesNotMatch.
func TestSignedURLV4HMACInProcess(t *testing.T) {
	_, base := signedServer(t, Options{})
	id, secret := hmacKeyFor(t, base)
	u := signHMACV4(t, base, "/signed/dir/secret.txt", id, secret, time.Now(), 300)
	if resp, body := xmlDo(t, "GET", u, "", nil); resp.StatusCode != 200 || body != "classified" {
		t.Fatalf("a signed GET = %d %s", resp.StatusCode, body)
	}
	tampered := strings.Replace(u, "X-Goog-Signature=", "X-Goog-Signature=00", 1)
	if resp, body := xmlDo(t, "GET", tampered, "", nil); resp.StatusCode != 403 || xmlCode(body) != "SignatureDoesNotMatch" {
		t.Errorf("a tampered signature = %d %s", resp.StatusCode, body)
	}
	other := strings.Replace(u, "secret.txt", "other.txt", 1)
	if resp, _ := xmlDo(t, "GET", other, "", nil); resp.StatusCode != 403 {
		t.Errorf("the signature reused for another object = %d", resp.StatusCode)
	}
	if resp, _ := xmlDo(t, "GET", base+"/signed/dir/secret.txt", "", nil); resp.StatusCode != 200 {
		t.Errorf("an unsigned request = %d; nothing else is authenticated", resp.StatusCode)
	}
}

// A signed URL is 403 AccessDenied once expired, on the server's clock,
// and an expiry over 604800 seconds is refused.
func TestSignedURLExpired403(t *testing.T) {
	clock := sched.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	_, base := signedServer(t, Options{Now: clock.Now})
	id, secret := hmacKeyFor(t, base)
	u := signHMACV4(t, base, "/signed/dir/secret.txt", id, secret, clock.Now(), 60)
	clock.Advance(59 * time.Second)
	if resp, _ := xmlDo(t, "GET", u, "", nil); resp.StatusCode != 200 {
		t.Errorf("a second before expiry = %d", resp.StatusCode)
	}
	clock.Advance(time.Second)
	if resp, body := xmlDo(t, "GET", u, "", nil); resp.StatusCode != 403 || xmlCode(body) != "AccessDenied" {
		t.Errorf("at expiry = %d %s", resp.StatusCode, body)
	}
	long := signHMACV4(t, base, "/signed/dir/secret.txt", id, secret, clock.Now(), 604801)
	if resp, _ := xmlDo(t, "GET", long, "", nil); resp.StatusCode != 400 {
		t.Errorf("X-Goog-Expires 604801 = %d", resp.StatusCode)
	}
}

// An unknown or inactive key, or an RSA signer with no registered
// certificate, is 403: never 200.
func TestSignedURLUnknownKey403(t *testing.T) {
	_, base := signedServer(t, Options{})
	if resp, body := xmlDo(t, "GET", signHMACV4(t, base, "/signed/dir/secret.txt", "GOOGNOSUCHKEY", "x", time.Now(), 60), "", nil); resp.StatusCode != 403 || xmlCode(body) != "AccessDenied" {
		t.Errorf("an unknown HMAC key = %d %s", resp.StatusCode, body)
	}
	id, secret := hmacKeyFor(t, base)
	raw(t, "PUT", base+"/storage/v1/projects/p/hmacKeys/"+id, `{"state":"INACTIVE"}`)
	if resp, _ := xmlDo(t, "GET", signHMACV4(t, base, "/signed/dir/secret.txt", id, secret, time.Now(), 60), "", nil); resp.StatusCode != 403 {
		t.Errorf("an INACTIVE key = %d", resp.StatusCode)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pk := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	u, err := gcs.SignedURL("signed", "dir/secret.txt", &gcs.SignedURLOptions{GoogleAccessID: "stranger@p.iam.gserviceaccount.com",
		PrivateKey: pk, Method: "GET", Expires: time.Now().Add(time.Minute), Scheme: gcs.SigningSchemeV4, Hostname: strings.TrimPrefix(base, "http://"), Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := xmlDo(t, "GET", u, "", nil); resp.StatusCode != 403 || xmlCode(body) != "AccessDenied" {
		t.Errorf("an RSA signer with no registered certificate = %d %s", resp.StatusCode, body)
	}
}

// RSA signed URLs from the official Go client, V4 and V2, verify against a
// registered key; a URL signed by a different key does not.
func TestSignedURLRSARegisteredKeyInProcess(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	const email = "app@p.iam.gserviceaccount.com"
	_, base := signedServer(t, Options{SigningKeys: map[string]*rsa.PublicKey{email: &key.PublicKey}})
	pk := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	host := strings.TrimPrefix(base, "http://")
	for _, scheme := range []gcs.SigningScheme{gcs.SigningSchemeV4, gcs.SigningSchemeV2} {
		u, err := gcs.SignedURL("signed", "dir/secret.txt", &gcs.SignedURLOptions{GoogleAccessID: email, PrivateKey: pk,
			Method: "GET", Expires: time.Now().Add(time.Minute), Scheme: scheme, Hostname: host, Insecure: true})
		if err != nil {
			t.Fatal(err)
		}
		if scheme == gcs.SigningSchemeV2 {
			u = strings.Replace(u, "https://", "http://", 1)
		}
		if resp, body := xmlDo(t, "GET", u, "", nil); resp.StatusCode != 200 || body != "classified" {
			t.Errorf("scheme %d: a signed GET = %d %s", scheme, resp.StatusCode, body)
		}
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	opk := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(other)})
	u, _ := gcs.SignedURL("signed", "dir/secret.txt", &gcs.SignedURLOptions{GoogleAccessID: email, PrivateKey: opk,
		Method: "GET", Expires: time.Now().Add(time.Minute), Scheme: gcs.SigningSchemeV4, Hostname: host, Insecure: true})
	if resp, body := xmlDo(t, "GET", u, "", nil); resp.StatusCode != 403 || xmlCode(body) != "SignatureDoesNotMatch" {
		t.Errorf("signed by another key = %d %s", resp.StatusCode, body)
	}
}
