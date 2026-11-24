package compute

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net/http"
	"time"
)

// What the load balancers' data plane reads from the APIs beside Compute
// Engine's (#2160): Certificate Manager's certificate maps and
// certificates, which target HTTPS and SSL proxies serve, and
// Identity-Aware Proxy's settings and access policies, which a backend
// service with iap.enabled enforces. cmd/cloudburrow wires each to its
// API; nil leaves a proxy that names a map unable to complete a TLS
// handshake, and IAP refusing every request, as nothing could grant it.

// CertificateMaps is Certificate Manager (certificatemanager.googleapis.com)
// as the TLS listeners read it.
type CertificateMaps interface {
	// MapExists reports whether a certificate map exists, by its name
	// (projects/P/locations/global/certificateMaps/M).
	MapExists(name string) bool
	// MapCertificates are the certificates a handshake for sni gets from a
	// map: those of the entry whose hostname matches sni (an exact name
	// first, then a wildcard), else of its PRIMARY entry; nil when none is
	// ready, and the handshake then fails, as Google's does.
	MapCertificates(name, sni string) []tls.Certificate
	// Certificates are Certificate Manager certificates by name
	// (projects/P/locations/L/certificates/C), those ready to serve: a
	// regional target HTTPS proxy's certificateManagerCertificates.
	Certificates(names []string) []tls.Certificate
}

// IAPGate is Identity-Aware Proxy (iap.googleapis.com) as a backend
// service's iap enforces it.
type IAPGate interface {
	// VerifyIDToken checks an OpenID Connect ID token this instance signed
	// (the metadata server's identity endpoint, IAM Credentials'
	// generateIdToken, gcloud auth print-identity-token), and is its
	// email and audience; ok is false for any other token.
	VerifyIDToken(tok string) (email, audience string, ok bool)
	// Allowed reports whether email (a user or service account) may reach
	// an IAP resource (projects/P/iap_web/compute/services/S, or
	// compute-REGION for a regional backend service; P is the project ID
	// and S the backend service's name, and the API's names by project
	// number or service ID are the same resource): whether it holds
	// iap.webServiceVersions.accessViaIAP (roles/iap.httpsResourceAccessor)
	// on the resource or above it (iap_web/compute, iap_web, the project),
	// with each binding's condition evaluated against r.
	Allowed(ctx context.Context, resource, email string, r IAPRequest) bool
	// Settings is the resource's IapSettings as JSON (accessSettings,
	// applicationSettings), merged from the levels above it; nil when
	// none is set.
	Settings(resource string) map[string]any
	// ProjectNumber is a project's number, for the signed header's
	// audience (/projects/NUMBER/global/backendServices/ID); "" when the
	// project is not known.
	ProjectNumber(project string) string
}

// IAPRequest is what an IAM condition on an IAP binding can read
// (request.host, request.path, request.time, levels).
type IAPRequest struct {
	Host, Path string
	Time       time.Time
}

// IssueLocalCertificate issues a certificate for domains from this
// instance's load balancing CA (GET /cloudburrow/lb/ca.pem), as a
// Google-managed certificate is issued here: an ECDSA P-256 key, valid
// for 90 days. It returns the certificate chain and key as PEM, and when
// the certificate expires.
func (s *Server) IssueLocalCertificate(domains []string) (chainPEM, keyPEM string, expire time.Time, err error) {
	if len(domains) == 0 {
		return "", "", time.Time{}, errors.New("no domains")
	}
	s.mu.Lock()
	ca, caKey, caPEM, err := s.lbCALocked()
	if err == nil {
		err = s.saveLocked()
	}
	now := s.opts.Now()
	s.mu.Unlock()
	if err != nil {
		return "", "", time.Time{}, err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", time.Time{}, err
	}
	tpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains,
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, 90), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
	if err != nil {
		return "", "", time.Time{}, err
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) + caPEM,
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})), tpl.NotAfter, nil
}

// NetworkSecurity is Network Security (networksecurity.googleapis.com) as
// the load balancers read it (#2160): a target HTTPS proxy's
// serverTlsPolicy (frontend mutual TLS) and the authorization policies
// (authzPolicies) that target a forwarding rule.
type NetworkSecurity interface {
	// PolicyExists is whether a policy exists, by its name
	// (projects/P/locations/L/serverTlsPolicies/X, .../authorizationPolicies/X
	// or .../clientTlsPolicies/X).
	PolicyExists(name string) bool
	// ServerTLS is a server TLS policy with its trust config resolved; ok
	// is false when there is no such policy.
	ServerTLS(name string) (ServerTLSPolicy, bool)
	// Authorize is the decision of the authorization policies that target
	// r.Rule on r: the CUSTOM policies' providers to ask, then the DENY and
	// ALLOW policies' outcome.
	Authorize(r AuthzRequest) AuthzVerdict
}

// ServerTLSPolicy is a server TLS policy as a TLS listener applies it.
type ServerTLSPolicy struct {
	// Mode is the mtlsPolicy's clientValidationMode:
	// ALLOW_INVALID_OR_MISSING_CLIENT_CERT or REJECT_INVALID; "" asks for
	// no client certificate.
	Mode string
	// TrustConfig is clientValidationTrustConfig; TrustFound is whether it
	// exists.
	TrustConfig string
	TrustFound  bool
	// Roots and Intermediates are the trust store's anchors and
	// intermediate CAs; Allowlisted are the always-valid certificates.
	Roots, Intermediates *x509.CertPool
	Allowlisted          []*x509.Certificate
}

// ClientCert is the outcome of a request's client certificate check, as
// the mTLS custom header variables report it.
type ClientCert struct {
	Present, Verified bool
	// Error is client_cert_error's value, "" when there is none.
	Error string
	Leaf  *x509.Certificate
	Chain []*x509.Certificate
}

// AuthzRequest is what an authorization policy matches a request on.
type AuthzRequest struct {
	// Rule is the forwarding rule's relative name.
	Rule                 string
	Host, Path, Method   string
	Scheme, SNI          string
	SourceIP, DestIP     string
	SourcePort, DestPort int
	Header               http.Header
	Cert                 ClientCert
	// SourceVM is the instance (projects/P/zones/Z/instances/I) whose
	// internal address the request comes from, "" for none; its first
	// service account, and the permanent IDs of its secure tags' values
	// (its effective tags), are what a rule's source resources match.
	SourceVM             string
	SourceServiceAccount string
	SourceTagValues      []int64
}
