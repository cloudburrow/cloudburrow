package resourcemanager

// IAM v2 deny policies (#1886): google.iam.v2.Policies on the IAM API's
// port (this one), over gRPC and JSON, as "Deny access to resources" and
// the v2 REST reference describe them
// (https://docs.cloud.google.com/iam/docs/deny-access,
// https://docs.cloud.google.com/iam/docs/reference/rest/v2/policies, read
// 2026-11-19):
//
//   - a policy is policies/{attachmentPoint}/denypolicies/{policyId}, the
//     attachment point the URL-encoded full resource name of a project
//     (by ID or number), a folder or the organization; names come back
//     with the project's number, as Google's examples show;
//   - policyId is 3 to 63 characters, lowercase letters, digits, dashes and
//     periods, starting with a lowercase letter;
//   - create, update and delete answer a long-running operation, done
//     here, whose response is the policy (google.iam.v2.Policy, metadata
//     google.iam.v2.PolicyOperationMetadata); GetOperation reads it back;
//   - list answers metadata only, without rules, in one page of up to 1000
//     (the page size is ignored, as Google documents);
//   - update replaces the display name, annotations and rules; an etag that
//     is not the stored one is ABORTED (409), as it is for delete;
//   - Google's limits: 500 deny policies per resource, 500 deny rules in a
//     policy and across a resource's policies, 500 domains and groups and
//     2500 principals across a resource's policies (every appearance
//     counted), 12 logic operators in a condition, a display name of 63
//     characters, annotation keys of 63 and values of 255, rule
//     descriptions of 256 (https://docs.cloud.google.com/iam/quotas);
//   - deniedPermissions and exceptionPermissions are permissions or
//     permission groups Google lists as supported in deny policies
//     (internal/iameval's catalog); principals are v2 identifiers;
//     denialCondition may use resource tag functions only.
//
// The wording of the INVALID_ARGUMENT and NOT_FOUND messages is
// UNVERIFIED: Google's pages do not quote them (re-read 2026-11-19:
// https://docs.cloud.google.com/iam/docs/reference/rest/v2/policies).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const (
	denyPolicyPrefix = "iam/denypolicies/"
	denyKind         = "denypolicies"
	crmHost          = "cloudresourcemanager.googleapis.com/"

	// Google's limits (https://docs.cloud.google.com/iam/quotas).
	MaxDenyPoliciesPerResource = 500
	MaxDenyRulesPerResource    = 500
	MaxDenyRulesPerPolicy      = 500
	MaxDenyGroupsPerResource   = 500
	MaxDenyPrincipalsPerResour = 2500
)

var (
	policyIDRe   = regexp.MustCompile(`^[a-z][a-z0-9.-]{2,62}$`)
	principalRes = []*regexp.Regexp{
		regexp.MustCompile(`^principal://goog/subject/[^/\s]+$`),
		regexp.MustCompile(`^principal://iam\.googleapis\.com/projects/-/serviceAccounts/[^/\s]+$`),
		regexp.MustCompile(`^principalSet://goog/group/[^/\s]+$`),
		regexp.MustCompile(`^principalSet://goog/cloudIdentityCustomerId/[^/\s]+$`),
		regexp.MustCompile(`^principal://iam\.googleapis\.com/locations/global/workforcePools/[^/\s]+/subject/[^\s]+$`),
		regexp.MustCompile(`^principalSet://iam\.googleapis\.com/locations/global/workforcePools/[^/\s]+/(group/[^/\s]+|attribute\.[^/\s]+/[^\s]+|\*)$`),
		regexp.MustCompile(`^principal://iam\.googleapis\.com/projects/[0-9]+/locations/global/workloadIdentityPools/[^/\s]+/subject/[^\s]+$`),
		regexp.MustCompile(`^principalSet://iam\.googleapis\.com/projects/[0-9]+/locations/global/workloadIdentityPools/[^/\s]+/(group/[^/\s]+|attribute\.[^/\s]+/[^\s]+|\*)$`),
	}
	publicAll = "principalSet://goog/public:all"
	// tagFunctions are what a denial condition may call.
	tagFunctions = map[string]bool{"resource.matchTag": true, "resource.matchTagId": true,
		"resource.hasTagKey": true, "resource.hasTagKeyId": true}
	identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*`)
	strRe   = regexp.MustCompile(`"(\\.|[^"\\])*"|'(\\.|[^'\\])*'`)
)

// denyName is a parsed policy or parent name.
type denyName struct {
	attachment string // "projects/p", "folders/n", "organizations/n" as given
	id         string // "" for a parent
}

// parseDenyName parses policies/{attachmentPoint}/denypolicies[/{id}], the
// attachment point URL-encoded (as gRPC and gcloud send it) or not (as a
// JSON path's capture decodes it).
func parseDenyName(name string, parent bool) (denyName, error) {
	what := "name"
	if parent {
		what = "parent"
	}
	rest, ok := strings.CutPrefix(name, "policies/")
	if !ok {
		return denyName{}, apierror.InvalidArgument("%s %q must be policies/{attachment_point}/denypolicies%s", what, name, map[bool]string{false: "/{policy_id}"}[parent])
	}
	var id string
	if !parent {
		i := strings.LastIndex(rest, "/")
		if i < 0 {
			return denyName{}, apierror.InvalidArgument("name %q must be policies/{attachment_point}/denypolicies/{policy_id}", name)
		}
		rest, id = rest[:i], rest[i+1:]
	}
	att, kind, ok := cutLast(rest, "/")
	if !ok {
		return denyName{}, apierror.InvalidArgument("%s %q must be policies/{attachment_point}/denypolicies", what, name)
	}
	if kind != denyKind {
		return denyName{}, apierror.InvalidArgument("%s %q: the policy kind %q is not supported; the supported kind is denypolicies", what, name, kind)
	}
	if dec, err := url.PathUnescape(att); err == nil {
		att = dec
	}
	a, ok := strings.CutPrefix(att, crmHost)
	if !ok {
		return denyName{}, apierror.InvalidArgument("%s %q: the attachment point must be cloudresourcemanager.googleapis.com/projects/{project}, folders/{folder} or organizations/{organization}, URL-encoded", what, name)
	}
	parts := strings.Split(a, "/")
	if len(parts) != 2 || parts[1] == "" || (parts[0] != "projects" && parts[0] != "folders" && parts[0] != "organizations") {
		return denyName{}, apierror.InvalidArgument("%s %q: the attachment point must be cloudresourcemanager.googleapis.com/projects/{project}, folders/{folder} or organizations/{organization}, URL-encoded", what, name)
	}
	return denyName{attachment: a, id: id}, nil
}

func cutLast(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// canonicalAttachment resolves an attachment point to the resource it
// names, a project by its number. The caller holds r.mu.
func (r *Registry) canonicalAttachment(a string) (string, error) {
	switch {
	case strings.HasPrefix(a, "projects/"):
		p, err := r.resolveProjectLocked(a)
		if err != nil {
			return "", apierror.NotFound("the attachment point %s%s was not found", crmHost, a)
		}
		return "projects/" + strconv.FormatInt(r.projectNumber(p.ProjectID), 10), nil
	case strings.HasPrefix(a, "folders/"):
		if _, err := r.loadFolder(a); err != nil {
			return "", apierror.NotFound("the attachment point %s%s was not found", crmHost, a)
		}
		return a, nil
	case strings.HasPrefix(a, "organizations/"):
		if checkOrg(a) != nil {
			return "", apierror.NotFound("the attachment point %s%s was not found", crmHost, a)
		}
		return a, nil
	}
	return "", apierror.InvalidArgument("attachment point %q is not a project, folder or organization", a)
}

func denyPolicyName(canonical, id string) string {
	return "policies/" + url.PathEscape(crmHost+canonical) + "/" + denyKind + "/" + id
}

func denyKey(canonical, id string) string { return denyPolicyPrefix + canonical + "/" + id }

func (r *Registry) readDeny(key string) (*iamv2.Policy, error) {
	raw, err := r.st.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the deny policy")
	}
	p := &iamv2.Policy{}
	if err := protojson.Unmarshal(raw, p); err != nil {
		return nil, apierror.Internal(err, "decoding the deny policy")
	}
	return p, nil
}

func (r *Registry) writeDeny(key string, p *iamv2.Policy) error {
	raw, err := protojson.Marshal(p)
	if err != nil {
		return apierror.Internal(err, "encoding the deny policy")
	}
	if err := r.st.Put(key, raw); err != nil {
		return apierror.Internal(err, "writing the deny policy")
	}
	return nil
}

// denyPoliciesLocked are the policies attached to canonical, by ID. The
// caller holds r.mu.
func (r *Registry) denyPoliciesLocked(canonical string) ([]*iamv2.Policy, error) {
	prefix := denyPolicyPrefix + canonical + "/"
	keys, err := r.st.List(prefix)
	if err != nil {
		return nil, apierror.Internal(err, "listing deny policies")
	}
	sort.Strings(keys)
	var out []*iamv2.Policy
	for _, k := range keys {
		p, err := r.readDeny(k)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// DenyPolicies are the deny policies attached to resource
// ("projects/{id or number}", "folders/N", "organizations/N"), for the
// evaluator; none when it does not exist.
func (r *Registry) DenyPolicies(resource string) []*iamv2.Policy {
	r.mu.Lock()
	defer r.unlock()
	c, err := r.canonicalAttachment(resource)
	if err != nil {
		return nil
	}
	ps, _ := r.denyPoliciesLocked(c)
	return ps
}

func newDenyUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func denyEtag(p *iamv2.Policy) string {
	c := proto.Clone(p).(*iamv2.Policy)
	c.Etag = ""
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	return policyEtag(b)
}

// checkDenyContent validates p's user-set fields.
func checkDenyContent(p *iamv2.Policy) error {
	if n := len([]rune(p.GetDisplayName())); n > 63 {
		return apierror.InvalidArgument("policy.display_name must be at most 63 characters, not %d", n)
	}
	for k, v := range p.GetAnnotations() {
		if len([]rune(k)) > 63 {
			return apierror.InvalidArgument("policy.annotations: the key %q is longer than 63 characters", k)
		}
		if len([]rune(v)) > 255 {
			return apierror.InvalidArgument("policy.annotations[%q]: the value is longer than 255 characters", k)
		}
	}
	if n := len(p.GetRules()); n > MaxDenyRulesPerPolicy {
		return apierror.InvalidArgument("policy.rules: a deny policy can contain at most %d deny rules, not %d", MaxDenyRulesPerPolicy, n)
	}
	for i, rule := range p.GetRules() {
		if n := len([]rune(rule.GetDescription())); n > 256 {
			return apierror.InvalidArgument("policy.rules[%d].description must be at most 256 characters, not %d", i, n)
		}
		dr := rule.GetDenyRule()
		if dr == nil {
			return apierror.InvalidArgument("policy.rules[%d]: a rule of a deny policy must be a deny_rule", i)
		}
		if len(dr.GetDeniedPrincipals()) == 0 {
			return apierror.InvalidArgument("policy.rules[%d].deny_rule.denied_principals is required", i)
		}
		if len(dr.GetDeniedPermissions()) == 0 {
			return apierror.InvalidArgument("policy.rules[%d].deny_rule.denied_permissions is required", i)
		}
		for j, pr := range dr.GetDeniedPrincipals() {
			if err := checkDenyPrincipal(pr, false); err != nil {
				return apierror.InvalidArgument("policy.rules[%d].deny_rule.denied_principals[%d]: %v", i, j, err)
			}
		}
		for j, pr := range dr.GetExceptionPrincipals() {
			if err := checkDenyPrincipal(pr, true); err != nil {
				return apierror.InvalidArgument("policy.rules[%d].deny_rule.exception_principals[%d]: %v", i, j, err)
			}
		}
		for j, pm := range dr.GetDeniedPermissions() {
			if _, ok := iameval.DeniablePermission(pm); !ok {
				return apierror.InvalidArgument("policy.rules[%d].deny_rule.denied_permissions[%d]: the permission %q is not supported in deny policies; name it as SERVICE_FQDN/RESOURCE.VERB, from https://cloud.google.com/iam/docs/deny-permissions-support", i, j, pm)
			}
		}
		for j, pm := range dr.GetExceptionPermissions() {
			if _, ok := iameval.DeniablePermission(pm); !ok {
				return apierror.InvalidArgument("policy.rules[%d].deny_rule.exception_permissions[%d]: the permission %q is not supported in deny policies; name it as SERVICE_FQDN/RESOURCE.VERB, from https://cloud.google.com/iam/docs/deny-permissions-support", i, j, pm)
			}
		}
		if c := dr.GetDenialCondition(); c != nil && c.GetExpression() != "" {
			if err := checkDenialCondition(c.GetExpression()); err != nil {
				return apierror.InvalidArgument("policy.rules[%d].deny_rule.denial_condition: %v", i, err)
			}
		}
	}
	return nil
}

func checkDenyPrincipal(p string, exception bool) error {
	if p == publicAll {
		if exception {
			return fmt.Errorf("%s cannot be an exception principal", publicAll)
		}
		return nil
	}
	id := strings.TrimPrefix(p, "deleted:")
	if id != p {
		if i := strings.Index(id, "?uid="); i > 0 {
			id = id[:i]
		}
	}
	for _, re := range principalRes {
		if re.MatchString(id) {
			return nil
		}
	}
	return fmt.Errorf("%q is not a principal identifier deny policies accept, such as principal://goog/subject/USER_EMAIL, principalSet://goog/group/GROUP_EMAIL or principal://iam.googleapis.com/projects/-/serviceAccounts/SA_EMAIL", p)
}

// checkDenialCondition is whether expr compiles and calls resource tag
// functions only.
func checkDenialCondition(expr string) error {
	if _, err := iamcond.Compile(expr); err != nil {
		return err
	}
	for _, id := range identRe.FindAllString(strRe.ReplaceAllString(expr, `""`), -1) {
		if id == "true" || id == "false" || tagFunctions[id] {
			continue
		}
		return fmt.Errorf("%q is not supported in a deny condition, which can use only the resource tag functions resource.matchTag, resource.matchTagId, resource.hasTagKey and resource.hasTagKeyId", id)
	}
	return nil
}

// checkDenyLimits applies the per-resource limits to policies, the
// resource's policies after the change.
func checkDenyLimits(policies []*iamv2.Policy) error {
	if len(policies) > MaxDenyPoliciesPerResource {
		return apierror.InvalidArgument("a resource can have at most %d deny policies attached", MaxDenyPoliciesPerResource)
	}
	rules, groups, principals := 0, 0, 0
	for _, p := range policies {
		rules += len(p.GetRules())
		for _, rule := range p.GetRules() {
			dr := rule.GetDenyRule()
			for _, pr := range append(append([]string(nil), dr.GetDeniedPrincipals()...), dr.GetExceptionPrincipals()...) {
				principals++
				id := strings.TrimPrefix(pr, "deleted:")
				if strings.HasPrefix(id, "principalSet://goog/group/") || strings.HasPrefix(id, "principalSet://goog/cloudIdentityCustomerId/") {
					groups++
				}
			}
		}
	}
	switch {
	case rules > MaxDenyRulesPerResource:
		return apierror.InvalidArgument("the deny policies attached to a resource can contain at most %d deny rules in all, not %d", MaxDenyRulesPerResource, rules)
	case groups > MaxDenyGroupsPerResource:
		return apierror.InvalidArgument("the deny policies attached to a resource can name at most %d domains and Google groups in all, not %d", MaxDenyGroupsPerResource, groups)
	case principals > MaxDenyPrincipalsPerResour:
		return apierror.InvalidArgument("the deny policies attached to a resource can name at most %d principals in all, not %d", MaxDenyPrincipalsPerResour, principals)
	}
	return nil
}

func replaceDeny(list []*iamv2.Policy, p *iamv2.Policy) []*iamv2.Policy {
	out := make([]*iamv2.Policy, 0, len(list)+1)
	for _, x := range list {
		if x.GetName() != p.GetName() {
			out = append(out, x)
		}
	}
	return append(out, p)
}

// CreateDenyPolicy attaches p to parent's attachment point as id.
func (r *Registry) CreateDenyPolicy(parent, id string, p *iamv2.Policy) (*iamv2.Policy, error) {
	n, err := parseDenyName(parent, true)
	if err != nil {
		return nil, err
	}
	if !policyIDRe.MatchString(id) {
		return nil, apierror.InvalidArgument("policy_id %q is not valid: it must contain 3 to 63 characters, lowercase letters, numbers, dashes (-) and periods (.), and start with a lowercase letter", id)
	}
	if p == nil {
		p = &iamv2.Policy{}
	}
	if err := checkDenyContent(p); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	c, err := r.canonicalAttachment(n.attachment)
	if err != nil {
		return nil, err
	}
	key := denyKey(c, id)
	if cur, err := r.readDeny(key); err != nil {
		return nil, err
	} else if cur != nil {
		return nil, apierror.AlreadyExists("policy %s already exists", denyPolicyName(c, id))
	}
	now := r.nowTS()
	out := &iamv2.Policy{Name: denyPolicyName(c, id), Uid: newDenyUID(), Kind: "DenyPolicy", DisplayName: p.GetDisplayName(),
		Annotations: p.GetAnnotations(), CreateTime: now, UpdateTime: now, Rules: p.GetRules()}
	list, err := r.denyPoliciesLocked(c)
	if err != nil {
		return nil, err
	}
	if err := checkDenyLimits(replaceDeny(list, out)); err != nil {
		return nil, err
	}
	out.Etag = denyEtag(out)
	if err := r.writeDeny(key, out); err != nil {
		return nil, err
	}
	return out, nil
}

// lookupDeny resolves name to its key and stored policy, NOT_FOUND when
// there is none. The caller holds r.mu.
func (r *Registry) lookupDeny(name string) (string, string, *iamv2.Policy, error) {
	n, err := parseDenyName(name, false)
	if err != nil {
		return "", "", nil, err
	}
	c, err := r.canonicalAttachment(n.attachment)
	if err != nil {
		return "", "", nil, err
	}
	key := denyKey(c, n.id)
	p, err := r.readDeny(key)
	if err != nil {
		return "", "", nil, err
	}
	if p == nil {
		return "", "", nil, apierror.NotFound("policy %s not found", name)
	}
	return c, key, p, nil
}

// GetDenyPolicy is the policy name names.
func (r *Registry) GetDenyPolicy(name string) (*iamv2.Policy, error) {
	r.mu.Lock()
	defer r.unlock()
	_, _, p, err := r.lookupDeny(name)
	return p, err
}

// ListDenyPolicies are parent's policies, metadata only.
func (r *Registry) ListDenyPolicies(parent string) ([]*iamv2.Policy, error) {
	n, err := parseDenyName(parent, true)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	c, err := r.canonicalAttachment(n.attachment)
	if err != nil {
		return nil, err
	}
	list, err := r.denyPoliciesLocked(c)
	if err != nil {
		return nil, err
	}
	for i, p := range list {
		m := proto.Clone(p).(*iamv2.Policy)
		m.Rules = nil
		list[i] = m
	}
	return list, nil
}

// UpdateDenyPolicy replaces the display name, annotations and rules of the
// policy p names.
func (r *Registry) UpdateDenyPolicy(p *iamv2.Policy) (*iamv2.Policy, error) {
	if p == nil || p.GetName() == "" {
		return nil, apierror.InvalidArgument("policy.name is required")
	}
	if err := checkDenyContent(p); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	c, key, cur, err := r.lookupDeny(p.GetName())
	if err != nil {
		return nil, err
	}
	if p.GetEtag() != "" && p.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("the etag %q of policy %s is not the current one; read the policy and try again", p.GetEtag(), cur.GetName())
	}
	if p.GetUid() != "" && p.GetUid() != cur.GetUid() {
		return nil, apierror.InvalidArgument("policy.uid is immutable: %q is not %q", p.GetUid(), cur.GetUid())
	}
	if p.GetKind() != "" && p.GetKind() != cur.GetKind() {
		return nil, apierror.InvalidArgument("policy.kind is immutable: %q is not %q", p.GetKind(), cur.GetKind())
	}
	out := proto.Clone(cur).(*iamv2.Policy)
	out.DisplayName, out.Annotations, out.Rules = p.GetDisplayName(), p.GetAnnotations(), p.GetRules()
	out.UpdateTime = r.nowTS()
	list, err := r.denyPoliciesLocked(c)
	if err != nil {
		return nil, err
	}
	if err := checkDenyLimits(replaceDeny(list, out)); err != nil {
		return nil, err
	}
	out.Etag = denyEtag(out)
	if err := r.writeDeny(key, out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteDenyPolicy removes the policy name names; an etag that is not the
// stored one is ABORTED. It answers the policy as deleted.
func (r *Registry) DeleteDenyPolicy(name, etag string) (*iamv2.Policy, error) {
	r.mu.Lock()
	defer r.unlock()
	_, key, cur, err := r.lookupDeny(name)
	if err != nil {
		return nil, err
	}
	if etag != "" && etag != cur.GetEtag() {
		return nil, apierror.Aborted("the etag %q of policy %s is not the current one; read the policy and try again", etag, cur.GetName())
	}
	if err := r.st.Delete(key); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, apierror.Internal(err, "deleting the deny policy")
	}
	out := proto.Clone(cur).(*iamv2.Policy)
	out.DeleteTime = r.nowTS()
	return out, nil
}

// PoliciesServer is google.iam.v2.Policies over reg.
type PoliciesServer struct {
	iamv2.UnimplementedPoliciesServer
	reg *Registry
}

// NewPoliciesServer is the IAM v2 Policies service over reg.
func NewPoliciesServer(reg *Registry) *PoliciesServer { return &PoliciesServer{reg: reg} }

func (s *PoliciesServer) operation(p *iamv2.Policy) (*longrunningpb.Operation, error) {
	resp, err := anypb.New(p)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "packing the policy"))
	}
	md, err := anypb.New(&iamv2.PolicyOperationMetadata{CreateTime: s.reg.nowTS()})
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "packing the metadata"))
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	op := &longrunningpb.Operation{Name: p.GetName() + "/operations/" + hex.EncodeToString(b[:]), Metadata: md, Done: true,
		Result: &longrunningpb.Operation_Response{Response: resp}}
	v3Operations.put(op)
	return op, nil
}

func (s *PoliciesServer) ListPolicies(_ context.Context, req *iamv2.ListPoliciesRequest) (*iamv2.ListPoliciesResponse, error) {
	if req.GetPageToken() != "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("page_token %q is not valid", req.GetPageToken()))
	}
	list, err := s.reg.ListDenyPolicies(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &iamv2.ListPoliciesResponse{Policies: list}, nil
}

func (s *PoliciesServer) GetPolicy(_ context.Context, req *iamv2.GetPolicyRequest) (*iamv2.Policy, error) {
	p, err := s.reg.GetDenyPolicy(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return p, nil
}

func (s *PoliciesServer) CreatePolicy(_ context.Context, req *iamv2.CreatePolicyRequest) (*longrunningpb.Operation, error) {
	p, err := s.reg.CreateDenyPolicy(req.GetParent(), req.GetPolicyId(), req.GetPolicy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.operation(p)
}

func (s *PoliciesServer) UpdatePolicy(_ context.Context, req *iamv2.UpdatePolicyRequest) (*longrunningpb.Operation, error) {
	p, err := s.reg.UpdateDenyPolicy(req.GetPolicy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.operation(p)
}

func (s *PoliciesServer) DeletePolicy(_ context.Context, req *iamv2.DeletePolicyRequest) (*longrunningpb.Operation, error) {
	p, err := s.reg.DeleteDenyPolicy(req.GetName(), req.GetEtag())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.operation(p)
}

// isIAMv2Path is whether a JSON request is IAM v2's.
func isIAMv2Path(path string) bool { return strings.HasPrefix(path, "/v2/policies/") }
