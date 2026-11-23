package compute

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Backend authenticated TLS and backend mTLS (#2160), as
// https://docs.cloud.google.com/load-balancing/docs/backend-authenticated-tls-backend-mtls
// and .../backend-mtls-setup describe them (read 2026-11-22):
//
//   - a backend service of protocol HTTPS or HTTP2 on a global or
//     regional external, or an internal, Application Load Balancer (not
//     the classic one) has tlsSettings: sni, the hostname the load
//     balancer sends in its ClientHello; subjectAltNames, at most 5
//     accepted SANs; and authenticationConfig, a Network Security backend
//     authentication config (//networksecurity.googleapis.com/projects/P/
//     locations/L/backendAuthenticationConfigs/N, in the backend
//     service's location);
//   - without authenticationConfig the load balancer accepts any
//     certificate; with it, the backend's chain must lead to the trust
//     config's anchors (through its intermediates or the chain's), or to
//     the public roots with wellKnownRoots PUBLIC_ROOTS and sni set, or
//     the leaf be on its allowlist; the leaf must have the serverAuth
//     extended key usage and not be a CA; with subjectAltNames one of
//     them must match a SAN of the leaf, else with sni the sni must;
//   - the chain is at most 10 certificates and 16 KB;
//   - with clientCertificate the load balancer presents that Certificate
//     Manager certificate when the backend asks for one (backend mTLS);
//   - a failed verification closes the connection and the client gets
//     502; health checks do not authenticate backends.

// BackendAuthentication is the Network Security API's backend
// authentication configs, with their trust configs and client
// certificates resolved, as the backends' TLS reads them; the
// NetworkSecurity hook implements it when they are served.
type BackendAuthentication interface {
	// BackendAuthConfig is a config by name
	// (projects/P/locations/L/backendAuthenticationConfigs/N); ok is
	// false when there is none.
	BackendAuthConfig(name string) (BackendAuthConfig, bool)
}

// BackendAuthConfig is a backend authentication config as a backend
// connection applies it.
type BackendAuthConfig struct {
	// TrustConfig is the trust config's name ("" for none); TrustFound is
	// whether it exists.
	TrustConfig string
	TrustFound  bool
	// Roots and Intermediates are its trust store; Allowlisted its
	// always-valid certificates.
	Roots         *x509.CertPool
	Intermediates []*x509.Certificate
	Allowlisted   []*x509.Certificate
	// PublicRoots is wellKnownRoots PUBLIC_ROOTS.
	PublicRoots bool
	// ClientCert is the client certificate, nil for none.
	ClientCert *tls.Certificate
	// Version changes whenever any of the above does.
	Version string
}

const maxTLSSANs = 5

// backendTLSSettings checks a backend service's tlsSettings into o.
func (s *Server) backendTLSSettings(x lbCtx, b, o obj, proto, scheme string) error {
	ts := objOf(b["tlsSettings"])
	if ts == nil {
		return nil
	}
	if proto != "HTTPS" && proto != "HTTP2" {
		return invalid("Invalid value for field 'resource.tlsSettings': ''. tlsSettings applies only to backend services of protocol HTTPS or HTTP2.")
	}
	if scheme != "EXTERNAL_MANAGED" && scheme != "INTERNAL_MANAGED" {
		return invalid("Invalid value for field 'resource.tlsSettings': ''. tlsSettings is supported only by the global and regional external, and the internal, Application Load Balancers (loadBalancingScheme EXTERNAL_MANAGED or INTERNAL_MANAGED).")
	}
	out := obj{}
	if v := str(ts, "sni"); v != "" {
		out["sni"] = v
	}
	sans := listOf(ts["subjectAltNames"])
	if len(sans) > maxTLSSANs {
		return invalid("Invalid value for field 'resource.tlsSettings.subjectAltNames': ''. At most %d subject alternative names are allowed.", maxTLSSANs)
	}
	var outSANs []any
	for i, e := range sans {
		san := objOf(e)
		d, u := str(san, "dnsName"), str(san, "uniformResourceIdentifier")
		if (d == "") == (u == "") {
			return invalid("Invalid value for field 'resource.tlsSettings.subjectAltNames[%d]': ''. Set exactly one of dnsName and uniformResourceIdentifier.", i)
		}
		if d != "" {
			outSANs = append(outSANs, obj{"dnsName": d})
		} else {
			outSANs = append(outSANs, obj{"uniformResourceIdentifier": u})
		}
	}
	if len(outSANs) > 0 {
		out["subjectAltNames"] = outSANs
	}
	if ref := str(ts, "authenticationConfig"); ref != "" {
		v, err := s.nsRef(x, "resource.tlsSettings.authenticationConfig", ref, "backendAuthenticationConfigs")
		if err != nil {
			return err
		}
		out["authenticationConfig"] = v
	}
	o["tlsSettings"] = out
	return nil
}

// backendTLS are the transports of backend services with tlsSettings, by
// their settings and configs' versions.
var backendTLS struct {
	sync.Mutex
	m map[string]http.RoundTripper
}

// backendTLSTransport is the transport to a backend service's endpoints
// under its tlsSettings; ok is false when it has none.
func (s *Server) backendTLSTransport(set lbBackendSet, h2 bool) (http.RoundTripper, bool) {
	ts := objOf(set.bs["tlsSettings"])
	if ts == nil {
		return nil, false
	}
	var cfg *BackendAuthConfig
	ref := strings.TrimPrefix(str(ts, "authenticationConfig"), nsPrefix)
	version := ""
	if ref != "" {
		c := BackendAuthConfig{}
		if ba, ok := s.opts.NetSec.(BackendAuthentication); ok {
			if got, found := ba.BackendAuthConfig(ref); found {
				c = got
			} else {
				c.TrustConfig, c.TrustFound = "missing", false
			}
		} else {
			c.TrustConfig = "missing"
		}
		cfg, version = &c, c.Version
	}
	raw, _ := json.Marshal(ts)
	key := set.rel + "|" + string(raw) + "|" + version
	if h2 {
		key += "|h2"
	}
	backendTLS.Lock()
	defer backendTLS.Unlock()
	if rt, ok := backendTLS.m[key]; ok {
		return rt, true
	}
	if backendTLS.m == nil || len(backendTLS.m) > 256 {
		backendTLS.m = map[string]http.RoundTripper{}
	}
	sni := str(ts, "sni")
	var sans []obj
	for _, e := range listOf(ts["subjectAltNames"]) {
		sans = append(sans, objOf(e))
	}
	now := s.opts.Now
	tc := &tls.Config{ServerName: sni, InsecureSkipVerify: true} //nolint:gosec // verified by VerifyPeerCertificate below, as Google verifies backends
	if cfg != nil {
		tc.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyBackendCert(cfg, sni, sans, rawCerts, now())
		}
		if cfg.ClientCert != nil {
			cc := cfg.ClientCert
			tc.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cc, nil }
		}
	}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: tc, MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	if h2 {
		tc.NextProtos = []string{"h2"}
		tr.ForceAttemptHTTP2 = true
	}
	backendTLS.m[key] = tr
	return tr, true
}

// The errors a failed backend verification is logged with.
var (
	errServerCertNotProvided   = errors.New("server_cert_not_provided")
	errServerCertSize          = errors.New("server_cert_exceeded_size_limit")
	errServerCertEKU           = errors.New("server_cert_chain_invalid_eku")
	errServerCertTrustNotFound = errors.New("server_cert_trust_config_not_found")
	errServerCertNotValidated  = errors.New("server_cert_validation_not_performed")
	errServerCertVerify        = errors.New("ssl_certificate_verification_failed")
)

// verifyBackendCert checks a backend's chain under a config.
func verifyBackendCert(cfg *BackendAuthConfig, sni string, sans []obj, rawCerts [][]byte, now time.Time) error {
	if len(rawCerts) == 0 {
		return errServerCertNotProvided
	}
	size := 0
	for _, r := range rawCerts {
		size += len(r)
	}
	if len(rawCerts) > maxClientChainCerts || size > maxClientChainBytes {
		return errServerCertSize
	}
	if cfg.TrustConfig != "" && !cfg.TrustFound {
		return errServerCertTrustNotFound
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, r := range rawCerts {
		c, err := x509.ParseCertificate(r)
		if err != nil {
			return errServerCertVerify
		}
		certs = append(certs, c)
	}
	leaf := certs[0]
	if leaf.IsCA {
		return errServerCertVerify
	}
	eku := len(leaf.ExtKeyUsage) == 0
	for _, u := range leaf.ExtKeyUsage {
		eku = eku || u == x509.ExtKeyUsageServerAuth
	}
	if !eku {
		return errServerCertEKU
	}
	allowed := false
	for _, a := range cfg.Allowlisted {
		allowed = allowed || a.Equal(leaf)
	}
	if !allowed {
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		for _, c := range cfg.Intermediates {
			inter.AddCert(c)
		}
		opts := x509.VerifyOptions{Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		ok := false
		tried := false
		if cfg.Roots != nil {
			tried = true
			opts.Roots = cfg.Roots
			_, err := leaf.Verify(opts)
			ok = err == nil
		}
		if !ok && cfg.PublicRoots && sni != "" {
			if sys, err := x509.SystemCertPool(); err == nil {
				tried = true
				opts.Roots = sys
				_, err := leaf.Verify(opts)
				ok = err == nil
			}
		}
		if !tried {
			return errServerCertNotValidated
		}
		if !ok {
			return errServerCertVerify
		}
	}
	if len(sans) > 0 {
		for _, san := range sans {
			if d := str(san, "dnsName"); d != "" && leaf.VerifyHostname(d) == nil {
				return nil
			}
			if u := str(san, "uniformResourceIdentifier"); u != "" {
				for _, lu := range leaf.URIs {
					if lu.String() == u {
						return nil
					}
				}
			}
		}
		return errServerCertVerify
	}
	if sni != "" && leaf.VerifyHostname(sni) != nil {
		return errServerCertVerify
	}
	return nil
}
