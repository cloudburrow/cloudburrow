package sqladmin

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// A MySQL 5.7 source negotiates only the RSA key exchange suites, which
// Go's defaults leave out: sourceTLSConfig completes that handshake, and a
// default client's is refused as main run 45047408927's was.
func TestSourceTLSConfigReachesAnRSAKeyExchangeServer(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "MySQL_Server_5.7.44_Auto_Generated_Server_Certificate"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_256_CBC_SHA, tls.TLS_RSA_WITH_AES_128_CBC_SHA}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _ = tls.Server(c, server).Handshake() }()
		}
	}()
	dial := func(cfg *tls.Config) error {
		c, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial(&tls.Config{InsecureSkipVerify: true}); err == nil { //nolint:gosec // a test server
		t.Log("Go's defaults reached an RSA key exchange server: the extra suites are not needed on this Go")
	}
	if err := dial(sourceTLSConfig()); err != nil {
		t.Fatalf("sourceTLSConfig's handshake with an RSA key exchange server: %v", err)
	}
}
