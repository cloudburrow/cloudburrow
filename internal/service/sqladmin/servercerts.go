package sqladmin

// GOOGLE_MANAGED_CAS_CA instances and their server certificates (#1614).
//
// Cloud SQL's shared CA mode (settings.ipConfiguration.serverCaMode
// GOOGLE_MANAGED_CAS_CA) signs every instance's server certificate with a
// Google-managed subordinate CA of the instance's region, under a root CA,
// instead of with a CA of the instance's own. connect.get answers that CA
// chain as serverCaCert and the instance's DNS name in dnsNames, and the
// connectors check the server certificate's SAN against it. The server
// certificate itself is rotated with addServerCertificate (an upcoming
// version), rotateServerCertificate (to the upcoming version, or back to
// the previous one) and listServerCertificates, as gcloud sql ssl
// server-certs create, rotate, rollback and list do.
//
// Here CloudBurrow plays the Google-managed CAs: each region's root and
// subordinate CA are generated locally on first use and kept in the
// connector's directory, like the instances' internal CAs, and never leave
// the host. CUSTOMER_MANAGED_CAS_CA, whose CA pool is the customer's in
// Certificate Authority Service, stays 501 (checkIPConfiguration).
//
// The client side is unchanged: ephemeral client certificates are still
// signed by the instance's own CA, which the listener on 3307 trusts.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // Cloud SQL names certificates by SHA-1 fingerprint
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	internalCA = "GOOGLE_MANAGED_INTERNAL_CA"
	casCA      = "GOOGLE_MANAGED_CAS_CA"
)

// serverCertLifetime is how long a CAS server certificate is valid.
const serverCertLifetime = 365 * 24 * time.Hour

// serverCaMode is the instance's settings.ipConfiguration.serverCaMode:
// a created instance's, or GOOGLE_MANAGED_INTERNAL_CA.
func (s *Server) serverCaMode(instance string) string {
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes != nil {
		if c := s.writes.byName[instance]; c != nil {
			ic, _ := c.spec.Settings["ipConfiguration"].(map[string]any)
			if m, _ := ic["serverCaMode"].(string); m == casCA {
				return casCA
			}
		}
	}
	return internalCA
}

// casChain is a region's Google-managed CA hierarchy: the subordinate CA
// that signs server certificates, under its root.
type casChain struct {
	root, sub *serverCA
	pem       string // the subordinate's certificate, then the root's
}

func casPath(dir, region, which string) string {
	return filepath.Join(dir, "cloudsql-cas-"+region+"-"+which+"-ca.json")
}

// casFor is the region's CA hierarchy, made on first use.
func (c *connector) casFor(region string) (*casChain, error) {
	c.camu.Lock()
	defer c.camu.Unlock()
	root, err := loadOrCreateCA(casPath(c.dir, region, "root"), "")
	if err != nil {
		return nil, err
	}
	sp := casPath(c.dir, region, "server")
	if _, err := os.Stat(sp); errors.Is(err, os.ErrNotExist) {
		if err := createSubCA(sp, region, root); err != nil {
			return nil, err
		}
	}
	sub := readCA(sp)
	if sub == nil {
		return nil, fmt.Errorf("%s is unreadable", sp)
	}
	return &casChain{root: root, sub: sub, pem: sub.certPEM + root.certPEM}, nil
}

// createSubCA writes, at path, a subordinate CA signed by root.
func createSubCA(path, region string, root *serverCA) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject: pkix.Name{Organization: []string{"CloudBurrow"},
			CommonName: "CloudBurrow Cloud SQL Server CA " + region},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		MaxPathLenZero:        true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, root.cert, &key.PublicKey, root.key)
	if err != nil {
		return err
	}
	return writeCertFile(path, der, key)
}

func writeCertFile(path string, der []byte, key *ecdsa.PrivateKey) error {
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	f := caFile{
		Cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		Key:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	return os.WriteFile(path, b, 0o600)
}

// serverCertVersion is one version of an instance's server certificate.
type serverCertVersion struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
	// Used is set once the version has been the active one: addServer
	// Certificate replaces an added version only while it never was.
	Used bool `json:"used,omitempty"`
	// Created is when it was issued, to the nanosecond: gcloud tells the
	// upcoming version from the previous one by createTime, and the
	// certificate's own NotBefore has whole seconds only.
	Created string `json:"created"`
}

type serverCertFile struct {
	Active   string              `json:"active"` // sha1 fingerprint
	Versions []serverCertVersion `json:"versions"`
}

func serverCertsPath(dir, instance string) string {
	return filepath.Join(dir, "cloudsql-"+instance+"-server-certs.json")
}

func certFingerprint(certPEM string) (string, *x509.Certificate) {
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil {
		return "", nil
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return "", nil
	}
	fp := sha1.Sum(cert.Raw) //nolint:gosec
	return hex.EncodeToString(fp[:]), cert
}

// issueServerCert signs a new server certificate for project's instance
// with the region's subordinate CA, its DNS name as the SAN.
func issueServerCert(chain *casChain, project, instance string) (serverCertVersion, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return serverCertVersion{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"CloudBurrow"}, CommonName: project + ":" + instance},
		DNSNames:     []string{dnsName(project, instance)},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(serverCertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, chain.sub.cert, &key.PublicKey, chain.sub.key)
	if err != nil {
		return serverCertVersion{}, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return serverCertVersion{}, err
	}
	return serverCertVersion{
		Cert:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		Key:     string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})),
		Created: now.UTC().Format(time.RFC3339Nano),
	}, nil
}

// serverCerts reads the instance's server certificate versions, issuing
// the first, active one when it has none. The caller holds c.camu.
func (c *connector) serverCertsLocked(chain *casChain, project, instance string) (*serverCertFile, error) {
	path := serverCertsPath(c.dir, instance)
	var f serverCertFile
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("%s is unreadable: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(f.Versions) == 0 {
		v, err := issueServerCert(chain, project, instance)
		if err != nil {
			return nil, err
		}
		v.Used = true
		f.Versions = []serverCertVersion{v}
		f.Active, _ = certFingerprint(v.Cert)
		if err := c.writeServerCertsLocked(instance, &f); err != nil {
			return nil, err
		}
	}
	return &f, nil
}

func (c *connector) writeServerCertsLocked(instance string, f *serverCertFile) error {
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(serverCertsPath(c.dir, instance), b, 0o600)
}

// casServerCert is the certificate the listener presents for a CAS
// instance: its active server certificate, with the subordinate CA.
func (c *connector) casServerCert(region, project, instance string) (*tls.Certificate, error) {
	chain, err := c.casFor(region)
	if err != nil {
		return nil, err
	}
	c.camu.Lock()
	f, err := c.serverCertsLocked(chain, project, instance)
	c.camu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, v := range f.Versions {
		if fp, _ := certFingerprint(v.Cert); fp == f.Active {
			crt, err := tls.X509KeyPair([]byte(v.Cert+chain.sub.certPEM), []byte(v.Key))
			if err != nil {
				return nil, err
			}
			return &crt, nil
		}
	}
	return nil, fmt.Errorf("instance %s has no active server certificate", instance)
}

// casReady is the CA hierarchy of a CAS instance, or why the server
// certificate methods do not apply to it.
func (s *Server) casReady(e Engine, method string) (*casChain, error) {
	if s.conn == nil || s.conn.cas == nil {
		return nil, unimplemented("server certificates", "this server was started without its connector, which holds the server certificates")
	}
	if m := s.serverCaMode(e.Instance()); m != casCA {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s applies to an instance whose serverCaMode is %s; "+
			"instance %s's is %s, whose server CA is managed with addServerCa, rotateServerCa and listServerCas.",
			method, casCA, e.Instance(), m)
	}
	return s.conn.casFor(s.regionOf(e.Instance()))
}

// listServerCertificates is instances.ListServerCertificates.
func (s *Server) listServerCertificates(project string, e Engine) (any, error) {
	chain, err := s.casReady(e, "listServerCertificates")
	if err != nil {
		return nil, err
	}
	inst := e.Instance()
	s.conn.camu.Lock()
	f, err := s.conn.serverCertsLocked(chain, project, inst)
	s.conn.camu.Unlock()
	if err != nil {
		return nil, err
	}
	// caCerts is index for index the CA that signed each server certificate
	// (gcloud reads caCerts[i] beside serverCerts[i]): here always the
	// region's subordinate CA.
	certs, cas := []any{}, []any{}
	for _, v := range f.Versions {
		if _, cert := certFingerprint(v.Cert); cert != nil {
			j := sslCertJSON(project, inst, cert, v.Cert)
			if v.Created != "" {
				j["createTime"] = v.Created
			}
			certs = append(certs, j)
			cas = append(cas, casCertJSON(project, inst, chain))
		}
	}
	return map[string]any{
		"kind":          "sql#instancesListServerCertificates",
		"caCerts":       cas,
		"serverCerts":   certs,
		"activeVersion": f.Active,
	}, nil
}

// casCertJSON is the CA chain as an SslCert: the subordinate CA's, whose
// cert holds the subordinate's PEM then the root's.
func casCertJSON(project, instance string, chain *casChain) map[string]any {
	return sslCertJSON(project, instance, chain.sub.cert, chain.pem)
}

// addServerCertificate is instances.addServerCertificate: a new upcoming
// version, replacing one added before and never rotated in. At most three
// versions are kept: the active one, the upcoming one and the previous one.
func (s *Server) addServerCertificate(base, project string, e Engine) (any, error) {
	chain, err := s.casReady(e, "addServerCertificate")
	if err != nil {
		return nil, err
	}
	inst := e.Instance()
	s.conn.camu.Lock()
	defer s.conn.camu.Unlock()
	f, err := s.conn.serverCertsLocked(chain, project, inst)
	if err != nil {
		return nil, err
	}
	v, err := issueServerCert(chain, project, inst)
	if err != nil {
		return nil, err
	}
	var active *serverCertVersion
	var previous []serverCertVersion
	for i := range f.Versions {
		x := f.Versions[i]
		switch fp, _ := certFingerprint(x.Cert); {
		case fp == f.Active:
			active = &x
		case x.Used:
			previous = append(previous, x)
		}
	}
	keep := []serverCertVersion{}
	if len(previous) > 0 {
		keep = append(keep, previous[len(previous)-1])
	}
	if active != nil {
		keep = append(keep, *active)
	}
	f.Versions = append(keep, v)
	if err := s.conn.writeServerCertsLocked(inst, f); err != nil {
		return nil, err
	}
	return s.done(base, project, e, "UPDATE", inst), nil
}

// rotateServerCertificate is instances.RotateServerCertificate: the
// version nextVersion names, or the most recently added one, becomes the
// active one; the listener presents it from the next connection on. The
// version rotated out stays listed, so gcloud's rollback can rotate back.
func (s *Server) rotateServerCertificate(r *http.Request, base, project string, e Engine) (any, error) {
	var req struct {
		RotateServerCertificateContext *struct {
			NextVersion string `json:"nextVersion"`
		} `json:"rotateServerCertificateContext"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	chain, err := s.casReady(e, "rotateServerCertificate")
	if err != nil {
		return nil, err
	}
	inst := e.Instance()
	s.conn.camu.Lock()
	defer s.conn.camu.Unlock()
	f, err := s.conn.serverCertsLocked(chain, project, inst)
	if err != nil {
		return nil, err
	}
	next := ""
	if c := req.RotateServerCertificateContext; c != nil {
		next = c.NextVersion
	}
	activeCreated := ""
	for _, v := range f.Versions {
		if fp, _ := certFingerprint(v.Cert); fp == f.Active {
			activeCreated = v.Created
		}
	}
	// Unnamed, the target is the most recently added version, if it is
	// newer than the active one (the upcoming version gcloud lists).
	target := -1
	for i, v := range f.Versions {
		fp, _ := certFingerprint(v.Cert)
		switch {
		case fp == f.Active:
		case next != "" && next == fp:
			target = i
		case next == "" && v.Created > activeCreated && (target < 0 || v.Created > f.Versions[target].Created):
			target = i
		}
	}
	if target < 0 {
		if next != "" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: nextVersion %s is not a server certificate version of instance %s "+
				"other than the active one.", next, inst)
		}
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance %s has no upcoming server certificate; "+
			"add one with addServerCertificate first.", inst)
	}
	f.Versions[target].Used = true
	f.Active, _ = certFingerprint(f.Versions[target].Cert)
	if err := s.conn.writeServerCertsLocked(inst, f); err != nil {
		return nil, err
	}
	return s.done(base, project, e, "UPDATE", inst), nil
}

// casConnectSettings adds a CAS instance's CA chain and DNS names to its
// connect settings.
func (s *Server) casConnectSettings(out map[string]any, project string, e Engine) error {
	if s.serverCaMode(e.Instance()) != casCA {
		return nil
	}
	chain, err := s.conn.casFor(s.regionOf(e.Instance()))
	if err != nil {
		return err
	}
	out["serverCaMode"] = casCA
	out["serverCaCert"] = casCertJSON(project, e.Instance(), chain)
	out["dnsNames"] = dnsNamesJSON(project, e.Instance())
	return nil
}

func dnsNamesJSON(project, instance string) []any {
	return []any{map[string]any{
		"name": dnsName(project, instance) + ".", "connectionType": "PUBLIC",
		"dnsScope": "INSTANCE", "recordManager": "CLOUD_SQL_AUTOMATION",
	}}
}

// casInstanceJSON adds a CAS instance's DNS names to its resource.
func (s *Server) casInstanceJSON(in map[string]any, spec Spec) {
	if s.serverCaMode(spec.Name) != casCA {
		return
	}
	in["dnsName"] = dnsName(spec.Project, spec.Name) + "."
	in["dnsNames"] = dnsNamesJSON(spec.Project, spec.Name)
}

// resolveConnectSettings is connect.resolve: the connect settings of the
// instance a DNS name connectSettings answered names, in location.
func (s *Server) resolveConnectSettings(r *http.Request, location, name string) (any, error) {
	project, instance, ok := parseDNSName(name)
	notFound := Errorf(http.StatusNotFound, "The Cloud SQL instance does not exist: no instance has the DNS name %s in %s.", name, location)
	if !ok {
		return nil, notFound
	}
	e := s.engine(instance)
	if e == nil {
		return nil, notFound
	}
	s.emu.RLock()
	other := s.writes != nil && s.writes.byName[instance] != nil && s.writes.byName[instance].spec.Project != project
	s.emu.RUnlock()
	if other || s.regionOf(instance) != location {
		return nil, notFound
	}
	return s.connectSettings(r, project, e)
}
