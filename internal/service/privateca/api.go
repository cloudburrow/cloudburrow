package privateca

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// API is google.cloud.security.privateca.v1.CertificateAuthorityService.
type API struct {
	privatecapb.UnimplementedCertificateAuthorityServiceServer
	s *Server
}

// API returns the service over s, for gRPC, the transcoder and the console.
func (s *Server) API() *API { return &API{s: s} }

// Register adds the service, its operations, locations and IAM mixins to a
// gRPC server or to the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	privatecapb.RegisterCertificateAuthorityServiceServer(g, s.API())
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	registerLocations(g, s)
	registerIAM(g, s)
}

// done records a finished operation for target, with resp as its response.
// The caller holds mu.
func (s *Server) done(location, verb, target string, resp proto.Message) (*longrunningpb.Operation, error) {
	now := timestamppb.New(s.now())
	md, err := anypb.New(&privatecapb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	op := &longrunningpb.Operation{Name: location + "/operations/operation-" + randomHex(16), Done: true, Metadata: md,
		Result: &longrunningpb.Operation_Response{Response: r}}
	s.ops[op.Name] = op
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(op), nil
}

func checkLocation(field, parent string) error {
	if !locationRE.MatchString(parent) {
		return apierror.InvalidArgument("%s %q must be projects/{project}/locations/{location}", field, parent)
	}
	return nil
}

func checkID(field, id string) error {
	if !idRE.MatchString(id) {
		return apierror.InvalidArgument("%s %q must match [a-zA-Z0-9_-]{1,63}", field, id)
	}
	return nil
}

// refuseFilter answers 501 for a list filter or order_by CloudBurrow does
// not evaluate, rather than ignoring it.
func refuseFilter(filter, orderBy string) error {
	if strings.TrimSpace(filter) != "" {
		return apierror.Unimplemented("list filter is not implemented in CloudBurrow yet")
	}
	if o := strings.TrimSpace(orderBy); o != "" && o != "name" && o != "name asc" {
		return apierror.Unimplemented("order_by %q is not implemented in CloudBurrow yet: only name", orderBy)
	}
	return nil
}

func page[T any](scope string, keys []string, token string, size int32, get func(string) T) ([]T, string, error) {
	pg, next, err := paging.Page(scope, keys, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	out := make([]T, 0, len(pg))
	for _, k := range pg {
		out = append(out, get(k))
	}
	return out, next, nil
}

// pool is the named pool; the caller holds mu.
func (s *Server) pool(name string) (*poolRecord, error) {
	if !poolRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/caPools/{ca_pool}", name)
	}
	p, ok := s.pools[name]
	if !ok {
		return nil, apierror.NotFound("CaPool %s not found", name)
	}
	return p, nil
}

// ---- CA pools ----

func (a *API) CreateCaPool(_ context.Context, req *privatecapb.CreateCaPoolRequest) (*longrunningpb.Operation, error) {
	s := a.s
	if err := checkLocation("parent", req.GetParent()); err != nil {
		return nil, err
	}
	if err := checkID("ca_pool_id", req.GetCaPoolId()); err != nil {
		return nil, err
	}
	p := req.GetCaPool()
	if p == nil {
		return nil, apierror.InvalidArgument("ca_pool is required")
	}
	if p.GetTier() == privatecapb.CaPool_TIER_UNSPECIFIED {
		return nil, apierror.InvalidArgument("ca_pool.tier is required: ENTERPRISE or DEVOPS")
	}
	if p.GetEncryptionSpec().GetCloudKmsKey() != "" {
		return nil, apierror.Unimplemented("ca_pool.encryption_spec (CMEK) is not implemented in CloudBurrow yet")
	}
	if err := checkPolicy(p.GetIssuancePolicy()); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/caPools/" + req.GetCaPoolId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pools[name]; ok {
		return nil, apierror.AlreadyExists("CaPool %s already exists", name)
	}
	rec := clone(p)
	rec.Name = name
	s.pools[name] = &poolRecord{Pool: rec}
	return s.done(req.GetParent(), "create", name, rec)
}

// checkPolicy validates an issuance policy's durations.
func checkPolicy(p *privatecapb.CaPool_IssuancePolicy) error {
	if p == nil {
		return nil
	}
	if d := p.GetMaximumLifetime(); d != nil && (d.CheckValid() != nil || d.AsDuration() < 0) {
		return apierror.InvalidArgument("issuance_policy.maximum_lifetime must be a non-negative duration")
	}
	if d := p.GetBackdateDuration(); d != nil && (d.CheckValid() != nil || d.AsDuration() < 0 || d.AsDuration() > 48*time.Hour) {
		// "This value must be between 1 minute and 48 hours" is not
		// enforced below one minute: zero means unset.
		return apierror.InvalidArgument("issuance_policy.backdate_duration must be between 1 minute and 48 hours")
	}
	if ic := p.GetIdentityConstraints(); ic != nil && ic.GetCelExpression().GetExpression() != "" {
		return apierror.Unimplemented("issuance_policy.identity_constraints.cel_expression is not implemented in CloudBurrow yet")
	}
	return nil
}

func (a *API) GetCaPool(_ context.Context, req *privatecapb.GetCaPoolRequest) (*privatecapb.CaPool, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	p, err := a.s.pool(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(p.Pool), nil
}

func (a *API) ListCaPools(_ context.Context, req *privatecapb.ListCaPoolsRequest) (*privatecapb.ListCaPoolsResponse, error) {
	if err := checkLocation("parent", req.GetParent()); err != nil {
		return nil, err
	}
	if err := refuseFilter(req.GetFilter(), req.GetOrderBy()); err != nil {
		return nil, err
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	var keys []string
	for _, k := range sortedKeys(a.s.pools, req.GetParent()+"/caPools/") {
		if poolRE.MatchString(k) {
			keys = append(keys, k)
		}
	}
	out, next, err := page("privateca-pools:"+req.GetParent(), keys, req.GetPageToken(), req.GetPageSize(),
		func(k string) *privatecapb.CaPool { return clone(a.s.pools[k].Pool) })
	if err != nil {
		return nil, err
	}
	return &privatecapb.ListCaPoolsResponse{CaPools: out, NextPageToken: next}, nil
}

func (a *API) UpdateCaPool(_ context.Context, req *privatecapb.UpdateCaPoolRequest) (*longrunningpb.Operation, error) {
	in := req.GetCaPool()
	if in == nil {
		return nil, apierror.InvalidArgument("ca_pool is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.pool(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(p.Pool)
	for _, path := range paths {
		switch path {
		case "labels":
			next.Labels = in.GetLabels()
		case "issuance_policy", "issuancePolicy":
			if err := checkPolicy(in.GetIssuancePolicy()); err != nil {
				return nil, err
			}
			next.IssuancePolicy = in.GetIssuancePolicy()
		case "publishing_options", "publishingOptions":
			next.PublishingOptions = in.GetPublishingOptions()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated: labels, issuance_policy and publishing_options can", path)
		}
	}
	p.Pool = next
	loc, _, _ := strings.Cut(next.Name, "/caPools/")
	return s.done(loc, "update", next.Name, next)
}

// DeleteCaPool deletes a pool holding no CA. A CA in its 30-day grace
// period still counts (UNVERIFIED: Google's delete page states no
// precondition, and a pool's deleted CAs can still be undeleted into it).
// Docs re-read 2026-11-18: https://docs.cloud.google.com/certificate-authority-service/docs/reference/rest/v1/projects.locations.caPools/delete
func (a *API) DeleteCaPool(_ context.Context, req *privatecapb.DeleteCaPoolRequest) (*longrunningpb.Operation, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.pool(req.GetName()); err != nil {
		return nil, err
	}
	s.purge()
	if n := len(sortedKeys(s.cas, req.GetName()+"/certificateAuthorities/")); n > 0 {
		return nil, apierror.FailedPrecondition("CaPool %s still contains %d CertificateAuthorities; delete them, with skip_grace_period, first", req.GetName(), n)
	}
	delete(s.pools, req.GetName())
	for _, k := range sortedKeys(s.certs, req.GetName()+"/certificates/") {
		delete(s.certs, k)
	}
	loc, _, _ := strings.Cut(req.GetName(), "/caPools/")
	return s.done(loc, "delete", req.GetName(), &emptypb.Empty{})
}

// FetchCaCerts is the pool's trust anchors: one chain per ENABLED,
// DISABLED or STAGED CA ("The CA will be part of the CaPool's trust
// anchor").
func (a *API) FetchCaCerts(_ context.Context, req *privatecapb.FetchCaCertsRequest) (*privatecapb.FetchCaCertsResponse, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.pool(req.GetCaPool()); err != nil {
		return nil, err
	}
	out := &privatecapb.FetchCaCertsResponse{}
	for _, k := range sortedKeys(s.cas, req.GetCaPool()+"/certificateAuthorities/") {
		switch s.cas[k].CA.GetState() {
		case privatecapb.CertificateAuthority_ENABLED, privatecapb.CertificateAuthority_DISABLED, privatecapb.CertificateAuthority_STAGED:
			out.CaCerts = append(out.CaCerts, &privatecapb.FetchCaCertsResponse_CertChain{Certificates: s.cas[k].CA.GetPemCaCertificates()})
		}
	}
	return out, nil
}

// ---- Certificate authorities ----

// purge removes CAs whose grace period has passed; the caller holds mu.
func (s *Server) purge() {
	now := s.now()
	for n, c := range s.cas {
		if c.CA.GetState() == privatecapb.CertificateAuthority_DELETED && c.CA.GetExpireTime() != nil && !now.Before(c.CA.GetExpireTime().AsTime()) {
			delete(s.cas, n)
		}
	}
}

func (s *Server) ca(name string) (*caRecord, error) {
	if !caRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/caPools/{ca_pool}/certificateAuthorities/{certificate_authority}", name)
	}
	s.purge()
	c, ok := s.cas[name]
	if !ok {
		return nil, apierror.NotFound("CertificateAuthority %s not found", name)
	}
	return c, nil
}

func (a *API) CreateCertificateAuthority(_ context.Context, req *privatecapb.CreateCertificateAuthorityRequest) (*longrunningpb.Operation, error) {
	s := a.s
	if err := checkID("certificate_authority_id", req.GetCertificateAuthorityId()); err != nil {
		return nil, err
	}
	in := req.GetCertificateAuthority()
	if in == nil {
		return nil, apierror.InvalidArgument("certificate_authority is required")
	}
	switch in.GetType() {
	case privatecapb.CertificateAuthority_SELF_SIGNED:
	case privatecapb.CertificateAuthority_SUBORDINATE:
		return nil, apierror.Unimplemented("subordinate CertificateAuthorities are not implemented in CloudBurrow yet (#1834 phase 2)")
	default:
		return nil, apierror.InvalidArgument("certificate_authority.type is required: SELF_SIGNED or SUBORDINATE")
	}
	if in.GetConfig() == nil {
		return nil, apierror.InvalidArgument("certificate_authority.config is required")
	}
	if !isCAConfig(in.GetConfig().GetX509Config()) {
		return nil, apierror.InvalidArgument("certificate_authority.config.x509_config.ca_options.is_ca must be true for a CertificateAuthority")
	}
	if in.GetKeySpec() == nil {
		return nil, apierror.InvalidArgument("certificate_authority.key_spec is required")
	}
	if in.GetKeySpec().GetCloudKmsKeyVersion() != "" {
		return nil, apierror.Unimplemented("key_spec.cloud_kms_key_version (a CA key held in Cloud KMS) is not implemented in CloudBurrow yet")
	}
	if in.GetGcsBucket() != "" {
		return nil, apierror.Unimplemented("certificate_authority.gcs_bucket is not implemented in CloudBurrow yet: CA certificates and CRLs are not published")
	}
	if in.GetLifetime() == nil || in.GetLifetime().CheckValid() != nil || in.GetLifetime().AsDuration() <= 0 {
		return nil, apierror.InvalidArgument("certificate_authority.lifetime must be a positive duration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pool, err := s.pool(req.GetParent())
	if err != nil {
		return nil, err
	}
	name := req.GetParent() + "/certificateAuthorities/" + req.GetCertificateAuthorityId()
	s.purge()
	if _, ok := s.cas[name]; ok {
		return nil, apierror.AlreadyExists("CertificateAuthority %s already exists", name)
	}
	loc, _, _ := strings.Cut(req.GetParent(), "/caPools/")
	if len(sortedKeys(s.cas, loc+"/")) >= MaxCAsPerLocation {
		return nil, status.Errorf(codes.ResourceExhausted, "quota exceeded: at most %d CAs per location per project", MaxCAsPerLocation)
	}
	key, sigAlg, err := newKey(in.GetKeySpec().GetAlgorithm())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	subj, err := subjectName(in.GetConfig().GetSubjectConfig().GetSubject())
	if err != nil {
		return nil, err
	}
	now := s.now()
	start := now.Truncate(time.Second)
	t := &x509.Certificate{SerialNumber: serial(), Subject: subj, NotBefore: start, NotAfter: start.Add(in.GetLifetime().AsDuration()),
		SignatureAlgorithm: sigAlg}
	if err := applySANs(t, in.GetConfig().GetSubjectConfig().GetSubjectAltName()); err != nil {
		return nil, err
	}
	if err := applyX509(t, in.GetConfig().GetX509Config()); err != nil {
		return nil, err
	}
	if t.SubjectKeyId, err = keyID(key.Public()); err != nil {
		return nil, apierror.Wrap(err)
	}
	der, err := x509.CreateCertificate(nil, t, t, key.Public(), key)
	if err != nil {
		return nil, apierror.InvalidArgument("the CA certificate cannot be made: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	rec := clone(in)
	rec.Name = name
	rec.Tier = pool.Pool.GetTier()
	// "All root CAs are created in the STAGED state by default."
	// https://docs.cloud.google.com/certificate-authority-service/docs/managing-ca-state
	rec.State = privatecapb.CertificateAuthority_STAGED
	rec.PemCaCertificates = []string{pemCert(der)}
	rec.CaCertificateDescriptions = []*privatecapb.CertificateDescription{describe(cert)}
	rec.CreateTime = timestamppb.New(now)
	rec.UpdateTime = rec.CreateTime
	rec.AccessUrls = &privatecapb.CertificateAuthority_AccessUrls{}
	s.cas[name] = &caRecord{CA: rec, Key: key, Cert: cert}
	return s.done(loc, "create", name, rec)
}

func (a *API) GetCertificateAuthority(_ context.Context, req *privatecapb.GetCertificateAuthorityRequest) (*privatecapb.CertificateAuthority, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	c, err := a.s.ca(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(c.CA), nil
}

func (a *API) ListCertificateAuthorities(_ context.Context, req *privatecapb.ListCertificateAuthoritiesRequest) (*privatecapb.ListCertificateAuthoritiesResponse, error) {
	if err := refuseFilter(req.GetFilter(), req.GetOrderBy()); err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if !poolRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be a CaPool name", req.GetParent())
	}
	s.purge()
	out, next, err := page("privateca-cas:"+req.GetParent(), sortedKeys(s.cas, req.GetParent()+"/certificateAuthorities/"), req.GetPageToken(), req.GetPageSize(),
		func(k string) *privatecapb.CertificateAuthority { return clone(s.cas[k].CA) })
	if err != nil {
		return nil, err
	}
	return &privatecapb.ListCertificateAuthoritiesResponse{CertificateAuthorities: out, NextPageToken: next}, nil
}

// transition moves a CA from one of from to to.
func (s *Server) transition(name, verb string, to privatecapb.CertificateAuthority_State, from ...privatecapb.CertificateAuthority_State) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.ca(name)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, f := range from {
		ok = ok || c.CA.GetState() == f
	}
	if !ok {
		return nil, wrongState(c.CA, verb, from...)
	}
	next := clone(c.CA)
	next.State = to
	next.UpdateTime = timestamppb.New(s.now())
	if to != privatecapb.CertificateAuthority_DELETED {
		next.DeleteTime, next.ExpireTime = nil, nil
	}
	c.CA = next
	loc, _, _ := strings.Cut(name, "/caPools/")
	return s.done(loc, verb, name, next)
}

// EnableCertificateAuthority: "To enable a CA that is in the STAGED or
// DISABLED state".
func (a *API) EnableCertificateAuthority(_ context.Context, req *privatecapb.EnableCertificateAuthorityRequest) (*longrunningpb.Operation, error) {
	return a.s.transition(req.GetName(), "enable", privatecapb.CertificateAuthority_ENABLED,
		privatecapb.CertificateAuthority_STAGED, privatecapb.CertificateAuthority_DISABLED)
}

// DisableCertificateAuthority disables an ENABLED or STAGED CA. Which
// states Google disables from is UNVERIFIED (docs re-read 2026-11-18:
// <https://docs.cloud.google.com/certificate-authority-service/docs/managing-ca-state>
// names none).
func (a *API) DisableCertificateAuthority(_ context.Context, req *privatecapb.DisableCertificateAuthorityRequest) (*longrunningpb.Operation, error) {
	return a.s.transition(req.GetName(), "disable", privatecapb.CertificateAuthority_DISABLED,
		privatecapb.CertificateAuthority_ENABLED, privatecapb.CertificateAuthority_STAGED)
}

// UndeleteCertificateAuthority: "CA state changes from DELETED to DISABLED
// if successfully restored", only in the grace period.
func (a *API) UndeleteCertificateAuthority(_ context.Context, req *privatecapb.UndeleteCertificateAuthorityRequest) (*longrunningpb.Operation, error) {
	return a.s.transition(req.GetName(), "undelete", privatecapb.CertificateAuthority_DISABLED, privatecapb.CertificateAuthority_DELETED)
}

// DeleteCertificateAuthority soft-deletes a DISABLED, STAGED or
// AWAITING_USER_ACTIVATION CA for the 30-day grace period, or purges it
// with skip_grace_period. An ENABLED CA is refused (UNVERIFIED (docs
// re-read 2026-11-18:
// <https://docs.cloud.google.com/certificate-authority-service/docs/reference/rest/v1/projects.locations.caPools.certificateAuthorities/delete>
// states no state precondition; gcloud disables first). A CA
// with active certificates, "both unrevoked and unexpired", is refused
// unless ignore_active_certificates.
func (a *API) DeleteCertificateAuthority(_ context.Context, req *privatecapb.DeleteCertificateAuthorityRequest) (*longrunningpb.Operation, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.ca(req.GetName())
	if err != nil {
		return nil, err
	}
	switch c.CA.GetState() {
	case privatecapb.CertificateAuthority_DISABLED, privatecapb.CertificateAuthority_STAGED, privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION:
	default:
		return nil, wrongState(c.CA, "deleted", privatecapb.CertificateAuthority_DISABLED, privatecapb.CertificateAuthority_STAGED,
			privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION)
	}
	if !req.GetIgnoreActiveCertificates() {
		pool, _, _ := strings.Cut(req.GetName(), "/certificateAuthorities/")
		now := s.now()
		for _, k := range sortedKeys(s.certs, pool+"/certificates/") {
			crt := s.certs[k]
			if crt.GetIssuerCertificateAuthority() == req.GetName() && crt.GetRevocationDetails() == nil &&
				now.Before(crt.GetCertificateDescription().GetSubjectDescription().GetNotAfterTime().AsTime()) {
				return nil, apierror.FailedPrecondition("CertificateAuthority %s has active certificates, such as %s; revoke them or set ignore_active_certificates", req.GetName(), k)
			}
		}
	}
	now := s.now()
	next := clone(c.CA)
	next.State = privatecapb.CertificateAuthority_DELETED
	next.DeleteTime = timestamppb.New(now)
	next.UpdateTime = next.DeleteTime
	if req.GetSkipGracePeriod() {
		next.ExpireTime = next.DeleteTime
	} else {
		next.ExpireTime = timestamppb.New(now.Add(GracePeriod))
	}
	c.CA = next
	if req.GetSkipGracePeriod() {
		delete(s.cas, req.GetName())
	}
	loc, _, _ := strings.Cut(req.GetName(), "/caPools/")
	return s.done(loc, "delete", req.GetName(), next)
}

func (a *API) UpdateCertificateAuthority(_ context.Context, req *privatecapb.UpdateCertificateAuthorityRequest) (*longrunningpb.Operation, error) {
	in := req.GetCertificateAuthority()
	if in == nil {
		return nil, apierror.InvalidArgument("certificate_authority is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.ca(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(c.CA)
	for _, p := range paths {
		switch p {
		case "labels":
			next.Labels = in.GetLabels()
		case "user_defined_access_urls", "userDefinedAccessUrls":
			next.UserDefinedAccessUrls = in.GetUserDefinedAccessUrls()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated: labels and user_defined_access_urls can", p)
		}
	}
	next.UpdateTime = timestamppb.New(s.now())
	c.CA = next
	loc, _, _ := strings.Cut(in.GetName(), "/caPools/")
	return s.done(loc, "update", in.GetName(), next)
}

func (a *API) ActivateCertificateAuthority(context.Context, *privatecapb.ActivateCertificateAuthorityRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("ActivateCertificateAuthority (subordinate CAs) is not implemented in CloudBurrow yet (#1834 phase 2)")
}

func (a *API) FetchCertificateAuthorityCsr(context.Context, *privatecapb.FetchCertificateAuthorityCsrRequest) (*privatecapb.FetchCertificateAuthorityCsrResponse, error) {
	return nil, apierror.Unimplemented("FetchCertificateAuthorityCsr (subordinate CAs) is not implemented in CloudBurrow yet (#1834 phase 2)")
}

// ---- Certificates ----

func (a *API) CreateCertificate(_ context.Context, req *privatecapb.CreateCertificateRequest) (*privatecapb.Certificate, error) {
	s := a.s
	in := req.GetCertificate()
	if in == nil {
		return nil, apierror.InvalidArgument("certificate is required")
	}
	if in.GetCertificateTemplate() != "" {
		return nil, apierror.Unimplemented("certificate_template is not implemented in CloudBurrow yet (#1834 phase 2)")
	}
	switch in.GetSubjectMode() {
	case privatecapb.SubjectRequestMode_SUBJECT_REQUEST_MODE_UNSPECIFIED, privatecapb.SubjectRequestMode_DEFAULT:
	default:
		return nil, apierror.Unimplemented("subject_mode %s is not implemented in CloudBurrow yet", in.GetSubjectMode())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pool, err := s.pool(req.GetParent())
	if err != nil {
		return nil, err
	}
	enterprise := pool.Pool.GetTier() == privatecapb.CaPool_ENTERPRISE
	id := req.GetCertificateId()
	switch {
	case id == "" && enterprise:
		return nil, apierror.InvalidArgument("certificate_id is required for a CaPool in the ENTERPRISE tier")
	case id == "":
		id = time.Now().UTC().Format("20060102-150405") + "-" + randomHex(4)
	default:
		if err := checkID("certificate_id", id); err != nil {
			return nil, err
		}
	}
	name := req.GetParent() + "/certificates/" + id
	if _, ok := s.certs[name]; ok {
		return nil, apierror.AlreadyExists("Certificate %s already exists", name)
	}
	issuer, err := s.issuer(req.GetParent(), req.GetIssuingCertificateAuthorityId())
	if err != nil {
		return nil, err
	}
	policy := pool.Pool.GetIssuancePolicy()
	modes := policy.GetAllowedIssuanceModes()
	t := &x509.Certificate{SerialNumber: serial(), SignatureAlgorithm: signatureAlgorithm(issuer.Cert)}
	var x509Req *privatecapb.X509Parameters
	var pub any
	switch cfg := in.GetCertificateConfig().(type) {
	case *privatecapb.Certificate_PemCsr:
		if modes != nil && !modes.GetAllowCsrBasedIssuance() {
			return nil, apierror.FailedPrecondition("CaPool %s does not allow CSR-based issuance", req.GetParent())
		}
		blk, _ := pem.Decode([]byte(cfg.PemCsr))
		if blk == nil || blk.Type != "CERTIFICATE REQUEST" && blk.Type != "NEW CERTIFICATE REQUEST" {
			return nil, apierror.InvalidArgument("pem_csr is not a PEM certificate signing request")
		}
		csr, err := x509.ParseCertificateRequest(blk.Bytes)
		if err != nil {
			return nil, apierror.InvalidArgument("pem_csr: %v", err)
		}
		if err := csr.CheckSignature(); err != nil {
			return nil, apierror.InvalidArgument("pem_csr signature: %v", err)
		}
		pub = csr.PublicKey
		if passthroughSubject(policy) {
			t.Subject = pkix.Name{CommonName: csr.Subject.CommonName, Country: csr.Subject.Country, Organization: csr.Subject.Organization,
				OrganizationalUnit: csr.Subject.OrganizationalUnit, Locality: csr.Subject.Locality, Province: csr.Subject.Province,
				StreetAddress: csr.Subject.StreetAddress, PostalCode: csr.Subject.PostalCode}
		}
		if passthroughSANs(policy) {
			t.DNSNames, t.EmailAddresses, t.IPAddresses, t.URIs = csr.DNSNames, csr.EmailAddresses, csr.IPAddresses, csr.URIs
		}
	case *privatecapb.Certificate_Config:
		if modes != nil && !modes.GetAllowConfigBasedIssuance() {
			return nil, apierror.FailedPrecondition("CaPool %s does not allow config-based issuance", req.GetParent())
		}
		c := cfg.Config
		if c.GetSubjectConfig() == nil {
			return nil, apierror.InvalidArgument("config.subject_config is required")
		}
		if c.GetX509Config() == nil {
			return nil, apierror.InvalidArgument("config.x509_config is required")
		}
		if pub, err = parsePublicKey(c.GetPublicKey()); err != nil {
			return nil, err
		}
		if passthroughSubject(policy) {
			if t.Subject, err = subjectName(c.GetSubjectConfig().GetSubject()); err != nil {
				return nil, err
			}
		}
		if passthroughSANs(policy) {
			if err := applySANs(t, c.GetSubjectConfig().GetSubjectAltName()); err != nil {
				return nil, err
			}
		}
		x509Req = c.GetX509Config()
		if kid := c.GetSubjectKeyId().GetKeyId(); kid != "" {
			return nil, apierror.Unimplemented("config.subject_key_id is not implemented in CloudBurrow yet")
		}
	default:
		return nil, apierror.InvalidArgument("certificate needs pem_csr or config")
	}
	if err := checkKeyType(pub, policy.GetAllowedKeyTypes()); err != nil {
		return nil, err
	}
	if t.Subject.String() == "" && len(t.DNSNames)+len(t.EmailAddresses)+len(t.IPAddresses)+len(t.URIs) == 0 {
		return nil, apierror.InvalidArgument("the certificate needs a subject or a subject alternative name")
	}
	if err := applyX509(t, mergeX509(x509Req, policy.GetBaselineValues())); err != nil {
		return nil, err
	}
	now := s.now()
	if in.GetRequestedNotBeforeTime() != nil {
		return nil, apierror.Unimplemented("requested_not_before_time is not implemented in CloudBurrow yet")
	}
	if t.NotBefore, t.NotAfter, err = validity(now, in.GetLifetime(), policy.GetMaximumLifetime(), policy.GetBackdateDuration(), issuer.Cert.NotAfter); err != nil {
		return nil, err
	}
	if t.SubjectKeyId, err = keyID(pub); err != nil {
		return nil, apierror.InvalidArgument("the public key: %v", err)
	}
	der, err := x509.CreateCertificate(nil, t, issuer.Cert, pub, issuer.Key)
	if err != nil {
		return nil, apierror.InvalidArgument("the certificate cannot be issued: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	out := clone(in)
	out.Name = name
	out.IssuerCertificateAuthority = issuer.CA.GetName()
	out.PemCertificate = pemCert(der)
	out.PemCertificateChain = issuer.CA.GetPemCaCertificates()
	out.CertificateDescription = describe(cert)
	out.CreateTime = timestamppb.New(now)
	out.UpdateTime = out.CreateTime
	out.SubjectMode = privatecapb.SubjectRequestMode_DEFAULT
	out.Lifetime = durationpb.New(t.NotAfter.Sub(t.NotBefore))
	// A DevOps pool "does not store certificates": the certificate is
	// returned and not kept, so Get, List and Revoke cannot find it.
	if enterprise {
		s.certs[name] = clone(out)
		if err := s.save(); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	return out, nil
}

// passthroughSubject is whether the request's subject is copied, by the
// pool's identity_constraints (all of it when there are none).
func passthroughSubject(p *privatecapb.CaPool_IssuancePolicy) bool {
	ic := p.GetIdentityConstraints()
	return ic == nil || ic.GetAllowSubjectPassthrough()
}

func passthroughSANs(p *privatecapb.CaPool_IssuancePolicy) bool {
	ic := p.GetIdentityConstraints()
	return ic == nil || ic.GetAllowSubjectAltNamesPassthrough()
}

// issuer is the CA that signs: the one named, which must be ENABLED, or
// the first ENABLED CA of the pool. "STAGED: ... will not be used to issue
// certificates from the CaPool."
func (s *Server) issuer(pool, id string) (*caRecord, error) {
	if id != "" {
		c, err := s.ca(pool + "/certificateAuthorities/" + id)
		if err != nil {
			return nil, err
		}
		if c.CA.GetState() != privatecapb.CertificateAuthority_ENABLED {
			return nil, apierror.FailedPrecondition("CertificateAuthority %s is %s, not ENABLED, so it cannot issue certificates", c.CA.GetName(), c.CA.GetState())
		}
		return c, nil
	}
	now := s.now()
	for _, k := range sortedKeys(s.cas, pool+"/certificateAuthorities/") {
		c := s.cas[k]
		if c.CA.GetState() == privatecapb.CertificateAuthority_ENABLED && now.Before(c.Cert.NotAfter) {
			return c, nil
		}
	}
	return nil, apierror.FailedPrecondition("CaPool %s has no ENABLED CertificateAuthority to issue the certificate", pool)
}

func (s *Server) cert(name string) (*privatecapb.Certificate, error) {
	if !certRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/caPools/{ca_pool}/certificates/{certificate}", name)
	}
	c, ok := s.certs[name]
	if !ok {
		return nil, apierror.NotFound("Certificate %s not found", name)
	}
	return c, nil
}

func (a *API) GetCertificate(_ context.Context, req *privatecapb.GetCertificateRequest) (*privatecapb.Certificate, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	c, err := a.s.cert(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(c), nil
}

func (a *API) ListCertificates(_ context.Context, req *privatecapb.ListCertificatesRequest) (*privatecapb.ListCertificatesResponse, error) {
	if err := refuseFilter(req.GetFilter(), req.GetOrderBy()); err != nil {
		return nil, err
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if !poolRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be a CaPool name", req.GetParent())
	}
	out, next, err := page("privateca-certs:"+req.GetParent(), sortedKeys(s.certs, req.GetParent()+"/certificates/"), req.GetPageToken(), req.GetPageSize(),
		func(k string) *privatecapb.Certificate { return clone(s.certs[k]) })
	if err != nil {
		return nil, err
	}
	return &privatecapb.ListCertificatesResponse{Certificates: out, NextPageToken: next}, nil
}

// RevokeCertificate revokes a certificate once; a second revocation is
// FAILED_PRECONDITION (UNVERIFIED (docs re-read 2026-11-18:
// <https://docs.cloud.google.com/certificate-authority-service/docs/revoking-certificates>
// says nothing of a second revocation)).
func (a *API) RevokeCertificate(_ context.Context, req *privatecapb.RevokeCertificateRequest) (*privatecapb.Certificate, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cert(req.GetName())
	if err != nil {
		return nil, err
	}
	if c.GetRevocationDetails() != nil {
		return nil, apierror.FailedPrecondition("Certificate %s is already revoked", req.GetName())
	}
	revoked := 0
	for _, o := range s.certs {
		if o.GetIssuerCertificateAuthority() == c.GetIssuerCertificateAuthority() && o.GetRevocationDetails() != nil &&
			s.now().Before(o.GetCertificateDescription().GetSubjectDescription().GetNotAfterTime().AsTime()) {
			revoked++
		}
	}
	if revoked >= MaxRevokedPerCA {
		return nil, apierror.FailedPrecondition("CertificateAuthority %s already has %d unexpired revoked certificates, the most a CRL holds", c.GetIssuerCertificateAuthority(), MaxRevokedPerCA)
	}
	next := clone(c)
	now := timestamppb.New(s.now())
	next.RevocationDetails = &privatecapb.Certificate_RevocationDetails{RevocationState: req.GetReason(), RevocationTime: now}
	next.UpdateTime = now
	s.certs[req.GetName()] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

func (a *API) UpdateCertificate(_ context.Context, req *privatecapb.UpdateCertificateRequest) (*privatecapb.Certificate, error) {
	in := req.GetCertificate()
	paths := req.GetUpdateMask().GetPaths()
	if in == nil || len(paths) == 0 {
		return nil, apierror.InvalidArgument("certificate and update_mask are required")
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cert(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(c)
	for _, p := range paths {
		if p != "labels" {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated: only labels can", p)
		}
		next.Labels = in.GetLabels()
	}
	next.UpdateTime = timestamppb.New(s.now())
	s.certs[in.GetName()] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

// ---- Not yet served (#1834 phase 2) ----

func (a *API) GetCertificateRevocationList(context.Context, *privatecapb.GetCertificateRevocationListRequest) (*privatecapb.CertificateRevocationList, error) {
	return nil, apierror.Unimplemented("CertificateRevocationLists are not implemented in CloudBurrow yet (#1834 phase 2)")
}

func (a *API) ListCertificateRevocationLists(context.Context, *privatecapb.ListCertificateRevocationListsRequest) (*privatecapb.ListCertificateRevocationListsResponse, error) {
	return nil, apierror.Unimplemented("CertificateRevocationLists are not implemented in CloudBurrow yet (#1834 phase 2)")
}

func (a *API) UpdateCertificateRevocationList(context.Context, *privatecapb.UpdateCertificateRevocationListRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("CertificateRevocationLists are not implemented in CloudBurrow yet (#1834 phase 2)")
}

func (a *API) CreateCertificateTemplate(context.Context, *privatecapb.CreateCertificateTemplateRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("CertificateTemplates are not implemented in CloudBurrow yet (#1834 phase 2)")
}

func (a *API) DeleteCertificateTemplate(context.Context, *privatecapb.DeleteCertificateTemplateRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("CertificateTemplates are not implemented in CloudBurrow yet (#1834 phase 2)")
}

func (a *API) GetCertificateTemplate(context.Context, *privatecapb.GetCertificateTemplateRequest) (*privatecapb.CertificateTemplate, error) {
	return nil, apierror.Unimplemented("CertificateTemplates are not implemented in CloudBurrow yet (#1834 phase 2)")
}

func (a *API) ListCertificateTemplates(context.Context, *privatecapb.ListCertificateTemplatesRequest) (*privatecapb.ListCertificateTemplatesResponse, error) {
	return nil, apierror.Unimplemented("CertificateTemplates are not implemented in CloudBurrow yet (#1834 phase 2)")
}

func (a *API) UpdateCertificateTemplate(context.Context, *privatecapb.UpdateCertificateTemplateRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("CertificateTemplates are not implemented in CloudBurrow yet (#1834 phase 2)")
}
