package firestoreadmin

// Firestore with MongoDB compatibility (#1738): TLS. Google's connection
// requirements say "Drivers must connect with SSL enabled", and its
// connection strings set tls=true, so the endpoint serves TLS only: a
// client that does not speak TLS is refused at its first message, as on
// Google. Google's endpoint has a certificate of a public CA for
// UID.LOCATION.firestore.goog; this one has a certificate of the
// instance's own CA, which a client trusts with tlsCAFile (the file
// `cloudburrow env` names as CLOUDBURROW_FIRESTORE_MONGODB_CA_FILE).
//
// The CA and certificate are made as Memorystore's in-transit encryption
// (redisadmin/tls.go) and Cloud SQL's server CA (sqladmin/connector.go)
// make theirs: ECDSA P-256 keys, a CA valid for ten years, a server
// certificate valid for 180 days for the host's loopback and the Firestore
// Service's names in the cluster, TLS 1.2 or later. They are kept in the
// instance's directory, so a client's CA file stays valid across `stop`
// and `up`; the server certificate is issued again when it has less than
// 30 days left.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Where the instance directory keeps the endpoint's CA and certificate.
const (
	MongoCAFile       = "firestore-mongodb-ca.pem"
	mongoCAKeyFile    = "firestore-mongodb-ca-key.pem"
	mongoCertFile     = "firestore-mongodb-server.pem"
	mongoCertKeyFile  = "firestore-mongodb-server-key.pem"
	mongoCALifetime   = 10 * 365 * 24 * time.Hour
	mongoCertLifetime = 180 * 24 * time.Hour
	mongoCertRenew    = 30 * 24 * time.Hour
)

// The front's environment that carries the server certificate and key.
const (
	MongoTLSCertEnv = "FIRESTORE_MONGODB_TLS_CERT"
	MongoTLSKeyEnv  = "FIRESTORE_MONGODB_TLS_KEY"
)

func mongoSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func pemKey(k *ecdsa.PrivateKey) ([]byte, error) {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), nil
}

func readPair(certPath, keyPath string) (*x509.Certificate, any, []byte, []byte, error) {
	cp, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	kp, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cb, _ := pem.Decode(cp)
	kb, _ := pem.Decode(kp)
	if cb == nil || kb == nil {
		return nil, nil, nil, nil, fmt.Errorf("%s or %s holds no PEM certificate or key", certPath, keyPath)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cert, key, cp, kp, nil
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// MongoServerTLS is the endpoint's server certificate and key, as PEM, for
// names (its Service's names in the cluster) and the host's loopback,
// issued by the instance's CA in dir, both made when missing, expiring or
// for other names.
func MongoServerTLS(dir string, now time.Time, names ...string) (certPEM, keyPEM []byte, err error) {
	ca, caKey, _, _, err := readPair(filepath.Join(dir, MongoCAFile), filepath.Join(dir, mongoCAKeyFile))
	if errors.Is(err, os.ErrNotExist) {
		k, gerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if gerr != nil {
			return nil, nil, gerr
		}
		tmpl := &x509.Certificate{
			SerialNumber:          mongoSerial(),
			Subject:               pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: "CloudBurrow Firestore with MongoDB compatibility CA"},
			NotBefore:             now.Add(-time.Hour),
			NotAfter:              now.Add(mongoCALifetime),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		}
		der, cerr := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
		if cerr != nil {
			return nil, nil, cerr
		}
		kp, kerr := pemKey(k)
		if kerr != nil {
			return nil, nil, kerr
		}
		if err := writeFile(filepath.Join(dir, mongoCAKeyFile), kp); err != nil {
			return nil, nil, err
		}
		if err := writeFile(filepath.Join(dir, MongoCAFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
			return nil, nil, err
		}
		if ca, caKey, _, _, err = readPair(filepath.Join(dir, MongoCAFile), filepath.Join(dir, mongoCAKeyFile)); err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, fmt.Errorf("the Firestore with MongoDB compatibility CA: %w (delete %s to make a new one)", err, MongoCAFile)
	}
	dns := append([]string{"localhost"}, names...)
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	certPath, keyPath := filepath.Join(dir, mongoCertFile), filepath.Join(dir, mongoCertKeyFile)
	if cert, _, cp, kp, err := readPair(certPath, keyPath); err == nil &&
		cert.NotAfter.Sub(now) > mongoCertRenew && slices.Equal(cert.DNSNames, dns) && cert.CheckSignatureFrom(ca) == nil {
		return cp, kp, nil
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: mongoSerial(),
		Subject:      pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(mongoCertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	kp, err := pemKey(k)
	if err != nil {
		return nil, nil, err
	}
	cp := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFile(certPath, cp); err != nil {
		return nil, nil, err
	}
	if err := writeFile(keyPath, kp); err != nil {
		return nil, nil, err
	}
	return cp, kp, nil
}

// MongoTLSConfig is the endpoint's TLS configuration, from the PEM the
// front's environment carries.
func MongoTLSConfig(certPEM, keyPEM string) (*tls.Config, error) {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}
