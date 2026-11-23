package compute

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Frontend mutual TLS and authorization policies (#2160), as
// https://docs.cloud.google.com/load-balancing/docs/mtls,
// .../load-balancing/docs/https/custom-headers-global (the mTLS
// variables), .../load-balancing/docs/auth-policy/auth-policy-overview and
// the targetHttpsProxies reference describe them (read 2026-11-22):
//
//   - a target HTTPS proxy's serverTlsPolicy names a Network Security
//     server TLS policy; with an mtlsPolicy the listener asks each client
//     for its certificate and checks it against the policy's trust config
//     (its trust store's anchors and intermediates, or its allowlist),
//     for the clientAuth extended key usage, a chain of at most 10
//     certificates and 16 KB, RSA keys of 2048 to 4096 bits and ECDSA
//     keys on P-256 or P-384; REJECT_INVALID ends the handshake of a
//     client without a valid certificate, ALLOW_INVALID_OR_MISSING_CLIENT_CERT
//     lets it through and tells the backend through the {client_cert_*}
//     variables of its custom request headers;
//   - the authorization policies (authzPolicies) that target the
//     forwarding rule are checked on each request, and one denied is
//     answered 403;
//   - a target HTTPS proxy's authorizationPolicy (the classic resource)
//     is checked and kept, and "currently has no impact", as on Google;
//   - a backend service's securitySettings.clientTlsPolicy is checked and
//     kept; Google applies it to INTERNAL_SELF_MANAGED backend services
//     (Traffic Director) only, whose clients are the proxyless gRPC and
//     Envoy clients themselves, not these load balancers.

const nsPrefix = "//networksecurity.googleapis.com/"

var nsRefRE = regexp.MustCompile(`^projects/[^/]+/locations/([a-z0-9-]+)/(serverTlsPolicies|authorizationPolicies|clientTlsPolicies|backendAuthenticationConfigs)/[A-Za-z0-9_-]{1,63}$`)

// nsRef checks a Network Security policy reference of coll and returns it
// in Google's form, //networksecurity.googleapis.com/projects/P/locations/L/coll/N.
func (s *Server) nsRef(x lbCtx, field, ref, coll string) (string, error) {
	name := ref
	for _, p := range []string{nsPrefix, "https://networksecurity.googleapis.com/v1/", "https://networksecurity.googleapis.com/v1beta1/"} {
		name = strings.TrimPrefix(name, p)
	}
	m := nsRefRE.FindStringSubmatch(name)
	if m == nil || m[2] != coll {
		return "", invalid("Invalid value for field '%s': '%s'. Must be a URL of the form //networksecurity.googleapis.com/projects/{project}/locations/{location}/%s/{name}.", field, ref, coll)
	}
	want := "global"
	if x.region != "" {
		want = x.region
	}
	if m[1] != want {
		return "", invalid("Invalid value for field '%s': '%s'. The policy must be in location '%s'.", field, ref, want)
	}
	if s.opts.NetSec == nil || !s.opts.NetSec.PolicyExists(name) {
		return "", errf(http.StatusNotFound, "notFound", "The resource '%s%s' was not found", nsPrefix, name)
	}
	return nsPrefix + name, nil
}

// proxyNSFields checks a target HTTPS proxy's serverTlsPolicy and
// authorizationPolicy into o.
func (s *Server) proxyNSFields(x lbCtx, b, o obj) error {
	if ref := str(b, "serverTlsPolicy"); ref != "" {
		v, err := s.nsRef(x, "resource.serverTlsPolicy", ref, "serverTlsPolicies")
		if err != nil {
			return err
		}
		o["serverTlsPolicy"] = v
	}
	if ref := str(b, "authorizationPolicy"); ref != "" {
		if x.region != "" {
			return invalid("Invalid value for field 'resource.authorizationPolicy': '%s'. authorizationPolicy only applies to a global TargetHttpsProxy.", ref)
		}
		v, err := s.nsRef(x, "resource.authorizationPolicy", ref, "authorizationPolicies")
		if err != nil {
			return err
		}
		o["authorizationPolicy"] = v
	}
	return nil
}

// backendSecuritySettings checks a backend service's securitySettings.
func (s *Server) backendSecuritySettings(x lbCtx, b, o obj) error {
	ss := objOf(b["securitySettings"])
	if ss == nil {
		return nil
	}
	if ref := str(ss, "clientTlsPolicy"); ref != "" {
		if x.region != "" || str(b, "loadBalancingScheme") != "INTERNAL_SELF_MANAGED" {
			return invalid("Invalid value for field 'resource.securitySettings.clientTlsPolicy': '%s'. clientTlsPolicy only applies to a global BackendService with the loadBalancingScheme set to INTERNAL_SELF_MANAGED.", ref)
		}
		v, err := s.nsRef(x, "resource.securitySettings.clientTlsPolicy", ref, "clientTlsPolicies")
		if err != nil {
			return err
		}
		ss = deepCopy(ss).(obj)
		ss["clientTlsPolicy"] = v
		o["securitySettings"] = ss
	} else if len(strs(ss, "subjectAltNames")) > 0 {
		return invalid("Invalid value for field 'resource.securitySettings.subjectAltNames': subjectAltNames only applies when the backend service has a clientTlsPolicy.")
	}
	return nil
}

// proxyServerTLS is a proxy's server TLS policy, ok false when it has none
// that asks for client certificates.
func (s *Server) proxyServerTLS(proxy obj) (ServerTLSPolicy, bool) {
	ref := str(proxy, "serverTlsPolicy")
	if ref == "" || s.opts.NetSec == nil {
		return ServerTLSPolicy{}, false
	}
	p, ok := s.opts.NetSec.ServerTLS(strings.TrimPrefix(ref, nsPrefix))
	if !ok || p.Mode == "" {
		return ServerTLSPolicy{}, false
	}
	return p, true
}

// The documented limits on a client's chain (load-balancing/docs/mtls).
const (
	maxClientChainCerts = 10
	maxClientChainBytes = 16 << 10
)

// verifyClient checks a client's certificates against a policy.
func verifyClient(p ServerTLSPolicy, certs []*x509.Certificate, now time.Time) ClientCert {
	if len(certs) == 0 {
		return ClientCert{Error: "client_cert_not_provided"}
	}
	c := ClientCert{Present: true, Leaf: certs[0]}
	size := 0
	for _, x := range certs {
		size += len(x.Raw)
	}
	switch {
	case len(certs) > maxClientChainCerts:
		c.Error = "client_cert_chain_exceeded_limit"
		return c
	case size > maxClientChainBytes:
		c.Error = "client_cert_exceeded_size_limit"
		return c
	}
	switch k := c.Leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		if n := k.N.BitLen(); n < 2048 || n > 4096 {
			c.Error = "client_cert_invalid_rsa_key_size"
			return c
		}
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			c.Error = "client_cert_unsupported_elliptic_curve_key"
			return c
		}
	case ed25519.PublicKey:
		c.Error = "client_cert_unsupported_key_algorithm"
		return c
	default:
		c.Error = "client_cert_unsupported_key_algorithm"
		return c
	}
	if p.TrustConfig != "" && !p.TrustFound {
		c.Error = "client_cert_trust_config_not_found"
		return c
	}
	for _, a := range p.Allowlisted {
		if bytes.Equal(a.Raw, c.Leaf.Raw) {
			c.Verified, c.Chain = true, certs[1:]
			return c
		}
	}
	if p.Roots == nil {
		c.Error = "client_cert_validation_failed"
		return c
	}
	inter := x509.NewCertPool()
	if p.Intermediates != nil {
		inter = p.Intermediates.Clone()
	}
	for _, x := range certs[1:] {
		inter.AddCert(x)
	}
	chains, err := c.Leaf.Verify(x509.VerifyOptions{Roots: p.Roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		var ci x509.CertificateInvalidError
		if errors.As(err, &ci) && ci.Reason == x509.IncompatibleUsage {
			c.Error = "client_cert_chain_invalid_eku"
		} else {
			c.Error = "client_cert_validation_failed"
		}
		return c
	}
	c.Verified = true
	if len(chains) > 0 && len(chains[0]) > 1 {
		c.Chain = chains[0][1:]
	}
	return c
}

// mtlsHandshake sets a TLS configuration to ask for client certificates
// as the proxy's server TLS policy says.
func (s *Server) mtlsHandshake(proxy obj, cfg *tls.Config) {
	p, ok := s.proxyServerTLS(proxy)
	if !ok {
		return
	}
	cfg.ClientAuth = tls.RequestClientCert
	if p.Mode != "REJECT_INVALID" {
		return
	}
	now := s.opts.Now
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if c := verifyClient(p, cs.PeerCertificates, now()); !c.Verified {
			// The connection is terminated (load-balancing/docs/mtls).
			return errors.New(c.Error)
		}
		return nil
	}
}

type clientCertKey struct{}

// clientCertOf is a request's client certificate check, nil when its proxy
// has no mTLS policy.
func clientCertOf(r *http.Request) *ClientCert {
	c, _ := r.Context().Value(clientCertKey{}).(*ClientCert)
	return c
}

// withClientCert checks r's client certificate when proxy has an mTLS
// policy, for the custom headers and the authorization policies.
func (s *Server) withClientCert(r *http.Request, proxy obj) *http.Request {
	p, ok := s.proxyServerTLS(proxy)
	if !ok || r.TLS == nil {
		return r
	}
	c := verifyClient(p, r.TLS.PeerCertificates, s.opts.Now())
	return r.WithContext(context.WithValue(r.Context(), clientCertKey{}, &c))
}

// mtlsVars are the mTLS custom header variables' values.
func mtlsVars(r *http.Request) []string {
	c := clientCertOf(r)
	if c == nil {
		c = &ClientCert{}
	}
	vars := map[string]string{"present": strconv.FormatBool(c.Present), "chain_verified": strconv.FormatBool(c.Verified), "error": c.Error}
	if l := c.Leaf; l != nil {
		sum := sha256.Sum256(l.Raw)
		vars["sha256_fingerprint"] = base64.StdEncoding.EncodeToString(sum[:])
		vars["serial_number"] = hex.EncodeToString(l.SerialNumber.Bytes())
		var uris, dns []string
		for _, u := range l.URIs {
			if u.Scheme == "spiffe" && vars["spiffe_id"] == "" {
				vars["spiffe_id"] = u.String()
				continue
			}
			uris = append(uris, base64.StdEncoding.EncodeToString([]byte(u.String())))
		}
		for _, d := range l.DNSNames {
			dns = append(dns, base64.StdEncoding.EncodeToString([]byte(d)))
		}
		vars["uri_sans"], vars["dnsname_sans"] = strings.Join(uris, ","), strings.Join(dns, ",")
		vars["valid_not_before"] = l.NotBefore.UTC().Format("2006-01-02T15:04:05-07:00")
		vars["valid_not_after"] = l.NotAfter.UTC().Format("2006-01-02T15:04:05-07:00")
		vars["issuer_dn"] = base64.StdEncoding.EncodeToString(l.RawIssuer)
		vars["subject_dn"] = base64.StdEncoding.EncodeToString(l.RawSubject)
		if c.Verified {
			vars["leaf"] = ":" + base64.StdEncoding.EncodeToString(l.Raw) + ":"
			var chain []string
			for _, x := range c.Chain {
				chain = append(chain, ":"+base64.StdEncoding.EncodeToString(x.Raw)+":")
			}
			vars["chain"] = strings.Join(chain, ",")
		}
	}
	var out []string
	for _, k := range []string{"present", "chain_verified", "error", "sha256_fingerprint", "serial_number", "spiffe_id", "uri_sans",
		"dnsname_sans", "valid_not_before", "valid_not_after", "issuer_dn", "subject_dn", "leaf", "chain"} {
		out = append(out, "{client_cert_"+k+"}", vars[k])
	}
	return out
}

// authorize checks the authorization policies that target the forwarding
// rule; false has answered 403.
func (h *lbHTTP) authorize(w http.ResponseWriter, r *http.Request, scheme string) bool {
	ns := h.s.opts.NetSec
	if ns == nil {
		return true
	}
	srcIP, srcPort, _ := net.SplitHostPort(r.RemoteAddr)
	sp, _ := strconv.Atoi(srcPort)
	dstIP := ""
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		dstIP, _, _ = net.SplitHostPort(a.String())
	}
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	req := AuthzRequest{Rule: h.rule, Host: r.Host, Path: path, Method: r.Method, Scheme: scheme, SNI: sniOf(r),
		SourceIP: srcIP, SourcePort: sp, DestIP: dstIP, DestPort: h.port, Header: r.Header}
	if c := clientCertOf(r); c != nil {
		req.Cert = *c
	}
	if ns.Authorize(req) {
		return true
	}
	lbError(w, http.StatusForbidden, "RBAC: access denied")
	return false
}
