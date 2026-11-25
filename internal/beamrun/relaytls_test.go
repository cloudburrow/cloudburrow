package beamrun

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The relay's certificate is of relayName, signed by the job's CA, which
// the Java workers trust (assets/java/pool.sh): a client trusting only
// ca.pem verifies it for relayName and for no other name; with the
// instance's Spanner and BigQuery Storage API live it is of their Google
// hosts too (googleHosts), which the job's containers resolve to the
// relay, and of no other Google host.
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
	google := jobGoogleHosts(map[string]string{"spanner": "127.0.0.1:1", "bigquery-storage": "127.0.0.1:2"})
	if want := []string{"batch-spanner.googleapis.com", "bigquerystorage.googleapis.com", "spanner.googleapis.com"}; strings.Join(google, ",") != strings.Join(want, ",") {
		t.Errorf("jobGoogleHosts = %v; want %v", google, want)
	}
	if got := jobGoogleHosts(map[string]string{"spanner": "127.0.0.1:1"}); len(got) != 2 {
		t.Errorf("jobGoogleHosts(spanner only) = %v; want Spanner's two hosts", got)
	}
	launcher := filepath.Join(t.TempDir(), "tls")
	if err := writeRelayTLS(launcher, google...); err != nil {
		t.Fatal(err)
	}
	pair, err = tls.LoadX509KeyPair(filepath.Join(launcher, "cert.pem"), filepath.Join(launcher, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caPEM, _ = os.ReadFile(filepath.Join(launcher, "ca.pem"))
	roots = x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	leaf, _ = x509.ParseCertificate(pair.Certificate[0])
	for _, name := range append([]string{relayName}, google...) {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err != nil {
			t.Errorf("the job's certificate for %s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "storage.googleapis.com", Roots: roots}); err == nil {
		t.Error("the job's certificate verifies for storage.googleapis.com")
	}
}
