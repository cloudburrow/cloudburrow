package resourcemanager

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"golang.org/x/crypto/pkcs12"
)

// encodePKCS12's file decodes with golang.org/x/crypto/pkcs12 and the
// password Google documents, to the key and certificate it was given.
func TestEncodePKCS12RoundTrips(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "a@p.iam.gserviceaccount.com"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	file, err := encodePKCS12(der, certDER)
	if err != nil {
		t.Fatal(err)
	}
	key, cert, err := pkcs12.Decode(file, "notasecret")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !key.(*rsa.PrivateKey).Equal(priv) || string(cert.Raw) != string(certDER) {
		t.Error("the decoded key or certificate differs")
	}
	if _, _, err := pkcs12.Decode(file, "other"); err == nil {
		t.Error("a wrong password decoded")
	}
}
