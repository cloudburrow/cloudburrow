package alloydb

// The connector path: what the AlloyDB Go, Java and Python connectors and
// the AlloyDB Auth Proxy do before they send a byte of SQL
// (docs.cloud.google.com/alloydb/docs/auth-proxy/overview, and the Go
// connector's source, cloud.google.com/go/alloydbconn):
//
//  1. GenerateClientCertificate signs a client certificate for the public
//     key the connector made, and answers the cluster's CA;
//  2. GetConnectionInfo answers the instance's IP address (and, through
//     v1alpha and v1beta, its PSC DNS name);
//  3. the connector dials that address on port 5433 with TLS 1.3, with the
//     client certificate, checking the server's against the CA and the
//     address; then, unless the certificate was asked for without it, it
//     sends a MetadataExchangeRequest (a big-endian uint32 length and the
//     message) with its OAuth 2.0 token and the authentication type, and
//     reads a MetadataExchangeResponse the same way.
//
// AlloyDB's server-side proxy answers on 5433. Here a listener in this
// process plays that part: it presents a certificate signed by the CA for
// the name or address dialled, requires a client certificate the CA
// signed, checks the token of an AUTO_IAM connection, and then forwards the
// connection, now plaintext, to the instance's PostgreSQL. The instance is
// the one whose PSC DNS name the TLS server name is; a connection with no
// server name (an IP address) reaches the primary of the cluster its
// client certificate was signed for, since every instance's IP address is
// the listener's.
//
// One CA signs for every cluster, kept in the instance's state directory
// so a connector's cached certificates stay valid across restarts; each
// cluster's ca_cert is that CA. Keys never leave the host.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"cloud.google.com/go/alloydb/connectors/apiv1alpha/connectorspb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ConnectorPort is the port the connectors dial: serverProxyPort, a
// constant in each connector, so --port-base cannot move it.
const ConnectorPort = 5433

// mdxMarker, in a client certificate's organizational units, says the
// connector that asked for it sends a metadata exchange.
const mdxMarker = "alloydb-metadata-exchange"

type authority struct {
	cert    *x509.Certificate
	certPEM string
	key     crypto.Signer
	pool    *x509.CertPool

	mu    sync.Mutex
	certs map[string]*tls.Certificate // server certificates, by name
}

type caFile struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

func loadOrCreateAuthority(path string) (*authority, error) {
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var f caFile
			if err := json.Unmarshal(b, &f); err != nil {
				return nil, fmt.Errorf("%s is unreadable; delete it to make a new CA: %w", path, err)
			}
			return parseAuthority(f)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: "CloudBurrow AlloyDB Cluster CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	f := caFile{
		Cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		Key:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})),
	}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(f, "", "  ")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	}
	return parseAuthority(f)
}

func parseAuthority(f caFile) (*authority, error) {
	cb, _ := pem.Decode([]byte(f.Cert))
	kb, _ := pem.Decode([]byte(f.Key))
	if cb == nil || kb == nil {
		return nil, errors.New("the CA file holds no PEM certificate or key")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("the CA key cannot sign")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &authority{cert: cert, certPEM: f.Cert, key: signer, pool: pool, certs: map[string]*tls.Certificate{}}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// serverCert is the certificate for name, a DNS name or an IP address.
func (a *authority) serverCert(name string) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.certs[name]; ok && time.Now().Before(c.Leaf.NotAfter.Add(-time.Hour)) {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, 30),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	c := &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key, Leaf: leaf}
	a.certs[name] = c
	return c, nil
}

// maxCertDuration is the longest client certificate signed.
// UNVERIFIED: Google's bound; the connectors ask for an hour.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
const maxCertDuration = 24 * time.Hour

func (s *Server) GenerateClientCertificate(_ context.Context, req *alloydbpb.GenerateClientCertificateRequest) (*alloydbpb.GenerateClientCertificateResponse, error) {
	s.mu.Lock()
	c, err := s.cluster(req.GetParent())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode([]byte(req.GetPublicKey()))
	if b == nil {
		return nil, apierror.InvalidArgument("public_key must be a PEM-encoded public key")
	}
	var pub any
	switch b.Type {
	case "RSA PUBLIC KEY":
		pub, err = x509.ParsePKCS1PublicKey(b.Bytes)
	default:
		pub, err = x509.ParsePKIXPublicKey(b.Bytes)
	}
	if err != nil {
		return nil, apierror.InvalidArgument("public_key: %v", err)
	}
	d := maxCertDuration
	if cd := req.GetCertDuration(); cd != nil && cd.AsDuration() > 0 && cd.AsDuration() < d {
		d = cd.AsDuration()
	}
	now := time.Now()
	subj := pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: c.GetName()}
	if req.GetUseMetadataExchange() {
		subj.OrganizationalUnit = []string{mdxMarker}
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      subj,
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(d),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca.cert, pub, s.ca.key)
	if err != nil {
		return nil, apierror.Internal(err, "sign the client certificate")
	}
	leaf := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return &alloydbpb.GenerateClientCertificateResponse{PemCertificateChain: []string{leaf, s.ca.certPEM}, CaCert: s.ca.certPEM}, nil
}

func closeWrite(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.CloseWrite()
	}
}

func readMDX(r io.Reader) (*connectorspb.MetadataExchangeRequest, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > 16*1024 {
		return nil, errors.New("metadata exchange request too large")
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	var req connectorspb.MetadataExchangeRequest
	if err := proto.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func writeMDX(w io.Writer, resp *connectorspb.MetadataExchangeResponse) error {
	b, err := proto.Marshal(resp)
	if err != nil {
		return err
	}
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	_, err = w.Write(append(out, b...))
	return err
}

// CACertPEM is the certificate authority's certificate, PEM: the clusters'
// ca_cert, which also signs the admin API's TLS endpoint.
func (s *Server) CACertPEM() string { return s.ca.certPEM }

// ServerCertificate is a certificate the authority signs for name, a DNS
// name or an IP address.
func (s *Server) ServerCertificate(name string) (*tls.Certificate, error) {
	return s.ca.serverCert(name)
}
