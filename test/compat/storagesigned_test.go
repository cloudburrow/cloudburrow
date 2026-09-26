//go:build compat

package compat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// Signed URLs, verified and failing closed (#509), to
// docs.cloud.google.com/storage/docs/access-control/signed-urls.

// EnvSigningKey and EnvSigningEmail name a private key and its service
// account whose certificate the server was started with (--signing-cert);
// CI generates both.
const (
	EnvSigningKey   = "CLOUDBURROW_TEST_SIGNING_KEY"
	EnvSigningEmail = "CLOUDBURROW_TEST_SIGNING_EMAIL"
)

// signV4HMAC signs a GET with GOOG4-HMAC-SHA256 to the signatures docs.
// The official Go client signs V4 only with RSA (GOOG4-RSA-SHA256 is fixed
// in its signedURLV4), so an HMAC key's URL is signed here by hand.
func signV4HMAC(endpoint, path, accessID, secret string, at time.Time, expires int) string {
	u, _ := url.Parse(endpoint)
	date := at.UTC().Format("20060102T150405Z")
	scope := at.UTC().Format("20060102") + "/auto/storage/goog4_request"
	q := url.Values{"X-Goog-Algorithm": {"GOOG4-HMAC-SHA256"}, "X-Goog-Credential": {accessID + "/" + scope},
		"X-Goog-Date": {date}, "X-Goog-Expires": {fmt.Sprint(expires)}, "X-Goog-SignedHeaders": {"host"}}
	var segs []string
	for _, s := range strings.Split(path, "/") {
		segs = append(segs, strings.ReplaceAll(url.QueryEscape(s), "+", "%20"))
	}
	enc := strings.Join(segs, "/")
	query := strings.ReplaceAll(q.Encode(), "+", "%20")
	sum := sha256.Sum256([]byte(strings.Join([]string{"GET", enc, query, "host:" + u.Host, "", "host", "UNSIGNED-PAYLOAD"}, "\n")))
	sts := "GOOG4-HMAC-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	mac := func(k []byte, d string) []byte { m := hmac.New(sha256.New, k); m.Write([]byte(d)); return m.Sum(nil) }
	k := mac([]byte("GOOG4"+secret), at.UTC().Format("20060102"))
	for _, p := range []string{"auto", "storage", "goog4_request"} {
		k = mac(k, p)
	}
	return endpoint + enc + "?" + query + "&X-Goog-Signature=" + hex.EncodeToString(mac(k, sts))
}

// TestStorageSignedURLV4HMAC: a V4 URL signed with an HMAC key the official
// client created reads the object; a tampered signature is 403.
// covers: storage.projects.hmacKeys.create
func TestStorageSignedURLV4HMAC(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, c)
	putObject(t, ctx, bh.Object("secret.txt"), "classified")
	sa := "signer@" + h.Project() + ".iam.gserviceaccount.com"
	t.Cleanup(func() { deleteHMACKeys(ctx, c, h.Project(), sa) })
	k, err := c.CreateHMACKey(ctx, h.Project(), sa)
	if err != nil {
		t.Fatal(err)
	}
	u := signV4HMAC(h.Endpoint(EnvStorage), "/"+bh.BucketName()+"/secret.txt", k.AccessID, k.Secret, time.Now(), 300)
	if resp, body := xmlCall(t, h, "GET", u, "", nil); resp.StatusCode != http.StatusOK || body != "classified" {
		t.Fatalf("a signed GET = %d %s", resp.StatusCode, body)
	}
	tampered := strings.Replace(u, "X-Goog-Signature=", "X-Goog-Signature=00", 1)
	if resp, body := xmlCall(t, h, "GET", tampered, "", nil); resp.StatusCode != http.StatusForbidden || xmlErrorCode(body) != "SignatureDoesNotMatch" {
		t.Errorf("a tampered signature = %d %s; want 403 SignatureDoesNotMatch", resp.StatusCode, body)
	}
}

// TestStorageSignedURLV2RegisteredCert: the official client's V2 (and V4
// RSA) signed URLs verify against the certificate the server was given; a
// service account with no registered certificate is 403.
func TestStorageSignedURLV2RegisteredCert(t *testing.T) {
	keyPath, email := os.Getenv(EnvSigningKey), os.Getenv(EnvSigningEmail)
	if keyPath == "" || email == "" {
		t.Skipf("%s and %s are not set: CI starts the server with a generated certificate", EnvSigningKey, EnvSigningEmail)
	}
	pk, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, c)
	putObject(t, ctx, bh.Object("secret.txt"), "classified")
	host := strings.TrimPrefix(h.Endpoint(EnvStorage), "http://")
	for _, sc := range []storage.SigningScheme{storage.SigningSchemeV2, storage.SigningSchemeV4} {
		u, err := storage.SignedURL(bh.BucketName(), "secret.txt", &storage.SignedURLOptions{GoogleAccessID: email, PrivateKey: pk,
			Method: "GET", Expires: time.Now().Add(time.Minute), Scheme: sc, Hostname: host, Insecure: true})
		if err != nil {
			t.Fatal(err)
		}
		u = strings.Replace(u, "https://", "http://", 1)
		if resp, body := xmlCall(t, h, "GET", u, "", nil); resp.StatusCode != http.StatusOK || body != "classified" {
			t.Errorf("scheme %d: a signed GET = %d %s", sc, resp.StatusCode, body)
		}
	}
	u, _ := storage.SignedURL(bh.BucketName(), "secret.txt", &storage.SignedURLOptions{GoogleAccessID: "stranger@" + h.Project() + ".iam.gserviceaccount.com",
		PrivateKey: pk, Method: "GET", Expires: time.Now().Add(time.Minute), Scheme: storage.SigningSchemeV2, Hostname: host, Insecure: true})
	u = strings.Replace(u, "https://", "http://", 1)
	if resp, body := xmlCall(t, h, "GET", u, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a service account with no registered certificate = %d %s; want 403", resp.StatusCode, body)
	}
}
