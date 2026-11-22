package networksecurity

import (
	"context"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

var trustConfigRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/trustConfigs/[^/]+$`)

// --- Server TLS policies -------------------------------------------------

func (s *Server) checkServerTLS(m proto.Message, location string) error {
	p := m.(*nspb.ServerTlsPolicy)
	mt := p.GetMtlsPolicy()
	alb := mt.GetClientValidationMode() != nspb.ServerTlsPolicy_MTLSPolicy_CLIENT_VALIDATION_MODE_UNSPECIFIED || mt.GetClientValidationTrustConfig() != ""
	if alb {
		// The Application Load Balancers' form.
		if p.GetAllowOpen() {
			return apierror.InvalidArgument("Invalid value for field 'allow_open': it must be false for a policy used with Application Load Balancers (mtls_policy.client_validation_mode is set).")
		}
		if p.GetServerCertificate() != nil {
			return apierror.InvalidArgument("Invalid value for field 'server_certificate': it must be empty for a policy used with Application Load Balancers (mtls_policy.client_validation_mode is set).")
		}
		if len(mt.GetClientValidationCa()) > 0 {
			return apierror.InvalidArgument("Invalid value for field 'mtls_policy.client_validation_ca': it must be empty for a policy used with Application Load Balancers; use client_validation_trust_config.")
		}
		if mt.GetClientValidationMode() == nspb.ServerTlsPolicy_MTLSPolicy_CLIENT_VALIDATION_MODE_UNSPECIFIED {
			return apierror.InvalidArgument("Invalid argument: mtls_policy.client_validation_mode is required for a policy used with Application Load Balancers.")
		}
		if tc := mt.GetClientValidationTrustConfig(); tc != "" {
			mm := trustConfigRE.FindStringSubmatch(tc)
			if mm == nil {
				return apierror.InvalidArgument("Invalid value for field 'mtls_policy.client_validation_trust_config': '%s'. It must be projects/{project}/locations/{location}/trustConfigs/{trust_config}.", tc)
			}
			if mm[2] != location {
				return apierror.InvalidArgument("Invalid value for field 'mtls_policy.client_validation_trust_config': '%s'. The trust config must be in the policy's location '%s'.", tc, location)
			}
			if f := s.hooksNow().TrustConfigExists; f != nil && !f(tc) {
				return apierror.NotFound("TrustConfig '%s' not found.", tc)
			}
		} else if mt.GetClientValidationMode() == nspb.ServerTlsPolicy_MTLSPolicy_REJECT_INVALID {
			return apierror.InvalidArgument("Invalid argument: mtls_policy.client_validation_trust_config is required when client_validation_mode is REJECT_INVALID.")
		}
		return nil
	}
	// Traffic Director's form.
	if p.GetAllowOpen() && p.GetServerCertificate() == nil && mt == nil {
		return nil
	}
	if err := checkProvider("server_certificate", p.GetServerCertificate()); err != nil {
		return err
	}
	if mt != nil {
		if len(mt.GetClientValidationCa()) == 0 {
			return apierror.InvalidArgument("Invalid argument: mtls_policy.client_validation_ca is required for a policy used with Traffic Director.")
		}
		for _, ca := range mt.GetClientValidationCa() {
			if err := checkValidationCA("mtls_policy.client_validation_ca", ca); err != nil {
				return err
			}
		}
		if p.GetServerCertificate() == nil {
			return apierror.InvalidArgument("Invalid argument: server_certificate is required with mtls_policy for a policy used with Traffic Director.")
		}
	}
	return nil
}

func checkProvider(field string, c *nspb.CertificateProvider) error {
	if c == nil {
		return nil
	}
	switch {
	case c.GetGrpcEndpoint() != nil:
		if c.GetGrpcEndpoint().GetTargetUri() == "" {
			return apierror.InvalidArgument("Invalid argument: %s.grpc_endpoint.target_uri is required.", field)
		}
	case c.GetCertificateProviderInstance() != nil:
		if c.GetCertificateProviderInstance().GetPluginInstance() == "" {
			return apierror.InvalidArgument("Invalid argument: %s.certificate_provider_instance.plugin_instance is required.", field)
		}
	default:
		return apierror.InvalidArgument("Invalid argument: %s needs grpc_endpoint or certificate_provider_instance.", field)
	}
	return nil
}

func checkValidationCA(field string, c *nspb.ValidationCA) error {
	switch {
	case c.GetGrpcEndpoint() != nil:
		if c.GetGrpcEndpoint().GetTargetUri() == "" {
			return apierror.InvalidArgument("Invalid argument: %s.grpc_endpoint.target_uri is required.", field)
		}
	case c.GetCertificateProviderInstance() != nil:
		if c.GetCertificateProviderInstance().GetPluginInstance() == "" {
			return apierror.InvalidArgument("Invalid argument: %s.certificate_provider_instance.plugin_instance is required.", field)
		}
	default:
		return apierror.InvalidArgument("Invalid argument: %s needs grpc_endpoint or certificate_provider_instance.", field)
	}
	return nil
}

func (s *Server) CreateServerTlsPolicy(_ context.Context, req *nspb.CreateServerTlsPolicyRequest) (*longrunningpb.Operation, error) {
	p, _ := proto.Clone(req.GetServerTlsPolicy()).(*nspb.ServerTlsPolicy)
	if p == nil {
		p = &nspb.ServerTlsPolicy{}
	}
	return s.create(req.GetParent(), collServerTLS, "server_tls_policy_id", req.GetServerTlsPolicyId(), p, s.checkServerTLS)
}

func (s *Server) GetServerTlsPolicy(_ context.Context, req *nspb.GetServerTlsPolicyRequest) (*nspb.ServerTlsPolicy, error) {
	m, err := s.get(collServerTLS, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.ServerTlsPolicy), nil
}

func (s *Server) ListServerTlsPolicies(_ context.Context, req *nspb.ListServerTlsPoliciesRequest) (*nspb.ListServerTlsPoliciesResponse, error) {
	l, err := s.list(req.GetParent(), collServerTLS)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.ServerTlsPolicy, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.ServerTlsPolicy)
	}
	page, next, err := aipfilter.ListPage("stp:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListServerTlsPoliciesResponse{ServerTlsPolicies: page, NextPageToken: next}, nil
}

func (s *Server) UpdateServerTlsPolicy(_ context.Context, req *nspb.UpdateServerTlsPolicyRequest) (*longrunningpb.Operation, error) {
	return s.update(collServerTLS, req.GetServerTlsPolicy(), req.GetUpdateMask(),
		[]string{"description", "labels", "allow_open", "server_certificate", "mtls_policy"}, s.checkServerTLS)
}

func (s *Server) DeleteServerTlsPolicy(_ context.Context, req *nspb.DeleteServerTlsPolicyRequest) (*longrunningpb.Operation, error) {
	return s.delete(collServerTLS, req.GetName())
}

// --- Client TLS policies -------------------------------------------------

func checkClientTLS(m proto.Message, _ string) error {
	p := m.(*nspb.ClientTlsPolicy)
	if err := checkProvider("client_certificate", p.GetClientCertificate()); err != nil {
		return err
	}
	for _, ca := range p.GetServerValidationCa() {
		if err := checkValidationCA("server_validation_ca", ca); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) CreateClientTlsPolicy(_ context.Context, req *nspb.CreateClientTlsPolicyRequest) (*longrunningpb.Operation, error) {
	p, _ := proto.Clone(req.GetClientTlsPolicy()).(*nspb.ClientTlsPolicy)
	if p == nil {
		p = &nspb.ClientTlsPolicy{}
	}
	return s.create(req.GetParent(), collClientTLS, "client_tls_policy_id", req.GetClientTlsPolicyId(), p, checkClientTLS)
}

func (s *Server) GetClientTlsPolicy(_ context.Context, req *nspb.GetClientTlsPolicyRequest) (*nspb.ClientTlsPolicy, error) {
	m, err := s.get(collClientTLS, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.ClientTlsPolicy), nil
}

func (s *Server) ListClientTlsPolicies(_ context.Context, req *nspb.ListClientTlsPoliciesRequest) (*nspb.ListClientTlsPoliciesResponse, error) {
	l, err := s.list(req.GetParent(), collClientTLS)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.ClientTlsPolicy, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.ClientTlsPolicy)
	}
	page, next, err := aipfilter.ListPage("ctp:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListClientTlsPoliciesResponse{ClientTlsPolicies: page, NextPageToken: next}, nil
}

func (s *Server) UpdateClientTlsPolicy(_ context.Context, req *nspb.UpdateClientTlsPolicyRequest) (*longrunningpb.Operation, error) {
	return s.update(collClientTLS, req.GetClientTlsPolicy(), req.GetUpdateMask(),
		[]string{"description", "labels", "sni", "client_certificate", "server_validation_ca"}, checkClientTLS)
}

func (s *Server) DeleteClientTlsPolicy(_ context.Context, req *nspb.DeleteClientTlsPolicyRequest) (*longrunningpb.Operation, error) {
	return s.delete(collClientTLS, req.GetName())
}

// --- Authorization policies (classic) ------------------------------------

func validIPBlock(b string) bool {
	if net.ParseIP(b) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(b)
	return err == nil
}

var httpMethods = map[string]bool{http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true}

func checkAuthorization(m proto.Message, _ string) error {
	p := m.(*nspb.AuthorizationPolicy)
	if p.GetAction() == nspb.AuthorizationPolicy_ACTION_UNSPECIFIED {
		return apierror.InvalidArgument("Invalid argument: action is required: ALLOW or DENY.")
	}
	for i, r := range p.GetRules() {
		for j, src := range r.GetSources() {
			for _, b := range src.GetIpBlocks() {
				if !validIPBlock(b) {
					return apierror.InvalidArgument("Invalid value for field 'rules[%d].sources[%d].ip_blocks': '%s'. It must be an IP address or a CIDR range.", i, j, b)
				}
			}
		}
		for j, d := range r.GetDestinations() {
			if len(d.GetHosts()) == 0 {
				return apierror.InvalidArgument("Invalid argument: rules[%d].destinations[%d].hosts is required.", i, j)
			}
			if len(d.GetPorts()) == 0 {
				return apierror.InvalidArgument("Invalid argument: rules[%d].destinations[%d].ports is required.", i, j)
			}
			for _, pt := range d.GetPorts() {
				if pt == 0 || pt > 65535 {
					return apierror.InvalidArgument("Invalid value for field 'rules[%d].destinations[%d].ports': %d.", i, j, pt)
				}
			}
			for _, mth := range d.GetMethods() {
				if !httpMethods[mth] {
					return apierror.InvalidArgument("Invalid value for field 'rules[%d].destinations[%d].methods': '%s' is not an HTTP method.", i, j, mth)
				}
			}
			if hm := d.GetHttpHeaderMatch(); hm != nil {
				if hm.GetHeaderName() == "" {
					return apierror.InvalidArgument("Invalid argument: rules[%d].destinations[%d].http_header_match.header_name is required.", i, j)
				}
				if _, err := regexp.Compile(hm.GetRegexMatch()); err != nil {
					return apierror.InvalidArgument("Invalid value for field 'rules[%d].destinations[%d].http_header_match.regex_match': %v.", i, j, err)
				}
			}
		}
	}
	return nil
}

func (s *Server) CreateAuthorizationPolicy(_ context.Context, req *nspb.CreateAuthorizationPolicyRequest) (*longrunningpb.Operation, error) {
	p, _ := proto.Clone(req.GetAuthorizationPolicy()).(*nspb.AuthorizationPolicy)
	if p == nil {
		p = &nspb.AuthorizationPolicy{}
	}
	return s.create(req.GetParent(), collAuthz, "authorization_policy_id", req.GetAuthorizationPolicyId(), p, checkAuthorization)
}

func (s *Server) GetAuthorizationPolicy(_ context.Context, req *nspb.GetAuthorizationPolicyRequest) (*nspb.AuthorizationPolicy, error) {
	m, err := s.get(collAuthz, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.AuthorizationPolicy), nil
}

func (s *Server) ListAuthorizationPolicies(_ context.Context, req *nspb.ListAuthorizationPoliciesRequest) (*nspb.ListAuthorizationPoliciesResponse, error) {
	l, err := s.list(req.GetParent(), collAuthz)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.AuthorizationPolicy, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.AuthorizationPolicy)
	}
	page, next, err := aipfilter.ListPage("ap:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListAuthorizationPoliciesResponse{AuthorizationPolicies: page, NextPageToken: next}, nil
}

func (s *Server) UpdateAuthorizationPolicy(_ context.Context, req *nspb.UpdateAuthorizationPolicyRequest) (*longrunningpb.Operation, error) {
	return s.update(collAuthz, req.GetAuthorizationPolicy(), req.GetUpdateMask(), []string{"description", "labels", "action", "rules"}, checkAuthorization)
}

func (s *Server) DeleteAuthorizationPolicy(_ context.Context, req *nspb.DeleteAuthorizationPolicyRequest) (*longrunningpb.Operation, error) {
	return s.delete(collAuthz, req.GetName())
}

// --- Authorization policies of the Application Load Balancers -------------

// The documented limits (the AuthzPolicy reference).
const (
	maxAuthzRules      = 5
	maxAuthzIPBlocks   = 10
	maxAuthzHosts      = 10
	maxAuthzPaths      = 10
	maxAuthzMethods    = 10
	maxAuthzPrincipals = 50
)

// ruleRel is a forwarding rule reference as its relative name.
func ruleRel(ref string) string {
	if i := strings.Index(ref, "projects/"); i >= 0 {
		return ref[i:]
	}
	return ref
}

var ruleRelRE = regexp.MustCompile(`^projects/[^/]+/(global|regions/[a-z0-9-]+)/forwardingRules/[^/]+$`)

func (s *Server) checkAuthzALB(m proto.Message, location string) error {
	p := m.(*nspb.AuthzPolicy)
	t := p.GetTarget()
	if t == nil || len(t.GetResources()) == 0 {
		return apierror.InvalidArgument("Invalid argument: target.resources is required.")
	}
	scheme := t.GetLoadBalancingScheme()
	if scheme != nspb.AuthzPolicy_EXTERNAL_MANAGED && scheme != nspb.AuthzPolicy_INTERNAL_MANAGED && scheme != nspb.AuthzPolicy_INTERNAL_SELF_MANAGED {
		return apierror.InvalidArgument("Invalid argument: target.load_balancing_scheme is required when targeting forwarding rules: INTERNAL_MANAGED or EXTERNAL_MANAGED.")
	}
	hook := s.hooksNow().ForwardingRule
	for i, r := range t.GetResources() {
		rel := ruleRel(r)
		if !ruleRelRE.MatchString(rel) {
			return apierror.InvalidArgument("Invalid value for field 'target.resources[%d]': '%s'. It must name a forwarding rule.", i, r)
		}
		loc := "global"
		if parts := strings.Split(rel, "/"); parts[2] == "regions" {
			loc = parts[3]
		}
		if loc != location {
			return apierror.InvalidArgument("Invalid value for field 'target.resources[%d]': '%s'. The forwarding rule must be in the policy's location '%s'.", i, r, location)
		}
		if hook != nil {
			sch, ok := hook(rel)
			if !ok {
				return apierror.NotFound("The resource '%s' was not found.", r)
			}
			if sch != scheme.String() {
				return apierror.InvalidArgument("Invalid value for field 'target.resources[%d]': '%s'. Its load balancing scheme %s is not the policy's %s.", i, r, sch, scheme)
			}
		}
	}
	switch p.GetAction() {
	case nspb.AuthzPolicy_ALLOW, nspb.AuthzPolicy_DENY:
		if p.GetCustomProvider() != nil {
			return apierror.InvalidArgument("Invalid argument: custom_provider can be set only when the action is CUSTOM.")
		}
		if len(p.GetHttpRules()) == 0 && len(p.GetNetworkRules()) == 0 {
			return apierror.InvalidArgument("Invalid argument: at least one HTTP rule is required for the ALLOW or DENY action.")
		}
	case nspb.AuthzPolicy_CUSTOM:
		cp := p.GetCustomProvider()
		if cp == nil || cp.GetCloudIap() == nil && cp.GetAuthzExtension() == nil {
			return apierror.InvalidArgument("Invalid argument: custom_provider is required when the action is CUSTOM: one of cloud_iap or authz_extension.")
		}
		// Neither provider is reachable from the local proxy: IAP is
		// enforced on backend services here, and Service Extensions'
		// authorization extensions are not served (docs/gaps.md).
		return apierror.Unimplemented("The CUSTOM action (Cloud IAP or authorization extensions through an AuthzPolicy) is not served by CloudBurrow's load balancers (#2160).")
	default:
		return apierror.InvalidArgument("Invalid argument: action is required: ALLOW, DENY or CUSTOM.")
	}
	if len(p.GetHttpRules()) > 0 && len(p.GetNetworkRules()) > 0 {
		return apierror.InvalidArgument("Invalid argument: network_rules are mutually exclusive with http_rules.")
	}
	if len(p.GetHttpRules()) > maxAuthzRules || len(p.GetNetworkRules()) > maxAuthzRules {
		return apierror.InvalidArgument("Invalid argument: an AuthzPolicy is limited to %d rules.", maxAuthzRules)
	}
	var principals, blocks, hosts, paths, methods int
	globalExternal := location == "global" && scheme == nspb.AuthzPolicy_EXTERNAL_MANAGED
	for i, r := range append(append([]*nspb.AuthzPolicy_AuthzRule{}, p.GetHttpRules()...), p.GetNetworkRules()...) {
		if f := r.GetFrom(); f != nil {
			if len(f.GetSources()) == 0 && len(f.GetNotSources()) == 0 {
				return apierror.InvalidArgument("Invalid argument: http_rules[%d].from needs sources or not_sources.", i)
			}
			if len(f.GetSources()) > 1 || len(f.GetNotSources()) > 1 {
				return apierror.InvalidArgument("Invalid argument: http_rules[%d].from is limited to 1 source.", i)
			}
			for _, src := range append(append([]*nspb.AuthzPolicy_AuthzRule_From_RequestSource{}, f.GetSources()...), f.GetNotSources()...) {
				if len(src.GetPrincipals()) > 0 && globalExternal {
					return apierror.InvalidArgument("Invalid argument: http_rules[%d].from.sources.principals is not supported for global external Application Load Balancers.", i)
				}
				principals += len(src.GetPrincipals())
				for _, pr := range src.GetPrincipals() {
					if pr.GetPrincipal() == nil || stringMatchEmpty(pr.GetPrincipal()) {
						return apierror.InvalidArgument("Invalid argument: http_rules[%d].from.sources.principals.principal is required.", i)
					}
					if pr.GetPrincipal().GetExact() == "" {
						return apierror.InvalidArgument("Invalid argument: http_rules[%d].from.sources.principals: only exact match can be applied for the %s selector.", i, pr.GetPrincipalSelector())
					}
				}
				blocks += len(src.GetIpBlocks())
				for _, b := range src.GetIpBlocks() {
					ip := net.ParseIP(b.GetPrefix())
					maxLen := int32(32)
					if ip != nil && ip.To4() == nil {
						maxLen = 128
					}
					if ip == nil || b.GetLength() < 0 || b.GetLength() > maxLen {
						return apierror.InvalidArgument("Invalid value for field 'http_rules[%d].from.sources.ip_blocks': '%s/%d'.", i, b.GetPrefix(), b.GetLength())
					}
				}
				if len(src.GetResources()) > 0 {
					return apierror.Unimplemented("Matching a request's source VM resources (tags, service accounts) is not served by CloudBurrow's load balancers (#2160).")
				}
			}
		}
		if to := r.GetTo(); to != nil {
			if len(to.GetOperations()) == 0 && len(to.GetNotOperations()) == 0 {
				return apierror.InvalidArgument("Invalid argument: http_rules[%d].to needs operations or not_operations.", i)
			}
			if len(to.GetOperations()) > 1 || len(to.GetNotOperations()) > 1 {
				return apierror.InvalidArgument("Invalid argument: http_rules[%d].to is limited to 1 operation.", i)
			}
			for _, op := range append(append([]*nspb.AuthzPolicy_AuthzRule_To_RequestOperation{}, to.GetOperations()...), to.GetNotOperations()...) {
				hosts += len(op.GetHosts())
				paths += len(op.GetPaths())
				methods += len(op.GetMethods())
				for _, mth := range op.GetMethods() {
					if !httpMethods[mth] {
						return apierror.InvalidArgument("Invalid value for field 'http_rules[%d].to.operations.methods': '%s' is not a valid HTTP method name.", i, mth)
					}
				}
				if op.GetMcp() != nil {
					return apierror.InvalidArgument("Invalid argument: http_rules[%d].to.operations.mcp can be set only for AuthzPolicies targeting Agent Gateway resources.", i)
				}
			}
		}
		if w := r.GetWhen(); w != "" {
			if _, err := compileWhen(w); err != nil {
				return apierror.InvalidArgument("Invalid value for field 'http_rules[%d].when': %v", i, err)
			}
		}
	}
	switch {
	case principals > maxAuthzPrincipals:
		return apierror.InvalidArgument("Invalid argument: limited to %d principals per Authorization Policy.", maxAuthzPrincipals)
	case blocks > maxAuthzIPBlocks:
		return apierror.InvalidArgument("Invalid argument: limited to %d ip_blocks per Authorization Policy.", maxAuthzIPBlocks)
	case hosts > maxAuthzHosts:
		return apierror.InvalidArgument("Invalid argument: limited to %d hosts per Authorization Policy.", maxAuthzHosts)
	case paths > maxAuthzPaths:
		return apierror.InvalidArgument("Invalid argument: limited to %d paths per Authorization Policy.", maxAuthzPaths)
	case methods > maxAuthzMethods:
		return apierror.InvalidArgument("Invalid argument: limited to %d methods per Authorization Policy.", maxAuthzMethods)
	}
	return nil
}

func stringMatchEmpty(m *nspb.AuthzPolicy_AuthzRule_StringMatch) bool {
	return m.GetExact() == "" && m.GetPrefix() == "" && m.GetSuffix() == "" && m.GetContains() == ""
}

func (s *Server) CreateAuthzPolicy(_ context.Context, req *nspb.CreateAuthzPolicyRequest) (*longrunningpb.Operation, error) {
	p, _ := proto.Clone(req.GetAuthzPolicy()).(*nspb.AuthzPolicy)
	if p == nil {
		p = &nspb.AuthzPolicy{}
	}
	if p.GetPolicyProfile() == nspb.AuthzPolicy_POLICY_PROFILE_UNSPECIFIED {
		p.PolicyProfile = nspb.AuthzPolicy_REQUEST_AUTHZ
	}
	return s.create(req.GetParent(), collAuthzALB, "authz_policy_id", req.GetAuthzPolicyId(), p, s.checkAuthzALB)
}

func (s *Server) GetAuthzPolicy(_ context.Context, req *nspb.GetAuthzPolicyRequest) (*nspb.AuthzPolicy, error) {
	m, err := s.get(collAuthzALB, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.AuthzPolicy), nil
}

func (s *Server) ListAuthzPolicies(_ context.Context, req *nspb.ListAuthzPoliciesRequest) (*nspb.ListAuthzPoliciesResponse, error) {
	l, err := s.list(req.GetParent(), collAuthzALB)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.AuthzPolicy, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.AuthzPolicy)
	}
	page, next, err := aipfilter.ListPage("azp:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListAuthzPoliciesResponse{AuthzPolicies: page, NextPageToken: next}, nil
}

func (s *Server) UpdateAuthzPolicy(_ context.Context, req *nspb.UpdateAuthzPolicyRequest) (*longrunningpb.Operation, error) {
	if in := req.GetAuthzPolicy(); in != nil && in.GetPolicyProfile() != nspb.AuthzPolicy_POLICY_PROFILE_UNSPECIFIED {
		for _, p := range req.GetUpdateMask().GetPaths() {
			if camelToSnake(p) == "policy_profile" {
				return nil, apierror.InvalidArgument("Invalid update_mask path 'policy_profile': the field is immutable.")
			}
		}
	}
	return s.update(collAuthzALB, req.GetAuthzPolicy(), req.GetUpdateMask(),
		[]string{"description", "labels", "target", "http_rules", "network_rules", "action", "custom_provider"}, s.checkAuthzALB)
}

func (s *Server) DeleteAuthzPolicy(_ context.Context, req *nspb.DeleteAuthzPolicyRequest) (*longrunningpb.Operation, error) {
	return s.delete(collAuthzALB, req.GetName())
}

// --- What the load balancers read ------------------------------------------

// ServerTLSPolicy is a server TLS policy by name
// (projects/P/locations/L/serverTlsPolicies/X), a copy.
func (s *Server) ServerTLSPolicy(name string) (*nspb.ServerTlsPolicy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.res[name].(*nspb.ServerTlsPolicy)
	if !ok {
		return nil, false
	}
	return proto.Clone(p).(*nspb.ServerTlsPolicy), true
}

// Exists is whether a policy of any collection exists by name.
func (s *Server) Exists(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.res[name]
	return ok
}

// ForwardingRulePolicies are the authorization policies that target a
// forwarding rule (its relative name), copies, sorted by name.
func (s *Server) ForwardingRulePolicies(rel string) []*nspb.AuthzPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*nspb.AuthzPolicy
	for _, k := range sortedKeys(s.res) {
		p, ok := s.res[k].(*nspb.AuthzPolicy)
		if !ok {
			continue
		}
		for _, r := range p.GetTarget().GetResources() {
			if ruleRel(r) == rel {
				out = append(out, proto.Clone(p).(*nspb.AuthzPolicy))
				break
			}
		}
	}
	return out
}

// TrustConfigUsers are the server TLS policies that name a trust config.
func (s *Server) TrustConfigUsers(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, k := range sortedKeys(s.res) {
		if p, ok := s.res[k].(*nspb.ServerTlsPolicy); ok && p.GetMtlsPolicy().GetClientValidationTrustConfig() == name {
			out = append(out, k)
		}
	}
	return out
}

// RuleUsers are the authorization policies that target a forwarding rule.
func (s *Server) RuleUsers(rel string) []string {
	var out []string
	for _, p := range s.ForwardingRulePolicies(rel) {
		out = append(out, p.GetName())
	}
	return out
}

func sortedKeys(m map[string]proto.Message) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
