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
	// compute-REGION for a regional backend service): whether it holds
	// iap.webServiceVersions.accessViaIAP (roles/iap.httpsResourceAccessor)
	// on the resource or above it (iap_web/compute, iap_web, the project),
	// with each binding's condition evaluated against r.
	Allowed(ctx context.Context, resource, email string, r IAPRequest) bool
	// Settings is the resource's IapSettings as JSON (accessSettings,
	// applicationSettings), merged from the levels above it; nil when
	// none is set.
	Settings(resource string) map[string]any
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
