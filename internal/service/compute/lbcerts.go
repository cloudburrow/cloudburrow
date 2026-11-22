package compute

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SSL certificates and SSL policies (#2153), as
// https://docs.cloud.google.com/load-balancing/docs/ssl-certificates,
// .../ssl-certificates/google-managed-certs, .../ssl-policies-concepts and
// the sslCertificates and sslPolicies REST references describe them (read
// 2026-11-22).
//
// A self-managed certificate is the PEM certificate chain and private key
// given, checked as a key pair (RSA of 2048 bits or more, or ECDSA P-256
// or P-384); its private key is never returned. A Google-managed
// certificate (managed.domains) is PROVISIONING until a target HTTPS or
// SSL proxy that a forwarding rule uses serves it, as Google provisions it
// only then; it is then minted on this machine, by CloudBurrow's own load
// balancing CA (GET /cloudburrow/lb/ca.pem on the Compute port), since no
// public CA can issue one for a local address, and is ACTIVE with each
// domain ACTIVE. An SSL policy's profile and minimum TLS version are what
// the TLS listeners (lbplane.go) offer.

func init() {
	registerLB(&lbColl{coll: "sslCertificates", kind: "compute#sslCertificate", global: true, regional: true,
		build: buildSslCertificate, decorate: decorateCert, noun: "sslCertificate"})
	registerLB(&lbColl{coll: "sslPolicies", kind: "compute#sslPolicy", global: true, regional: true, patch: true,
		fingerprint: true, testIam: true, build: buildSslPolicy, noun: "sslPolicy",
		collGet: map[string]func(*Server, lbCtx, call) (obj, error){"listAvailableFeatures": func(*Server, lbCtx, call) (obj, error) {
			return obj{"features": toAny(sslFeatures)}, nil
		}}})
}

var domainRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z][-a-z0-9]{0,61}[a-z0-9]\.?$`)

func buildSslCertificate(s *Server, x lbCtx, b, old obj) (obj, error) {
	typ := str(b, "type")
	mg := objOf(b["managed"])
	sm := objOf(b["selfManaged"])
	cert, key := str(b, "certificate"), str(b, "privateKey")
	if sm != nil {
		cert, key = str(sm, "certificate"), str(sm, "privateKey")
	}
	if typ == "" {
		typ = "SELF_MANAGED"
		if mg != nil {
			typ = "MANAGED"
		}
	}
	switch typ {
	case "MANAGED":
		if x.region != "" {
			return nil, invalid("Invalid value for field 'resource.type': 'MANAGED'. Google-managed SSL certificates are global only; use a self-managed regional certificate.")
		}
		ds := strs(mg, "domains")
		if len(ds) == 0 {
			return nil, required("resource.managed.domains")
		}
		if len(ds) > 100 {
			return nil, invalid("Invalid value for field 'resource.managed.domains': ''. A Google-managed certificate can have at most 100 domains.")
		}
		for i, d := range ds {
			if strings.Contains(d, "*") {
				return nil, invalid("Invalid value for field 'resource.managed.domains[%d]': '%s'. Google-managed certificates do not support wildcard domains.", i, d)
			}
			if !domainRE.MatchString(strings.ToLower(d)) || len(d) > 253 {
				return nil, invalid("Invalid value for field 'resource.managed.domains[%d]': '%s'. Must be a fully qualified domain name.", i, d)
			}
		}
		if cert != "" || key != "" {
			return nil, invalid("Invalid value for field 'resource.certificate': ''. A Google-managed certificate takes no certificate or private key.")
		}
		return obj{"type": "MANAGED", "managed": obj{"domains": toAny(ds)}, "subjectAlternativeNames": toAny(ds)}, nil
	case "SELF_MANAGED":
	default:
		return nil, invalid("Invalid value for field 'resource.type': '%s'. Must be one of [MANAGED, SELF_MANAGED]", typ)
	}
	if cert == "" {
		return nil, required("resource.certificate")
	}
	if key == "" {
		return nil, required("resource.privateKey")
	}
	pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
	if err != nil {
		return nil, invalid("Invalid value for field 'resource.certificate': ''. The SSL certificate and key do not match, or are not valid PEM: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, invalid("Invalid value for field 'resource.certificate': ''. The certificate could not be parsed: %v", err)
	}
	switch k := leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, invalid("Invalid value for field 'resource.privateKey': ''. An RSA key must be at least 2048 bits.")
		}
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, invalid("Invalid value for field 'resource.privateKey': ''. An ECDSA key must be P-256 or P-384.")
		}
	default:
		return nil, invalid("Invalid value for field 'resource.privateKey': ''. The key must be RSA or ECDSA.")
	}
	sans := append([]string(nil), leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	if len(sans) == 0 && leaf.Subject.CommonName != "" {
		sans = []string{leaf.Subject.CommonName}
	}
	return obj{"type": "SELF_MANAGED", "certificate": cert, "selfManaged": obj{"certificate": cert},
		"subjectAlternativeNames": toAny(sans), "expireTime": leaf.NotAfter.UTC().Format(time.RFC3339), "_privateKey": key}, nil
}

// decorateCert gives a Google-managed certificate its status.
func decorateCert(s *Server, o obj) obj {
	if str(o, "type") != "MANAGED" {
		return o
	}
	m := objOf(o["managed"])
	status, ds := "PROVISIONING", obj{}
	if str(o, "_certPEM") != "" {
		status = "ACTIVE"
		o["certificate"] = str(o, "_certPEM")
		o["expireTime"] = str(o, "_expireTime")
	}
	for _, d := range strs(m, "domains") {
		ds[d] = status
	}
	m["status"], m["domainStatus"] = status, ds
	return o
}

// lbCAKey is where the load balancing CA is kept in the state.
const lbCAKey = "cloudburrow/lbCA"

// lbCALocked is the load balancing CA, made on first use.
func (s *Server) lbCALocked() (*x509.Certificate, *ecdsa.PrivateKey, string, error) {
	if ca, ok := s.res[lbCAKey]; ok {
		cb, _ := pem.Decode([]byte(str(ca, "cert")))
		kb, _ := pem.Decode([]byte(str(ca, "key")))
		if cb != nil && kb != nil {
			c, err1 := x509.ParseCertificate(cb.Bytes)
			k, err2 := x509.ParseECPrivateKey(kb.Bytes)
			if err1 == nil && err2 == nil {
				return c, k, str(ca, "cert"), nil
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	tpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "CloudBurrow load balancing CA", Organization: []string{"CloudBurrow"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		return nil, nil, "", err
	}
	c, _ := x509.ParseCertificate(der)
	kd, _ := x509.MarshalECPrivateKey(k)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	s.res[lbCAKey] = obj{"cert": certPEM, "key": string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))}
	return c, k, certPEM, nil
}

// LBCACertificate is the load balancing CA's certificate (PEM), which a
// client trusts to verify a Google-managed certificate minted here.
func (s *Server) LBCACertificate() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, p, err := s.lbCALocked()
	if err == nil {
		err = s.saveLocked()
	}
	return p, err
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

// mintManagedLocked issues a Google-managed certificate's certificate and
// key, valid for 90 days as Google's are.
func (s *Server) mintManagedLocked(o obj) error {
	ca, caKey, caPEM, err := s.lbCALocked()
	if err != nil {
		return err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	ds := strs(objOf(o["managed"]), "domains")
	now := s.opts.Now()
	tpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: ds[0]}, DNSNames: ds,
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, 90), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
	if err != nil {
		return err
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	o["_certPEM"] = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) + caPEM
	o["_privateKey"] = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
	o["_expireTime"] = tpl.NotAfter.UTC().Format(time.RFC3339)
	return nil
}

// certPair is a certificate record's key pair, ok false while a managed
// one is provisioning.
func certPair(o obj) (tls.Certificate, bool) {
	c := str(o, "certificate")
	if str(o, "type") == "MANAGED" {
		c = str(o, "_certPEM")
	}
	if c == "" {
		return tls.Certificate{}, false
	}
	p, err := tls.X509KeyPair([]byte(c), []byte(str(o, "_privateKey")))
	return p, err == nil
}

// sslFeatures are the features an SSL policy can enable
// (ssl-policies-concepts, "Cipher suites").
var sslFeatures = []string{
	"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384", "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
	"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384", "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
	"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA", "TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA", "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA",
	"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA", "TLS_RSA_WITH_AES_128_GCM_SHA256", "TLS_RSA_WITH_AES_256_GCM_SHA384",
	"TLS_RSA_WITH_AES_128_CBC_SHA", "TLS_RSA_WITH_AES_256_CBC_SHA", "TLS_RSA_WITH_3DES_EDE_CBC_SHA",
}

var sslSuiteIDs = map[string]uint16{
	"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256":       tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384":       tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256":         tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384":         tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA":          tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA":          tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA":            tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA":            tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	"TLS_RSA_WITH_AES_128_GCM_SHA256":               tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	"TLS_RSA_WITH_AES_256_GCM_SHA384":               tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	"TLS_RSA_WITH_AES_128_CBC_SHA":                  tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	"TLS_RSA_WITH_AES_256_CBC_SHA":                  tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	"TLS_RSA_WITH_3DES_EDE_CBC_SHA":                 tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
}

// profileFeatures are each pre-configured profile's features.
func profileFeatures(profile string) []string {
	var out []string
	for _, f := range sslFeatures {
		switch profile {
		case "COMPATIBLE":
			out = append(out, f)
		case "MODERN":
			if strings.HasPrefix(f, "TLS_ECDHE_") {
				out = append(out, f)
			}
		case "RESTRICTED":
			if strings.HasPrefix(f, "TLS_ECDHE_") && !strings.HasSuffix(f, "_CBC_SHA") {
				out = append(out, f)
			}
		}
	}
	return out
}

var tlsVersions = map[string]uint16{"TLS_1_0": tls.VersionTLS10, "TLS_1_1": tls.VersionTLS11, "TLS_1_2": tls.VersionTLS12, "TLS_1_3": tls.VersionTLS13}

func buildSslPolicy(s *Server, x lbCtx, b, old obj) (obj, error) {
	profile, err := oneOf("resource.profile", str(b, "profile"), "COMPATIBLE", "COMPATIBLE", "MODERN", "RESTRICTED", "CUSTOM", "FIPS_202205")
	if err != nil {
		return nil, err
	}
	minV, err := oneOf("resource.minTlsVersion", str(b, "minTlsVersion"), "TLS_1_0", "TLS_1_0", "TLS_1_1", "TLS_1_2", "TLS_1_3")
	if err != nil {
		return nil, err
	}
	o := obj{"profile": profile, "minTlsVersion": minV}
	var features []string
	switch profile {
	case "CUSTOM":
		cf := strs(b, "customFeatures")
		if len(cf) == 0 {
			return nil, invalid("Invalid value for field 'resource.customFeatures': ''. A CUSTOM profile needs custom features.")
		}
		for i, f := range cf {
			if _, ok := sslSuiteIDs[f]; !ok {
				return nil, invalid("Invalid value for field 'resource.customFeatures[%d]': '%s'. Not an available SSL feature.", i, f)
			}
		}
		sort.Strings(cf)
		o["customFeatures"] = toAny(cf)
		features = cf
	case "FIPS_202205":
		features = []string{"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"}
	default:
		if len(strs(b, "customFeatures")) > 0 {
			return nil, invalid("Invalid value for field 'resource.customFeatures': ''. Custom features are set only with the CUSTOM profile.")
		}
		features = profileFeatures(profile)
	}
	o["enabledFeatures"] = toAny(features)
	return o, nil
}

// tlsConfigOf is the server TLS configuration of an SSL policy (nil: the
// default, COMPATIBLE with TLS 1.0).
func tlsConfigOf(policy obj) (minVersion uint16, suites []uint16) {
	minVersion = tls.VersionTLS10
	features := sslFeatures
	if policy != nil {
		if v, ok := tlsVersions[str(policy, "minTlsVersion")]; ok {
			minVersion = v
		}
		features = strs(policy, "enabledFeatures")
	}
	for _, f := range features {
		if id, ok := sslSuiteIDs[f]; ok {
			suites = append(suites, id)
		}
	}
	return minVersion, suites
}

// LBCAPath is where the certificate authority that issues Google-managed
// certificates here is served, as PEM, for clients to trust.
const LBCAPath = "/cloudburrow/lb/ca.pem"

func (s *Server) serveLBCA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p, err := s.LBCACertificate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write([]byte(p))
}
