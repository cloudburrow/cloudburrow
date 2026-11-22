package certificatemanager

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"regexp"
	"strings"

	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// DNS authorizations, certificate issuance configs and trust configs, as
// their REST references and .../certificate-manager/docs/dns-authorizations
// describe them (read 2026-11-22).
//
// A DNS authorization's record is the CNAME Google asks for: for
// FIXED_RECORD (the default in global) `_acme-challenge.DOMAIN.`, for
// PER_PROJECT_RECORD (the default in a region)
// `_acme-challenge_SUFFIX.DOMAIN.` with a suffix of the project, each
// pointing at `UUID.4.authorize.certificatemanager.goog.`. Google derives
// the UUID and suffix in ways it does not document; here they are hashes
// of the project and domain, stable for both.

func dnsRecord(project, domain string, t cmpb.DnsAuthorization_Type) *cmpb.DnsAuthorization_DnsResourceRecord {
	d := strings.ToLower(strings.TrimSuffix(domain, "."))
	h := sha256.Sum256([]byte("cloudburrow-dns-authorization|" + project + "|" + d))
	u := hex.EncodeToString(h[:16])
	uuid := u[0:8] + "-" + u[8:12] + "-" + u[12:16] + "-" + u[16:20] + "-" + u[20:32]
	name := "_acme-challenge." + d + "."
	if t == cmpb.DnsAuthorization_PER_PROJECT_RECORD {
		ph := sha256.Sum256([]byte("cloudburrow-project|" + project))
		sfx := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(ph[:10]))
		name = "_acme-challenge_" + sfx + "." + d + "."
	}
	return &cmpb.DnsAuthorization_DnsResourceRecord{Name: name, Type: "CNAME", Data: uuid + ".4.authorize.certificatemanager.goog."}
}

func (s *Server) CreateDnsAuthorization(_ context.Context, req *cmpb.CreateDnsAuthorizationRequest) (*longrunningpb.Operation, error) {
	project, loc, err := parseParent(req.GetParent(), false)
	if err != nil {
		return nil, err
	}
	if err := checkID("dns_authorization_id", req.GetDnsAuthorizationId()); err != nil {
		return nil, err
	}
	a, _ := proto.Clone(req.GetDnsAuthorization()).(*cmpb.DnsAuthorization)
	if a == nil {
		a = &cmpb.DnsAuthorization{}
	}
	if a.GetDomain() == "" {
		return nil, apierror.InvalidArgument("Invalid argument: domain is required.")
	}
	if strings.Contains(a.GetDomain(), "*") {
		return nil, apierror.InvalidArgument("Invalid value for field 'domain': '%s'. A DNS authorization's domain cannot be a wildcard: authorize the parent domain, which covers its wildcard.", a.GetDomain())
	}
	if len(a.GetDomain()) > 253 || !validDomain(a.GetDomain(), false) {
		return nil, apierror.InvalidArgument("Invalid value for field 'domain': '%s'. It must be a fully qualified domain name.", a.GetDomain())
	}
	if a.GetType() == cmpb.DnsAuthorization_TYPE_UNSPECIFIED {
		a.Type = cmpb.DnsAuthorization_FIXED_RECORD
		if loc != "global" {
			a.Type = cmpb.DnsAuthorization_PER_PROJECT_RECORD
		}
	}
	name := req.GetParent() + "/" + collDNS + "/" + req.GetDnsAuthorizationId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; ok {
		return nil, apierror.AlreadyExists("DnsAuthorization '%s' already exists.", name)
	}
	a.Name, a.Domain = name, strings.ToLower(strings.TrimSuffix(a.GetDomain(), "."))
	a.DnsResourceRecord = dnsRecord(project, a.GetDomain(), a.GetType())
	stamp(a, s.opts.Now(), true)
	s.res[name] = a
	return s.opLocked(name, "create", a)
}

func (s *Server) GetDnsAuthorization(_ context.Context, req *cmpb.GetDnsAuthorizationRequest) (*cmpb.DnsAuthorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.getLocked(collDNS, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*cmpb.DnsAuthorization), nil
}

func (s *Server) ListDnsAuthorizations(_ context.Context, req *cmpb.ListDnsAuthorizationsRequest) (*cmpb.ListDnsAuthorizationsResponse, error) {
	if _, _, err := parseListParent(req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []*cmpb.DnsAuthorization
	for _, k := range s.childrenLocked(req.GetParent(), collDNS) {
		items = append(items, proto.Clone(s.res[k]).(*cmpb.DnsAuthorization))
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("dns:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &cmpb.ListDnsAuthorizationsResponse{DnsAuthorizations: page, NextPageToken: next}, nil
}

func (s *Server) UpdateDnsAuthorization(_ context.Context, req *cmpb.UpdateDnsAuthorizationRequest) (*longrunningpb.Operation, error) {
	in := req.GetDnsAuthorization()
	if err := checkName(in.GetName(), collDNS); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.res[in.GetName()].(*cmpb.DnsAuthorization)
	if !ok {
		return nil, notFound(collDNS, in.GetName())
	}
	next := proto.Clone(cur).(*cmpb.DnsAuthorization)
	if err := applyMask(next, in, req.GetUpdateMask(), "description", "labels"); err != nil {
		return nil, err
	}
	stamp(next, s.opts.Now(), false)
	s.res[cur.GetName()] = next
	return s.opLocked(cur.GetName(), "update", next)
}

// usersLocked are the certificates whose managed config names rel.
func (s *Server) usersLocked(rel string) []string {
	var out []string
	for _, k := range sortedKeys(s.res) {
		c, ok := s.res[k].(*cmpb.Certificate)
		if !ok || c.GetManaged() == nil {
			continue
		}
		if c.GetManaged().GetIssuanceConfig() == rel {
			out = append(out, k)
		}
		for _, a := range c.GetManaged().GetDnsAuthorizations() {
			if a == rel {
				out = append(out, k)
			}
		}
	}
	return out
}

func (s *Server) DeleteDnsAuthorization(_ context.Context, req *cmpb.DeleteDnsAuthorizationRequest) (*longrunningpb.Operation, error) {
	return s.deleteUnused(collDNS, req.GetName())
}

func (s *Server) deleteUnused(coll, name string) (*longrunningpb.Operation, error) {
	if err := checkName(name, coll); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; !ok {
		return nil, notFound(coll, name)
	}
	if u := s.usersLocked(name); len(u) > 0 {
		return nil, apierror.FailedPrecondition("%s '%s' is in use by certificate '%s'.", noun[coll], name, u[0])
	}
	delete(s.res, name)
	return s.opLocked(name, "delete", nil)
}

var caPoolRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/caPools/[^/]+$`)

func (s *Server) CreateCertificateIssuanceConfig(_ context.Context, req *cmpb.CreateCertificateIssuanceConfigRequest) (*longrunningpb.Operation, error) {
	if _, _, err := parseParent(req.GetParent(), false); err != nil {
		return nil, err
	}
	if err := checkID("certificate_issuance_config_id", req.GetCertificateIssuanceConfigId()); err != nil {
		return nil, err
	}
	c, _ := proto.Clone(req.GetCertificateIssuanceConfig()).(*cmpb.CertificateIssuanceConfig)
	if c == nil {
		c = &cmpb.CertificateIssuanceConfig{}
	}
	pool := c.GetCertificateAuthorityConfig().GetCertificateAuthorityServiceConfig().GetCaPool()
	if pool == "" {
		return nil, apierror.InvalidArgument("Invalid argument: certificate_authority_config.certificate_authority_service_config.ca_pool is required.")
	}
	if !caPoolRE.MatchString(pool) {
		return nil, apierror.InvalidArgument("Invalid value for field 'certificate_authority_config.certificate_authority_service_config.ca_pool': '%s'. It must be projects/{project}/locations/{location}/caPools/{caPool}.", pool)
	}
	if c.GetLifetime() == nil || c.GetLifetime().AsDuration() <= 0 {
		return nil, apierror.InvalidArgument("Invalid argument: lifetime is required.")
	}
	if p := c.GetRotationWindowPercentage(); p < 1 || p > 99 {
		return nil, apierror.InvalidArgument("Invalid value for field 'rotation_window_percentage': %d. Must be a number between 1-99, inclusive.", p)
	}
	if c.GetKeyAlgorithm() == cmpb.CertificateIssuanceConfig_KEY_ALGORITHM_UNSPECIFIED {
		return nil, apierror.InvalidArgument("Invalid argument: key_algorithm is required (RSA_2048 or ECDSA_P256).")
	}
	name := req.GetParent() + "/" + collIssue + "/" + req.GetCertificateIssuanceConfigId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; ok {
		return nil, apierror.AlreadyExists("CertificateIssuanceConfig '%s' already exists.", name)
	}
	c.Name = name
	stamp(c, s.opts.Now(), true)
	s.res[name] = c
	return s.opLocked(name, "create", c)
}

func (s *Server) GetCertificateIssuanceConfig(_ context.Context, req *cmpb.GetCertificateIssuanceConfigRequest) (*cmpb.CertificateIssuanceConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.getLocked(collIssue, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*cmpb.CertificateIssuanceConfig), nil
}

func (s *Server) ListCertificateIssuanceConfigs(_ context.Context, req *cmpb.ListCertificateIssuanceConfigsRequest) (*cmpb.ListCertificateIssuanceConfigsResponse, error) {
	if _, _, err := parseListParent(req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []*cmpb.CertificateIssuanceConfig
	for _, k := range s.childrenLocked(req.GetParent(), collIssue) {
		items = append(items, proto.Clone(s.res[k]).(*cmpb.CertificateIssuanceConfig))
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("issuance:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &cmpb.ListCertificateIssuanceConfigsResponse{CertificateIssuanceConfigs: page, NextPageToken: next}, nil
}

func (s *Server) UpdateCertificateIssuanceConfig(_ context.Context, req *cmpb.UpdateCertificateIssuanceConfigRequest) (*longrunningpb.Operation, error) {
	in := req.GetCertificateIssuanceConfig()
	if err := checkName(in.GetName(), collIssue); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.res[in.GetName()].(*cmpb.CertificateIssuanceConfig)
	if !ok {
		return nil, notFound(collIssue, in.GetName())
	}
	next := proto.Clone(cur).(*cmpb.CertificateIssuanceConfig)
	if err := applyMask(next, in, req.GetUpdateMask(), "description", "labels"); err != nil {
		return nil, err
	}
	stamp(next, s.opts.Now(), false)
	s.res[cur.GetName()] = next
	return s.opLocked(cur.GetName(), "update", next)
}

func (s *Server) DeleteCertificateIssuanceConfig(_ context.Context, req *cmpb.DeleteCertificateIssuanceConfigRequest) (*longrunningpb.Operation, error) {
	return s.deleteUnused(collIssue, req.GetName())
}

// Trust config limits (certificate-manager/docs/quotas, "Trust config
// limits", read 2026-11-22).
const (
	maxTrustStores   = 1
	maxAnchorsAndInt = 200
	maxIntermediates = 100
	maxAllowlisted   = 500
)

func checkPEMCert(field, p string) error {
	b, _ := pem.Decode([]byte(p))
	if b == nil || b.Type != "CERTIFICATE" {
		return apierror.InvalidArgument("Invalid value for field '%s': not a PEM certificate.", field)
	}
	if _, err := x509.ParseCertificate(b.Bytes); err != nil {
		return apierror.InvalidArgument("Invalid value for field '%s': %v.", field, err)
	}
	return nil
}

func checkTrust(t *cmpb.TrustConfig) error {
	if len(t.GetTrustStores()) > maxTrustStores {
		return apierror.InvalidArgument("A trust config can have at most %d trust store, got %d.", maxTrustStores, len(t.GetTrustStores()))
	}
	for i, ts := range t.GetTrustStores() {
		if n := len(ts.GetTrustAnchors()) + len(ts.GetIntermediateCas()); n > maxAnchorsAndInt {
			return apierror.InvalidArgument("A trust store can have at most %d trust anchors and intermediate certificates, got %d.", maxAnchorsAndInt, n)
		}
		if n := len(ts.GetIntermediateCas()); n > maxIntermediates {
			return apierror.InvalidArgument("A trust store can have at most %d intermediate certificates, got %d.", maxIntermediates, n)
		}
		for j, a := range ts.GetTrustAnchors() {
			if err := checkPEMCert(fmt.Sprintf("trust_stores[%d].trust_anchors[%d].pem_certificate", i, j), a.GetPemCertificate()); err != nil {
				return err
			}
		}
		for j, a := range ts.GetIntermediateCas() {
			if err := checkPEMCert(fmt.Sprintf("trust_stores[%d].intermediate_cas[%d].pem_certificate", i, j), a.GetPemCertificate()); err != nil {
				return err
			}
		}
	}
	if n := len(t.GetAllowlistedCertificates()); n > maxAllowlisted {
		return apierror.InvalidArgument("A trust config can allowlist at most %d certificates, got %d.", maxAllowlisted, n)
	}
	for j, a := range t.GetAllowlistedCertificates() {
		if err := checkPEMCert(fmt.Sprintf("allowlisted_certificates[%d].pem_certificate", j), a.GetPemCertificate()); err != nil {
			return err
		}
	}
	return nil
}

func trustEtag(t *cmpb.TrustConfig) string {
	c := proto.Clone(t).(*cmpb.TrustConfig)
	c.Etag = ""
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func (s *Server) CreateTrustConfig(_ context.Context, req *cmpb.CreateTrustConfigRequest) (*longrunningpb.Operation, error) {
	if _, _, err := parseParent(req.GetParent(), false); err != nil {
		return nil, err
	}
	if err := checkID("trust_config_id", req.GetTrustConfigId()); err != nil {
		return nil, err
	}
	t, _ := proto.Clone(req.GetTrustConfig()).(*cmpb.TrustConfig)
	if t == nil {
		t = &cmpb.TrustConfig{}
	}
	if err := checkTrust(t); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/" + collTrust + "/" + req.GetTrustConfigId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; ok {
		return nil, apierror.AlreadyExists("TrustConfig '%s' already exists.", name)
	}
	t.Name = name
	stamp(t, s.opts.Now(), true)
	t.Etag = trustEtag(t)
	s.res[name] = t
	return s.opLocked(name, "create", t)
}

func (s *Server) GetTrustConfig(_ context.Context, req *cmpb.GetTrustConfigRequest) (*cmpb.TrustConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.getLocked(collTrust, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*cmpb.TrustConfig), nil
}

func (s *Server) ListTrustConfigs(_ context.Context, req *cmpb.ListTrustConfigsRequest) (*cmpb.ListTrustConfigsResponse, error) {
	if _, _, err := parseListParent(req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []*cmpb.TrustConfig
	for _, k := range s.childrenLocked(req.GetParent(), collTrust) {
		items = append(items, proto.Clone(s.res[k]).(*cmpb.TrustConfig))
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("trust:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &cmpb.ListTrustConfigsResponse{TrustConfigs: page, NextPageToken: next}, nil
}

func (s *Server) UpdateTrustConfig(_ context.Context, req *cmpb.UpdateTrustConfigRequest) (*longrunningpb.Operation, error) {
	in := req.GetTrustConfig()
	if err := checkName(in.GetName(), collTrust); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.res[in.GetName()].(*cmpb.TrustConfig)
	if !ok {
		return nil, notFound(collTrust, in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("The etag '%s' does not match the trust config's current etag.", in.GetEtag())
	}
	next := proto.Clone(cur).(*cmpb.TrustConfig)
	if err := applyMask(next, in, req.GetUpdateMask(), "description", "labels", "trust_stores", "allowlisted_certificates", "spiffe_trust_stores"); err != nil {
		return nil, err
	}
	if err := checkTrust(next); err != nil {
		return nil, err
	}
	stamp(next, s.opts.Now(), false)
	next.Etag = trustEtag(next)
	s.res[cur.GetName()] = next
	return s.opLocked(cur.GetName(), "update", next)
}

func (s *Server) DeleteTrustConfig(_ context.Context, req *cmpb.DeleteTrustConfigRequest) (*longrunningpb.Operation, error) {
	if err := checkName(req.GetName(), collTrust); err != nil {
		return nil, err
	}
	s.mu.Lock()
	cur, ok := s.res[req.GetName()].(*cmpb.TrustConfig)
	if ok && req.GetEtag() != "" && req.GetEtag() != cur.GetEtag() {
		s.mu.Unlock()
		return nil, apierror.Aborted("The etag '%s' does not match the trust config's current etag.", req.GetEtag())
	}
	s.mu.Unlock()
	return s.deleteUnused(collTrust, req.GetName())
}
