package privateca

import (
	"context"
	"crypto"
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

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
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

// filteredPage is page with a list's AIP-160 filter and AIP-132 order_by
// (#1943): without either it pages keys as before; with one it reads every
// resource, filters and orders them, and pages the result in that order.
func filteredPage[T proto.Message](filter, orderBy, scope string, keys []string, token string, size int32, get func(string) T) ([]T, string, error) {
	if strings.TrimSpace(filter) == "" && strings.TrimSpace(orderBy) == "" {
		return page(scope, keys, token, size, get)
	}
	all := make([]T, 0, len(keys))
	for _, k := range keys {
		all = append(all, get(k))
	}
	return aipfilter.ListPage(scope, filter, orderBy, all, token, int(size))
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

func (a *API) CreateCaPool(ctx context.Context, req *privatecapb.CreateCaPoolRequest) (*longrunningpb.Operation, error) {
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
	if err := checkPolicy(p.GetIssuancePolicy()); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/caPools/" + req.GetCaPoolId()
	if err := s.checkEncryption(ctx, name, p.GetTier(), p.GetEncryptionSpec()); err != nil {
		return nil, err
	}
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
	if _, err := compileIdentity("issuance_policy.identity_constraints", p.GetIdentityConstraints()); err != nil {
		return err
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
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	var keys []string
	for _, k := range sortedKeys(a.s.pools, req.GetParent()+"/caPools/") {
		if poolRE.MatchString(k) {
			keys = append(keys, k)
		}
	}
	out, next, err := filteredPage(req.GetFilter(), req.GetOrderBy(), "privateca-pools:"+req.GetParent(), keys, req.GetPageToken(), req.GetPageSize(),
		func(k string) *privatecapb.CaPool { return clone(a.s.pools[k].Pool) })
	if err != nil {
		return nil, err
	}
	return &privatecapb.ListCaPoolsResponse{CaPools: out, NextPageToken: next}, nil
}

func (a *API) UpdateCaPool(ctx context.Context, req *privatecapb.UpdateCaPoolRequest) (*longrunningpb.Operation, error) {
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
		case "encryption_spec", "encryptionSpec":
			// gcloud privateca pools update --encryption-key; the pool's
			// certificates are encrypted with the new key at the next save.
			if err := s.checkEncryption(ctx, next.GetName(), next.GetTier(), in.GetEncryptionSpec()); err != nil {
				return nil, err
			}
			next.EncryptionSpec = in.GetEncryptionSpec()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated: labels, issuance_policy, publishing_options and encryption_spec can", path)
		}
	}
	p.Pool = next
	s.refreshAccessURLs(next.Name)
	for _, k := range sortedKeys(s.cas, next.Name+"/certificateAuthorities/") {
		if err := s.publish(k); err != nil {
			return nil, err
		}
	}
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
	for _, k := range sortedKeys(s.sealed, req.GetName()+"/certificates/") {
		delete(s.sealed, k)
		delete(s.sealedCache, k)
	}
	for _, k := range sortedKeys(s.crls, req.GetName()+"/certificateAuthorities/") {
		delete(s.crls, k)
		delete(s.crlPolicies, k)
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

func (a *API) CreateCertificateAuthority(ctx context.Context, req *privatecapb.CreateCertificateAuthorityRequest) (*longrunningpb.Operation, error) {
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
	if err := checkBucket(in.GetGcsBucket()); err != nil {
		return nil, err
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
	if in.GetGcsBucket() != "" && s.objects == nil {
		return nil, apierror.FailedPrecondition("certificate_authority.gcs_bucket needs Cloud Storage, which this instance does not serve")
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
	var (
		key    crypto.Signer
		sigAlg x509.SignatureAlgorithm
	)
	if v := in.GetKeySpec().GetCloudKmsKeyVersion(); v != "" {
		// "Cloud KMS defines the key's algorithm and size": the CA signs
		// with that version, through the instance's Cloud KMS.
		k, err := s.kmsKey(ctx, v, req.GetParent())
		if err != nil {
			return nil, err
		}
		key, sigAlg = k, sigAlgOf(k.alg)
	} else if key, sigAlg, err = newKey(in.GetKeySpec().GetAlgorithm()); err != nil {
		return nil, apierror.Wrap(err)
	}
	subj, err := subjectName(in.GetConfig().GetSubjectConfig().GetSubject())
	if err != nil {
		return nil, err
	}
	now := s.now()
	if in.GetType() == privatecapb.CertificateAuthority_SUBORDINATE {
		// "AWAITING_USER_ACTIVATION: Certificates cannot be issued from
		// this CA. CRLs will not be generated." until its issuer signs
		// the CSR and ActivateCertificateAuthority installs it; gcloud
		// and Terraform do both for an issuer of this service.
		rec := clone(in)
		rec.Name = name
		rec.Tier = pool.Pool.GetTier()
		rec.State = privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION
		rec.CreateTime = timestamppb.New(now)
		rec.UpdateTime = rec.CreateTime
		rec.PemCaCertificates, rec.CaCertificateDescriptions = nil, nil
		cr := &caRecord{CA: rec, Key: key}
		if cr.CSR, err = makeCSR(cr); err != nil {
			return nil, signErr(err, func(error) error { return err })
		}
		rec.AccessUrls = s.accessURLs(rec)
		s.cas[name] = cr
		return s.done(loc, "create", name, clone(rec))
	}
	start := now.Truncate(time.Second)
	t := &x509.Certificate{SerialNumber: serial(), Subject: subj, NotBefore: start, NotAfter: start.Add(in.GetLifetime().AsDuration()),
		SignatureAlgorithm: sigAlg}
	if t.RawSubject, err = rdnSubject(in.GetConfig().GetSubjectConfig().GetSubject()); err != nil {
		return nil, err
	}
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
		return nil, signErr(err, func(err error) error { return apierror.InvalidArgument("the CA certificate cannot be made: %v", err) })
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
	if len(t.RawSubject) > 0 {
		rec.CaCertificateDescriptions[0].SubjectDescription.Subject.RdnSequence = rdnProto(cert.RawSubject)
	}
	rec.CreateTime = timestamppb.New(now)
	rec.UpdateTime = rec.CreateTime
	s.cas[name] = &caRecord{CA: rec, Key: key, Cert: cert}
	rec.AccessUrls = s.accessURLs(rec)
	if err := s.regenCRL(name); err != nil {
		// Its content could not be published (gcs_bucket): no CA.
		delete(s.cas, name)
		for _, k := range sortedKeys(s.crls, name+"/certificateRevocationLists/") {
			delete(s.crls, k)
		}
		return nil, err
	}
	return s.done(loc, "create", name, clone(rec))
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
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if !poolRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be a CaPool name", req.GetParent())
	}
	s.purge()
	// gcloud's roots list and subordinates list send type="SELF_SIGNED" or
	// type:SUBORDINATE; any AIP-160 filter and order_by applies.
	out, next, err := filteredPage(req.GetFilter(), req.GetOrderBy(), "privateca-cas:"+req.GetParent(), sortedKeys(s.cas, req.GetParent()+"/certificateAuthorities/"), req.GetPageToken(), req.GetPageSize(),
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
	next.AccessUrls = s.accessURLs(next)
	if err := s.regenCRL(name); err != nil {
		return nil, err
	}
	loc, _, _ := strings.Cut(name, "/caPools/")
	return s.done(loc, verb, name, clone(next))
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

// ---- Certificates ----

func (a *API) CreateCertificate(_ context.Context, req *privatecapb.CreateCertificateRequest) (*privatecapb.Certificate, error) {
	s := a.s
	in := req.GetCertificate()
	if in == nil {
		return nil, apierror.InvalidArgument("certificate is required")
	}
	mode := in.GetSubjectMode()
	switch mode {
	case privatecapb.SubjectRequestMode_SUBJECT_REQUEST_MODE_UNSPECIFIED:
		mode = privatecapb.SubjectRequestMode_DEFAULT
	case privatecapb.SubjectRequestMode_DEFAULT, privatecapb.SubjectRequestMode_RDN_SEQUENCE:
	case privatecapb.SubjectRequestMode_REFLECTED_SPIFFE:
		// The SPIFFE ID is the caller's federated workload identity
		// (tutorials/using-3pi-with-reflection). No caller of a local
		// instance has one: CloudBurrow serves no Workload Identity
		// Federation, so every request gets the error Google documents for
		// "the caller does not have a SPIFFE identity" (troubleshooting).
		// Its code is UNVERIFIED (docs re-read 2026-11-18: the page names
		// none).
		return nil, apierror.FailedPrecondition("Could not use the REFLECTED_SPIFFE subject mode because the caller does not have a SPIFFE identity. Please visit the CA Service documentation to ensure that this is a supported use-case.")
	default:
		return nil, apierror.InvalidArgument("subject_mode %s is not a SubjectRequestMode", mode)
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
	tmpl, err := s.applyTemplate(in.GetCertificateTemplate(), req.GetParent())
	if err != nil {
		return nil, err
	}
	t := &x509.Certificate{SerialNumber: serial(), SignatureAlgorithm: signatureAlgorithm(issuer)}
	var x509Req *privatecapb.X509Parameters
	var requestedKID []byte // config.subject_key_id (#1961)
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
		if sub := s.csrCA(csr); sub != nil {
			// The CSR FetchCertificateAuthorityCsr made: its CA's own
			// config is what it asks the issuer for.
			x509Req = sub.CA.GetConfig().GetX509Config()
		}
		if passthroughSubject(policy) && passthroughSubject(tmplPolicy(tmpl)) && mode == privatecapb.SubjectRequestMode_RDN_SEQUENCE {
			// "When parsing a PEM CSR this mode will maintain the sequence
			// of RDNs found in the CSR's subject field."
			t.RawSubject, t.Subject = csr.RawSubject, csr.Subject
		} else if passthroughSubject(policy) && passthroughSubject(tmplPolicy(tmpl)) {
			t.Subject = pkix.Name{CommonName: csr.Subject.CommonName, Country: csr.Subject.Country, Organization: csr.Subject.Organization,
				OrganizationalUnit: csr.Subject.OrganizationalUnit, Locality: csr.Subject.Locality, Province: csr.Subject.Province,
				StreetAddress: csr.Subject.StreetAddress, PostalCode: csr.Subject.PostalCode}
		}
		if passthroughSANs(policy) && passthroughSANs(tmplPolicy(tmpl)) {
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
		if passthroughSubject(policy) && passthroughSubject(tmplPolicy(tmpl)) {
			if t.Subject, err = subjectName(c.GetSubjectConfig().GetSubject()); err != nil {
				return nil, err
			}
			if t.RawSubject, err = rdnSubject(c.GetSubjectConfig().GetSubject()); err != nil {
				return nil, err
			}
		}
		if passthroughSANs(policy) && passthroughSANs(tmplPolicy(tmpl)) {
			if err := applySANs(t, c.GetSubjectConfig().GetSubjectAltName()); err != nil {
				return nil, err
			}
		}
		x509Req = c.GetX509Config()
		if requestedKID, err = requestedKeyID(c.GetSubjectKeyId()); err != nil {
			return nil, err
		}
	default:
		return nil, apierror.InvalidArgument("certificate needs pem_csr or config")
	}
	if err := checkKeyType(pub, policy.GetAllowedKeyTypes()); err != nil {
		return nil, err
	}
	if t.Subject.String() == "" && len(t.DNSNames)+len(t.EmailAddresses)+len(t.IPAddresses)+len(t.URIs) == 0 && !hasCustomSANs(t) {
		return nil, apierror.InvalidArgument("the certificate needs a subject or a subject alternative name")
	}
	// passthrough_extensions: the request's extensions outside the pool's
	// or the template's are dropped; a template's predefined_values outside
	// the pool's, or the pool's baseline_values outside the template's,
	// fail the issuance (the code is UNVERIFIED (docs re-read 2026-11-18:
	// <https://docs.cloud.google.com/certificate-authority-service/docs/reference/rest/v1/projects.locations.certificateTemplates>
	// names none)).
	x509Req = filterExtensions(filterExtensions(x509Req, policy.GetPassthroughExtensions()), tmpl.GetPassthroughExtensions())
	if e := outsideExtensions(tmpl.GetPredefinedValues(), policy.GetPassthroughExtensions()); e != "" {
		return nil, apierror.InvalidArgument("certificate_template %s's predefined_values set %s, which CaPool %s's issuance_policy.passthrough_extensions does not allow", tmpl.GetName(), e, req.GetParent())
	}
	if e := outsideExtensions(policy.GetBaselineValues(), tmpl.GetPassthroughExtensions()); e != "" {
		return nil, apierror.InvalidArgument("CaPool %s's baseline_values set %s, which certificate_template %s's passthrough_extensions does not allow", req.GetParent(), e, tmpl.GetName())
	}
	if err := applyX509(t, mergeX509(mergeX509(x509Req, tmpl.GetPredefinedValues()), policy.GetBaselineValues())); err != nil {
		return nil, err
	}
	// The resolved subject and SANs must satisfy the pool's and the
	// template's cel_expression.
	if err := checkIdentity("issuance_policy.identity_constraints", policy.GetIdentityConstraints(), t); err != nil {
		return nil, err
	}
	if err := checkIdentity("certificate_template.identity_constraints", tmpl.GetIdentityConstraints(), t); err != nil {
		return nil, err
	}
	publishTo(t, issuer.CA)
	now := s.now()
	maxLife := policy.GetMaximumLifetime()
	if tl := tmpl.GetMaximumLifetime(); tl != nil && (maxLife == nil || tl.AsDuration() < maxLife.AsDuration()) {
		maxLife = tl
	}
	if t.NotBefore, t.NotAfter, err = validity(now, in.GetLifetime(), maxLife, policy.GetBackdateDuration(), issuer.Cert.NotAfter); err != nil {
		return nil, err
	}
	if nb := in.GetRequestedNotBeforeTime(); nb != nil {
		// "This field may only be set if the
		// CaPool.IssuancePolicy.allow_requester_specified_not_before_time
		// field is set to true"; then "the certificate will be issued with
		// this 'not_before_time'", and the lifetime runs from it.
		if !policy.GetAllowRequesterSpecifiedNotBeforeTime() {
			return nil, apierror.InvalidArgument("requested_not_before_time may only be set when CaPool %s's issuance_policy.allow_requester_specified_not_before_time is true", req.GetParent())
		}
		if err := nb.CheckValid(); err != nil {
			return nil, apierror.InvalidArgument("requested_not_before_time: %v", err)
		}
		life := t.NotAfter.Sub(t.NotBefore)
		if t.NotBefore, t.NotAfter, err = validity(nb.AsTime(), durationpb.New(life), nil, nil, issuer.Cert.NotAfter); err != nil {
			return nil, err
		}
		if !t.NotBefore.Before(t.NotAfter) {
			return nil, apierror.InvalidArgument("requested_not_before_time %s is not before the issuing CA's expiry %s", nb.AsTime().UTC().Format(time.RFC3339), issuer.Cert.NotAfter.UTC().Format(time.RFC3339))
		}
	}
	if t.SubjectKeyId, err = keyID(pub); err != nil {
		return nil, apierror.InvalidArgument("the public key: %v", err)
	}
	if requestedKID != nil {
		// config.subject_key_id (#1961) replaces the computed one.
		t.SubjectKeyId = requestedKID
	}
	der, err := x509.CreateCertificate(nil, t, issuer.Cert, pub, issuer.Key)
	if err != nil {
		return nil, signErr(err, func(err error) error { return apierror.InvalidArgument("the certificate cannot be issued: %v", err) })
	}
	cert, _ := x509.ParseCertificate(der)
	out := clone(in)
	out.Name = name
	out.IssuerCertificateAuthority = issuer.CA.GetName()
	out.PemCertificate = pemCert(der)
	out.PemCertificateChain = issuer.CA.GetPemCaCertificates()
	out.CertificateDescription = describe(cert)
	if len(t.RawSubject) > 0 {
		out.CertificateDescription.SubjectDescription.Subject.RdnSequence = rdnProto(cert.RawSubject)
	}
	out.CreateTime = timestamppb.New(now)
	out.UpdateTime = out.CreateTime
	out.SubjectMode = mode
	out.Lifetime = durationpb.New(t.NotAfter.Sub(t.NotBefore))
	// A DevOps pool "does not store certificates": the certificate is
	// returned and not kept, so Get, List and Revoke cannot find it.
	if enterprise {
		s.certs[name] = clone(out)
		if err := s.save(); err != nil {
			// A CMEK pool whose key cannot encrypt it keeps no certificate.
			delete(s.certs, name)
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
	s.unseal()
	if sc, ok := s.sealed[name]; ok {
		return nil, sealedErr(name, sc.Key)
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
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if !poolRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("parent %q must be a CaPool name", req.GetParent())
	}
	s.unseal()
	if k := sortedKeys(s.sealed, req.GetParent()+"/certificates/"); len(k) > 0 {
		return nil, sealedErr(k[0], s.sealed[k[0]].Key)
	}
	out, next, err := filteredPage(req.GetFilter(), req.GetOrderBy(), "privateca-certs:"+req.GetParent(), sortedKeys(s.certs, req.GetParent()+"/certificates/"), req.GetPageToken(), req.GetPageSize(),
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
		s.certs[req.GetName()] = c
		return nil, apierror.Wrap(err)
	}
	if err := s.regenCRL(next.GetIssuerCertificateAuthority()); err != nil {
		return nil, err
	}
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
		s.certs[in.GetName()] = c
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}
