package certificatemanager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"time"

	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Limits from https://docs.cloud.google.com/certificate-manager/docs/quotas
// (read 2026-11-22): system limits, which cannot be changed.
const (
	maxDomainsLB       = 5
	maxDomainsDNS      = 100
	maxDomainLenLB     = 253
	maxDomainLenDNS    = 237
	maxDomainsTotalLen = 6300
)

// checkPair checks a self-managed certificate's PEM chain and key, and is
// its leaf.
func checkPair(certPEM, keyPEM string) (*x509.Certificate, error) {
	if certPEM == "" {
		return nil, apierror.InvalidArgument("Invalid argument: self_managed.pem_certificate is required.")
	}
	if keyPEM == "" {
		return nil, apierror.InvalidArgument("Invalid argument: self_managed.pem_private_key is required.")
	}
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		if strings.Contains(err.Error(), "does not match") {
			return nil, apierror.InvalidArgument("The private key does not match the certificate's public key.")
		}
		return nil, apierror.InvalidArgument("Failed to parse the certificate or the private key: %v.", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, apierror.InvalidArgument("Failed to parse the certificate: %v.", err)
	}
	switch k := leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		if b := k.N.BitLen(); b != 2048 && b != 3072 && b != 4096 {
			return nil, apierror.InvalidArgument("Unsupported key type RSA-%d: the supported key types are RSA-2048, RSA-3072, RSA-4096, ECDSA P-256 and ECDSA P-384.", b)
		}
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, apierror.InvalidArgument("Unsupported key type ECDSA %s: the supported key types are RSA-2048, RSA-3072, RSA-4096, ECDSA P-256 and ECDSA P-384.", k.Curve.Params().Name)
		}
	default:
		return nil, apierror.InvalidArgument("Unsupported key type: the supported key types are RSA-2048, RSA-3072, RSA-4096, ECDSA P-256 and ECDSA P-384.")
	}
	return leaf, nil
}

// leafOf is the first certificate of a PEM chain.
func leafOf(chain string) *x509.Certificate {
	b, _ := pem.Decode([]byte(chain))
	if b == nil {
		return nil
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil
	}
	return c
}

func validDomain(d string, wildcard bool) bool {
	d = strings.ToLower(strings.TrimSuffix(d, "."))
	if wildcard && strings.HasPrefix(d, "*.") {
		d = d[2:]
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func (s *Server) CreateCertificate(_ context.Context, req *cmpb.CreateCertificateRequest) (*longrunningpb.Operation, error) {
	name, _, err := s.createCertificateLocked(req)
	if err != nil {
		return nil, err
	}
	// DNS authorization and a certificate issuance config need nothing of
	// the load balancer: such a certificate is issued now, with no lock
	// held while the CA (the Compute Engine API) issues it.
	s.issue(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.res[name].(*cmpb.Certificate)
	if !ok {
		return nil, notFound(collCerts, name)
	}
	return s.opLocked(name, "create", s.outLocked(c))
}

func (s *Server) createCertificateLocked(req *cmpb.CreateCertificateRequest) (string, time.Time, error) {
	_, loc, err := parseParent(req.GetParent(), false)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := checkID("certificate_id", req.GetCertificateId()); err != nil {
		return "", time.Time{}, err
	}
	c, _ := proto.Clone(req.GetCertificate()).(*cmpb.Certificate)
	if c == nil {
		c = &cmpb.Certificate{}
	}
	name := req.GetParent() + "/" + collCerts + "/" + req.GetCertificateId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; ok {
		return "", time.Time{}, apierror.AlreadyExists("Certificate '%s' already exists.", name)
	}
	c.Name, c.UsedBy = name, nil
	now := s.opts.Now()
	switch {
	case c.GetSelfManaged() != nil:
		sm := c.GetSelfManaged()
		leaf, err := checkPair(sm.GetPemCertificate(), sm.GetPemPrivateKey())
		if err != nil {
			return "", time.Time{}, err
		}
		s.keys[name] = sm.GetPemPrivateKey()
		c.PemCertificate = sm.GetPemCertificate()
		c.Type = &cmpb.Certificate_SelfManaged{SelfManaged: &cmpb.Certificate_SelfManagedCertificate{}}
		c.SanDnsnames = leaf.DNSNames
		c.ExpireTime = timestamppb.New(leaf.NotAfter)
	case c.GetManaged() != nil:
		if err := s.checkManagedLocked(loc, c.GetManaged()); err != nil {
			return "", time.Time{}, err
		}
		mg := c.GetManaged()
		mg.State, mg.ProvisioningIssue, mg.AuthorizationAttemptInfo = cmpb.Certificate_ManagedCertificate_PROVISIONING, nil, nil
		c.PemCertificate, c.ExpireTime = "", nil
		c.SanDnsnames = append([]string(nil), mg.GetDomains()...)
		for _, d := range mg.GetDomains() {
			mg.AuthorizationAttemptInfo = append(mg.AuthorizationAttemptInfo, &cmpb.Certificate_ManagedCertificate_AuthorizationAttemptInfo{
				Domain: d, State: cmpb.Certificate_ManagedCertificate_AuthorizationAttemptInfo_AUTHORIZING, AttemptTime: timestamppb.New(now)})
		}
	case c.GetManagedIdentity() != nil:
		return "", time.Time{}, apierror.InvalidArgument("Managed identity certificates are not supported in location '%s': they are for Cloud Service Mesh workload identities.", loc)
	default:
		return "", time.Time{}, apierror.InvalidArgument("Invalid argument: one of self_managed or managed must be set.")
	}
	if c.GetScope() == cmpb.Certificate_ALL_REGIONS && loc != "global" {
		return "", time.Time{}, apierror.InvalidArgument("Scope ALL_REGIONS is only supported in location 'global'.")
	}
	if c.GetScope() == cmpb.Certificate_EDGE_CACHE && loc != "global" {
		return "", time.Time{}, apierror.InvalidArgument("Scope EDGE_CACHE is only supported in location 'global'.")
	}
	stamp(c, now, true)
	s.res[name] = c
	return name, now, s.saveLocked()
}

// checkManagedLocked checks a managed certificate's domains against its
// authorization.
func (s *Server) checkManagedLocked(loc string, mg *cmpb.Certificate_ManagedCertificate) error {
	ds := mg.GetDomains()
	if len(ds) == 0 {
		return apierror.InvalidArgument("Invalid argument: managed.domains must have at least one domain.")
	}
	dns, ic := mg.GetDnsAuthorizations(), mg.GetIssuanceConfig()
	if len(dns) > 0 && ic != "" {
		return apierror.InvalidArgument("Invalid argument: managed.dns_authorizations and managed.issuance_config cannot both be set.")
	}
	total := 0
	for _, d := range ds {
		total += len(d)
	}
	if total > maxDomainsTotalLen {
		return apierror.InvalidArgument("The total length of the certificate's domains is %d characters: the limit is %d.", total, maxDomainsTotalLen)
	}
	switch {
	case ic != "":
		if err := checkName(ic, collIssue); err != nil {
			return apierror.InvalidArgument("Invalid value for field 'managed.issuance_config': '%s'.", ic)
		}
		if _, ok := s.res[ic]; !ok {
			return apierror.InvalidArgument("CertificateIssuanceConfig '%s' not found.", ic)
		}
		if locationOf(ic) != loc {
			return apierror.InvalidArgument("CertificateIssuanceConfig '%s' must be in the certificate's location '%s'.", ic, loc)
		}
		for _, d := range ds {
			if !validDomain(d, true) {
				return apierror.InvalidArgument("Invalid domain name '%s'.", d)
			}
		}
	case len(dns) > 0:
		if len(ds) > maxDomainsDNS {
			return apierror.InvalidArgument("A Google-managed certificate with DNS authorization can have at most %d domains, got %d.", maxDomainsDNS, len(ds))
		}
		covered := map[string]bool{}
		for _, a := range dns {
			if err := checkName(a, collDNS); err != nil {
				return apierror.InvalidArgument("Invalid value for field 'managed.dns_authorizations': '%s'.", a)
			}
			o, ok := s.res[a].(*cmpb.DnsAuthorization)
			if !ok {
				return apierror.InvalidArgument("DnsAuthorization '%s' not found.", a)
			}
			if locationOf(a) != loc {
				return apierror.InvalidArgument("DnsAuthorization '%s' must be in the certificate's location '%s'.", a, loc)
			}
			covered[strings.ToLower(o.GetDomain())] = true
		}
		for _, d := range ds {
			if len(d) > maxDomainLenDNS {
				return apierror.InvalidArgument("Domain '%s' is longer than %d characters.", d, maxDomainLenDNS)
			}
			if !validDomain(d, true) {
				return apierror.InvalidArgument("Invalid domain name '%s'.", d)
			}
			if !covered[strings.TrimPrefix(strings.ToLower(d), "*.")] {
				return apierror.InvalidArgument("Domain '%s' is not covered by any of the certificate's DNS authorizations.", d)
			}
		}
	default:
		if loc != "global" {
			return apierror.InvalidArgument("Regional Google-managed certificates support only DNS authorization: set managed.dns_authorizations.")
		}
		if len(ds) > maxDomainsLB {
			return apierror.InvalidArgument("A Google-managed certificate with load balancer authorization can have at most %d domains, got %d.", maxDomainsLB, len(ds))
		}
		for _, d := range ds {
			if strings.Contains(d, "*") {
				return apierror.InvalidArgument("Wildcard domain '%s' requires DNS authorization: wildcard domains are supported only with DNS challenge resolution.", d)
			}
			if len(d) > maxDomainLenLB || !validDomain(d, false) {
				return apierror.InvalidArgument("Invalid domain name '%s'.", d)
			}
		}
	}
	return nil
}

// issue issues a managed certificate by the local CA when its
// authorization allows it now (DNS authorization, an issuance config) or
// served is set (load balancer authorization, once a proxy serves it),
// leaving it PROVISIONING when there is no CA. No lock is held while the
// CA, the Compute Engine API, issues it: Compute calls this API while it
// holds its own lock.
func (s *Server) issue(name string) { s.issueIf(name, false) }

func (s *Server) issueIf(name string, served bool) {
	s.mu.Lock()
	c, ok := s.res[name].(*cmpb.Certificate)
	mg := c.GetManaged()
	if !ok || mg == nil || mg.GetState() == cmpb.Certificate_ManagedCertificate_ACTIVE || s.issuer == nil || s.issuing[name] ||
		(!served && len(mg.GetDnsAuthorizations()) == 0 && mg.GetIssuanceConfig() == "") {
		s.mu.Unlock()
		return
	}
	if s.issuing == nil {
		s.issuing = map[string]bool{}
	}
	s.issuing[name] = true
	iss, domains := s.issuer, append([]string(nil), mg.GetDomains()...)
	s.mu.Unlock()
	chain, key, exp, err := iss.IssueLocalCertificate(domains)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.issuing, name)
	c, ok = s.res[name].(*cmpb.Certificate)
	if !ok || c.GetManaged() == nil {
		return
	}
	mg = c.GetManaged()
	now := s.opts.Now()
	if err != nil {
		mg.ProvisioningIssue = &cmpb.Certificate_ManagedCertificate_ProvisioningIssue{
			Reason: cmpb.Certificate_ManagedCertificate_ProvisioningIssue_REASON_UNSPECIFIED, Details: err.Error()}
		return
	}
	s.keys[name] = key
	c.PemCertificate, c.ExpireTime = chain, timestamppb.New(exp)
	mg.State, mg.ProvisioningIssue = cmpb.Certificate_ManagedCertificate_ACTIVE, nil
	for _, a := range mg.GetAuthorizationAttemptInfo() {
		a.State, a.AttemptTime = cmpb.Certificate_ManagedCertificate_AuthorizationAttemptInfo_AUTHORIZED, timestamppb.New(now)
	}
	c.UpdateTime = timestamppb.New(now)
	_ = s.saveLocked()
}

// outLocked is a certificate as the API answers it: its used_by filled.
func (s *Server) outLocked(c *cmpb.Certificate) *cmpb.Certificate {
	o := proto.Clone(c).(*cmpb.Certificate)
	o.UsedBy = nil
	for _, k := range sortedKeys(s.res) {
		if e, ok := s.res[k].(*cmpb.CertificateMapEntry); ok {
			for _, cn := range e.GetCertificates() {
				if cn == c.GetName() {
					o.UsedBy = append(o.UsedBy, &cmpb.Certificate_UsedBy{Name: k})
				}
			}
		}
	}
	return o
}

// provision issues the load-balancer-authorized certificates of project
// that a served target proxy's map names, as Google provisions one only
// then. It reads the targets outside the lock: the proxies' API may call
// back.
func (s *Server) provision(project string) {
	s.mu.Lock()
	f := s.targets
	s.mu.Unlock()
	if f == nil {
		return
	}
	served := map[string]bool{}
	for _, t := range f(project) {
		if len(t.IPs) > 0 {
			served[t.Map] = true
		}
	}
	if len(served) == 0 {
		return
	}
	s.mu.Lock()
	var names []string
	for _, k := range sortedKeys(s.res) {
		e, ok := s.res[k].(*cmpb.CertificateMapEntry)
		if !ok || !served[normMap(mapOfEntry(k))] {
			continue
		}
		names = append(names, e.GetCertificates()...)
	}
	s.mu.Unlock()
	for _, n := range names {
		s.issueIf(n, true)
	}
}

func (s *Server) GetCertificate(_ context.Context, req *cmpb.GetCertificateRequest) (*cmpb.Certificate, error) {
	if err := checkName(req.GetName(), collCerts); err != nil {
		return nil, err
	}
	s.provision(projectOf(req.GetName()))
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.res[req.GetName()].(*cmpb.Certificate)
	if !ok {
		return nil, notFound(collCerts, req.GetName())
	}
	return s.outLocked(c), nil
}

func (s *Server) ListCertificates(_ context.Context, req *cmpb.ListCertificatesRequest) (*cmpb.ListCertificatesResponse, error) {
	project, _, err := parseListParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	s.provision(project)
	s.mu.Lock()
	var items []*cmpb.Certificate
	for _, k := range s.childrenLocked(req.GetParent(), collCerts) {
		items = append(items, s.outLocked(s.res[k].(*cmpb.Certificate)))
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("certificates:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &cmpb.ListCertificatesResponse{Certificates: page, NextPageToken: next}, nil
}

func (s *Server) UpdateCertificate(_ context.Context, req *cmpb.UpdateCertificateRequest) (*longrunningpb.Operation, error) {
	in := req.GetCertificate()
	if err := checkName(in.GetName(), collCerts); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.res[in.GetName()].(*cmpb.Certificate)
	if !ok {
		return nil, notFound(collCerts, in.GetName())
	}
	next := proto.Clone(cur).(*cmpb.Certificate)
	// self_managed replaces the certificate and key; managed's fields are
	// immutable.
	if err := applyMask(next, in, req.GetUpdateMask(), "description", "labels", "self_managed"); err != nil {
		return nil, err
	}
	if hasPath(req.GetUpdateMask().GetPaths(), "self_managed") {
		if cur.GetSelfManaged() == nil {
			return nil, apierror.InvalidArgument("self_managed can be updated only on a self-managed certificate.")
		}
		sm := in.GetSelfManaged()
		leaf, err := checkPair(sm.GetPemCertificate(), sm.GetPemPrivateKey())
		if err != nil {
			return nil, err
		}
		s.keys[cur.GetName()] = sm.GetPemPrivateKey()
		next.PemCertificate, next.SanDnsnames, next.ExpireTime = sm.GetPemCertificate(), leaf.DNSNames, timestamppb.New(leaf.NotAfter)
		next.Type = &cmpb.Certificate_SelfManaged{SelfManaged: &cmpb.Certificate_SelfManagedCertificate{}}
	}
	stamp(next, s.opts.Now(), false)
	s.res[cur.GetName()] = next
	return s.opLocked(cur.GetName(), "update", s.outLocked(next))
}

func hasPath(paths []string, want string) bool {
	for _, raw := range paths {
		for _, p := range strings.Split(raw, ",") {
			if camelToSnake(strings.TrimSpace(p)) == want {
				return true
			}
		}
	}
	return false
}

func (s *Server) DeleteCertificate(_ context.Context, req *cmpb.DeleteCertificateRequest) (*longrunningpb.Operation, error) {
	if err := checkName(req.GetName(), collCerts); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.res[req.GetName()].(*cmpb.Certificate)
	if !ok {
		return nil, notFound(collCerts, req.GetName())
	}
	if used := s.outLocked(c).GetUsedBy(); len(used) > 0 {
		return nil, apierror.FailedPrecondition("Certificate '%s' is in use by '%s': remove it from there first.", req.GetName(), used[0].GetName())
	}
	if s.certUsers != nil {
		if u := s.certUsers(req.GetName()); len(u) > 0 {
			return nil, apierror.FailedPrecondition("Certificate '%s' is in use by '%s': remove it from there first.", req.GetName(), u[0])
		}
	}
	delete(s.res, req.GetName())
	delete(s.keys, req.GetName())
	return s.opLocked(req.GetName(), "delete", nil)
}

// pairLocked is a certificate's key pair, ok false while it has none.
func (s *Server) pairLocked(name string) (tls.Certificate, bool) {
	c, ok := s.res[name].(*cmpb.Certificate)
	if !ok || c.GetPemCertificate() == "" || s.keys[name] == "" {
		return tls.Certificate{}, false
	}
	pair, err := tls.X509KeyPair([]byte(c.GetPemCertificate()), []byte(s.keys[name]))
	if err != nil {
		return tls.Certificate{}, false
	}
	pair.Leaf, _ = x509.ParseCertificate(pair.Certificate[0])
	if pair.Leaf != nil && s.opts.Now().After(pair.Leaf.NotAfter) {
		return tls.Certificate{}, false
	}
	return pair, true
}
