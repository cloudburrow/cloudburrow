package sqladmin

// CUSTOMER_MANAGED_CAS_CA instances (#1834 phase 4).
//
// With settings.ipConfiguration.serverCaMode CUSTOMER_MANAGED_CAS_CA and
// serverCaPool projects/PROJECT/locations/REGION/caPools/POOL, Cloud SQL
// has a CA of the customer's pool, in Certificate Authority Service, sign
// the instance's server certificate: "Cloud SQL generates server
// certificates with EC P-384 (SHA-384) elliptic curve keys", and requests
// them from the pool as configuration-based certificate requests, with
// the instance's DNS name in the subject alternative names
// (docs.cloud.google.com/sql/docs/postgres/customer-managed-ca). Here the
// pool is this instance's own Certificate Authority Service (--services
// privateca), reached in process: each server certificate is a
// privateca.certificates.create on the pool, so it is listed among the
// pool's certificates (in the Enterprise tier) and chains to the pool's
// CA, and to its root. The server certificate is rotated as a
// GOOGLE_MANAGED_CAS_CA instance's is (servercerts.go):
// addServerCertificate requests a new one from the pool, and
// rotateServerCertificate makes it the one presented.
//
// What Google documents, and enforces here:
//   - the pool name's format ("Invalid format for server CA pool");
//   - the pool is in the instance's region ("The instance's server CA pool
//     must be in the same region as the instance.");
//   - the pool exists ("parent resource CA_POOL_ID not found") and has an
//     enabled CA ("There are no enabled CAs in the CaPool");
//   - the pool allows configuration-based requests and DNS names in the
//     SANs (the issuance itself is refused otherwise);
//   - MySQL 8.0.30 and later only;
//   - an instance on a customer-managed CA cannot go back to
//     GOOGLE_MANAGED_INTERNAL_CA, only to GOOGLE_MANAGED_CAS_CA, with
//     serverCaPool "".
//
// Not enforced: the service agent
// (service-PROJECT@gcp-sa-cloud-sql.iam.gserviceaccount.com) and its
// roles/privateca.certificateRequester on the pool. No call here is
// authorized (ADR-0004), and the pool's IAM policy is stored, not
// enforced (ADR-0006). serverCertificateRotationMode
// AUTOMATIC_ROTATION_DURING_MAINTENANCE rotates the server certificate in
// a maintenance run when it expires within 180 days (certrotation.go).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const customerCA = "CUSTOMER_MANAGED_CAS_CA"

// isCASMode is whether mode signs the server certificate with a CA
// hierarchy of Certificate Authority Service: the region's shared one, or
// the customer's pool.
func isCASMode(mode string) bool { return mode == casCA || mode == customerCA }

// CertificateRequest is a server certificate Cloud SQL requests from a CA
// pool: a configuration-based request for PublicKeyPEM.
type CertificateRequest struct {
	// ID is the certificate's ID in the pool.
	ID           string
	CommonName   string
	DNSNames     []string
	PublicKeyPEM []byte
	Lifetime     time.Duration
}

// IssuedCertificate is a certificate a CA pool issued.
type IssuedCertificate struct {
	// Name is the certificate's resource name.
	Name    string
	CertPEM string
	// ChainPEM is the issuing CA's certificate chain, the issuer first and
	// the root last.
	ChainPEM []string
}

// CertificateAuthorityService is Certificate Authority Service, as
// CUSTOMER_MANAGED_CAS_CA uses it. Its errors are gRPC statuses.
type CertificateAuthorityService interface {
	// EnabledCAs is how many of the pool's CAs are ENABLED; a pool that
	// does not exist is NotFound.
	EnabledCAs(ctx context.Context, pool string) (int, error)
	// IssueCertificate has the pool issue a certificate.
	IssueCertificate(ctx context.Context, pool string, r CertificateRequest) (IssuedCertificate, error)
}

// UseCertificateAuthorityService makes CUSTOMER_MANAGED_CAS_CA work, over
// c: without it, an instance with a customer's CA pool is refused, as
// Certificate Authority Service is not enabled.
func (s *Server) UseCertificateAuthorityService(c CertificateAuthorityService) {
	s.emu.Lock()
	s.privateca = c
	s.emu.Unlock()
}

var caPoolRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/caPools/([^/]+)$`)

// ipConfig is spec's settings.ipConfiguration.
func ipConfig(spec *Spec) map[string]any {
	if spec == nil {
		return nil
	}
	ic, _ := spec.Settings["ipConfiguration"].(map[string]any)
	return ic
}

// specCAMode is spec's server CA mode, GOOGLE_MANAGED_INTERNAL_CA when it
// sets none.
func specCAMode(spec *Spec) string {
	switch m, _ := ipConfig(spec)["serverCaMode"].(string); m {
	case casCA, customerCA:
		return m
	}
	return internalCA
}

func specCAPool(spec *Spec) string {
	p, _ := ipConfig(spec)["serverCaPool"].(string)
	return p
}

// keepCASettings carries the instance's server CA settings over a patch
// whose ipConfiguration does not name them: a patch changes only what it
// sets.
func keepCASettings(prev Spec, next *Spec, raw map[string]any) {
	nic, ok := raw["ipConfiguration"].(map[string]any)
	if !ok {
		return
	}
	pic := ipConfig(&prev)
	merged := map[string]any{}
	for k, v := range nic {
		merged[k] = v
	}
	for _, k := range []string{"serverCaMode", "serverCaPool", "serverCertificateRotationMode"} {
		if _, set := nic[k]; !set {
			if v, had := pic[k]; had {
				merged[k] = v
			}
		}
	}
	settings := map[string]any{}
	for k, v := range next.Settings {
		settings[k] = v
	}
	settings["ipConfiguration"] = merged
	next.Settings = settings
}

// pendingServerCert is a first server certificate issued by a customer's
// pool before the instance is kept, which installServerCert then keeps.
type pendingServerCert struct {
	pool    string
	version serverCertVersion
}

// checkServerCA checks next's server CA settings, against prev's (nil on
// insert). When next names a customer's CA pool it did not before, the
// pool issues the instance's first server certificate, so a pool that
// cannot is refused on the request, as Cloud SQL refuses it. The caller
// may hold emu: cas is passed in.
func checkServerCA(ctx context.Context, cas CertificateAuthorityService, prev *Spec, next Spec) (*pendingServerCert, error) {
	mode, pool := specCAMode(&next), specCAPool(&next)
	if pool != "" && mode != customerCA {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: settings.ipConfiguration.serverCaPool is set only with serverCaMode %s; "+
			"to use serverCaMode %s, set serverCaPool to \"\".", customerCA, mode)
	}
	if prev != nil && specCAMode(prev) == customerCA && mode == internalCA {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: an instance that uses a customer-managed CA (%s) cannot be "+
			"updated to use the per-instance CA (%s); it can be updated to use the shared CA, %s, with serverCaPool \"\".",
			customerCA, internalCA, casCA)
	}
	if mode != customerCA {
		return nil, nil
	}
	if legacyMySQL(next.DatabaseVersion) {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: the customer-managed CA option (%s) is supported only on MySQL 8.0.30 "+
			"and later, not on %s.", customerCA, next.DatabaseVersion)
	}
	if pool == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: serverCaMode %s needs settings.ipConfiguration.serverCaPool, "+
			"projects/PROJECT_ID/locations/REGION/caPools/CA_POOL_ID.", customerCA)
	}
	m := caPoolRE.FindStringSubmatch(pool)
	if m == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid format for server CA pool: %q; it must be "+
			"projects/PROJECT_ID/locations/REGION/caPools/CA_POOL_ID.", pool)
	}
	if m[2] != next.region() {
		return nil, Errorf(http.StatusBadRequest, "The instance's server CA pool must be in the same region as the instance. "+
			"%s is in %s; the instance is in %s.", pool, m[2], next.region())
	}
	if prev != nil && specCAMode(prev) == customerCA && specCAPool(prev) == pool {
		return nil, nil
	}
	if cas == nil {
		return nil, Errorf(http.StatusPreconditionFailed, "Invalid request: serverCaMode %s needs Certificate Authority Service, "+
			"whose CA pool signs the server certificate: run `cloudburrow up` with privateca in --services.", customerCA)
	}
	n, err := cas.EnabledCAs(ctx, pool)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, Errorf(http.StatusNotFound, "parent resource %s not found.", pool)
		}
		return nil, caServiceError(pool, err)
	}
	if n == 0 {
		return nil, &apiError{code: http.StatusBadRequest, status: "FAILED_PRECONDITION",
			msg: fmt.Sprintf("There are no enabled CAs in the CaPool %s.", pool)}
	}
	v, err := issueFromPool(ctx, cas, pool, next.Project, next.Name)
	if err != nil {
		return nil, err
	}
	return &pendingServerCert{pool: pool, version: v}, nil
}

// caServiceError is the answer for an error of Certificate Authority
// Service's, its status kept.
func caServiceError(pool string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	codeOf := map[codes.Code]struct {
		http int
		name string
	}{
		codes.InvalidArgument: {http.StatusBadRequest, "INVALID_ARGUMENT"}, codes.FailedPrecondition: {http.StatusBadRequest, "FAILED_PRECONDITION"},
		codes.NotFound: {http.StatusNotFound, "NOT_FOUND"}, codes.PermissionDenied: {http.StatusForbidden, "PERMISSION_DENIED"},
		codes.ResourceExhausted: {http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"}, codes.AlreadyExists: {http.StatusConflict, "ALREADY_EXISTS"},
		codes.Unimplemented: {http.StatusNotImplemented, "UNIMPLEMENTED"}, codes.Unavailable: {http.StatusServiceUnavailable, "UNAVAILABLE"},
	}
	c, ok := codeOf[st.Code()]
	if !ok {
		c.http, c.name = http.StatusInternalServerError, "INTERNAL"
	}
	code, name := c.http, c.name
	return &apiError{code: code, status: name,
		msg: fmt.Sprintf("The server certificate cannot be issued by the CA pool %s: %s", pool, st.Message())}
}

// certificateID is a server certificate's ID in the pool, unique per
// request: [a-zA-Z0-9_-]{1,63}.
func certificateID(instance string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	if len(instance) > 40 {
		instance = instance[:40]
	}
	return "cloudsql-" + instance + "-" + hex.EncodeToString(b)
}

// issueFromPool has pool issue a server certificate for project's
// instance: a new EC P-384 key, the instance's DNS name in the SANs.
func issueFromPool(ctx context.Context, cas CertificateAuthorityService, pool, project, instance string) (serverCertVersion, error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return serverCertVersion{}, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return serverCertVersion{}, err
	}
	dns := dnsName(project, instance)
	got, err := cas.IssueCertificate(ctx, pool, CertificateRequest{
		ID: certificateID(instance), CommonName: project + ":" + instance, DNSNames: []string{dns},
		PublicKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), Lifetime: serverCertLifetime,
	})
	if err != nil {
		return serverCertVersion{}, caServiceError(pool, err)
	}
	_, cert := certFingerprint(got.CertPEM)
	if cert == nil {
		return serverCertVersion{}, fmt.Errorf("the CA pool %s issued an unreadable certificate", pool)
	}
	if !slices.Contains(cert.DNSNames, dns) {
		return serverCertVersion{}, Errorf(http.StatusBadRequest, "The server certificate cannot be issued by the CA pool %s: "+
			"its issuance policy does not pass the DNS names of the subject alternative names through; "+
			"allow DNS names in subject alternative names (SAN) on the pool.", pool)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return serverCertVersion{}, err
	}
	chain := ""
	for _, c := range got.ChainPEM {
		chain += strings.TrimSpace(c) + "\n"
	}
	return serverCertVersion{
		Cert:    got.CertPEM,
		Key:     string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})),
		Created: time.Now().UTC().Format(time.RFC3339Nano),
		Chain:   chain,
		Name:    got.Name,
	}, nil
}

// installServerCert keeps p, the first certificate of instance's pool, as
// its only, active server certificate.
func (c *connector) installServerCert(instance string, p *pendingServerCert) error {
	if p == nil || c == nil {
		return nil
	}
	c.camu.Lock()
	defer c.camu.Unlock()
	v := p.version
	v.Used = true
	fp, _ := certFingerprint(v.Cert)
	return c.writeServerCertsLocked(instance, &serverCertFile{Issuer: p.pool, Active: fp, Versions: []serverCertVersion{v}})
}

// errNoCAService is the answer for a customer-managed instance while
// Certificate Authority Service is not enabled.
func errNoCAService(instance, pool string) error {
	return Errorf(http.StatusPreconditionFailed, "The instance %s's server certificate is issued by the CA pool %s, and Certificate "+
		"Authority Service is not enabled: run `cloudburrow up` with privateca in --services.", instance, pool)
}

// selfSigned is whether cert is a root.
func selfSigned(cert *x509.Certificate) bool {
	return cert.CheckSignatureFrom(cert) == nil
}

// pemCerts parses every certificate of a PEM bundle.
func pemCerts(bundle string) []*x509.Certificate {
	var out []*x509.Certificate
	rest := []byte(bundle)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return out
		}
		if c, err := x509.ParseCertificate(b.Bytes); err == nil {
			out = append(out, c)
		}
	}
}
