package firestoreadmin

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMongoServerTLS (#1738): the instance's CA is made once and kept;
// the server certificate is of it, for the loopback and the Service's
// names, kept while it has more than 30 days left and issued again after.
func TestMongoServerTLS(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cert, key, err := MongoServerTLS(dir, now, "firestore", "firestore.ns.svc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MongoTLSConfig(string(cert), string(key)); err != nil {
		t.Fatalf("MongoTLSConfig: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, MongoCAFile))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	b, _ := pem.Decode(cert)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"localhost", "127.0.0.1", "firestore.ns.svc"} {
		if _, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("the certificate does not verify for %s: %v", name, err)
		}
	}
	again, _, err := MongoServerTLS(dir, now.Add(24*time.Hour), "firestore", "firestore.ns.svc")
	if err != nil || !bytes.Equal(again, cert) {
		t.Errorf("a day later the certificate was issued again (%v)", err)
	}
	later, _, err := MongoServerTLS(dir, now.Add(160*24*time.Hour), "firestore", "firestore.ns.svc")
	if err != nil || bytes.Equal(later, cert) {
		t.Errorf("20 days before it expires the certificate was kept (%v)", err)
	}
	if ca2, _ := os.ReadFile(filepath.Join(dir, MongoCAFile)); !bytes.Equal(ca2, caPEM) {
		t.Error("the CA changed")
	}
}
