package sqladmin

// The connector path (#1330): what the Cloud SQL Go, Java and Python
// connectors and the Auth Proxy do before they send a byte of SQL.
//
//  1. connect.get (connectSettings) returns the instance's addresses, its
//     server CA and a DNS name;
//  2. connect.generateEphemeralCert signs a client certificate for the
//     public key the connector generated;
//  3. the connector dials the address on port 3307 with TLS 1.3, presenting
//     that certificate and checking the server's against the CA.
//
// Cloud SQL's server-side proxy is what answers on 3307. Here a listener in
// this process plays that part: it picks the instance from the TLS server
// name (the DNS name connectSettings returned), presents a server
// certificate signed by that instance's CA, requires a client certificate
// signed by the same CA for the same project, and then forwards the
// connection, now plaintext, to the instance's PostgreSQL or MySQL server.
//
// Each instance's CA is generated locally on first use and kept in the
// instance's state directory, so a connector's cached CA stays valid across
// restarts. Keys never leave the host and are never printed.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // Cloud SQL's sha1Fingerprint is SHA-1 by definition.
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ConnectorPort is the port the connectors dial. It is a constant in every
// connector (serverProxyPort in cloud.google.com/go/cloudsqlconn), not a
// setting, so it cannot be moved by --port-base.
const ConnectorPort = 3307

// dnsSuffix is the domain of the DNS names connectSettings returns:
// {instance}.{project}.{region}.sql.cloudburrow.internal.
const dnsSuffix = "." + Region + ".sql.cloudburrow.internal"

// ephemeralLifetime is how long a client certificate is valid, as Cloud
// SQL's are.
const ephemeralLifetime = time.Hour

type serverCA struct {
	cert    *x509.Certificate
	certPEM string
	key     crypto.Signer
	pool    *x509.CertPool
}

type caFile struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// loadOrCreateCA reads the instance's CA from path, generating it first if
// the file does not exist.
func loadOrCreateCA(path, instance string) (*serverCA, error) {
	if b, err := os.ReadFile(path); err == nil {
		var f caFile
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("%s is unreadable; delete it to generate a new CA: %w", path, err)
		}
		return parseCA(f)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject: pkix.Name{Organization: []string{"CloudBurrow"},
			CommonName: "CloudBurrow Cloud SQL Server CA " + instance},
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return parseCA(f)
}

func parseCA(f caFile) (*serverCA, error) {
	cb, _ := pem.Decode([]byte(f.Cert))
	kb, _ := pem.Decode([]byte(f.Key))
	if cb == nil || kb == nil {
		return nil, errors.New("the server CA file holds no PEM certificate or key")
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
		return nil, errors.New("the server CA key cannot sign")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &serverCA{cert: cert, certPEM: f.Cert, key: signer, pool: pool}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// connector is the TLS listener on 3307 and the instances' CAs.
type connector struct {
	addr string
	cas  map[string]*serverCA // by instance

	ln net.Listener
	// unavailable says why the listener is not serving, empty when it is.
	unavailable string

	mu    sync.Mutex
	certs map[string]*tls.Certificate // server certificates, by DNS name

	// dir holds each instance's CA file; camu guards cas, which instances
	// created and deleted through the API change (#1330).
	dir  string
	camu sync.RWMutex
}

func caPath(dir, instance string) string {
	return filepath.Join(dir, "cloudsql-"+instance+"-server-ca.json")
}

// ca is the instance's CA, nil when it has none.
func (c *connector) ca(instance string) *serverCA {
	c.camu.RLock()
	defer c.camu.RUnlock()
	return c.cas[instance]
}

// addInstance gives a created instance its own CA, kept beside the others.
func (c *connector) addInstance(instance string) error {
	if c.unavailable != "" || c.cas == nil {
		return nil
	}
	ca, err := loadOrCreateCA(caPath(c.dir, instance), instance)
	if err != nil {
		return fmt.Errorf("sqladmin: server CA for %s: %w", instance, err)
	}
	c.camu.Lock()
	c.cas[instance] = ca
	c.camu.Unlock()
	return nil
}

// removeInstance forgets a deleted instance's CA and its server
// certificates, so an instance created again under the name has a new one,
// as Cloud SQL's has.
func (c *connector) removeInstance(instance string) {
	if c.cas == nil {
		return
	}
	c.camu.Lock()
	delete(c.cas, instance)
	c.camu.Unlock()
	_ = os.Remove(caPath(c.dir, instance))
	c.mu.Lock()
	for name := range c.certs {
		if _, in, ok := parseDNSName(name); ok && in == instance {
			delete(c.certs, name)
		}
	}
	c.mu.Unlock()
}

// EnableConnector serves the connector path: connectSettings and
// generateEphemeralCert, and the TLS listener at addr (host:3307) they
// point at. Each instance's CA is kept in dir.
func (s *Server) EnableConnector(addr, dir string) error {
	c := &connector{addr: addr, dir: dir, cas: map[string]*serverCA{}, certs: map[string]*tls.Certificate{}}
	for _, e := range s.engines {
		ca, err := loadOrCreateCA(caPath(dir, e.Instance()), e.Instance())
		if err != nil {
			err = fmt.Errorf("sqladmin: server CA for %s: %w", e.Instance(), err)
			s.conn = &connector{unavailable: err.Error()}
			return err
		}
		c.cas[e.Instance()] = ca
	}
	s.conn = c
	return nil
}

// start binds the listener. A port already in use is not fatal: the rest of
// the Admin API still serves, and the connector methods say why they cannot.
func (c *connector) start(s *Server) {
	if c.unavailable != "" {
		return
	}
	ln, err := net.Listen("tcp", c.addr)
	if err != nil {
		c.unavailable = fmt.Sprintf("the connector listener could not bind %s (%v); "+
			"the Cloud SQL connectors always dial port %d, so only one CloudBurrow instance at a time can serve them",
			c.addr, err, ConnectorPort)
		return
	}
	c.ln = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go c.serve(s, conn)
		}
	}()
}

func (c *connector) stop() {
	if c.ln != nil {
		_ = c.ln.Close()
	}
}

func dnsName(project, instance string) string { return instance + "." + project + dnsSuffix }

// parseDNSName is dnsName's inverse.
func parseDNSName(name string) (project, instance string, ok bool) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	rest, found := strings.CutSuffix(name, dnsSuffix)
	if !found {
		return "", "", false
	}
	instance, project, ok = strings.Cut(rest, ".")
	return project, instance, ok && project != "" && instance != ""
}

// serverCert is the certificate presented for project's instance: signed by
// the instance's CA, with the DNS name as its SAN and project:instance as
// its CN, which is what the connectors check.
func (c *connector) serverCert(project, instance string) (*tls.Certificate, error) {
	name := dnsName(project, instance)
	c.mu.Lock()
	defer c.mu.Unlock()
	if crt, ok := c.certs[name]; ok && time.Now().Before(crt.Leaf.NotAfter.Add(-time.Hour)) {
		return crt, nil
	}
	ca := c.ca(instance)
	if ca == nil {
		return nil, fmt.Errorf("no instance %q", instance)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: project + ":" + instance},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	crt := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	c.certs[name] = crt
	return crt, nil
}

func (c *connector) serve(s *Server, raw net.Conn) {
	defer raw.Close()
	var target Engine
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			project, instance, ok := parseDNSName(hello.ServerName)
			if !ok {
				return nil, fmt.Errorf("server name %q is not a CloudBurrow Cloud SQL instance", hello.ServerName)
			}
			e := s.engine(instance)
			ca := c.ca(instance)
			if e == nil || ca == nil {
				return nil, fmt.Errorf("no instance %q", instance)
			}
			crt, err := c.serverCert(project, instance)
			if err != nil {
				return nil, err
			}
			target = e
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{*crt},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    ca.pool,
				// The CA signs for every project; the certificate must be
				// the one issued for this project's instance.
				VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error {
					if len(chains) == 0 || chains[0][0].Subject.CommonName != project+":"+instance {
						return errors.New("the client certificate was not issued for " + project + ":" + instance)
					}
					return nil
				},
			}, nil
		},
	}
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	tc := tls.Server(raw, cfg)
	if err := tc.Handshake(); err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})
	addr := target.Addr()
	if addr == "" {
		return
	}
	up, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, tc); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(tc, up); _ = tc.CloseWrite(); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

func (s *Server) connectorReady() error {
	if s.conn == nil {
		return unimplemented("the connector path", "this server was started without its connector listener")
	}
	if s.conn.unavailable != "" {
		return Errorf(http.StatusServiceUnavailable, "%s", s.conn.unavailable)
	}
	return nil
}

func sslCertJSON(project, instance string, cert *x509.Certificate, certPEM string) map[string]any {
	fp := sha1.Sum(cert.Raw) //nolint:gosec
	return map[string]any{
		"kind": "sql#sslCert", "instance": instance, "cert": certPEM,
		"certSerialNumber": cert.SerialNumber.String(), "commonName": cert.Subject.CommonName,
		"sha1Fingerprint": hex.EncodeToString(fp[:]),
		"createTime":      cert.NotBefore.UTC().Format(time.RFC3339Nano),
		"expirationTime":  cert.NotAfter.UTC().Format(time.RFC3339Nano),
		"selfLink":        "",
	}
}

// connectSettings is connect.get.
func (s *Server) connectSettings(project string, e Engine) (any, error) {
	if err := s.connectorReady(); err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(s.conn.addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ca := s.conn.ca(e.Instance())
	if ca == nil {
		return nil, Errorf(http.StatusServiceUnavailable, "instance %s has no server CA yet", e.Instance())
	}
	return map[string]any{
		"kind":            "sql#connectSettings",
		"serverCaCert":    sslCertJSON(project, e.Instance(), ca.cert, ca.certPEM),
		"ipAddresses":     []any{map[string]any{"type": "PRIMARY", "ipAddress": host}},
		"region":          Region,
		"databaseVersion": e.DatabaseVersion(),
		"backendType":     "SECOND_GEN",
		"pscEnabled":      false,
		"dnsName":         dnsName(project, e.Instance()) + ".",
		"serverCaMode":    "GOOGLE_MANAGED_INTERNAL_CA",
	}, nil
}

// generateEphemeralCert is connect.generateEphemeralCert: it signs the
// request's public key with the instance's CA, for one hour.
// iamWhy is why IAM database authentication is not served: it signs a
// database user in as a Google identity, and CloudBurrow has none (ADR-0004).
const iamWhy = "IAM database authentication signs in as a Google identity, and CloudBurrow has no identities: " +
	"every local caller is trusted (docs/adr/0004-local-access-and-no-authentication.md)"

func (s *Server) generateEphemeralCert(r *http.Request, project string, e Engine) (any, error) {
	if err := s.connectorReady(); err != nil {
		return nil, err
	}
	var req struct {
		PublicKey     string `json:"public_key"`
		PublicKeyAlt  string `json:"publicKey"`
		AccessToken   string `json:"access_token"`
		AccessToken2  string `json:"accessToken"`
		ValidDuration string `json:"validDuration"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.AccessToken != "" || req.AccessToken2 != "" {
		return nil, unimplemented("IAM database authentication (an access_token in generateEphemeralCert)",
			iamWhy+"; connect as a built-in user without WithIAMAuthN (docs/cloudsql.md)")
	}
	pk := req.PublicKey
	if pk == "" {
		pk = req.PublicKeyAlt
	}
	b, _ := pem.Decode([]byte(pk))
	if b == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: public_key is not a PEM public key.")
	}
	var pub any
	var err error
	if pub, err = x509.ParsePKIXPublicKey(b.Bytes); err != nil {
		if pub, err = x509.ParsePKCS1PublicKey(b.Bytes); err != nil {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: public_key cannot be parsed: %v", err)
		}
	}
	life := ephemeralLifetime
	if d, err := time.ParseDuration(req.ValidDuration); err == nil && d > 0 && d < life {
		life = d
	}
	ca := s.conn.ca(e.Instance())
	if ca == nil {
		return nil, Errorf(http.StatusServiceUnavailable, "instance %s has no server CA yet", e.Instance())
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: project + ":" + e.Instance()},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(life),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: cannot sign public_key: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return map[string]any{"ephemeralCert": sslCertJSON(project, e.Instance(), cert, certPEM)}, nil
}
