package networksecurity

import (
	"context"
	"regexp"
	"sort"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/service/networkservices"
)

// Secure Web Proxy's resources (#2160): gateway security policies, their
// rules, URL lists and TLS inspection policies, as the REST reference,
// https://docs.cloud.google.com/secure-web-proxy/docs/configure-rules,
// .../cel-matcher-language-reference, .../url-list-syntax-reference and
// .../quotas describe them (read 2026-11-22). A Secure Web Proxy gateway
// (networkservices) applies a policy's rules to each request through it.

const collRules = "rules"

// swpIDRE is these resources' ID rule: (^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$).
var swpIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Secure Web Proxy's limits (secure-web-proxy/docs/quotas).
const (
	maxPoliciesPerRegion = 10
	maxURLListsPerRegion = 10
	maxRulesPerPolicy    = 500
	maxURLListEntries    = 2500
	maxMatcherLen        = 2048
	maxRegexPerPolicy    = 5
	maxRegexLen          = 100
)

var (
	caPoolRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/caPools/[^/]+$`)
	tipRE    = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/tlsInspectionPolicies/[^/]+$`)
	regexRE  = regexp.MustCompile(`\.matches\(\s*(?:R?'([^']*)'|R?"([^"]*)")`)
)

func checkSWPID(field, id string) error {
	if !swpIDRE.MatchString(id) {
		return apierror.InvalidArgument("Invalid value for field '%s': '%s'. It must match the pattern (^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$).", field, id)
	}
	return nil
}

func regionalOnly(location, what string) error {
	if location == "global" {
		return apierror.InvalidArgument("%s resources are regional: location 'global' is not supported.", what)
	}
	return nil
}

// countLocked is how many of coll a project has in a location.
func (s *Server) countLocked(project, location, coll string) int {
	n := 0
	for k := range s.res {
		p := strings.Split(k, "/")
		if len(p) == 6 && p[1] == project && p[3] == location && p[4] == coll {
			n++
		}
	}
	return n
}

func quota(metric, project string, limit int) error {
	return status.Errorf(codes.ResourceExhausted, "Quota exceeded for quota metric '%s' and limit '%s per project per region' of service 'networksecurity.googleapis.com' for consumer 'project:%s': %d.", metric, metric, project, limit)
}

// --- Gateway security policies ---------------------------------------------

func (s *Server) checkGSP(m proto.Message, location string) error {
	p := m.(*nspb.GatewaySecurityPolicy)
	if err := regionalOnly(location, "GatewaySecurityPolicy"); err != nil {
		return err
	}
	if len(p.GetDescription()) > 1024 {
		return apierror.InvalidArgument("Invalid value for field 'description': it must be at most 1024 characters long.")
	}
	if t := p.GetTlsInspectionPolicy(); t != "" {
		ref := ruleRel(t)
		mm := tipRE.FindStringSubmatch(ref)
		if mm == nil {
			return apierror.InvalidArgument("Invalid value for field 'tls_inspection_policy': '%s'. It must be projects/{project}/locations/{location}/tlsInspectionPolicies/{tls_inspection_policy}.", t)
		}
		if mm[2] != location {
			return apierror.InvalidArgument("Invalid value for field 'tls_inspection_policy': '%s'. The TLS inspection policy must be in the policy's location '%s'.", t, location)
		}
		if !s.Exists(ref) {
			return apierror.NotFound("TlsInspectionPolicy '%s' not found.", ref)
		}
	}
	return nil
}

func (s *Server) CreateGatewaySecurityPolicy(_ context.Context, req *nspb.CreateGatewaySecurityPolicyRequest) (*longrunningpb.Operation, error) {
	if err := checkSWPID("gateway_security_policy_id", req.GetGatewaySecurityPolicyId()); err != nil {
		return nil, err
	}
	if project, loc, err := parseParent(req.GetParent()); err == nil {
		s.mu.Lock()
		n := s.countLocked(project, loc, collGSP)
		s.mu.Unlock()
		if n >= maxPoliciesPerRegion {
			return nil, quota("Secure Web Proxy policies", project, maxPoliciesPerRegion)
		}
	}
	p, _ := proto.Clone(req.GetGatewaySecurityPolicy()).(*nspb.GatewaySecurityPolicy)
	if p == nil {
		p = &nspb.GatewaySecurityPolicy{}
	}
	return s.create(req.GetParent(), collGSP, "gateway_security_policy_id", req.GetGatewaySecurityPolicyId(), p, s.checkGSP)
}

func (s *Server) GetGatewaySecurityPolicy(_ context.Context, req *nspb.GetGatewaySecurityPolicyRequest) (*nspb.GatewaySecurityPolicy, error) {
	m, err := s.get(collGSP, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.GatewaySecurityPolicy), nil
}

func (s *Server) ListGatewaySecurityPolicies(_ context.Context, req *nspb.ListGatewaySecurityPoliciesRequest) (*nspb.ListGatewaySecurityPoliciesResponse, error) {
	l, err := s.list(req.GetParent(), collGSP)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.GatewaySecurityPolicy, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.GatewaySecurityPolicy)
	}
	page, next, err := aipfilter.ListPage("gsp:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListGatewaySecurityPoliciesResponse{GatewaySecurityPolicies: page, NextPageToken: next}, nil
}

func (s *Server) UpdateGatewaySecurityPolicy(_ context.Context, req *nspb.UpdateGatewaySecurityPolicyRequest) (*longrunningpb.Operation, error) {
	return s.update(collGSP, req.GetGatewaySecurityPolicy(), req.GetUpdateMask(), []string{"description", "tls_inspection_policy"}, func(m proto.Message, loc string) error {
		if err := s.checkGSP(m, loc); err != nil {
			return err
		}
		if m.(*nspb.GatewaySecurityPolicy).GetTlsInspectionPolicy() == "" {
			name := m.(*nspb.GatewaySecurityPolicy).GetName()
			for _, r := range s.rulesOf(name) {
				if r.GetTlsInspectionEnabled() {
					return apierror.FailedPrecondition("The rule '%s' has tls_inspection_enabled, which needs the policy's tls_inspection_policy.", r.GetName())
				}
			}
		}
		return nil
	})
}

// DeleteGatewaySecurityPolicy deletes a policy and its rules; a policy a
// gateway names is in use.
func (s *Server) DeleteGatewaySecurityPolicy(_ context.Context, req *nspb.DeleteGatewaySecurityPolicyRequest) (*longrunningpb.Operation, error) {
	op, err := s.delete(collGSP, req.GetName())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.res {
		if strings.HasPrefix(k, req.GetName()+"/rules/") {
			delete(s.res, k)
		}
	}
	return op, s.saveLocked()
}

// --- Gateway security policy rules -----------------------------------------

func checkRuleName(name string) error {
	p := strings.Split(name, "/")
	if len(p) != 8 || p[0] != "projects" || p[2] != "locations" || p[4] != collGSP || p[6] != collRules || p[7] == "" {
		return apierror.InvalidArgument("Invalid resource field value in the request: name %q is not a GatewaySecurityPolicyRule name.", name)
	}
	return nil
}

// rulesOf are a policy's rules, by priority, copies.
func (s *Server) rulesOf(policy string) []*nspb.GatewaySecurityPolicyRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rulesOfLocked(policy)
}

func (s *Server) rulesOfLocked(policy string) []*nspb.GatewaySecurityPolicyRule {
	var out []*nspb.GatewaySecurityPolicyRule
	for k, m := range s.res {
		if strings.HasPrefix(k, policy+"/rules/") && strings.Count(k, "/") == 7 {
			out = append(out, proto.Clone(m).(*nspb.GatewaySecurityPolicyRule))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetPriority() != out[j].GetPriority() {
			return out[i].GetPriority() < out[j].GetPriority()
		}
		return out[i].GetName() < out[j].GetName()
	})
	return out
}

func regexCount(e string) (n int, long string) {
	for _, m := range regexRE.FindAllStringSubmatch(e, -1) {
		n++
		re := m[1] + m[2]
		if len(re) > maxRegexLen {
			long = re
		}
	}
	return n, long
}

// checkRule checks a rule in its policy: required fields, the matchers'
// CEL, unique priority, TLS inspection and the limits.
func (s *Server) checkRule(r *nspb.GatewaySecurityPolicyRule) error {
	policy := r.GetName()[:strings.Index(r.GetName(), "/rules/")]
	if r.GetBasicProfile() == nspb.GatewaySecurityPolicyRule_BASIC_PROFILE_UNSPECIFIED {
		return apierror.InvalidArgument("Invalid argument: basic_profile is required: ALLOW or DENY.")
	}
	if r.GetPriority() < 0 {
		return apierror.InvalidArgument("Invalid value for field 'priority': %d. It must be 0 or more.", r.GetPriority())
	}
	if strings.TrimSpace(r.GetSessionMatcher()) == "" {
		return apierror.InvalidArgument("Invalid argument: session_matcher is required.")
	}
	if len(r.GetDescription()) > 1024 {
		return apierror.InvalidArgument("Invalid value for field 'description': it must be at most 1024 characters long.")
	}
	for _, f := range []struct {
		field, expr string
		app         bool
	}{{"session_matcher", r.GetSessionMatcher(), false}, {"application_matcher", r.GetApplicationMatcher(), true}} {
		if f.expr == "" {
			continue
		}
		if len(f.expr) > maxMatcherLen {
			return apierror.InvalidArgument("Invalid value for field '%s': the matcher is longer than %d characters.", f.field, maxMatcherLen)
		}
		if _, long := regexCount(f.expr); long != "" {
			return apierror.InvalidArgument("Invalid value for field '%s': the regular expression '%s' is longer than %d characters.", f.field, long, maxRegexLen)
		}
		if err := networkservices.CheckMatcher(f.expr, f.app); err != nil {
			return apierror.InvalidArgument("Invalid value for field '%s': '%s' is not a valid CEL expression: %v", f.field, f.expr, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pol, ok := s.res[policy].(*nspb.GatewaySecurityPolicy)
	if !ok {
		return apierror.NotFound("GatewaySecurityPolicy '%s' not found.", policy)
	}
	if r.GetTlsInspectionEnabled() && pol.GetTlsInspectionPolicy() == "" {
		return apierror.InvalidArgument("Invalid value for field 'tls_inspection_enabled': it can only be true if the parent GatewaySecurityPolicy references a TLS inspection policy.")
	}
	rules := s.rulesOfLocked(policy)
	regex, _ := regexCount(r.GetSessionMatcher() + " " + r.GetApplicationMatcher())
	count := 0
	for _, o := range rules {
		if o.GetName() == r.GetName() {
			continue
		}
		count++
		if o.GetPriority() == r.GetPriority() {
			return apierror.InvalidArgument("Invalid value for field 'priority': %d. The rule '%s' already has this priority.", r.GetPriority(), o.GetName())
		}
		n, _ := regexCount(o.GetSessionMatcher() + " " + o.GetApplicationMatcher())
		regex += n
	}
	if count >= maxRulesPerPolicy {
		return apierror.FailedPrecondition("A gateway security policy has at most %d rules.", maxRulesPerPolicy)
	}
	if regex > maxRegexPerPolicy {
		return apierror.InvalidArgument("A gateway security policy has at most %d CEL regular expression matchers.", maxRegexPerPolicy)
	}
	return nil
}

func (s *Server) CreateGatewaySecurityPolicyRule(_ context.Context, req *nspb.CreateGatewaySecurityPolicyRuleRequest) (*longrunningpb.Operation, error) {
	parent := req.GetParent()
	if p := strings.Split(parent, "/"); len(p) != 6 || p[4] != collGSP || !parentRE.MatchString(strings.Join(p[:4], "/")) {
		return nil, apierror.InvalidArgument("Invalid resource field value in the request: parent %q must be projects/{project}/locations/{location}/gatewaySecurityPolicies/{gateway_security_policy}.", parent)
	}
	id := req.GetGatewaySecurityPolicyRuleId()
	if id == "" {
		// gcloud's import names the rule in its body.
		if n := req.GetGatewaySecurityPolicyRule().GetName(); strings.HasPrefix(n, parent+"/rules/") {
			id = strings.TrimPrefix(n, parent+"/rules/")
		}
	}
	if err := checkSWPID("gateway_security_policy_rule_id", id); err != nil {
		return nil, err
	}
	r, _ := proto.Clone(req.GetGatewaySecurityPolicyRule()).(*nspb.GatewaySecurityPolicyRule)
	if r == nil {
		r = &nspb.GatewaySecurityPolicyRule{}
	}
	r.Name = parent + "/rules/" + id
	if err := s.checkRule(r); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[r.Name]; ok {
		return nil, apierror.AlreadyExists("GatewaySecurityPolicyRule '%s' already exists.", r.Name)
	}
	stamp(r, s.opts.Now(), true)
	s.res[r.Name] = r
	return s.opLocked(r.Name, "create", r)
}

func (s *Server) GetGatewaySecurityPolicyRule(_ context.Context, req *nspb.GetGatewaySecurityPolicyRuleRequest) (*nspb.GatewaySecurityPolicyRule, error) {
	if err := checkRuleName(req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.res[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("GatewaySecurityPolicyRule '%s' not found.", req.GetName())
	}
	return proto.Clone(m).(*nspb.GatewaySecurityPolicyRule), nil
}

func (s *Server) ListGatewaySecurityPolicyRules(_ context.Context, req *nspb.ListGatewaySecurityPolicyRulesRequest) (*nspb.ListGatewaySecurityPolicyRulesResponse, error) {
	parent := req.GetParent()
	if p := strings.Split(parent, "/"); len(p) != 6 || p[4] != collGSP {
		return nil, apierror.InvalidArgument("Invalid resource field value in the request: parent %q must be projects/{project}/locations/{location}/gatewaySecurityPolicies/{gateway_security_policy}.", parent)
	}
	if !s.Exists(parent) {
		return nil, apierror.NotFound("GatewaySecurityPolicy '%s' not found.", parent)
	}
	page, next, err := aipfilter.ListPage("gspr:"+parent, "", "", s.rulesOf(parent), req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListGatewaySecurityPolicyRulesResponse{GatewaySecurityPolicyRules: page, NextPageToken: next}, nil
}

var ruleUpdatable = []string{"enabled", "priority", "description", "session_matcher", "application_matcher", "tls_inspection_enabled", "basic_profile"}

func (s *Server) UpdateGatewaySecurityPolicyRule(_ context.Context, req *nspb.UpdateGatewaySecurityPolicyRuleRequest) (*longrunningpb.Operation, error) {
	in := req.GetGatewaySecurityPolicyRule()
	if in == nil {
		return nil, apierror.InvalidArgument("Invalid argument: the GatewaySecurityPolicyRule is required.")
	}
	if err := checkRuleName(in.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	cur, ok := s.res[in.GetName()]
	s.mu.Unlock()
	if !ok {
		return nil, apierror.NotFound("GatewaySecurityPolicyRule '%s' not found.", in.GetName())
	}
	next := proto.Clone(cur).(*nspb.GatewaySecurityPolicyRule)
	if err := applyMask(next, in, req.GetUpdateMask(), ruleUpdatable); err != nil {
		return nil, err
	}
	if err := s.checkRule(next); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp(next, s.opts.Now(), false)
	s.res[next.Name] = next
	return s.opLocked(next.Name, "update", next)
}

// applyMask copies in's masked fields (every allowed one without a mask)
// to next.
func applyMask(next, in proto.Message, mask *fieldmaskpb.FieldMask, allowed []string) error {
	paths := allowed
	if len(mask.GetPaths()) > 0 {
		paths = nil
		for _, raw := range mask.GetPaths() {
			for _, p := range strings.Split(raw, ",") {
				paths = append(paths, camelToSnake(strings.TrimSpace(p)))
			}
		}
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	d, r := next.ProtoReflect(), in.ProtoReflect()
	for _, p := range paths {
		if !ok[p] {
			return apierror.InvalidArgument("Invalid update_mask path '%s': the field cannot be updated. Updatable fields are: %s.", p, strings.Join(allowed, ", "))
		}
		f := d.Descriptor().Fields().ByName(protoreflect.Name(p))
		if f == nil {
			continue
		}
		if r.Has(f) {
			d.Set(f, r.Get(f))
		} else {
			d.Clear(f)
		}
	}
	return nil
}

func (s *Server) DeleteGatewaySecurityPolicyRule(_ context.Context, req *nspb.DeleteGatewaySecurityPolicyRuleRequest) (*longrunningpb.Operation, error) {
	if err := checkRuleName(req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[req.GetName()]; !ok {
		return nil, apierror.NotFound("GatewaySecurityPolicyRule '%s' not found.", req.GetName())
	}
	delete(s.res, req.GetName())
	return s.opLocked(req.GetName(), "delete", nil)
}

// --- URL lists --------------------------------------------------------------

func checkURLList(m proto.Message, location string) error {
	u := m.(*nspb.UrlList)
	if err := regionalOnly(location, "UrlList"); err != nil {
		return err
	}
	if len(u.GetValues()) == 0 {
		return apierror.InvalidArgument("Invalid argument: values is required.")
	}
	if len(u.GetValues()) > maxURLListEntries {
		return apierror.InvalidArgument("Invalid value for field 'values': a URL list has at most %d entries.", maxURLListEntries)
	}
	for i, v := range u.GetValues() {
		if !networkservices.ValidURLListEntry(v) {
			return apierror.InvalidArgument("Invalid value for field 'values[%d]': '%s'. An entry is a domain (with a period) or, with a forward slash, a well-formed URL.", i, v)
		}
	}
	return nil
}

func (s *Server) CreateUrlList(_ context.Context, req *nspb.CreateUrlListRequest) (*longrunningpb.Operation, error) {
	if err := checkSWPID("url_list_id", req.GetUrlListId()); err != nil {
		return nil, err
	}
	if project, loc, err := parseParent(req.GetParent()); err == nil {
		s.mu.Lock()
		n := s.countLocked(project, loc, collURLList)
		s.mu.Unlock()
		if n >= maxURLListsPerRegion {
			return nil, quota("URL lists", project, maxURLListsPerRegion)
		}
	}
	u, _ := proto.Clone(req.GetUrlList()).(*nspb.UrlList)
	if u == nil {
		u = &nspb.UrlList{}
	}
	return s.create(req.GetParent(), collURLList, "url_list_id", req.GetUrlListId(), u, checkURLList)
}

func (s *Server) GetUrlList(_ context.Context, req *nspb.GetUrlListRequest) (*nspb.UrlList, error) {
	m, err := s.get(collURLList, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.UrlList), nil
}

func (s *Server) ListUrlLists(_ context.Context, req *nspb.ListUrlListsRequest) (*nspb.ListUrlListsResponse, error) {
	l, err := s.list(req.GetParent(), collURLList)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.UrlList, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.UrlList)
	}
	page, next, err := aipfilter.ListPage("url:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListUrlListsResponse{UrlLists: page, NextPageToken: next}, nil
}

func (s *Server) UpdateUrlList(_ context.Context, req *nspb.UpdateUrlListRequest) (*longrunningpb.Operation, error) {
	return s.update(collURLList, req.GetUrlList(), req.GetUpdateMask(), []string{"description", "values"}, checkURLList)
}

// DeleteUrlList refuses a list a rule names (inUrlList).
func (s *Server) DeleteUrlList(_ context.Context, req *nspb.DeleteUrlListRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	for k, m := range s.res {
		if r, ok := m.(*nspb.GatewaySecurityPolicyRule); ok && req.GetName() != "" &&
			(strings.Contains(r.GetSessionMatcher(), req.GetName()) || strings.Contains(r.GetApplicationMatcher(), req.GetName())) {
			s.mu.Unlock()
			return nil, apierror.FailedPrecondition("UrlList '%s' is in use by '%s'.", req.GetName(), k)
		}
	}
	s.mu.Unlock()
	return s.delete(collURLList, req.GetName())
}

// --- TLS inspection policies ------------------------------------------------

func (s *Server) checkTLSInspection(m proto.Message, location string) error {
	t := m.(*nspb.TlsInspectionPolicy)
	if err := regionalOnly(location, "TlsInspectionPolicy"); err != nil {
		return err
	}
	if t.GetCaPool() == "" {
		return apierror.InvalidArgument("Invalid argument: ca_pool is required.")
	}
	mm := caPoolRE.FindStringSubmatch(t.GetCaPool())
	if mm == nil {
		return apierror.InvalidArgument("Invalid value for field 'ca_pool': '%s'. It must be projects/{project}/locations/{location}/caPools/{ca_pool}.", t.GetCaPool())
	}
	if mm[2] != location {
		return apierror.InvalidArgument("Invalid value for field 'ca_pool': '%s'. The CA pool must be in the policy's location '%s'.", t.GetCaPool(), location)
	}
	if f := s.hooksNow().CAPoolExists; f != nil && !f(t.GetCaPool()) {
		return apierror.NotFound("CaPool '%s' not found.", t.GetCaPool())
	}
	if tc := t.GetTrustConfig(); tc != "" {
		if !trustConfigRE.MatchString(tc) {
			return apierror.InvalidArgument("Invalid value for field 'trust_config': '%s'. It must be projects/{project}/locations/{location}/trustConfigs/{trust_config}.", tc)
		}
		if f := s.hooksNow().TrustConfigExists; f != nil && !f(tc) {
			return apierror.NotFound("TrustConfig '%s' not found.", tc)
		}
	}
	if len(t.GetCustomTlsFeatures()) > 0 && t.GetTlsFeatureProfile() != nspb.TlsInspectionPolicy_PROFILE_CUSTOM {
		return apierror.InvalidArgument("Invalid value for field 'custom_tls_features': it is valid only if tls_feature_profile is PROFILE_CUSTOM.")
	}
	return nil
}

func (s *Server) CreateTlsInspectionPolicy(_ context.Context, req *nspb.CreateTlsInspectionPolicyRequest) (*longrunningpb.Operation, error) {
	if err := checkSWPID("tls_inspection_policy_id", req.GetTlsInspectionPolicyId()); err != nil {
		return nil, err
	}
	t, _ := proto.Clone(req.GetTlsInspectionPolicy()).(*nspb.TlsInspectionPolicy)
	if t == nil {
		t = &nspb.TlsInspectionPolicy{}
	}
	return s.create(req.GetParent(), collTLSInspect, "tls_inspection_policy_id", req.GetTlsInspectionPolicyId(), t, s.checkTLSInspection)
}

func (s *Server) GetTlsInspectionPolicy(_ context.Context, req *nspb.GetTlsInspectionPolicyRequest) (*nspb.TlsInspectionPolicy, error) {
	m, err := s.get(collTLSInspect, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*nspb.TlsInspectionPolicy), nil
}

func (s *Server) ListTlsInspectionPolicies(_ context.Context, req *nspb.ListTlsInspectionPoliciesRequest) (*nspb.ListTlsInspectionPoliciesResponse, error) {
	l, err := s.list(req.GetParent(), collTLSInspect)
	if err != nil {
		return nil, err
	}
	items := make([]*nspb.TlsInspectionPolicy, len(l))
	for i, m := range l {
		items[i] = m.(*nspb.TlsInspectionPolicy)
	}
	page, next, err := aipfilter.ListPage("tip:"+req.GetParent(), "", "", items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &nspb.ListTlsInspectionPoliciesResponse{TlsInspectionPolicies: page, NextPageToken: next}, nil
}

func (s *Server) UpdateTlsInspectionPolicy(_ context.Context, req *nspb.UpdateTlsInspectionPolicyRequest) (*longrunningpb.Operation, error) {
	return s.update(collTLSInspect, req.GetTlsInspectionPolicy(), req.GetUpdateMask(),
		[]string{"description", "ca_pool", "trust_config", "exclude_public_ca_set", "min_tls_version", "tls_feature_profile", "custom_tls_features"}, s.checkTLSInspection)
}

// DeleteTlsInspectionPolicy refuses a policy a gateway security policy
// names, unless force.
func (s *Server) DeleteTlsInspectionPolicy(_ context.Context, req *nspb.DeleteTlsInspectionPolicyRequest) (*longrunningpb.Operation, error) {
	if !req.GetForce() {
		s.mu.Lock()
		for k, m := range s.res {
			if g, ok := m.(*nspb.GatewaySecurityPolicy); ok && ruleRel(g.GetTlsInspectionPolicy()) == req.GetName() {
				s.mu.Unlock()
				return nil, apierror.FailedPrecondition("TlsInspectionPolicy '%s' is in use by '%s'.", req.GetName(), k)
			}
		}
		s.mu.Unlock()
	}
	return s.delete(collTLSInspect, req.GetName())
}

// --- What Secure Web Proxy reads ---------------------------------------------

// GatewayRules are a gateway security policy's rules as the proxy applies
// them (networkservices.Policies).
func (s *Server) GatewayRules(policy string) ([]networkservices.GatewayRule, bool) {
	if !s.Exists(policy) {
		return nil, false
	}
	var out []networkservices.GatewayRule
	for _, r := range s.rulesOf(policy) {
		out = append(out, networkservices.GatewayRule{Name: r.GetName(), Priority: r.GetPriority(), Enabled: r.GetEnabled(),
			Allow: r.GetBasicProfile() == nspb.GatewaySecurityPolicyRule_ALLOW, TLSInspection: r.GetTlsInspectionEnabled(),
			Session: r.GetSessionMatcher(), Application: r.GetApplicationMatcher()})
	}
	return out, true
}

// URLList is a URL list's values.
func (s *Server) URLList(name string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.res[name].(*nspb.UrlList)
	if !ok {
		return nil, false
	}
	return append([]string(nil), u.GetValues()...), true
}

// TLSInspectionCAPool is the CA pool of a gateway security policy's TLS
// inspection policy (networkservices.TLSInspections); ok is false when the
// policy names none, or it is gone.
func (s *Server) TLSInspectionCAPool(policy string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.res[policy].(*nspb.GatewaySecurityPolicy)
	if !ok || g.GetTlsInspectionPolicy() == "" {
		return "", false
	}
	t, ok := s.res[ruleRel(g.GetTlsInspectionPolicy())].(*nspb.TlsInspectionPolicy)
	if !ok || t.GetCaPool() == "" {
		return "", false
	}
	return ruleRel(t.GetCaPool()), true
}

var _ networkservices.TLSInspections = (*Server)(nil)
