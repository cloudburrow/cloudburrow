package storage

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
)

// A V4 RSA URL signed as an account of a certificate source's domain
// verifies against the certificates the source serves for it (#1786); an
// account the source does not know, a key it does not serve and a source
// that is down are refused.
func TestSignedURLCertSource(t *testing.T) {
	const email = "bqcx-1-ab12@gcp-sa-bigquery-condel.iam.gserviceaccount.com"
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: email},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/service_accounts/v1/metadata/x509/"+email {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"k1": cert})
	}))
	defer certs.Close()
	_, base := signedServer(t, Options{CertSources: map[string]string{"gcp-sa-bigquery-condel.iam.gserviceaccount.com": certs.URL}})
	host := strings.TrimPrefix(base, "http://")
	sign := func(account string, k *rsa.PrivateKey) string {
		pk := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
		u, err := gcs.SignedURL("signed", "dir/secret.txt", &gcs.SignedURLOptions{GoogleAccessID: account, PrivateKey: pk,
			Method: "GET", Expires: time.Now().Add(time.Minute), Scheme: gcs.SigningSchemeV4, Hostname: host, Insecure: true})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if resp, body := xmlDo(t, "GET", sign(email, key), "", nil); resp.StatusCode != 200 || body != "classified" {
		t.Errorf("signed by the served key = %d %s", resp.StatusCode, body)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if resp, body := xmlDo(t, "GET", sign(email, other), "", nil); resp.StatusCode != 403 || xmlCode(body) != "SignatureDoesNotMatch" {
		t.Errorf("signed by another key = %d %s", resp.StatusCode, body)
	}
	if resp, body := xmlDo(t, "GET", sign("bqcx-1-ffff@gcp-sa-bigquery-condel.iam.gserviceaccount.com", key), "", nil); resp.StatusCode != 403 || xmlCode(body) != "AccessDenied" {
		t.Errorf("an account the source does not know = %d %s", resp.StatusCode, body)
	}
	if resp, body := xmlDo(t, "GET", sign("app@p.iam.gserviceaccount.com", key), "", nil); resp.StatusCode != 403 || xmlCode(body) != "AccessDenied" {
		t.Errorf("an account of no source = %d %s", resp.StatusCode, body)
	}
	u := sign(email, key)
	certs.Close()
	if resp, body := xmlDo(t, "GET", u, "", nil); resp.StatusCode != 403 || xmlCode(body) != "AccessDenied" {
		t.Errorf("with the source down = %d %s", resp.StatusCode, body)
	}
}
