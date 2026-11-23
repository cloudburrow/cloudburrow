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
	"bufio"
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
	"strings"
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

// Connector is the server-side proxy's listener.
type Connector struct {
	s  *Server
	ln net.Listener
	wg sync.WaitGroup
}

// ServeConnector listens on addr (host:5433) for the connectors, until
// Close.
func (s *Server) ServeConnector(addr string) (*Connector, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Connector{s: s, ln: ln}
	c.wg.Add(1)
	go c.accept()
	return c, nil
}

// Addr is the bound address.
func (c *Connector) Addr() string {
	if c == nil || c.ln == nil {
		return ""
	}
	return c.ln.Addr().String()
}

// Close stops the listener; connections in progress go on.
func (c *Connector) Close() error {
	if c == nil {
		return nil
	}
	err := c.ln.Close()
	c.wg.Wait()
	return err
}

func (c *Connector) accept() {
	defer c.wg.Done()
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		go c.serve(conn)
	}
}

func (c *Connector) tlsConfig(local string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  c.s.ca.pool,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = local
			}
			return c.s.ca.serverCert(name)
		},
	}
}

func (c *Connector) serve(raw net.Conn) {
	defer raw.Close()
	host := hostOnly(raw.LocalAddr().String())
	if h := c.s.connectorHost(); net.ParseIP(h) != nil {
		host = h
	}
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	conn := tls.Server(raw, c.tlsConfig(host))
	if err := conn.Handshake(); err != nil {
		return
	}
	st := conn.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return
	}
	client := st.PeerCertificates[0]
	cluster := client.Subject.CommonName
	mdx := false
	for _, ou := range client.Subject.OrganizationalUnit {
		mdx = mdx || ou == mdxMarker
	}
	r := bufio.NewReader(conn)
	var authType connectorspb.MetadataExchangeRequest_AuthType
	var token string
	if mdx {
		req, err := readMDX(r)
		if err != nil {
			return
		}
		authType, token = req.GetAuthType(), req.GetOauth2Token()
	}
	addr, err := c.s.route(cluster, st.ServerName, authType, token)
	if mdx {
		resp := &connectorspb.MetadataExchangeResponse{ResponseCode: connectorspb.MetadataExchangeResponse_OK}
		if err != nil {
			resp = &connectorspb.MetadataExchangeResponse{ResponseCode: connectorspb.MetadataExchangeResponse_ERROR, Error: err.Error()}
		}
		if werr := writeMDX(conn, resp); werr != nil {
			return
		}
	}
	if err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})
	backend, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return
	}
	defer backend.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, r); closeWrite(backend); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, backend); _ = conn.CloseWrite(); done <- struct{}{} }()
	<-done
	<-done
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

// route is the PostgreSQL address of the instance a connection reaches:
// the instance whose PSC DNS name serverName is, or the primary of
// cluster. An AUTO_IAM connection's token must be one this instance minted,
// for a principal the cluster has an IAM user for.
func (s *Server) route(cluster, sni string, auth connectorspb.MetadataExchangeRequest_AuthType, token string) (string, error) {
	s.mu.Lock()
	var in *alloydbpb.Instance
	if sni != "" && net.ParseIP(sni) == nil {
		for _, k := range sortedKeys(s.instances, cluster+"/instances/") {
			if PSCDNSName(k) == strings.TrimSuffix(strings.ToLower(sni), ".") {
				in = s.instances[k]
			}
		}
		if in == nil {
			s.mu.Unlock()
			return "", fmt.Errorf("no instance of cluster %s is named %s", cluster, sni)
		}
	} else {
		in = s.primaryOf(cluster)
	}
	if in == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("cluster %s has no primary instance", cluster)
	}
	if in.GetState() != alloydbpb.Instance_READY {
		s.mu.Unlock()
		return "", fmt.Errorf("instance %s is %s", in.GetName(), in.GetState())
	}
	server := serverName(in.GetName())
	var iamUsers []string
	for _, k := range sortedKeys(s.users, cluster+"/users/") {
		if s.users[k].GetUserType() == alloydbpb.User_ALLOYDB_IAM_USER {
			iamUsers = append(iamUsers, strings.ToLower(k[strings.LastIndex(k, "/")+1:]))
		}
	}
	s.mu.Unlock()
	if auth == connectorspb.MetadataExchangeRequest_AUTO_IAM {
		if s.opts.IAMToken == nil {
			return "", errors.New("IAM database authentication: no token can be checked here")
		}
		email, ok := s.opts.IAMToken(token)
		if !ok {
			return "", errors.New("IAM database authentication failed: the OAuth 2.0 token is not valid")
		}
		found := false
		for _, u := range iamUsers {
			found = found || u == strings.ToLower(email)
		}
		if !found {
			return "", fmt.Errorf("IAM database authentication failed: %s is not an IAM user of cluster %s", email, cluster)
		}
	}
	addr := s.opts.Engine.Addr(server)
	if addr == "" {
		return "", fmt.Errorf("instance %s does not answer", in.GetName())
	}
	return addr, nil
}
