package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	asset "google.golang.org/api/cloudasset/v1"
	ptv1 "google.golang.org/api/policytroubleshooter/v1"
	pt "google.golang.org/api/policytroubleshooter/v3"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Policy Troubleshooter v3's iam:troubleshoot and Cloud Asset's
// analyzeIamPolicy (#1879), on the Resource Manager port, over the same
// evaluator enforcement uses (internal/iameval), so what they explain is
// what --iam-enforcement=on decides. Both answer whatever the enforcement
// mode: on Google they explain the stored policies, which exist either way.
//
// iam:troubleshoot takes accessTuple.principal (an email),
// fullResourceName and permission, and conditionContext.request.receiveTime
// for request.time (now when absent). It explains every allow policy of the
// resource's chain that is set, binding by binding, with Google's access
// states and relevances, and every IAM v2 deny policy attached to the
// resource's project, folders and organization, rule by rule (#1886): a
// denied permission is CANNOT_ACCESS whatever the allow policies grant.
//
// analyzeIamPolicy (GET v1/{scope}:analyzeIamPolicy) answers from the
// policies of the scope (a project, folder or organization) and of the
// project's ancestors, or, with analysisQuery.resourceSelector, of that
// resource's chain: each binding that names the identity
// (analysisQuery.identitySelector.identity, matched through its groups and
// domain) and holds one of the permissions or roles
// (analysisQuery.accessSelector), as an analysis result. Policies of a
// service's resources are read only through resourceSelector: the
// resources under a scope are not enumerated (UNVERIFIED against Google's
// expanded output, which lists them; docs re-read 2026-11-19:
// https://docs.cloud.google.com/asset-inventory/docs/analyzing-iam-policy).

const (
	troubleshootPath = "/v3/iam:troubleshoot"
	crmPrefix        = "//cloudresourcemanager.googleapis.com/"
)

// troubleshootV1Paths are v1's and v2alpha1's iam:troubleshoot, one shape:
// gcloud policy-troubleshoot iam calls v2alpha1 in every release track
// (Cloud SDK 593.0.0's surface/policy_troubleshoot/iam.yaml).
var troubleshootV1Paths = []string{"/v1/iam:troubleshoot", "/v2alpha1/iam:troubleshoot"}

// isIAMAnalysisPath is whether r is one of the two methods.
func isIAMAnalysisPath(r *http.Request) bool {
	return r.URL.Path == troubleshootPath || slices.Contains(troubleshootV1Paths, r.URL.Path) ||
		strings.HasSuffix(r.URL.Path, ":analyzeIamPolicy")
}

// iamAnalysis serves both methods over e.
type iamAnalysis struct{ e *iameval.Enforcer }

func (a iamAnalysis) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == troubleshootPath && r.Method == http.MethodPost:
		a.troubleshoot(w, r)
	case slices.Contains(troubleshootV1Paths, r.URL.Path) && r.Method == http.MethodPost:
		a.troubleshootV1(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/") && strings.HasSuffix(r.URL.Path, ":analyzeIamPolicy") && r.Method == http.MethodGet:
		a.analyze(w, r)
	default:
		apierror.WriteJSON(w, apierror.NotFound("%s %s is not served", r.Method, r.URL.Path))
	}
}

// requestFor is the check of permission on a full resource name.
func (a iamAnalysis) requestFor(ctx context.Context, email, permission, full string, at time.Time) (iameval.Request, error) {
	host, name, ok := strings.Cut(strings.TrimPrefix(full, "//"), "/")
	if !strings.HasPrefix(full, "//") || !ok || name == "" {
		return iameval.Request{}, apierror.InvalidArgument("fullResourceName %q is not a full resource name, such as //cloudresourcemanager.googleapis.com/projects/my-project", full)
	}
	var req iameval.Request
	if svc := iameval.ServiceByHost(host); svc != nil {
		req = a.e.Request(ctx, svc, email, permission, name)
	} else {
		req = iameval.Request{Email: email, Permission: permission, Resource: name, Service: host, Project: iameval.Project(name)}
		if host == "cloudresourcemanager.googleapis.com" {
			req.Type = "cloudresourcemanager.googleapis.com/Project"
			switch {
			case strings.HasPrefix(name, "folders/"):
				req.Type = "cloudresourcemanager.googleapis.com/Folder"
				req.Own = []iameval.Attached{{Resource: name, Policy: a.e.Eval.Source.Policy(name)}}
			case strings.HasPrefix(name, "organizations/"):
				req.Type = "cloudresourcemanager.googleapis.com/Organization"
				req.Own = []iameval.Attached{{Resource: name, Policy: a.e.Eval.Source.Policy(name)}}
			}
		}
	}
	req.Time = at
	return req, nil
}

// fullName is a chain entry's full resource name.
func fullName(resource string) string {
	if strings.HasPrefix(resource, "//") {
		return resource
	}
	return crmPrefix + resource
}

func (a iamAnalysis) troubleshoot(w http.ResponseWriter, r *http.Request) {
	var in pt.GoogleCloudPolicytroubleshooterIamV3TroubleshootIamPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("Invalid JSON payload received. %v", err))
		return
	}
	t := in.AccessTuple
	switch {
	case t == nil || t.Principal == "":
		apierror.WriteJSON(w, apierror.InvalidArgument("access_tuple.principal is required"))
		return
	case t.FullResourceName == "":
		apierror.WriteJSON(w, apierror.InvalidArgument("access_tuple.full_resource_name is required"))
		return
	case t.Permission == "":
		apierror.WriteJSON(w, apierror.InvalidArgument("access_tuple.permission is required"))
		return
	}
	at := time.Now()
	if c := t.ConditionContext; c != nil && c.Request != nil && c.Request.ReceiveTime != "" {
		p, err := time.Parse(time.RFC3339Nano, c.Request.ReceiveTime)
		if err != nil {
			apierror.WriteJSON(w, apierror.InvalidArgument("access_tuple.condition_context.request.receive_time: %v", err))
			return
		}
		at = p
	}
	email := strings.ToLower(strings.TrimSpace(t.Principal))
	email = strings.TrimPrefix(strings.TrimPrefix(email, "user:"), "serviceaccount:")
	req, err := a.requestFor(r.Context(), email, t.Permission, t.FullResourceName, at)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	d := a.e.Eval.Check(req)
	members := map[string]bool{}
	for _, m := range a.e.Eval.Members(email) {
		members[m] = true
	}
	out := &pt.GoogleCloudPolicytroubleshooterIamV3TroubleshootIamPolicyResponse{
		AccessTuple:        t,
		OverallAccessState: "CANNOT_ACCESS",
		AllowPolicyExplanation: &pt.GoogleCloudPolicytroubleshooterIamV3AllowPolicyExplanation{
			AllowAccessState: string(iameval.NotGranted), Relevance: "HEURISTIC_RELEVANCE_NORMAL"},
		DenyPolicyExplanation: ptDeny(d, t.Permission),
	}
	if d.Granted {
		out.OverallAccessState = "CAN_ACCESS"
	}
	if d.Allowed {
		out.AllowPolicyExplanation.AllowAccessState = string(iameval.Granted)
		out.AllowPolicyExplanation.Relevance = "HEURISTIC_RELEVANCE_HIGH"
	}
	for _, p := range d.Policies {
		if p.Policy == nil {
			continue
		}
		ep := &pt.GoogleCloudPolicytroubleshooterIamV3ExplainedAllowPolicy{FullResourceName: fullName(p.Resource),
			Policy: ptPolicy(p.Policy), AllowAccessState: string(iameval.NotGranted), Relevance: "HEURISTIC_RELEVANCE_NORMAL"}
		if p.Grants {
			ep.AllowAccessState, ep.Relevance = string(iameval.Granted), "HEURISTIC_RELEVANCE_HIGH"
		}
		for _, b := range p.Bindings {
			ep.BindingExplanations = append(ep.BindingExplanations, ptBinding(b, t.Permission, members))
		}
		out.AllowPolicyExplanation.ExplainedPolicies = append(out.AllowPolicyExplanation.ExplainedPolicies, ep)
	}
	writeIAMJSON(w, out)
}

// troubleshootV1 is v1's and v2alpha1's iam:troubleshoot: one access
// state and the explained policies, with v1's enum names.
func (a iamAnalysis) troubleshootV1(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccessTuple struct {
			Principal        string `json:"principal"`
			FullResourceName string `json:"fullResourceName"`
			Permission       string `json:"permission"`
			ConditionContext *struct {
				Request *struct {
					ReceiveTime string `json:"receiveTime"`
				} `json:"request"`
			} `json:"conditionContext"`
		} `json:"accessTuple"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("Invalid JSON payload received. %v", err))
		return
	}
	t := in.AccessTuple
	switch {
	case t.Principal == "":
		apierror.WriteJSON(w, apierror.InvalidArgument("access_tuple.principal is required"))
		return
	case t.FullResourceName == "":
		apierror.WriteJSON(w, apierror.InvalidArgument("access_tuple.full_resource_name is required"))
		return
	case t.Permission == "":
		apierror.WriteJSON(w, apierror.InvalidArgument("access_tuple.permission is required"))
		return
	}
	at := time.Now()
	if c := t.ConditionContext; c != nil && c.Request != nil && c.Request.ReceiveTime != "" {
		p, err := time.Parse(time.RFC3339Nano, c.Request.ReceiveTime)
		if err != nil {
			apierror.WriteJSON(w, apierror.InvalidArgument("access_tuple.condition_context.request.receive_time: %v", err))
			return
		}
		at = p
	}
	email := strings.ToLower(strings.TrimSpace(t.Principal))
	req, err := a.requestFor(r.Context(), email, t.Permission, t.FullResourceName, at)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	d := a.e.Eval.Check(req)
	members := map[string]bool{}
	for _, m := range a.e.Eval.Members(email) {
		members[m] = true
	}
	v1Rel := func(high bool) string {
		if high {
			return "HIGH"
		}
		return "NORMAL"
	}
	v1Access := func(granted bool) string {
		if granted {
			return "GRANTED"
		}
		return "NOT_GRANTED"
	}
	// v1 and v2alpha1 explain allow policies only; a denied permission is
	// NOT_GRANTED, as the caller cannot use it.
	out := &ptv1.GoogleCloudPolicytroubleshooterV1TroubleshootIamPolicyResponse{Access: v1Access(d.Granted)}
	for _, p := range d.Policies {
		if p.Policy == nil {
			continue
		}
		pp := ptPolicy(p.Policy)
		ep := &ptv1.GoogleCloudPolicytroubleshooterV1ExplainedPolicy{FullResourceName: fullName(p.Resource), Access: v1Access(p.Grants),
			Relevance: v1Rel(p.Grants), Policy: &ptv1.GoogleIamV1Policy{Etag: pp.Etag, Version: pp.Version}}
		for _, b := range pp.Bindings {
			pb := &ptv1.GoogleIamV1Binding{Role: b.Role, Members: b.Members}
			if b.Condition != nil {
				pb.Condition = &ptv1.GoogleTypeExpr{Title: b.Condition.Title, Description: b.Condition.Description, Expression: b.Condition.Expression}
			}
			ep.Policy.Bindings = append(ep.Policy.Bindings, pb)
		}
		for _, b := range p.Bindings {
			be := &ptv1.GoogleCloudPolicytroubleshooterV1BindingExplanation{Role: b.Role, Access: v1Access(b.Grants),
				Relevance: v1Rel(b.Grants), RolePermission: "ROLE_PERMISSION_NOT_INCLUDED", RolePermissionRelevance: v1Rel(!b.RoleHasPermission),
				Memberships: map[string]ptv1.GoogleCloudPolicytroubleshooterV1BindingExplanationAnnotatedMembership{}}
			if b.RoleHasPermission {
				be.RolePermission = "ROLE_PERMISSION_INCLUDED"
			}
			for _, m := range b.Members {
				state := "MEMBERSHIP_NOT_INCLUDED"
				if members[strings.ToLower(strings.TrimSpace(m))] {
					state = "MEMBERSHIP_INCLUDED"
				}
				be.Memberships[m] = ptv1.GoogleCloudPolicytroubleshooterV1BindingExplanationAnnotatedMembership{
					Membership: state, Relevance: v1Rel(state == "MEMBERSHIP_INCLUDED")}
			}
			if c := b.Condition; c != nil {
				be.Condition = &ptv1.GoogleTypeExpr{Title: c.Title, Description: c.Description, Expression: c.Expression}
			}
			ep.BindingExplanations = append(ep.BindingExplanations, be)
		}
		out.ExplainedPolicies = append(out.ExplainedPolicies, ep)
	}
	writeIAMJSON(w, out)
}

// ptDeny is v3's deny explanation (#1886): every resource of the chain
// with deny policies attached, each policy and each rule, with the
// permission and principal matches.
func ptDeny(d iameval.Decision, permission string) *pt.GoogleCloudPolicytroubleshooterIamV3DenyPolicyExplanation {
	state := func(denies bool) string {
		if denies {
			return iameval.Denied
		}
		return iameval.NotDenied
	}
	out := &pt.GoogleCloudPolicytroubleshooterIamV3DenyPolicyExplanation{DenyAccessState: state(d.Denied),
		Relevance: relevance(d.Denied), PermissionDeniable: iameval.Deniable(permission)}
	byRes := map[string]*pt.GoogleCloudPolicytroubleshooterIamV3ExplainedDenyResource{}
	for _, p := range d.Deny {
		res := byRes[p.Resource]
		if res == nil {
			res = &pt.GoogleCloudPolicytroubleshooterIamV3ExplainedDenyResource{FullResourceName: fullName(p.Resource),
				DenyAccessState: iameval.NotDenied, Relevance: relevance(false)}
			byRes[p.Resource] = res
			out.ExplainedResources = append(out.ExplainedResources, res)
		}
		if p.Denies {
			res.DenyAccessState, res.Relevance = iameval.Denied, relevance(true)
		}
		ep := &pt.GoogleCloudPolicytroubleshooterIamV3ExplainedDenyPolicy{DenyAccessState: state(p.Denies), Relevance: relevance(p.Denies),
			Policy: ptV2Policy(p)}
		for _, r := range p.Rules {
			ep.RuleExplanations = append(ep.RuleExplanations, ptDenyRule(r))
		}
		res.ExplainedPolicies = append(res.ExplainedPolicies, ep)
	}
	return out
}

func ptV2Policy(p iameval.DenyPolicyResult) *pt.GoogleIamV2Policy {
	var raw []byte
	if p.Policy != nil {
		raw, _ = protojson.Marshal(p.Policy)
	}
	out := &pt.GoogleIamV2Policy{}
	_ = json.Unmarshal(raw, out)
	return out
}

func ptDenyRule(r iameval.DenyRuleResult) *pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanation {
	perm := func(matched bool) pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedPermissionMatching {
		st := "PERMISSION_PATTERN_NOT_MATCHED"
		if matched {
			st = "PERMISSION_PATTERN_MATCHED"
		}
		return pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedPermissionMatching{PermissionMatchingState: st, Relevance: relevance(matched)}
	}
	member := func(matched bool) pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedDenyPrincipalMatching {
		st := "MEMBERSHIP_NOT_MATCHED"
		if matched {
			st = "MEMBERSHIP_MATCHED"
		}
		return pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedDenyPrincipalMatching{Membership: st, Relevance: relevance(matched)}
	}
	ref := func(v pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedPermissionMatching) *pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedPermissionMatching {
		return &v
	}
	refm := func(v pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedDenyPrincipalMatching) *pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedDenyPrincipalMatching {
		return &v
	}
	st := iameval.NotDenied
	if r.Denies {
		st = iameval.Denied
	}
	out := &pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanation{DenyAccessState: st, Relevance: relevance(r.Denies),
		CombinedDeniedPermission: ref(perm(r.Permission != "")), CombinedExceptionPermission: ref(perm(r.ExceptPermission != "")),
		CombinedDeniedPrincipal: refm(member(r.Principal != "")), CombinedExceptionPrincipal: refm(member(r.ExceptPrincipal != "")),
		DeniedPermissions:    map[string]pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedPermissionMatching{},
		ExceptionPermissions: map[string]pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedPermissionMatching{},
		DeniedPrincipals:     map[string]pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedDenyPrincipalMatching{},
		ExceptionPrincipals:  map[string]pt.GoogleCloudPolicytroubleshooterIamV3DenyRuleExplanationAnnotatedDenyPrincipalMatching{}}
	for _, p := range r.Rule.GetDeniedPermissions() {
		out.DeniedPermissions[p] = perm(p == r.Permission)
	}
	for _, p := range r.Rule.GetExceptionPermissions() {
		out.ExceptionPermissions[p] = perm(p == r.ExceptPermission)
	}
	for _, p := range r.Rule.GetDeniedPrincipals() {
		out.DeniedPrincipals[p] = member(p == r.Principal)
	}
	for _, p := range r.Rule.GetExceptionPrincipals() {
		out.ExceptionPrincipals[p] = member(p == r.ExceptPrincipal)
	}
	if c := r.Rule.GetDenialCondition(); c != nil {
		out.Condition = &pt.GoogleTypeExpr{Title: c.GetTitle(), Description: c.GetDescription(), Expression: c.GetExpression(), Location: c.GetLocation()}
		ce := &pt.GoogleCloudPolicytroubleshooterIamV3ConditionExplanation{}
		switch r.Condition {
		case "true":
			ce.Value = true
		case "false":
			ce.Value = false
		default:
			ce.Errors = []*pt.GoogleRpcStatus{{Code: 3, Message: "The condition could not be evaluated, so the rule denies the permission"}}
		}
		out.ConditionExplanation = ce
	}
	return out
}

func relevance(high bool) string {
	if high {
		return "HEURISTIC_RELEVANCE_HIGH"
	}
	return "HEURISTIC_RELEVANCE_NORMAL"
}

func ptBinding(b iameval.BindingResult, permission string, members map[string]bool) *pt.GoogleCloudPolicytroubleshooterIamV3AllowBindingExplanation {
	be := &pt.GoogleCloudPolicytroubleshooterIamV3AllowBindingExplanation{Role: b.Role,
		AllowAccessState: string(iameval.NotGranted), Relevance: relevance(b.Grants),
		RolePermission: "ROLE_PERMISSION_NOT_INCLUDED", RolePermissionRelevance: relevance(!b.RoleHasPermission),
		Memberships: map[string]pt.GoogleCloudPolicytroubleshooterIamV3AllowBindingExplanationAnnotatedAllowMembership{},
		CombinedMembership: &pt.GoogleCloudPolicytroubleshooterIamV3AllowBindingExplanationAnnotatedAllowMembership{
			Membership: "MEMBERSHIP_NOT_MATCHED", Relevance: relevance(b.Member == "")}}
	if b.Grants {
		be.AllowAccessState = string(iameval.Granted)
	}
	if b.RoleHasPermission {
		be.RolePermission = "ROLE_PERMISSION_INCLUDED"
	}
	if b.Member != "" {
		be.CombinedMembership.Membership = "MEMBERSHIP_MATCHED"
	}
	for _, m := range b.Members {
		state := "MEMBERSHIP_NOT_MATCHED"
		if members[strings.ToLower(strings.TrimSpace(m))] {
			state = "MEMBERSHIP_MATCHED"
		}
		be.Memberships[m] = pt.GoogleCloudPolicytroubleshooterIamV3AllowBindingExplanationAnnotatedAllowMembership{
			Membership: state, Relevance: relevance(state == "MEMBERSHIP_MATCHED")}
	}
	if c := b.Condition; c != nil {
		be.Condition = &pt.GoogleTypeExpr{Title: c.Title, Description: c.Description, Expression: c.Expression}
		be.ConditionExplanation = &pt.GoogleCloudPolicytroubleshooterIamV3ConditionExplanation{Value: b.ConditionTrue}
	}
	return be
}

func ptPolicy(s *iampolicy.Stored) *pt.GoogleIamV1Policy {
	p := &pt.GoogleIamV1Policy{Version: 1}
	if len(s.Etag) > 0 {
		p.Etag = base64.StdEncoding.EncodeToString(s.Etag)
	}
	for _, b := range s.Bindings {
		pb := &pt.GoogleIamV1Binding{Role: b.Role, Members: b.Members}
		if c := b.Condition; c != nil {
			pb.Condition = &pt.GoogleTypeExpr{Title: c.Title, Description: c.Description, Expression: c.Expression}
			p.Version = 3
		}
		p.Bindings = append(p.Bindings, pb)
	}
	return p
}

func (a iamAnalysis) analyze(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/"), ":analyzeIamPolicy")
	if !(strings.HasPrefix(scope, "projects/") || strings.HasPrefix(scope, "folders/") || strings.HasPrefix(scope, "organizations/")) ||
		strings.Count(scope, "/") != 1 {
		apierror.WriteJSON(w, apierror.InvalidArgument("scope %q must be projects/{PROJECT_ID}, projects/{PROJECT_NUMBER}, folders/{FOLDER_NUMBER} or organizations/{ORGANIZATION_NUMBER}", scope))
		return
	}
	q := r.URL.Query()
	identity := strings.TrimSpace(q.Get("analysisQuery.identitySelector.identity"))
	perms := q["analysisQuery.accessSelector.permissions"]
	roles := q["analysisQuery.accessSelector.roles"]
	fullResource := q.Get("analysisQuery.resourceSelector.fullResourceName")
	if identity == "" && len(perms) == 0 && len(roles) == 0 && fullResource == "" {
		apierror.WriteJSON(w, apierror.InvalidArgument("At least one of resource selector, identity selector or access selector must be specified."))
		return
	}
	at := time.Now()
	if v := q.Get("analysisQuery.conditionContext.accessTime"); v != "" {
		p, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			apierror.WriteJSON(w, apierror.InvalidArgument("analysis_query.condition_context.access_time: %v", err))
			return
		}
		at = p
	}
	var chain []iameval.Attached
	resourceName := crmPrefix + scope
	condReq := iameval.Request{Resource: scope, Time: at}
	if fullResource != "" {
		req, err := a.requestFor(r.Context(), "", "", fullResource, at)
		if err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		chain, resourceName, condReq = a.e.Eval.Chain(req), fullResource, req
	} else if strings.HasPrefix(scope, "projects/") {
		chain = a.e.Eval.Chain(iameval.Request{Project: strings.TrimPrefix(scope, "projects/")})
	} else {
		chain = []iameval.Attached{{Resource: scope, Policy: a.e.Eval.Source.Policy(scope)}}
	}
	// The identity's members: an email given as user:, serviceAccount:,
	// group: or domain:, matched through its groups and domain.
	idMembers := map[string]bool{}
	if identity != "" {
		kind, email, _ := strings.Cut(identity, ":")
		switch strings.ToLower(kind) {
		case "user", "serviceaccount":
			for _, m := range a.e.Eval.Members(email) {
				idMembers[m] = true
			}
		default:
			idMembers[strings.ToLower(identity)] = true
		}
	}
	main := &asset.IamPolicyAnalysis{FullyExplored: true, AnalysisQuery: &asset.IamPolicyAnalysisQuery{Scope: scope}}
	if identity != "" {
		main.AnalysisQuery.IdentitySelector = &asset.IdentitySelector{Identity: identity}
	}
	if len(perms) > 0 || len(roles) > 0 {
		main.AnalysisQuery.AccessSelector = &asset.AccessSelector{Permissions: perms, Roles: roles}
	}
	if fullResource != "" {
		main.AnalysisQuery.ResourceSelector = &asset.ResourceSelector{FullResourceName: fullResource}
	}
	for _, p := range chain {
		if p.Policy == nil {
			continue
		}
		for _, b := range p.Policy.Bindings {
			var matched []string
			for _, m := range b.Members {
				if identity == "" || idMembers[strings.ToLower(strings.TrimSpace(m))] {
					matched = append(matched, m)
				}
			}
			if len(matched) == 0 {
				continue
			}
			var accesses []*asset.GoogleCloudAssetV1Access
			for _, perm := range perms {
				if a.e.Eval.RoleGrants(b.Role, perm, "") {
					accesses = append(accesses, &asset.GoogleCloudAssetV1Access{Permission: perm})
				}
			}
			if slices.Contains(roles, b.Role) {
				accesses = append(accesses, &asset.GoogleCloudAssetV1Access{Role: b.Role})
			}
			if len(perms) == 0 && len(roles) == 0 {
				accesses = append(accesses, &asset.GoogleCloudAssetV1Access{Role: b.Role})
			}
			if len(accesses) == 0 {
				continue
			}
			acl := &asset.GoogleCloudAssetV1AccessControlList{Accesses: accesses,
				Resources: []*asset.GoogleCloudAssetV1Resource{{FullResourceName: resourceName}}}
			ab := &asset.Binding{Role: b.Role, Members: b.Members}
			if c := b.Condition; c != nil {
				ab.Condition = &asset.Expr{Title: c.Title, Description: c.Description, Expression: c.Expression}
				eval := "FALSE"
				if b.Grants(condReq.Attributes()) {
					eval = "TRUE"
				}
				acl.ConditionEvaluation = &asset.ConditionEvaluation{EvaluationValue: eval}
			}
			sort.Strings(matched)
			ids := &asset.GoogleCloudAssetV1IdentityList{}
			for _, m := range matched {
				ids.Identities = append(ids.Identities, &asset.GoogleCloudAssetV1Identity{Name: m})
			}
			main.AnalysisResults = append(main.AnalysisResults, &asset.IamPolicyAnalysisResult{
				AttachedResourceFullName: fullName(p.Resource), IamBinding: ab, FullyExplored: true,
				AccessControlLists: []*asset.GoogleCloudAssetV1AccessControlList{acl}, IdentityList: ids})
		}
	}
	writeIAMJSON(w, &asset.AnalyzeIamPolicyResponse{MainAnalysis: main, FullyExplored: true})
}

func writeIAMJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		apierror.WriteJSON(w, apierror.Internal(err, "encode the response"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// consoleTroubleshoot is the console's Policy Troubleshooter form.
type consoleTroubleshoot struct {
	Principal  string `json:"principal"`
	Resource   string `json:"resource"`
	Permission string `json:"permission"`
}

// consoleAnalyze is the console's Policy Analyzer query.
type consoleAnalyze struct {
	Scope       string   `json:"scope"`
	Identity    string   `json:"identity"`
	Permissions []string `json:"permissions"`
	Roles       []string `json:"roles"`
	Resource    string   `json:"resource"`
}

// analysisCall answers the console's Policy Troubleshooter and Policy
// Analyzer pages through the same methods a client calls, and reports the
// enforcement mode.
func (p projectIAMProvider) analysisCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	if p.iam == nil {
		return nil, errors.New("IAM evaluation is not available on this instance")
	}
	var r *http.Request
	switch call {
	case "enforcement":
		mode := config.IAMEnforcementOff
		if p.iam.On != nil && p.iam.On() {
			mode = config.IAMEnforcementOn
		}
		return map[string]string{"mode": mode}, nil
	case "troubleshoot":
		var in consoleTroubleshoot
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("malformed request: %v", err)
		}
		b, _ := json.Marshal(pt.GoogleCloudPolicytroubleshooterIamV3TroubleshootIamPolicyRequest{AccessTuple: &pt.GoogleCloudPolicytroubleshooterIamV3AccessTuple{
			Principal: in.Principal, FullResourceName: in.Resource, Permission: in.Permission}})
		r = httptest.NewRequestWithContext(ctx, http.MethodPost, troubleshootPath, bytes.NewReader(b))
	default:
		var in consoleAnalyze
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("malformed request: %v", err)
		}
		scope := in.Scope
		if scope == "" {
			scope = "projects/" + project
		}
		q := url.Values{}
		if in.Identity != "" {
			q.Set("analysisQuery.identitySelector.identity", in.Identity)
		}
		for _, x := range in.Permissions {
			q.Add("analysisQuery.accessSelector.permissions", x)
		}
		for _, x := range in.Roles {
			q.Add("analysisQuery.accessSelector.roles", x)
		}
		if in.Resource != "" {
			q.Set("analysisQuery.resourceSelector.fullResourceName", in.Resource)
		}
		r = httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/"+scope+":analyzeIamPolicy?"+q.Encode(), nil)
	}
	w := httptest.NewRecorder()
	iamAnalysis{e: p.iam}.ServeHTTP(w, r)
	var out any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		return nil, err
	}
	if w.Code != http.StatusOK {
		if m, ok := out.(map[string]any); ok {
			if e, ok := m["error"].(map[string]any); ok {
				return nil, fmt.Errorf("%v", e["message"])
			}
		}
		return nil, fmt.Errorf("%s", w.Body.String())
	}
	return out, nil
}
