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
	"syscall"
	"time"
)

// ConnectorPort is the port the connectors dial. It is a constant in every
// connector (serverProxyPort in cloud.google.com/go/cloudsqlconn), not a
// setting, so it cannot be moved by --port-base. What a second instance
// changes is the address (--cloudsql-connector, #1364): connect.get reports
// the listener's host, and the connector dials it at this port.
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
	// owner is the CloudBurrow instance serving it, recorded beside the
	// listener so a second instance that cannot bind the address can say
	// which one holds it (#1364). Empty records nothing.
	owner  string
	record string               // the holder record this listener wrote, "" for none
	cas    map[string]*serverCA // by instance

	ln net.Listener
	// unavailable says why the listener is not serving, empty when it is.
	unavailable string

	mu    sync.Mutex
	certs map[string]*tls.Certificate // server certificates, by DNS name

	// dir holds each instance's CA file; camu guards cas, which instances
	// created and deleted through the API change (#1330).
	dir  string
	camu sync.RWMutex
	// Pods (#1346) have a listener of their own, podLn, on podAddr: a Cloud
	// SQL Auth Proxy in a pod always dials port 3307, which the cluster
	// maps to it, so it serves even while another instance on this
	// machine holds 3307. podHost is the address pods dial, or nil.
	podAddr string
	podLn   net.Listener
	podHost func() string
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
	ca, err := loadInstanceCA(c.dir, instance)
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
	_ = os.Remove(caPath(c.dir, instance) + upcomingSuffix)
	_ = os.Remove(caPath(c.dir, instance) + previousSuffix)
	_ = os.Remove((&sslCertStore{dir: c.dir}).path(instance))
	c.mu.Lock()
	for name := range c.certs {
		if _, in, ok := parseDNSName(name); ok && in == instance {
			delete(c.certs, name)
		}
	}
	c.mu.Unlock()
}

// ConnectorAddr is the address of the listener pods are published to, or ""
// when it is not serving.
func (s *Server) ConnectorAddr() string {
	if s.conn == nil || s.conn.podLn == nil {
		return ""
	}
	return s.conn.podLn.Addr().String()
}

// ServeConnectorToPods publishes the connector to pods (#1346), before
// Start: a second listener on addr (host:0 for any port), and host, the
// address pods dial port 3307 at, which the cluster forwards to it. A
// connectSettings request that came through the cluster name pods use
// (hostguard.ClusterHostService) is answered with host, so the proxy dials
// that rather than its own loopback. TLS is unchanged: the server name is
// still the instance's DNS name.
func (s *Server) ServeConnectorToPods(addr string, host func() string) {
	if s.conn != nil {
		s.conn.podAddr, s.conn.podHost = addr, host
	}
}

// fromPod reports whether r came through the cluster name pods use.
func fromPod(r *http.Request) bool {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return h == "cloudburrow-host" || strings.HasPrefix(h, "cloudburrow-host.")
}

// EnableConnector serves the connector path: connectSettings and
// generateEphemeralCert, and the TLS listener at addr (host:3307) they
// point at. Each instance's CA is kept in dir.
func (s *Server) EnableConnector(addr, dir string) error {
	c := &connector{addr: addr, dir: dir, cas: map[string]*serverCA{}, certs: map[string]*tls.Certificate{}}
	for _, e := range s.engines {
		ca, err := loadInstanceCA(dir, e.Instance())
		if err != nil {
			err = fmt.Errorf("sqladmin: server CA for %s: %w", e.Instance(), err)
			s.conn = &connector{unavailable: err.Error()}
			return err
		}
		c.cas[e.Instance()] = ca
	}
	s.conn = c
	s.certs = &sslCertStore{dir: dir}
	return nil
}

// SetConnectorOwner names the CloudBurrow instance serving the connector
// listener, so another instance that finds the address taken can say which
// one holds it. Call it after EnableConnector and before Start.
func (s *Server) SetConnectorOwner(instance string) {
	if s.conn != nil {
		s.conn.owner = instance
	}
}

// holder is what a listener records about itself, so the next instance to
// try its address can name it.
type holder struct {
	Instance string `json:"instance"`
	PID      int    `json:"pid"`
}

// holderPath is the record for addr, in the user's temporary directory,
// which every instance this user runs shares whatever its --state-dir.
func holderPath(addr string) string {
	r := strings.NewReplacer(":", "_", "[", "", "]", "", "/", "_")
	return filepath.Join(os.TempDir(), "cloudburrow-cloudsql-connector-"+r.Replace(addr)+".json")
}

// heldBy says which CloudBurrow instance holds addr, from its record, or
// "" when no running instance recorded it.
func heldBy(addr string) string {
	b, err := os.ReadFile(holderPath(addr))
	if err != nil {
		return ""
	}
	var h holder
	if json.Unmarshal(b, &h) != nil || h.Instance == "" || !processAlive(h.PID) {
		return ""
	}
	return fmt.Sprintf("CloudBurrow instance %q (pid %d)", h.Instance, h.PID)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// bindError is why the listener could not bind, naming the instance that
// holds the address when one recorded it, and the setting that moves it.
func bindError(addr string, err error) string {
	who := heldBy(addr)
	if who == "" {
		who = "another process (no running CloudBurrow instance recorded it)"
	}
	return fmt.Sprintf("the connector listener could not bind %s (%v): %s holds it. "+
		"The Cloud SQL connectors dial port %d at the address connect.get reports, so start this "+
		"instance with another --cloudsql-connector: another loopback address on Linux (127.0.0.2:%d), "+
		"or on macOS another port, which the application's dialer must redirect to (docs/cloudsql.md)",
		addr, err, who, ConnectorPort, ConnectorPort)
}

// start binds the listener. A port already in use is not fatal: the rest of
// the Admin API still serves, and the connector methods say why they cannot.
func (c *connector) start(s *Server) {
	if len(c.cas) == 0 {
		return
	}
	if c.podAddr != "" {
		if ln, err := net.Listen("tcp", c.podAddr); err == nil {
			c.podLn = ln
			go c.accept(s, ln)
		}
	}
	if c.unavailable != "" {
		return
	}
	ln, err := net.Listen("tcp", c.addr)
	if err != nil {
		c.unavailable = bindError(c.addr, err)
		return
	}
	c.ln = ln
	if c.owner != "" {
		b, _ := json.Marshal(holder{Instance: c.owner, PID: os.Getpid()})
		if os.WriteFile(holderPath(ln.Addr().String()), b, 0o600) == nil {
			c.record = holderPath(ln.Addr().String())
		}
	}
	go c.accept(s, ln)
}

func (c *connector) accept(s *Server, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go c.serve(s, conn)
	}
}

func (c *connector) stop() {
	if c.ln != nil {
		_ = c.ln.Close()
	}
	if c.podLn != nil {
		_ = c.podLn.Close()
	}
	if c.record != "" {
		_ = os.Remove(c.record)
		c.record = ""
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

func (s *Server) connectorReady(r *http.Request) error {
	if s.conn == nil {
		return unimplemented("the connector path", "this server was started without its connector listener")
	}
	if s.conn.podLn != nil && s.conn.podHost != nil && s.conn.podHost() != "" && fromPod(r) {
		return nil
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
func (s *Server) connectSettings(r *http.Request, project string, e Engine) (any, error) {
	if err := s.connectorReady(r); err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(s.conn.addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if s.conn.podLn != nil && s.conn.podHost != nil && fromPod(r) {
		if h := s.conn.podHost(); h != "" {
			host = h
		}
	}
	ca := s.conn.ca(e.Instance())
	if ca == nil {
		return nil, Errorf(http.StatusServiceUnavailable, "instance %s has no server CA yet", e.Instance())
	}
	return map[string]any{
		"kind":            "sql#connectSettings",
		"serverCaCert":    sslCertJSON(project, e.Instance(), ca.cert, ca.certPEM),
		"ipAddresses":     []any{map[string]any{"type": "PRIMARY", "ipAddress": host}},
		"region":          s.regionOf(e.Instance()),
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
	if err := s.connectorReady(r); err != nil {
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

// Instances are the instance IDs served, in every project.
func (s *Server) Instances() []string {
	out := make([]string, 0, len(s.engines))
	for _, e := range s.engines {
		out = append(out, e.Instance())
	}
	return out
}

// regionOf is the region instance reports: a created instance's own
// (#1381), or Region.
func (s *Server) regionOf(instance string) string {
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes != nil {
		if c := s.writes.byName[instance]; c != nil {
			return c.spec.region()
		}
	}
	return Region
}
