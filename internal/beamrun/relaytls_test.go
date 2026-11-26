package beamrun

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

// The relay's certificate is of relayName, signed by the job's CA, which
// the Java workers trust (assets/java/pool.sh): a client trusting only
// ca.pem verifies it for relayName and for no other name.
func TestWriteRelayTLS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if err := writeRelayTLS(dir); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem holds no certificate")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: relayName, Roots: roots}); err != nil {
		t.Errorf("verify for %s: %v", relayName, err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "bigquerystorage.googleapis.com", Roots: roots}); err == nil {
		t.Error("the relay's certificate verifies for bigquerystorage.googleapis.com; want only " + relayName)
	}
}
