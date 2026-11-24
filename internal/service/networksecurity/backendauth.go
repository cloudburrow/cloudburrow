package networksecurity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Backend authentication configs (#2160), as the BackendAuthenticationConfig
// reference, https://docs.cloud.google.com/load-balancing/docs/backend-authenticated-tls-backend-mtls
// and .../backend-mtls-setup describe them (read 2026-11-22): a trust
// config that validates the backends' server certificates (required
// unless wellKnownRoots is PUBLIC_ROOTS), and a Certificate Manager
// certificate of scope CLIENT_AUTH the load balancer presents to them
// (backend mTLS), which cannot be attached after creation. Both are in
// the config's own location. A backend service's
// tlsSettings.authenticationConfig names it (compute's lbbackendtls.go).

var (
	cmCertRE  = regexp.MustCompile(`^projects/[^/]+/locations/([^/]+)/certificates/[^/]+$`)
	cmTrustRE = regexp.MustCompile(`^projects/[^/]+/locations/([^/]+)/trustConfigs/[^/]+$`)
)

func (s *Server) checkBackendAuth(old *nspb.BackendAuthenticationConfig) func(m proto.Message, location string) error {
	return func(m proto.Message, location string) error {
		c := m.(*nspb.BackendAuthenticationConfig)
		h := s.hooksNow()
		if old != nil && c.GetClientCertificate() != old.GetClientCertificate() {
			return apierror.InvalidArgument("Invalid argument: client_certificate cannot be changed after the backend authentication config is created.")
		}
		if t := c.GetTrustConfig(); t != "" {
			mm := cmTrustRE.FindStringSubmatch(t)
			if mm == nil {
				return apierror.InvalidArgument("Invalid argument: trust_config %q must be projects/{project}/locations/{location}/trustConfigs/{trust_config}.", t)
			}
			if mm[1] != location {
				return apierror.InvalidArgument("Invalid argument: trust_config %q must be in the backend authentication config's location '%s'.", t, location)
			}
			if h.TrustConfigExists != nil && !h.TrustConfigExists(t) {
				return apierror.NotFound("TrustConfig '%s' not found.", t)
			}
		} else if c.GetWellKnownRoots() != nspb.BackendAuthenticationConfig_PUBLIC_ROOTS {
			return apierror.InvalidArgument("Invalid argument: trust_config is required unless well_known_roots is PUBLIC_ROOTS.")
		}
		if cc := c.GetClientCertificate(); cc != "" && (old == nil || cc != old.GetClientCertificate()) {
			mm := cmCertRE.FindStringSubmatch(cc)
			if mm == nil {
				return apierror.InvalidArgument("Invalid argument: client_certificate %q must be projects/{project}/locations/{location}/certificates/{certificate}.", cc)
			}
			if mm[1] != location {
				return apierror.InvalidArgument("Invalid argument: client_certificate %q must be in the backend authentication config's location '%s'.", cc, location)
			}
			if h.CertificateScope != nil {
				scope, ok := h.CertificateScope(cc)
				if !ok {
					return apierror.NotFound("Certificate '%s' not found.", cc)
				}
				if scope != "CLIENT_AUTH" {
					return apierror.InvalidArgument("Invalid argument: client_certificate %q must have the CLIENT_AUTH scope, not %s.", cc, scope)
				}
			}
		}
		c.Etag = newEtag()
		return nil
	}
}

func (s *Server) CreateBackendAuthenticationConfig(_ context.Context, req *nspb.CreateBackendAuthenticationConfigRequest) (*longrunningpb.Operation, error) {
	c, _ := proto.Clone(req.GetBackendAuthenticationConfig()).(*nspb.BackendAuthenticationConfig)
	if c == nil {
		c = &nspb.BackendAuthenticationConfig{}
	}
	return s.create(req.GetParent(), collBackendAu, "backend_authentication_config_id", req.GetBackendAuthenticationConfigId(), c, s.checkBackendAuth(nil))
}

func (s *Server) GetBackendAuthenticationConfig(_ context.Context, req *nspb.GetBackendAuthenticationConfigRequest) (*nspb.BackendAuthenticationConfig, error) {
	m, err := s.get(collBackendAu, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.BackendAuthenticationConfig), nil
}

func (s *Server) ListBackendAuthenticationConfigs(_ context.Context, req *nspb.ListBackendAuthenticationConfigsRequest) (*nspb.ListBackendAuthenticationConfigsResponse, error) {
	l, err := s.list(req.GetParent(), collBackendAu)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.BackendAuthenticationConfig, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.BackendAuthenticationConfig)
	}
	page, next, err := aipfilter.ListPage("bac:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListBackendAuthenticationConfigsResponse{BackendAuthenticationConfigs: page, NextPageToken: next}, nil
}

func (s *Server) UpdateBackendAuthenticationConfig(_ context.Context, req *nspb.UpdateBackendAuthenticationConfigRequest) (*longrunningpb.Operation, error) {
	in := req.GetBackendAuthenticationConfig()
	s.mu.Lock()
	old, _ := s.res[in.GetName()].(*nspb.BackendAuthenticationConfig)
	s.mu.Unlock()
	if old != nil {
		old = proto.Clone(old).(*nspb.BackendAuthenticationConfig)
		if e := in.GetEtag(); e != "" && e != old.GetEtag() {
			return nil, apierror.Aborted("The etag '%s' does not match the backend authentication config's current etag.", e)
		}
	}
	return s.update(collBackendAu, in, req.GetUpdateMask(),
		[]string{"description", "labels", "client_certificate", "trust_config", "well_known_roots"}, s.checkBackendAuth(old))
}

func (s *Server) DeleteBackendAuthenticationConfig(_ context.Context, req *nspb.DeleteBackendAuthenticationConfigRequest) (*longrunningpb.Operation, error) {
	if e := req.GetEtag(); e != "" {
		s.mu.Lock()
		cur, ok := s.res[req.GetName()].(*nspb.BackendAuthenticationConfig)
		s.mu.Unlock()
		if ok && cur.GetEtag() != e {
			return nil, apierror.Aborted("The etag '%s' does not match the backend authentication config's current etag.", e)
		}
	}
	return s.delete(collBackendAu, req.GetName())
}

// BackendAuthenticationConfig is a config by name, a copy.
func (s *Server) BackendAuthenticationConfig(name string) (*nspb.BackendAuthenticationConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.res[name].(*nspb.BackendAuthenticationConfig)
	if !ok {
		return nil, false
	}
	return proto.Clone(c).(*nspb.BackendAuthenticationConfig), true
}

// CertificateUsers are the backend authentication configs that name a
// Certificate Manager certificate.
func (s *Server) CertificateUsers(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, k := range sortedKeys(s.res) {
		if c, ok := s.res[k].(*nspb.BackendAuthenticationConfig); ok && c.GetClientCertificate() == name {
			out = append(out, k)
		}
	}
	return out
}

// newEtag is a fresh etag.
func newEtag() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
