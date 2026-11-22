// Package iap serves the Identity-Aware Proxy API v1 (iap.googleapis.com,
// #2160) in the CLI process, on the Compute Engine API's port, over JSON as
// Google serves it: IdentityAwareProxyAdminService (the IAM policies and
// IAP settings of IAP's resources, attribute-expression validation and the
// TCP forwarding destination groups) and IdentityAwareProxyOAuthService
// (brands and IAP OAuth clients).
//
// It follows https://docs.cloud.google.com/iap/docs/reference/rest,
// .../iap/docs/managing-access, .../iap/docs/customizing,
// .../iap/docs/load-balancer-howto and
// .../iap/docs/deprecations/migrate-oauth-client (read 2026-11-22):
//
//   - IAP's resources are projects/P/iap_web (every web resource of a
//     project), .../iap_web/compute (its global backend services),
//     .../iap_web/compute-REGION (a region's), .../iap_web/compute/services/S,
//     App Engine's and Cloud Run's alike, and the TCP tunnel's
//     (projects/P/iap_tunnel, zones, instances and destination groups). A
//     policy at a level applies to every resource below it, and the
//     project's to all, as IAM inherits; Allowed reads them that way, with
//     their conditions (request.host, request.path, request.time);
//   - IAP settings are kept per resource and merged from the top level
//     down, the deepest one winning field by field;
//   - the OAuth Admin API is deprecated since January 22, 2025: brands and
//     IAP OAuth clients can no longer be created, and IAP uses a
//     Google-managed OAuth client when none is configured. Here none
//     exists, so List answers none and Create is refused.
package iap

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/iap/apiv1/iappb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Options configure a Server.
type Options struct {
	// State is the JSON file the policies and settings are kept in; empty
	// keeps them in memory.
	State string
	// BackendService reports whether a backend service exists
	// (projects/P/global/backendServices/S or .../regions/R/...); nil
	// accepts every one.
	BackendService func(rel string) bool
	// Normalize is a resource name with its project as its ID and its
	// backend service as its name, where the name gives the project's
	// number or the service's ID: both name the same resource on Google.
	// Nil keeps names as they are.
	Normalize func(name string) string
}

// Server is the Identity-Aware Proxy API.
type Server struct {
	iappb.UnimplementedIdentityAwareProxyAdminServiceServer
	iappb.UnimplementedIdentityAwareProxyOAuthServiceServer
	opts Options

	mu       sync.Mutex
	policies map[string]*iampolicy.Stored
	settings map[string]*iappb.IapSettings
	groups   map[string]*iappb.TunnelDestGroup
}

// New returns a Server with its state restored.
func New(opts Options) (*Server, error) {
	s := &Server{opts: opts, policies: map[string]*iampolicy.Stored{}, settings: map[string]*iappb.IapSettings{},
		groups: map[string]*iappb.TunnelDestGroup{}}
	if err := s.restore(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetNormalizer sets Options.Normalize after New.
func (s *Server) SetNormalizer(f func(string) string) {
	s.mu.Lock()
	s.opts.Normalize = f
	s.mu.Unlock()
}

// norm is name normalized (Options.Normalize).
func (s *Server) norm(name string) string {
	s.mu.Lock()
	f := s.opts.Normalize
	s.mu.Unlock()
	if f == nil {
		return name
	}
	return f(name)
}

// SetBackendServices sets the backend-service lookup after New.
func (s *Server) SetBackendServices(f func(rel string) bool) {
	s.mu.Lock()
	s.opts.BackendService = f
	s.mu.Unlock()
}

var (
	// resourceRE are IAP's resources, as the REST reference's
	// {resource=**} and {name=**} take them.
	resourceRE    = regexp.MustCompile(`^projects/([^/]+)/(iap_web(/(compute|compute-[a-z0-9-]+|appengine-[^/]+|cloud_run-[a-z0-9-]+|forwarding_rule|forwarding_rule-[a-z0-9-]+)(/services/[^/]+(/versions/[^/]+)?)?)?|iap_tunnel(/zones/[^/]+(/instances/[^/]+)?|/locations/[^/]+(/destGroups/[^/]+)?)?)$`)
	groupParentRE = regexp.MustCompile(`^projects/([^/]+)/iap_tunnel/locations/([^/]+)$`)
	groupRE       = regexp.MustCompile(`^projects/([^/]+)/iap_tunnel/locations/([^/]+)/destGroups/([^/]+)$`)
	groupIDRE     = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	fqdnRE        = regexp.MustCompile(`^(\*\.)?([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

// project is a resource's project, "" when it is not one of IAP's.
func checkResource(name string) (string, error) {
	m := resourceRE.FindStringSubmatch(name)
	if m == nil {
		return "", apierror.InvalidArgument("Invalid resource name '%s': it must name an IAP resource, such as projects/{project}/iap_web/compute/services/{backend_service}.", name)
	}
	return m[1], nil
}

// backendOf is the backend service an iap_web compute resource names, "".
func backendOf(name string) string {
	p := strings.Split(name, "/")
	if len(p) < 5 || p[2] != "iap_web" || p[4] != "services" || !(p[3] == "compute" || strings.HasPrefix(p[3], "compute-")) {
		return ""
	}
	if p[3] == "compute" {
		return "projects/" + p[1] + "/global/backendServices/" + p[5]
	}
	return "projects/" + p[1] + "/regions/" + strings.TrimPrefix(p[3], "compute-") + "/backendServices/" + p[5]
}

// checkExistsLocked refuses a backend service's resource whose backend
// service does not exist.
func (s *Server) checkExistsLocked(name string) error {
	if bs := backendOf(name); bs != "" && s.opts.BackendService != nil && !s.opts.BackendService(bs) {
		return apierror.NotFound("Requested entity was not found: backend service '%s' does not exist.", bs)
	}
	return nil
}

// Levels are a resource and the IAP levels above it, the resource first:
// projects/P/iap_web/compute/services/S, projects/P/iap_web/compute,
// projects/P/iap_web.
func Levels(name string) []string {
	var out []string
	p := strings.Split(name, "/")
	for n := len(p); n >= 3; n-- {
		seg := p[n-1]
		if seg == "services" || seg == "versions" || seg == "zones" || seg == "instances" || seg == "locations" || seg == "destGroups" {
			continue
		}
		out = append(out, strings.Join(p[:n], "/"))
	}
	return out
}

func (s *Server) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	req = proto.Clone(req).(*iampb.SetIamPolicyRequest)
	req.Resource = s.norm(req.GetResource())
	if _, err := checkResource(req.GetResource()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkExistsLocked(req.GetResource()); err != nil {
		return nil, err
	}
	st, err := iampolicy.SetConditional(s.policies[req.GetResource()], req)
	if err != nil {
		return nil, err
	}
	s.policies[req.GetResource()] = &st
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return iampolicy.Get(&st, &iampb.GetPolicyOptions{RequestedPolicyVersion: 3})
}

func (s *Server) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	req = proto.Clone(req).(*iampb.GetIamPolicyRequest)
	req.Resource = s.norm(req.GetResource())
	if _, err := checkResource(req.GetResource()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkExistsLocked(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Get(s.policies[req.GetResource()], req.GetOptions())
}

func (s *Server) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	req = proto.Clone(req).(*iampb.TestIamPermissionsRequest)
	req.Resource = s.norm(req.GetResource())
	if _, err := checkResource(req.GetResource()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkExistsLocked(req.GetResource()); err != nil {
		return nil, err
	}
	for _, p := range req.GetPermissions() {
		if !strings.HasPrefix(p, "iap.") {
			return nil, apierror.InvalidArgument("Permission '%s' is not valid for this resource.", p)
		}
	}
	return iampolicy.Permissions(req), nil
}

// Policy is a resource's own policy, nil when none is set: the
// enforcement's reader. name gives the project by ID and the backend
// service by name, as the load balancers ask: it is not normalized, since
// they ask while the Compute Engine API holds its lock.
func (s *Server) Policy(name string) *iampolicy.Stored {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.policies[name]; p != nil {
		cp := *p
		return &cp
	}
	return nil
}

func (s *Server) GetIapSettings(_ context.Context, req *iappb.GetIapSettingsRequest) (*iappb.IapSettings, error) {
	req = proto.Clone(req).(*iappb.GetIapSettingsRequest)
	req.Name = s.norm(req.GetName())
	if _, err := checkResource(req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkExistsLocked(req.GetName()); err != nil {
		return nil, err
	}
	if st := s.settings[req.GetName()]; st != nil {
		return proto.Clone(st).(*iappb.IapSettings), nil
	}
	return &iappb.IapSettings{Name: req.GetName()}, nil
}

func (s *Server) UpdateIapSettings(_ context.Context, req *iappb.UpdateIapSettingsRequest) (*iappb.IapSettings, error) {
	in := &iappb.IapSettings{}
	if req.GetIapSettings() != nil {
		in = proto.Clone(req.GetIapSettings()).(*iappb.IapSettings)
	}
	in.Name = s.norm(in.GetName())
	if _, err := checkResource(in.GetName()); err != nil {
		return nil, err
	}
	if err := checkSettings(in); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkExistsLocked(in.GetName()); err != nil {
		return nil, err
	}
	cur := s.settings[in.GetName()]
	if cur == nil {
		cur = &iappb.IapSettings{Name: in.GetName()}
	}
	next := proto.Clone(cur).(*iappb.IapSettings)
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		next = proto.Clone(in).(*iappb.IapSettings)
	} else if err := applyMask(next, in, req.GetUpdateMask()); err != nil {
		return nil, err
	}
	next.Name = in.GetName()
	s.settings[in.GetName()] = next
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return proto.Clone(next).(*iappb.IapSettings), nil
}

// checkSettings checks the settings' documented rules.
func checkSettings(st *iappb.IapSettings) error {
	as := st.GetAccessSettings()
	if r := as.GetReauthSettings(); r != nil {
		if r.GetMaxAge() != nil && (r.GetMaxAge().AsDuration() < 5*time.Minute || r.GetMaxAge().AsDuration() > 7*24*time.Hour) {
			return apierror.InvalidArgument("Invalid reauth_settings.max_age: it must be between 5 minutes and 7 days.")
		}
		if r.GetMethod() != iappb.ReauthSettings_METHOD_UNSPECIFIED && r.GetMaxAge() == nil {
			return apierror.InvalidArgument("Invalid reauth_settings: max_age is required with a method.")
		}
	}
	for _, d := range as.GetAllowedDomainsSettings().GetDomains() {
		if !fqdnRE.MatchString(strings.ToLower(d)) {
			return apierror.InvalidArgument("Invalid allowed_domains_settings.domains entry '%s'.", d)
		}
	}
	if u := st.GetApplicationSettings().GetAccessDeniedPageSettings().GetAccessDeniedPageUri(); u != nil && u.GetValue() != "" &&
		!strings.HasPrefix(u.GetValue(), "https://") {
		return apierror.InvalidArgument("Invalid access_denied_page_uri '%s': it must be an https URL.", u.GetValue())
	}
	if ap := st.GetApplicationSettings().GetAttributePropagationSettings(); ap != nil && ap.GetEnable() && ap.GetExpression() == "" {
		return apierror.InvalidArgument("Invalid attribute_propagation_settings: an expression is required when it is enabled.")
	}
	return nil
}

// applyMask copies the (dotted) paths of mask from src into dst.
func applyMask(dst, src proto.Message, mask *fieldmaskpb.FieldMask) error {
	for _, raw := range mask.GetPaths() {
		for _, path := range strings.Split(raw, ",") {
			path = strings.TrimSpace(path)
			if err := copyPath(dst.ProtoReflect(), src.ProtoReflect(), strings.Split(camelToSnake(path), ".")); err != nil {
				return apierror.InvalidArgument("Invalid update_mask path '%s'.", path)
			}
		}
	}
	return nil
}

func copyPath(d, s protoreflect.Message, path []string) error {
	f := d.Descriptor().Fields().ByName(protoreflect.Name(path[0]))
	if f == nil {
		return fmt.Errorf("no field %s", path[0])
	}
	if len(path) == 1 {
		if s.Has(f) {
			d.Set(f, s.Get(f))
		} else {
			d.Clear(f)
		}
		return nil
	}
	if f.Message() == nil || f.IsList() || f.IsMap() {
		return fmt.Errorf("%s is not a message", path[0])
	}
	if !s.Has(f) {
		if d.Has(f) {
			return copyPath(d.Mutable(f).Message(), d.Get(f).Message().Type().New(), path[1:])
		}
		return nil
	}
	return copyPath(d.Mutable(f).Message(), s.Get(f).Message(), path[1:])
}

func camelToSnake(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Settings are a resource's settings merged from the top level down, as
// JSON (compute.IAPGate); nil when none is set.
//
// As Policy, name is not normalized.
func (s *Server) Settings(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	lv := Levels(name)
	var merged *iappb.IapSettings
	for i := len(lv) - 1; i >= 0; i-- {
		if st := s.settings[lv[i]]; st != nil {
			if merged == nil {
				merged = &iappb.IapSettings{}
			}
			proto.Merge(merged, st)
		}
	}
	if merged == nil {
		return nil
	}
	merged.Name = name
	b, err := protojson.Marshal(merged)
	if err != nil {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// ValidateIapAttributeExpression checks an attribute propagation
// expression (iap/docs/customizing, "Attribute propagation"): a CEL
// expression over attributes.saml_attributes or
// attributes.iap_attributes, such as
// attributes.saml_attributes.filter(attribute, attribute.name in ["test1"]).
func (s *Server) ValidateIapAttributeExpression(_ context.Context, req *iappb.ValidateIapAttributeExpressionRequest) (*iappb.ValidateIapAttributeExpressionResponse, error) {
	if _, err := checkResource(req.GetName()); err != nil {
		return nil, err
	}
	e := strings.TrimSpace(req.GetExpression())
	if e == "" {
		return nil, apierror.InvalidArgument("Invalid expression: it is empty.")
	}
	if !strings.Contains(e, "attributes.saml_attributes") && !strings.Contains(e, "attributes.iap_attributes") {
		return nil, apierror.InvalidArgument("Invalid expression '%s': it must select from attributes.saml_attributes or attributes.iap_attributes.", e)
	}
	depth, quote := 0, rune(0)
	for _, r := range e {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '(' || r == '[':
			depth++
		case r == ')' || r == ']':
			depth--
			if depth < 0 {
				return nil, apierror.InvalidArgument("Invalid expression '%s': unbalanced brackets.", e)
			}
		}
	}
	if depth != 0 || quote != 0 {
		return nil, apierror.InvalidArgument("Invalid expression '%s': unbalanced brackets or quotes.", e)
	}
	return &iappb.ValidateIapAttributeExpressionResponse{}, nil
}

// checkGroup checks a destination group's CIDRs and FQDNs.
func checkGroup(g *iappb.TunnelDestGroup) error {
	if len(g.GetCidrs()) == 0 && len(g.GetFqdns()) == 0 {
		return apierror.InvalidArgument("A destination group needs at least one CIDR or FQDN.")
	}
	for _, c := range g.GetCidrs() {
		if _, n, err := net.ParseCIDR(c); err != nil || n.String() != c {
			return apierror.InvalidArgument("Invalid CIDR '%s'.", c)
		}
	}
	for _, f := range g.GetFqdns() {
		if !fqdnRE.MatchString(strings.ToLower(f)) {
			return apierror.InvalidArgument("Invalid FQDN '%s'.", f)
		}
	}
	return nil
}

func (s *Server) CreateTunnelDestGroup(_ context.Context, req *iappb.CreateTunnelDestGroupRequest) (*iappb.TunnelDestGroup, error) {
	m := groupParentRE.FindStringSubmatch(req.GetParent())
	if m == nil || !compute.IsRegion(m[2]) {
		return nil, apierror.InvalidArgument("Invalid parent '%s': it must be projects/{project}/iap_tunnel/locations/{region}.", req.GetParent())
	}
	if !groupIDRE.MatchString(req.GetTunnelDestGroupId()) {
		return nil, apierror.InvalidArgument("Invalid tunnel_dest_group_id '%s': it must be 1 to 63 lowercase letters, digits and hyphens, starting with a letter.", req.GetTunnelDestGroupId())
	}
	g, _ := proto.Clone(req.GetTunnelDestGroup()).(*iappb.TunnelDestGroup)
	if g == nil {
		g = &iappb.TunnelDestGroup{}
	}
	if err := checkGroup(g); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/destGroups/" + req.GetTunnelDestGroupId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[name]; ok {
		return nil, apierror.AlreadyExists("Destination group '%s' already exists.", name)
	}
	g.Name = name
	s.groups[name] = g
	return proto.Clone(g).(*iappb.TunnelDestGroup), s.saveLocked()
}

func (s *Server) GetTunnelDestGroup(_ context.Context, req *iappb.GetTunnelDestGroupRequest) (*iappb.TunnelDestGroup, error) {
	if !groupRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("Invalid name '%s'.", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[req.GetName()]
	if g == nil {
		return nil, apierror.NotFound("Destination group '%s' not found.", req.GetName())
	}
	return proto.Clone(g).(*iappb.TunnelDestGroup), nil
}

func (s *Server) ListTunnelDestGroups(_ context.Context, req *iappb.ListTunnelDestGroupsRequest) (*iappb.ListTunnelDestGroupsResponse, error) {
	m := groupParentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("Invalid parent '%s'.", req.GetParent())
	}
	s.mu.Lock()
	var names []string
	for k := range s.groups {
		if strings.HasPrefix(k, req.GetParent()+"/destGroups/") || (m[2] == "-" && strings.HasPrefix(k, "projects/"+m[1]+"/iap_tunnel/")) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	page, next, err := paging.Page("groups:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	out := &iappb.ListTunnelDestGroupsResponse{NextPageToken: next}
	for _, k := range page {
		out.TunnelDestGroups = append(out.TunnelDestGroups, proto.Clone(s.groups[k]).(*iappb.TunnelDestGroup))
	}
	s.mu.Unlock()
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	return out, nil
}

func (s *Server) UpdateTunnelDestGroup(_ context.Context, req *iappb.UpdateTunnelDestGroupRequest) (*iappb.TunnelDestGroup, error) {
	in := req.GetTunnelDestGroup()
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.groups[in.GetName()]
	if cur == nil {
		return nil, apierror.NotFound("Destination group '%s' not found.", in.GetName())
	}
	next := proto.Clone(cur).(*iappb.TunnelDestGroup)
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		next.Cidrs, next.Fqdns = in.GetCidrs(), in.GetFqdns()
	} else {
		for _, p := range req.GetUpdateMask().GetPaths() {
			if p != "cidrs" && p != "fqdns" {
				return nil, apierror.InvalidArgument("Invalid update_mask path '%s': only cidrs and fqdns can be updated.", p)
			}
		}
		if err := applyMask(next, in, req.GetUpdateMask()); err != nil {
			return nil, err
		}
	}
	if err := checkGroup(next); err != nil {
		return nil, err
	}
	s.groups[in.GetName()] = next
	return proto.Clone(next).(*iappb.TunnelDestGroup), s.saveLocked()
}

func (s *Server) DeleteTunnelDestGroup(_ context.Context, req *iappb.DeleteTunnelDestGroupRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups[req.GetName()] == nil {
		return nil, apierror.NotFound("Destination group '%s' not found.", req.GetName())
	}
	delete(s.groups, req.GetName())
	return &emptypb.Empty{}, s.saveLocked()
}

// The OAuth Admin API (brands and IAP OAuth clients), deprecated since
// January 22, 2025 (iap/docs/deprecations/migrate-oauth-client): custom
// OAuth clients can no longer be created or managed through it, and IAP
// uses a Google-managed OAuth client by default. No brand or client was
// ever created here, so lists are empty and creation is refused.

const oauthDeprecated = "The IAP OAuth Admin API is deprecated: as of January 22, 2025, brands and IAP OAuth clients can no longer be created or managed through it. IAP uses a Google-managed OAuth client by default; configure a custom OAuth client in the Google Cloud console instead."

var projectRE = regexp.MustCompile(`^projects/[^/]+$`)

func (s *Server) ListBrands(_ context.Context, req *iappb.ListBrandsRequest) (*iappb.ListBrandsResponse, error) {
	if !projectRE.MatchString(req.GetParent()) {
		return nil, apierror.InvalidArgument("Invalid parent '%s': it must be projects/{project_number or project_id}.", req.GetParent())
	}
	return &iappb.ListBrandsResponse{}, nil
}

func (s *Server) CreateBrand(_ context.Context, req *iappb.CreateBrandRequest) (*iappb.Brand, error) {
	return nil, apierror.FailedPrecondition("%s", oauthDeprecated)
}

func (s *Server) GetBrand(_ context.Context, req *iappb.GetBrandRequest) (*iappb.Brand, error) {
	return nil, apierror.NotFound("Brand '%s' not found.", req.GetName())
}

func (s *Server) CreateIdentityAwareProxyClient(context.Context, *iappb.CreateIdentityAwareProxyClientRequest) (*iappb.IdentityAwareProxyClient, error) {
	return nil, apierror.FailedPrecondition("%s", oauthDeprecated)
}

func (s *Server) ListIdentityAwareProxyClients(_ context.Context, req *iappb.ListIdentityAwareProxyClientsRequest) (*iappb.ListIdentityAwareProxyClientsResponse, error) {
	return nil, apierror.NotFound("Brand '%s' not found.", req.GetParent())
}

func (s *Server) GetIdentityAwareProxyClient(_ context.Context, req *iappb.GetIdentityAwareProxyClientRequest) (*iappb.IdentityAwareProxyClient, error) {
	return nil, apierror.NotFound("IAP OAuth client '%s' not found.", req.GetName())
}

func (s *Server) ResetIdentityAwareProxyClientSecret(_ context.Context, req *iappb.ResetIdentityAwareProxyClientSecretRequest) (*iappb.IdentityAwareProxyClient, error) {
	return nil, apierror.NotFound("IAP OAuth client '%s' not found.", req.GetName())
}

func (s *Server) DeleteIdentityAwareProxyClient(_ context.Context, req *iappb.DeleteIdentityAwareProxyClientRequest) (*emptypb.Empty, error) {
	return nil, apierror.NotFound("IAP OAuth client '%s' not found.", req.GetName())
}

// ResetProject forgets a project's policies, settings and groups, or every
// project's when project is "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := func(k string) bool { return project == "" || strings.HasPrefix(k, "projects/"+project+"/") }
	for k := range s.policies {
		if in(k) {
			delete(s.policies, k)
		}
	}
	for k := range s.settings {
		if in(k) {
			delete(s.settings, k)
		}
	}
	for k := range s.groups {
		if in(k) {
			delete(s.groups, k)
		}
	}
	return s.saveLocked()
}

type state struct {
	Policies map[string]*iampolicy.Stored `json:"policies"`
	Settings map[string]json.RawMessage   `json:"settings"`
	Groups   map[string]json.RawMessage   `json:"groups"`
}

func (s *Server) restore() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.State)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("iap: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("iap: %s: %w", s.opts.State, err)
	}
	for k, p := range st.Policies {
		s.policies[k] = p
	}
	for k, raw := range st.Settings {
		m := &iappb.IapSettings{}
		if err := protojson.Unmarshal(raw, m); err != nil {
			return fmt.Errorf("iap: settings %s: %w", k, err)
		}
		s.settings[k] = m
	}
	for k, raw := range st.Groups {
		m := &iappb.TunnelDestGroup{}
		if err := protojson.Unmarshal(raw, m); err != nil {
			return fmt.Errorf("iap: group %s: %w", k, err)
		}
		s.groups[k] = m
	}
	return nil
}

func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	st := state{Policies: s.policies, Settings: map[string]json.RawMessage{}, Groups: map[string]json.RawMessage{}}
	for k, m := range s.settings {
		b, err := protojson.Marshal(m)
		if err != nil {
			return apierror.Internal(err, "save")
		}
		st.Settings[k] = b
	}
	for k, m := range s.groups {
		b, err := protojson.Marshal(m)
		if err != nil {
			return apierror.Internal(err, "save")
		}
		st.Groups[k] = b
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return apierror.Internal(err, "save")
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.State), 0o755); err != nil {
		return apierror.Internal(err, "save")
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return apierror.Internal(err, "save")
	}
	if err := os.Rename(tmp, s.opts.State); err != nil {
		return apierror.Internal(err, "save")
	}
	return nil
}
