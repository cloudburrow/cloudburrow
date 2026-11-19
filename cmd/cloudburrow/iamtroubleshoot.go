package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	asset "google.golang.org/api/cloudasset/v1"
	ptv1 "google.golang.org/api/policytroubleshooter/v1"
	pt "google.golang.org/api/policytroubleshooter/v3"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
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
// states and relevances; deny policies are not stored here, so
// denyPolicyExplanation is always DENY_ACCESS_STATE_NOT_DENIED.
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
// expanded output, which lists them).

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
		DenyPolicyExplanation: &pt.GoogleCloudPolicytroubleshooterIamV3DenyPolicyExplanation{
			DenyAccessState: "DENY_ACCESS_STATE_NOT_DENIED", Relevance: "HEURISTIC_RELEVANCE_NORMAL", PermissionDeniable: true},
	}
	if d.Granted {
		out.OverallAccessState = "CAN_ACCESS"
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
