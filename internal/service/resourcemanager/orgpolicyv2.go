package resourcemanager

// The Organization Policy API v2 (orgpolicy.googleapis.com, #1890):
// google.cloud.orgpolicy.v2.OrgPolicy on the Resource Manager port, over
// gRPC and JSON, as its reference describes it
// (https://docs.cloud.google.com/organization-policy/reference/rest/v2,
// read 2026-11-19):
//
//   - policies {projects/{id or number}|folders/N|organizations/N}/
//     policies/{constraint}: create (ALREADY_EXISTS when one is set),
//     get and update (NOT_FOUND when none is), delete, list, and
//     getEffectivePolicy, the policy evaluated down the hierarchy
//     (internal/orgpolicy.Merge); names come back with the project's
//     number, as Google's do;
//   - an etag on update or delete that is not the stored one is ABORTED;
//   - update_mask names spec, dry_run_spec or alternate (relative to the
//     policy); an empty mask replaces them all;
//   - constraints {parent}/constraints/{constraint}: the constraints v1's
//     listAvailableOrgPolicyConstraints lists, with the custom
//     constraints of the organization;
//   - custom constraints organizations/N/customConstraints/custom.{name}:
//     create, get, update, list, delete. They are stored, not evaluated:
//     a custom constraint's CEL condition over a service's resource is
//     not enforced (#1890 follow-up).
//
// v1 (orgpolicy.go) and v2 are one store, as on Google Cloud: a policy set
// through either reads back through both. A policy is kept under one key
// at a time, v1's or v2's; v2 reads a v1 policy converted (a boolean
// policy is one enforce rule, a list policy one values, allowAll or
// denyAll rule, restoreDefault reset), and v1 reads a v2 policy converted
// back, without its conditions or dry-run spec.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const (
	orgPolicyV2Prefix        = "resourcemanager/orgpolicies-v2/"
	customConstraintPrefix   = "resourcemanager/orgpolicy-customconstraints/"
	maxConditionalBoolRules  = 10
	orgPolicyV2DefaultPageSz = 100
)

// extraConstraints are enforced constraints v1 did not list.
var extraConstraints = []*crmv1.Constraint{
	boolConstraint("constraints/iam.disableServiceAccountCreation", "Disable service account creation"),
	boolConstraint("constraints/iam.managed.disableServiceAccountKeyCreation", "Block service account API key bindings"),
}

func knownConstraintV2(short string) *crmv1.Constraint {
	if c := KnownConstraint("constraints/" + short); c != nil {
		return c
	}
	for _, c := range extraConstraints {
		if c.Name == "constraints/"+short {
			return c
		}
	}
	return nil
}

var constraintNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// splitPolicyName is a policy name's parent and constraint.
func splitPolicyName(name string) (parent, constraint string, err error) {
	i := strings.LastIndex(name, "/policies/")
	if i <= 0 {
		return "", "", apierror.InvalidArgument("policy name %q must be {projects|folders|organizations}/{id}/policies/{constraint}", name)
	}
	parent, constraint = name[:i], name[i+len("/policies/"):]
	if !constraintNameRE.MatchString(constraint) || strings.Count(parent, "/") != 1 {
		return "", "", apierror.InvalidArgument("policy name %q must be {projects|folders|organizations}/{id}/policies/{constraint}", name)
	}
	return parent, constraint, nil
}

// canonicalPolicyParent is parent as v1's store keys it (a project by ID)
// and as v2 names it (a project by number). The caller holds r.mu.
func (r *Registry) canonicalPolicyParent(parent string) (key, named string, err error) {
	switch {
	case strings.HasPrefix(parent, "projects/"):
		p, err := r.resolveProjectLocked(parent)
		if err != nil {
			return "", "", apierror.NotFound("resource %q not found", parent)
		}
		return "projects/" + p.ProjectID, "projects/" + strconv.FormatInt(r.projectNumber(p.ProjectID), 10), nil
	case strings.HasPrefix(parent, "folders/"), strings.HasPrefix(parent, "organizations/"):
		if err := r.checkPolicyResource(parent); err != nil {
			return "", "", apierror.NotFound("resource %q not found", parent)
		}
		return parent, parent, nil
	}
	return "", "", apierror.InvalidArgument("parent %q must be projects/{project}, folders/{folder} or organizations/{organization}", parent)
}

func v2Key(keyParent, constraint string) string {
	return orgPolicyV2Prefix + keyParent + "/" + constraint
}

// v2Stored is the v2 policy stored for constraint on keyParent, or nil.
func (r *Registry) v2Stored(keyParent, constraint string) (*orgpolicypb.Policy, error) {
	raw, err := r.st.Get(v2Key(keyParent, constraint))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the org policy")
	}
	p := &orgpolicypb.Policy{}
	if err := protojson.Unmarshal(raw, p); err != nil {
		return nil, apierror.Internal(err, "decoding the org policy")
	}
	return p, nil
}

// v1Stored is the policy stored by v1 on keyParent, without v2's.
func (r *Registry) v1Stored(keyParent, constraint string) (*crmv1.OrgPolicy, error) {
	scope, err := orgPolicyScope(keyParent)
	if err != nil {
		return nil, err
	}
	raw, err := r.st.Get(scope + constraint)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the org policy")
	}
	p := &crmv1.OrgPolicy{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, apierror.Internal(err, "decoding the org policy")
	}
	return p, nil
}

// policyV2Locked is the policy for constraint on keyParent, v2's or v1's
// converted, named under named; nil when none is set.
func (r *Registry) policyV2Locked(keyParent, named, constraint string) (*orgpolicypb.Policy, error) {
	p, err := r.v2Stored(keyParent, constraint)
	if err != nil || p != nil {
		if p != nil {
			p.Name = named + "/policies/" + constraint
		}
		return p, err
	}
	v1, err := r.v1Stored(keyParent, constraint)
	if err != nil || v1 == nil {
		return nil, err
	}
	out := &orgpolicypb.Policy{Name: named + "/policies/" + constraint, Spec: v1ToSpec(v1), Etag: v1.Etag}
	return out, nil
}

// v1ToSpec is a v1 policy as a v2 spec.
func v1ToSpec(p *crmv1.OrgPolicy) *orgpolicypb.PolicySpec {
	spec := &orgpolicypb.PolicySpec{Etag: p.Etag}
	if t, err := parseTime(p.UpdateTime); err == nil {
		spec.UpdateTime = t
	}
	switch {
	case p.RestoreDefault != nil:
		spec.Reset_ = true
	case p.BooleanPolicy != nil:
		spec.Rules = []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: p.BooleanPolicy.Enforced}}}
	case p.ListPolicy != nil:
		lp := p.ListPolicy
		spec.InheritFromParent = lp.InheritFromParent
		switch lp.AllValues {
		case "ALLOW":
			spec.Rules = []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_AllowAll{AllowAll: true}}}
		case "DENY":
			spec.Rules = []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_DenyAll{DenyAll: true}}}
		default:
			if len(lp.AllowedValues) > 0 || len(lp.DeniedValues) > 0 {
				spec.Rules = []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Values{
					Values: &orgpolicypb.PolicySpec_PolicyRule_StringValues{AllowedValues: lp.AllowedValues, DeniedValues: lp.DeniedValues}}}}
			}
		}
	}
	return spec
}

// specToV1 is a v2 policy as v1 reads it: its unconditional rules.
func specToV1(constraint string, p *orgpolicypb.Policy) *crmv1.OrgPolicy {
	out := &crmv1.OrgPolicy{Constraint: "constraints/" + constraint, Version: 1, Etag: p.GetEtag()}
	spec := p.GetSpec()
	if t := spec.GetUpdateTime(); t != nil {
		out.UpdateTime = t.AsTime().UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	if spec.GetReset_() {
		out.RestoreDefault = &crmv1.RestoreDefault{}
		return out
	}
	c := knownConstraintV2(constraint)
	boolean := c != nil && c.BooleanConstraint != nil
	var lp *crmv1.ListPolicy
	for _, r := range spec.GetRules() {
		if r.GetCondition().GetExpression() != "" {
			continue
		}
		switch k := r.GetKind().(type) {
		case *orgpolicypb.PolicySpec_PolicyRule_Enforce:
			out.BooleanPolicy = &crmv1.BooleanPolicy{Enforced: k.Enforce}
		case *orgpolicypb.PolicySpec_PolicyRule_AllowAll:
			lp = &crmv1.ListPolicy{AllValues: "ALLOW"}
		case *orgpolicypb.PolicySpec_PolicyRule_DenyAll:
			lp = &crmv1.ListPolicy{AllValues: "DENY"}
		case *orgpolicypb.PolicySpec_PolicyRule_Values:
			if lp == nil {
				lp = &crmv1.ListPolicy{}
			}
			lp.AllowedValues = append(lp.AllowedValues, k.Values.GetAllowedValues()...)
			lp.DeniedValues = append(lp.DeniedValues, k.Values.GetDeniedValues()...)
		}
	}
	if lp != nil && !boolean {
		lp.InheritFromParent = spec.GetInheritFromParent()
		out.ListPolicy = lp
	}
	return out
}

func parseTime(s string) (*timestamppb.Timestamp, error) {
	if s == "" {
		return nil, errors.New("no time")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, err
	}
	return timestamppb.New(t), nil
}

// isListConstraint reports whether constraint takes values.
func isListConstraint(constraint string, specs ...*orgpolicypb.PolicySpec) bool {
	if c := knownConstraintV2(constraint); c != nil {
		return c.ListConstraint != nil
	}
	for _, s := range specs {
		for _, r := range s.GetRules() {
			if _, ok := r.GetKind().(*orgpolicypb.PolicySpec_PolicyRule_Enforce); ok {
				return false
			}
		}
	}
	return true
}

// checkSpec validates a spec set on constraint.
func checkSpec(constraint, field string, s *orgpolicypb.PolicySpec) error {
	if s == nil {
		return nil
	}
	if s.GetReset_() && len(s.GetRules()) > 0 {
		return apierror.InvalidArgument("%s: a policy with reset set cannot have rules", field)
	}
	c := knownConstraintV2(constraint)
	boolean := c != nil && c.BooleanConstraint != nil
	list := c != nil && c.ListConstraint != nil
	if boolean && s.GetInheritFromParent() {
		return apierror.InvalidArgument("%s: inheritFromParent can only be set for list constraints; %s is a boolean constraint", field, constraint)
	}
	conditional, unconditional := 0, 0
	for _, r := range s.GetRules() {
		if r.GetKind() == nil {
			return apierror.InvalidArgument("%s: a rule must set values, allowAll, denyAll or enforce", field)
		}
		if expr := r.GetCondition().GetExpression(); expr != "" {
			conditional++
			if _, err := iamcond.Compile(expr); err != nil {
				return apierror.InvalidArgument("%s: condition %q is not valid: %v", field, expr, err)
			}
		} else {
			unconditional++
		}
		_, enforce := r.GetKind().(*orgpolicypb.PolicySpec_PolicyRule_Enforce)
		if boolean && !enforce {
			return apierror.InvalidArgument("%s: %s is a boolean constraint: its rules set enforce", field, constraint)
		}
		if list && enforce {
			return apierror.InvalidArgument("%s: %s is a list constraint: its rules set values, allowAll or denyAll", field, constraint)
		}
		switch constraint {
		case orgpolicy.AllowedPolicyMemberDomains:
			if r.GetDenyAll() || len(r.GetValues().GetDeniedValues()) > 0 {
				return apierror.InvalidArgument("%s: constraints/%s does not support denied values or denyAll", field, constraint)
			}
		case orgpolicy.ResourceLocations:
			for _, v := range append(append([]string{}, r.GetValues().GetAllowedValues()...), r.GetValues().GetDeniedValues()...) {
				if strings.HasPrefix(v, "in:") && !orgpolicy.KnownGroup(v) {
					return apierror.InvalidArgument("%s: %q is not a valid value group of constraints/%s", field, v, constraint)
				}
			}
		}
	}
	if boolean && (unconditional > 1 || conditional > maxConditionalBoolRules) {
		return apierror.InvalidArgument("%s: a boolean policy has at most one unconditional rule and %d conditional rules", field, maxConditionalBoolRules)
	}
	return nil
}

func v2Etag(p *orgpolicypb.Policy) string {
	c := proto.Clone(p).(*orgpolicypb.Policy)
	c.Etag, c.Name = "", ""
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	return policyEtag(b)
}

// writeV2Locked stores p for constraint on keyParent, replacing any v1
// policy, and answers it named under named.
func (r *Registry) writeV2Locked(keyParent, named, constraint string, p *orgpolicypb.Policy) (*orgpolicypb.Policy, error) {
	out := proto.Clone(p).(*orgpolicypb.Policy)
	now := timestamppb.New(r.now().UTC())
	for _, s := range []*orgpolicypb.PolicySpec{out.Spec, out.DryRunSpec, out.GetAlternate().GetSpec()} {
		if s != nil {
			s.UpdateTime, s.Etag = now, ""
			b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(s)
			s.Etag = policyEtag(b)
		}
	}
	out.Name = named + "/policies/" + constraint
	out.Etag = v2Etag(out)
	raw, err := protojson.Marshal(out)
	if err != nil {
		return nil, apierror.Internal(err, "encoding the org policy")
	}
	if err := r.st.Put(v2Key(keyParent, constraint), raw); err != nil {
		return nil, apierror.Internal(err, "writing the org policy")
	}
	if scope, err := orgPolicyScope(keyParent); err == nil {
		if err := r.st.Delete(scope + constraint); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, apierror.Internal(err, "replacing the v1 org policy")
		}
	}
	return out, nil
}

// PolicyV2 is the policy name names; NOT_FOUND when none is set.
func (r *Registry) PolicyV2(name string) (*orgpolicypb.Policy, error) {
	parent, constraint, err := splitPolicyName(name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	key, named, err := r.canonicalPolicyParent(parent)
	if err != nil {
		return nil, err
	}
	p, err := r.policyV2Locked(key, named, constraint)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, apierror.NotFound("Policy %s not found", name)
	}
	return p, nil
}

// SetPolicyV2 creates p (create: ALREADY_EXISTS when one is set) or
// replaces it (update: NOT_FOUND when none is, unless allowMissing),
// keeping the fields mask does not name.
func (r *Registry) setPolicyV2(p *orgpolicypb.Policy, create, allowMissing bool, mask []string) (*orgpolicypb.Policy, error) {
	if p == nil {
		return nil, apierror.InvalidArgument("policy is required")
	}
	parent, constraint, err := splitPolicyName(p.GetName())
	if err != nil {
		return nil, err
	}
	for _, f := range mask {
		switch f {
		case "spec", "dry_run_spec", "dryRunSpec", "alternate", "policy.spec", "policy.dry_run_spec":
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not supported: spec, dry_run_spec and alternate are", f)
		}
	}
	if err := checkSpec(constraint, "spec", p.GetSpec()); err != nil {
		return nil, err
	}
	if err := checkSpec(constraint, "dryRunSpec", p.GetDryRunSpec()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	key, named, err := r.canonicalPolicyParent(parent)
	if err != nil {
		return nil, err
	}
	cur, err := r.policyV2Locked(key, named, constraint)
	if err != nil {
		return nil, err
	}
	switch {
	case create && cur != nil:
		return nil, apierror.AlreadyExists("Policy %s already exists", p.GetName())
	case !create && cur == nil && !allowMissing:
		return nil, apierror.NotFound("Policy %s not found", p.GetName())
	}
	if cur != nil && p.GetEtag() != "" && !sameEtag(p.GetEtag(), cur.GetEtag()) {
		return nil, apierror.Aborted("policy %s: etag %q is not the current one; read the policy and try again", p.GetName(), p.GetEtag())
	}
	next := proto.Clone(p).(*orgpolicypb.Policy)
	if len(mask) > 0 && cur != nil {
		next = proto.Clone(cur).(*orgpolicypb.Policy)
		for _, f := range mask {
			switch f {
			case "spec", "policy.spec":
				next.Spec = p.GetSpec()
			case "dry_run_spec", "dryRunSpec", "policy.dry_run_spec":
				next.DryRunSpec = p.GetDryRunSpec()
			case "alternate":
				next.Alternate = p.GetAlternate()
			}
		}
	}
	return r.writeV2Locked(key, named, constraint, next)
}

// SetPolicyV2 creates or replaces p, the console's Edit policy.
func (r *Registry) SetPolicyV2(p *orgpolicypb.Policy, allowMissing bool) (*orgpolicypb.Policy, error) {
	return r.setPolicyV2(p, false, allowMissing, nil)
}

// DeletePolicyV2 removes the policy name names; NOT_FOUND when none is
// set, ABORTED for an etag that is not its own.
func (r *Registry) DeletePolicyV2(name, etag string) error {
	parent, constraint, err := splitPolicyName(name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.unlock()
	key, named, err := r.canonicalPolicyParent(parent)
	if err != nil {
		return err
	}
	cur, err := r.policyV2Locked(key, named, constraint)
	if err != nil {
		return err
	}
	if cur == nil {
		return apierror.NotFound("Policy %s not found", name)
	}
	if etag != "" && !sameEtag(etag, cur.GetEtag()) {
		return apierror.Aborted("policy %s: etag %q is not the current one; read the policy and try again", name, etag)
	}
	for _, k := range []string{v2Key(key, constraint)} {
		if err := r.st.Delete(k); err != nil && !errors.Is(err, store.ErrNotFound) {
			return apierror.Internal(err, "deleting the org policy")
		}
	}
	if scope, err := orgPolicyScope(key); err == nil {
		if err := r.st.Delete(scope + constraint); err != nil && !errors.Is(err, store.ErrNotFound) {
			return apierror.Internal(err, "deleting the org policy")
		}
	}
	return nil
}

// constraintsSetLocked are the constraints with a policy on keyParent,
// v1's and v2's.
func (r *Registry) constraintsSetLocked(keyParent string) ([]string, error) {
	set := map[string]bool{}
	if scope, err := orgPolicyScope(keyParent); err == nil {
		keys, err := r.st.List(scope)
		if err != nil {
			return nil, apierror.Internal(err, "listing org policies")
		}
		for _, k := range keys {
			set[strings.TrimPrefix(k, scope)] = true
		}
	}
	prefix := orgPolicyV2Prefix + keyParent + "/"
	keys, err := r.st.List(prefix)
	if err != nil {
		return nil, apierror.Internal(err, "listing org policies")
	}
	for _, k := range keys {
		set[strings.TrimPrefix(k, prefix)] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// PoliciesV2 are the policies set on parent, by constraint.
func (r *Registry) PoliciesV2(parent string) ([]*orgpolicypb.Policy, error) {
	r.mu.Lock()
	defer r.unlock()
	key, named, err := r.canonicalPolicyParent(parent)
	if err != nil {
		return nil, err
	}
	cs, err := r.constraintsSetLocked(key)
	if err != nil {
		return nil, err
	}
	var out []*orgpolicypb.Policy
	for _, c := range cs {
		p, err := r.policyV2Locked(key, named, c)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// effectiveSpecsLocked are constraint's effective spec and dry-run spec
// on keyParent, evaluated from the organization down.
func (r *Registry) effectiveSpecsLocked(keyParent, constraint string) (spec, dry *orgpolicypb.PolicySpec, err error) {
	chain, err := r.policyChain(keyParent)
	if err != nil {
		return nil, nil, err
	}
	var specs, drys []*orgpolicypb.PolicySpec
	anyDry := false
	for _, res := range chain {
		p, err := r.policyV2Locked(res, res, constraint)
		if err != nil {
			return nil, nil, err
		}
		specs = append(specs, p.GetSpec())
		if p.GetDryRunSpec() != nil {
			anyDry = true
			drys = append(drys, p.GetDryRunSpec())
		} else {
			drys = append(drys, p.GetSpec())
		}
	}
	list := isListConstraint(constraint, specs...)
	for _, s := range specs {
		spec = orgpolicy.Merge(spec, s, list)
	}
	if anyDry {
		for _, s := range drys {
			dry = orgpolicy.Merge(dry, s, list)
		}
	}
	return spec, dry, nil
}

// EffectivePolicyV2 is getEffectivePolicy.
func (r *Registry) EffectivePolicyV2(name string) (*orgpolicypb.Policy, error) {
	parent, constraint, err := splitPolicyName(name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	key, named, err := r.canonicalPolicyParent(parent)
	if err != nil {
		return nil, err
	}
	spec, dry, err := r.effectiveSpecsLocked(key, constraint)
	if err != nil {
		return nil, err
	}
	out := &orgpolicypb.Policy{Name: named + "/policies/" + constraint, Spec: &orgpolicypb.PolicySpec{}}
	if spec != nil {
		out.Spec = proto.Clone(spec).(*orgpolicypb.PolicySpec)
		out.Spec.Etag = ""
	}
	if dry != nil {
		out.DryRunSpec = proto.Clone(dry).(*orgpolicypb.PolicySpec)
		out.DryRunSpec.Etag = ""
	}
	return out, nil
}

// OrgPolicySource answers internal/orgpolicy's checks from the registry.
type OrgPolicySource struct{ Reg func() *Registry }

func (s OrgPolicySource) Effective(_ context.Context, resource, constraint string) (orgpolicy.Effective, error) {
	r := s.Reg()
	if r == nil {
		return orgpolicy.Effective{}, nil
	}
	r.mu.Lock()
	key, _, err := r.canonicalPolicyParent(resource)
	var spec, dry *orgpolicypb.PolicySpec
	if err == nil {
		spec, dry, err = r.effectiveSpecsLocked(key, constraint)
	}
	r.unlock()
	if err != nil || (spec == nil && dry == nil) {
		return orgpolicy.Effective{}, err
	}
	e := orgpolicy.Effective{Spec: spec, DryRun: dry}
	if hasConditions(spec) || hasConditions(dry) {
		full := crmHost + resource
		if strings.HasPrefix(key, "projects/") {
			full = crmHost + "projects/" + strconv.FormatInt(r.projectNumber(strings.TrimPrefix(key, "projects/")), 10)
		}
		if eff, err := r.EffectiveTags(full); err == nil {
			for _, t := range eff {
				short := t.GetNamespacedTagValue()
				if i := strings.LastIndex(short, "/"); i >= 0 {
					short = short[i+1:]
				}
				e.Tags = append(e.Tags, iamcond.Tag{NamespacedKey: t.GetNamespacedTagKey(), ShortValue: short, KeyID: t.GetTagKey(), ValueID: t.GetTagValue()})
			}
		}
	}
	return e, nil
}

func hasConditions(s *orgpolicypb.PolicySpec) bool {
	for _, r := range s.GetRules() {
		if r.GetCondition().GetExpression() != "" {
			return true
		}
	}
	return false
}

// v2Constraint is c as v2 lists it under named.
func v2Constraint(named string, c *crmv1.Constraint) *orgpolicypb.Constraint {
	short := strings.TrimPrefix(c.Name, "constraints/")
	out := &orgpolicypb.Constraint{Name: named + "/constraints/" + short, DisplayName: c.DisplayName, Description: c.Description,
		ConstraintDefault: orgpolicypb.Constraint_ALLOW, SupportsDryRun: true}
	if c.BooleanConstraint != nil {
		out.ConstraintType = &orgpolicypb.Constraint_BooleanConstraint_{BooleanConstraint: &orgpolicypb.Constraint_BooleanConstraint{}}
	} else {
		out.ConstraintType = &orgpolicypb.Constraint_ListConstraint_{ListConstraint: &orgpolicypb.Constraint_ListConstraint{
			SupportsIn: short == orgpolicy.ResourceLocations, SupportsUnder: false}}
	}
	return out
}

// ConstraintsV2 are the constraints available on parent: the listed ones
// and the organization's custom constraints.
func (r *Registry) ConstraintsV2(parent string) ([]*orgpolicypb.Constraint, error) {
	r.mu.Lock()
	_, named, err := r.canonicalPolicyParent(parent)
	r.unlock()
	if err != nil {
		return nil, err
	}
	var out []*orgpolicypb.Constraint
	for _, c := range append(append([]*crmv1.Constraint{}, Constraints...), extraConstraints...) {
		out = append(out, v2Constraint(named, c))
	}
	customs, err := r.CustomConstraints(OrganizationName)
	if err != nil {
		return nil, err
	}
	for _, cc := range customs {
		short := cc.GetName()[strings.LastIndex(cc.GetName(), "/")+1:]
		out = append(out, &orgpolicypb.Constraint{Name: named + "/constraints/" + short, DisplayName: cc.GetDisplayName(),
			Description: cc.GetDescription(), ConstraintDefault: orgpolicypb.Constraint_ALLOW, SupportsDryRun: true,
			ConstraintType: &orgpolicypb.Constraint_BooleanConstraint_{BooleanConstraint: &orgpolicypb.Constraint_BooleanConstraint{}}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---- custom constraints ----

var customNameRE = regexp.MustCompile(`^organizations/([0-9]+)/customConstraints/(custom\.[A-Za-z0-9]{1,62})$`)

func (r *Registry) customKey(name string) (string, error) {
	m := customNameRE.FindStringSubmatch(name)
	if m == nil {
		return "", apierror.InvalidArgument("custom constraint name %q must be organizations/{organization}/customConstraints/custom.{name}, the name up to 62 letters and digits", name)
	}
	if err := checkOrg("organizations/" + m[1]); err != nil {
		return "", apierror.NotFound("organization %s not found", m[1])
	}
	return customConstraintPrefix + m[2], nil
}

func checkCustom(c *orgpolicypb.CustomConstraint) error {
	if len(c.GetResourceTypes()) == 0 {
		return apierror.InvalidArgument("custom constraint %s: resource_types is required", c.GetName())
	}
	if c.GetCondition() == "" {
		return apierror.InvalidArgument("custom constraint %s: condition is required", c.GetName())
	}
	if len(c.GetCondition()) > 1000 {
		return apierror.InvalidArgument("custom constraint %s: condition is longer than 1000 characters", c.GetName())
	}
	if c.GetActionType() == orgpolicypb.CustomConstraint_ACTION_TYPE_UNSPECIFIED {
		return apierror.InvalidArgument("custom constraint %s: action_type must be ALLOW or DENY", c.GetName())
	}
	if len(c.GetMethodTypes()) == 0 {
		return apierror.InvalidArgument("custom constraint %s: method_types is required", c.GetName())
	}
	if len([]rune(c.GetDisplayName())) > 200 || len([]rune(c.GetDescription())) > 2000 {
		return apierror.InvalidArgument("custom constraint %s: display_name is at most 200 characters, description 2000", c.GetName())
	}
	return nil
}

func (r *Registry) readCustom(key string) (*orgpolicypb.CustomConstraint, error) {
	raw, err := r.st.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the custom constraint")
	}
	c := &orgpolicypb.CustomConstraint{}
	if err := protojson.Unmarshal(raw, c); err != nil {
		return nil, apierror.Internal(err, "decoding the custom constraint")
	}
	return c, nil
}

func (r *Registry) putCustom(key string, c *orgpolicypb.CustomConstraint, create bool) (*orgpolicypb.CustomConstraint, error) {
	if err := checkCustom(c); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	cur, err := r.readCustom(key)
	if err != nil {
		return nil, err
	}
	if create && cur != nil {
		return nil, apierror.AlreadyExists("Custom constraint %s already exists", c.GetName())
	}
	if !create && cur == nil {
		return nil, apierror.NotFound("Custom constraint %s not found", c.GetName())
	}
	out := proto.Clone(c).(*orgpolicypb.CustomConstraint)
	out.UpdateTime = timestamppb.New(r.now().UTC())
	raw, err := protojson.Marshal(out)
	if err != nil {
		return nil, apierror.Internal(err, "encoding the custom constraint")
	}
	if err := r.st.Put(key, raw); err != nil {
		return nil, apierror.Internal(err, "writing the custom constraint")
	}
	return out, nil
}

// CustomConstraints are parent's (the organization's) custom constraints.
func (r *Registry) CustomConstraints(parent string) ([]*orgpolicypb.CustomConstraint, error) {
	if !strings.HasPrefix(parent, "organizations/") || checkOrg(parent) != nil {
		return nil, apierror.InvalidArgument("parent %q must be the organization, organizations/%s", parent, OrganizationNumber)
	}
	r.mu.Lock()
	defer r.unlock()
	keys, err := r.st.List(customConstraintPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "listing custom constraints")
	}
	sort.Strings(keys)
	var out []*orgpolicypb.CustomConstraint
	for _, k := range keys {
		c, err := r.readCustom(k)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

// ---- the gRPC service ----

// OrgPolicyServer is google.cloud.orgpolicy.v2.OrgPolicy over the
// registry.
type OrgPolicyServer struct {
	orgpolicypb.UnimplementedOrgPolicyServer
	reg *Registry
}

// NewOrgPolicyServer serves reg's org policies.
func NewOrgPolicyServer(reg *Registry) *OrgPolicyServer { return &OrgPolicyServer{reg: reg} }

func pageSize(n int32) int {
	if n <= 0 {
		return orgPolicyV2DefaultPageSz
	}
	return int(n)
}

func (s *OrgPolicyServer) ListConstraints(_ context.Context, req *orgpolicypb.ListConstraintsRequest) (*orgpolicypb.ListConstraintsResponse, error) {
	all, err := s.reg.ConstraintsV2(req.GetParent())
	if err != nil {
		return nil, err
	}
	byName := map[string]*orgpolicypb.Constraint{}
	names := make([]string, 0, len(all))
	for _, c := range all {
		byName[c.Name] = c
		names = append(names, c.Name)
	}
	page, next, err := paging.Page("orgpolicy-constraints:"+req.GetParent(), names, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("page_token: %v", err)
	}
	out := &orgpolicypb.ListConstraintsResponse{NextPageToken: next}
	for _, n := range page {
		out.Constraints = append(out.Constraints, byName[n])
	}
	return out, nil
}

func (s *OrgPolicyServer) ListPolicies(_ context.Context, req *orgpolicypb.ListPoliciesRequest) (*orgpolicypb.ListPoliciesResponse, error) {
	all, err := s.reg.PoliciesV2(req.GetParent())
	if err != nil {
		return nil, err
	}
	byName := map[string]*orgpolicypb.Policy{}
	names := make([]string, 0, len(all))
	for _, p := range all {
		byName[p.Name] = p
		names = append(names, p.Name)
	}
	page, next, err := paging.Page("orgpolicy-policies:"+req.GetParent(), names, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("page_token: %v", err)
	}
	out := &orgpolicypb.ListPoliciesResponse{NextPageToken: next}
	for _, n := range page {
		out.Policies = append(out.Policies, byName[n])
	}
	return out, nil
}

func (s *OrgPolicyServer) GetPolicy(_ context.Context, req *orgpolicypb.GetPolicyRequest) (*orgpolicypb.Policy, error) {
	return s.reg.PolicyV2(req.GetName())
}

func (s *OrgPolicyServer) GetEffectivePolicy(_ context.Context, req *orgpolicypb.GetEffectivePolicyRequest) (*orgpolicypb.Policy, error) {
	return s.reg.EffectivePolicyV2(req.GetName())
}

func (s *OrgPolicyServer) CreatePolicy(_ context.Context, req *orgpolicypb.CreatePolicyRequest) (*orgpolicypb.Policy, error) {
	p := req.GetPolicy()
	if p == nil {
		return nil, apierror.InvalidArgument("policy is required")
	}
	parent, _, err := splitPolicyName(p.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetParent() != "" && req.GetParent() != parent {
		// The parent may name a project by ID and the policy by number,
		// or the other way round: both must be the same resource.
		s.reg.mu.Lock()
		a, _, errA := s.reg.canonicalPolicyParent(req.GetParent())
		b, _, errB := s.reg.canonicalPolicyParent(parent)
		s.reg.unlock()
		if errA != nil {
			return nil, errA
		}
		if errB != nil || a != b {
			return nil, apierror.InvalidArgument("policy %s is not under parent %s", p.GetName(), req.GetParent())
		}
	}
	return s.reg.setPolicyV2(p, true, false, nil)
}

func (s *OrgPolicyServer) UpdatePolicy(_ context.Context, req *orgpolicypb.UpdatePolicyRequest) (*orgpolicypb.Policy, error) {
	return s.reg.setPolicyV2(req.GetPolicy(), false, false, req.GetUpdateMask().GetPaths())
}

func (s *OrgPolicyServer) DeletePolicy(_ context.Context, req *orgpolicypb.DeletePolicyRequest) (*emptypb.Empty, error) {
	if err := s.reg.DeletePolicyV2(req.GetName(), req.GetEtag()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *OrgPolicyServer) CreateCustomConstraint(_ context.Context, req *orgpolicypb.CreateCustomConstraintRequest) (*orgpolicypb.CustomConstraint, error) {
	c := req.GetCustomConstraint()
	if c == nil {
		return nil, apierror.InvalidArgument("custom_constraint is required")
	}
	key, err := s.reg.customKey(c.GetName())
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(c.GetName(), req.GetParent()+"/") {
		return nil, apierror.InvalidArgument("custom constraint %s is not under parent %s", c.GetName(), req.GetParent())
	}
	return s.reg.putCustom(key, c, true)
}

func (s *OrgPolicyServer) UpdateCustomConstraint(_ context.Context, req *orgpolicypb.UpdateCustomConstraintRequest) (*orgpolicypb.CustomConstraint, error) {
	c := req.GetCustomConstraint()
	key, err := s.reg.customKey(c.GetName())
	if err != nil {
		return nil, err
	}
	return s.reg.putCustom(key, c, false)
}

func (s *OrgPolicyServer) GetCustomConstraint(_ context.Context, req *orgpolicypb.GetCustomConstraintRequest) (*orgpolicypb.CustomConstraint, error) {
	key, err := s.reg.customKey(req.GetName())
	if err != nil {
		return nil, err
	}
	s.reg.mu.Lock()
	defer s.reg.unlock()
	c, err := s.reg.readCustom(key)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, apierror.NotFound("Custom constraint %s not found", req.GetName())
	}
	return c, nil
}

func (s *OrgPolicyServer) ListCustomConstraints(_ context.Context, req *orgpolicypb.ListCustomConstraintsRequest) (*orgpolicypb.ListCustomConstraintsResponse, error) {
	all, err := s.reg.CustomConstraints(req.GetParent())
	if err != nil {
		return nil, err
	}
	byName := map[string]*orgpolicypb.CustomConstraint{}
	names := make([]string, 0, len(all))
	for _, c := range all {
		byName[c.Name] = c
		names = append(names, c.Name)
	}
	page, next, err := paging.Page("orgpolicy-custom:"+req.GetParent(), names, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("page_token: %v", err)
	}
	out := &orgpolicypb.ListCustomConstraintsResponse{NextPageToken: next}
	for _, n := range page {
		out.CustomConstraints = append(out.CustomConstraints, byName[n])
	}
	return out, nil
}

func (s *OrgPolicyServer) DeleteCustomConstraint(_ context.Context, req *orgpolicypb.DeleteCustomConstraintRequest) (*emptypb.Empty, error) {
	key, err := s.reg.customKey(req.GetName())
	if err != nil {
		return nil, err
	}
	s.reg.mu.Lock()
	defer s.reg.unlock()
	c, err := s.reg.readCustom(key)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, apierror.NotFound("Custom constraint %s not found", req.GetName())
	}
	if err := s.reg.st.Delete(key); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, apierror.Internal(err, "deleting the custom constraint")
	}
	return &emptypb.Empty{}, nil
}

// isOrgPolicyPath reports whether path is the Org Policy v2 JSON API's.
func isOrgPolicyPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return false
	}
	switch parts[0] {
	case "projects", "folders", "organizations":
	default:
		return false
	}
	seg := parts[2]
	if i := strings.Index(seg, ":"); i >= 0 {
		seg = seg[:i]
	}
	return seg == "policies" || seg == "constraints" || seg == "customConstraints"
}
