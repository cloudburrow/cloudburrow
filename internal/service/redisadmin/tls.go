package redisadmin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// In-transit encryption (#1413). An instance created with
// transit_encryption_mode SERVER_AUTHENTICATION serves TLS only, on port
// TLSPort, as Memorystore's does ("Redis clients communicate exclusively
// across a secure port connection"; clients "not configured for TLS will
// be blocked"), with a server certificate issued by a Certificate
// Authority of the instance's own, which server_ca_certs lists for its
// clients to trust. As Memorystore's: the CA is valid for ten years, the
// server certificate for 180 days (it is issued again each time the
// server starts), TLS 1.2 or later, and clients are not authenticated by
// certificate (SERVER_AUTHENTICATION: only the server is). The keys are
// generated as the Cloud SQL connector's are: ECDSA P-256. Replication
// between a Standard tier instance's nodes is TLS too.

// TLSPort is the port a server with in-transit encryption serves, as
// Memorystore's does.
const TLSPort = 6378

// caLifetime and certLifetime are Memorystore's documented validity of
// the instance's CA and of its server certificate.
const (
	caLifetime   = 10 * 365 * 24 * time.Hour
	certLifetime = 180 * 24 * time.Hour
)

// TLSFiles are what a server with in-transit encryption is started with,
// as PEM.
type TLSFiles struct {
	Cert, Key, CA string
}

// tlsOn is whether r serves TLS.
func (r *record) tlsOn() bool { return r.TransitCA != "" }

// caPEM is r's CA certificates, "" without in-transit encryption: its CA
// and, after a server CA rotation, the ones it replaced that are still
// valid, which a client trusts too (#1463).
func (r *record) caPEM() string { return r.TransitCA + r.PreviousCAs }

// clusterAddr is where r's server (or Service) server is reached in the
// cluster: on TLSPort with in-transit encryption.
func (r *record) clusterAddr(rs ReplicaSet, server string) (string, int) {
	host, port := rs.ClusterAddr(server)
	if r.tlsOn() {
		port = TLSPort
	}
	return host, port
}

// servicePort is the port of r's Services: 0 for the default.
func (r *record) servicePort() int {
	if r.tlsOn() {
		return TLSPort
	}
	return 0
}

func serialNumber() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func encodeKey(k *ecdsa.PrivateKey) (string, error) {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})), nil
}

// newCA is a fresh CA for the instance name: certificate and key, as PEM.
func newCA(name string, now time.Time) (cert, key string, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serialNumber(),
		Subject:               pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: "CloudBurrow Memorystore Server CA " + name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return "", "", err
	}
	key, err = encodeKey(k)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), key, nil
}

func parseCAPair(certPEM, keyPEM string) (*x509.Certificate, any, error) {
	cb, _ := pem.Decode([]byte(certPEM))
	kb, _ := pem.Decode([]byte(keyPEM))
	if cb == nil || kb == nil {
		return nil, nil, errors.New("the instance's CA holds no PEM certificate or key")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// ServerTLS issues spec's server a certificate of its instance's CA, for
// the names its clients reach it at: the host's loopback, where its
// tunnels and endpoints are, and names, its Services in the cluster, or
// further IP addresses (a cluster node's, #1549). It is nil for a server
// without in-transit encryption.
func ServerTLS(spec Spec, now time.Time, names ...string) (*TLSFiles, error) {
	if spec.TransitCA == "" {
		return nil, nil
	}
	ca, caKey, err := parseCAPair(spec.TransitCA, spec.TransitCAKey)
	if err != nil {
		return nil, err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serialNumber(),
		Subject:      pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: spec.Server},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost", spec.Server},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if n != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	key, err := encodeKey(k)
	if err != nil {
		return nil, err
	}
	trust := spec.TransitTrust
	if trust == "" {
		trust = spec.TransitCA
	}
	return &TLSFiles{Cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), Key: key, CA: trust}, nil
}

// serverCACerts is server_ca_certs: r's CAs, as Google lists them, the
// newest first; an expired one a rotation replaced is no longer listed.
func (r *record) serverCACerts() []*redispb.TlsCertificate {
	return listCAs(r.caPEM(), time.Now())
}

func listCAs(bundle string, now time.Time) []*redispb.TlsCertificate {
	var out []*redispb.TlsCertificate
	rest := []byte(bundle)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return out
		}
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil || (len(out) > 0 && now.After(cert.NotAfter)) {
			continue
		}
		sum := sha1.Sum(cert.Raw)
		out = append(out, &redispb.TlsCertificate{
			SerialNumber:    cert.SerialNumber.String(),
			Cert:            string(pem.EncodeToMemory(b)),
			CreateTime:      timestamppb.New(cert.NotBefore.Add(time.Hour)),
			ExpireTime:      timestamppb.New(cert.NotAfter),
			Sha1Fingerprint: hex.EncodeToString(sum[:]),
		})
	}
}

// ClientTLS is the TLS configuration of a client of a server with the CA
// caPEM, nil for "": TLS 1.2 or later, as Memorystore requires.
func ClientTLS(caPEM string) (*tls.Config, error) {
	if caPEM == "" {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, fmt.Errorf("the instance's CA is not a PEM certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
