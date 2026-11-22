package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	adminpb "cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	asset "google.golang.org/api/cloudasset/v1"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// analysisFixture is an organization with a folder, a project in it with a
// service account, a group with a nested group, and policies on each.
type analysisFixture struct {
	h      http.Handler
	reg    *resourcemanager.Registry
	folder string
	sa     string
}

func newAnalysisFixture(t *testing.T) analysisFixture {
	t.Helper()
	reg := resourcemanager.New(store.NewMemory())
	f, err := reg.CreateFolder(resourcemanager.OrganizationName, "eng")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "an-proj", Parent: f.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "an-other"}); err != nil {
		t.Fatal(err)
	}
	sa, err := reg.CreateServiceAccount(&adminpb.CreateServiceAccountRequest{Name: "projects/an-proj", AccountId: "robot-one"})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := reg.CreateGroup(resourcemanager.GroupInput{Email: "outer@cloudburrow.local", Parent: resourcemanager.CustomerName, Labels: map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := reg.CreateGroup(resourcemanager.GroupInput{Email: "inner@cloudburrow.local", Parent: resourcemanager.CustomerName, Labels: map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ g, email string }{{outer.Name(), "inner@cloudburrow.local"}, {inner.Name(), "ann@cloudburrow.local"}} {
		if _, _, err := reg.CreateMembership(m.g, m.email, "", []string{"MEMBER"}); err != nil {
			t.Fatal(err)
		}
	}
	set := func(res string, bs ...*iampb.Binding) {
		t.Helper()
		if _, err := reg.SetIAMPolicy(res, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Version: 3, Bindings: bs}}); err != nil {
			t.Fatalf("SetIAMPolicy %s: %v", res, err)
		}
	}
	set(f.Name, &iampb.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{"group:outer@cloudburrow.local"}})
	set("projects/an-proj",
		&iampb.Binding{Role: "roles/viewer", Members: []string{"serviceAccount:" + sa.GetEmail()}},
		&iampb.Binding{Role: "roles/iam.serviceAccountTokenCreator", Members: []string{"user:bob@cloudburrow.local"}},
		&iampb.Binding{Role: "roles/secretmanager.admin", Members: []string{"user:carol@cloudburrow.local"},
			Condition: &expr.Expr{Title: "until 2100", Expression: `request.time < timestamp("2100-01-01T00:00:00Z")`}})
	e := newIAMEnforcer(config.Config{}, nil, func() *resourcemanager.Registry { return reg })
	h := iamAnalysis{e: e, reg: func() *resourcemanager.Registry { return reg }}
	return analysisFixture{h: h, reg: reg, folder: f.Name, sa: sa.GetEmail()}
}

func (f analysisFixture) call(t *testing.T, method, path string, q url.Values, body string) (int, []byte) {
	t.Helper()
	u := path
	if q != nil {
		u += "?" + q.Encode()
	}
	r := httptest.NewRequest(method, u, strings.NewReader(body))
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func (f analysisFixture) analyze(t *testing.T, scope string, q url.Values) *asset.AnalyzeIamPolicyResponse {
	t.Helper()
	code, b := f.call(t, http.MethodGet, "/v1/"+scope+":analyzeIamPolicy", q, "")
	if code != http.StatusOK {
		t.Fatalf("analyzeIamPolicy %s %v = %d %s", scope, q, code, b)
	}
	var out asset.AnalyzeIamPolicyResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func resultFor(resp *asset.AnalyzeIamPolicyResponse, role string) *asset.IamPolicyAnalysisResult {
	for _, r := range resp.MainAnalysis.AnalysisResults {
		if r.IamBinding.Role == role {
			return r
		}
	}
	return nil
}

func TestAnalyzeScopeAndOptions(t *testing.T) {
	f := newAnalysisFixture(t)
	// A project scope reads only its own policies and its descendants'.
	resp := f.analyze(t, "projects/an-proj", url.Values{"analysisQuery.accessSelector.roles": {"roles/secretmanager.secretAccessor"}})
	if len(resp.MainAnalysis.AnalysisResults) != 0 {
		t.Errorf("project scope read the folder's policy: %+v", resp.MainAnalysis.AnalysisResults)
	}
	// The folder scope with expandGroups, expandResources and both edges.
	resp = f.analyze(t, f.folder, url.Values{
		"analysisQuery.accessSelector.permissions":  {"secretmanager.versions.access"},
		"analysisQuery.options.expandGroups":        {"true"},
		"analysisQuery.options.expandResources":     {"true"},
		"analysisQuery.options.outputGroupEdges":    {"true"},
		"analysisQuery.options.outputResourceEdges": {"true"},
	})
	r := resultFor(resp, "roles/secretmanager.secretAccessor")
	if r == nil {
		t.Fatalf("no folder result: %+v", resp.MainAnalysis.AnalysisResults)
	}
	var ids []string
	for _, id := range r.IdentityList.Identities {
		ids = append(ids, id.Name)
	}
	if strings.Join(ids, ",") != "group:outer@cloudburrow.local,group:inner@cloudburrow.local,user:ann@cloudburrow.local" {
		t.Errorf("identities = %v", ids)
	}
	if len(r.IdentityList.GroupEdges) != 2 || r.IdentityList.GroupEdges[1].TargetNode != "user:ann@cloudburrow.local" {
		t.Errorf("group edges = %+v", r.IdentityList.GroupEdges)
	}
	acl := r.AccessControlLists[0]
	have := map[string]bool{}
	for _, x := range acl.Resources {
		have[x.FullResourceName] = true
	}
	proj := "//cloudresourcemanager.googleapis.com/projects/an-proj"
	saFull := "//iam.googleapis.com/projects/an-proj/serviceAccounts/" + f.sa
	if !have["//cloudresourcemanager.googleapis.com/"+f.folder] || !have[proj] || !have[saFull] || have["//cloudresourcemanager.googleapis.com/projects/an-other"] {
		t.Errorf("expanded resources = %v", have)
	}
	if len(acl.ResourceEdges) < 2 {
		t.Errorf("resource edges = %+v", acl.ResourceEdges)
	}
	// An identity selector through nested groups, with the path.
	resp = f.analyze(t, f.folder, url.Values{"analysisQuery.identitySelector.identity": {"user:ann@cloudburrow.local"},
		"analysisQuery.options.outputGroupEdges": {"true"}})
	r = resultFor(resp, "roles/secretmanager.secretAccessor")
	if r == nil || r.IdentityList.Identities[0].Name != "user:ann@cloudburrow.local" || len(r.IdentityList.GroupEdges) != 2 {
		t.Fatalf("identity result = %+v", r)
	}
	// expandRoles lists the role's permissions.
	resp = f.analyze(t, f.folder, url.Values{"analysisQuery.resourceSelector.fullResourceName": {proj}, "analysisQuery.options.expandRoles": {"true"}})
	r = resultFor(resp, "roles/secretmanager.secretAccessor")
	if r == nil || len(r.AccessControlLists[0].Accesses) == 0 || r.AccessControlLists[0].Accesses[0].Permission == "" {
		t.Fatalf("expandRoles = %+v", r)
	}
}

func TestAnalyzeConditionsAndErrors(t *testing.T) {
	f := newAnalysisFixture(t)
	q := url.Values{"analysisQuery.accessSelector.roles": {"roles/secretmanager.admin"}}
	r := resultFor(f.analyze(t, "projects/an-proj", q), "roles/secretmanager.admin")
	if r == nil || r.AccessControlLists[0].ConditionEvaluation.EvaluationValue != "CONDITIONAL" {
		t.Fatalf("without an access time = %+v", r)
	}
	q.Set("analysisQuery.conditionContext.accessTime", "2099-02-01T00:00:00Z")
	r = resultFor(f.analyze(t, "projects/an-proj", q), "roles/secretmanager.admin")
	if r == nil || r.AccessControlLists[0].ConditionEvaluation.EvaluationValue != "TRUE" {
		t.Fatalf("with 2099 = %+v", r)
	}
	q.Set("analysisQuery.conditionContext.accessTime", "2101-02-01T00:00:00Z")
	if r := resultFor(f.analyze(t, "projects/an-proj", q), "roles/secretmanager.admin"); r != nil {
		t.Fatalf("a false condition is included: %+v", r)
	}
	for name, v := range map[string]url.Values{
		"expandGroups with identity": {"analysisQuery.identitySelector.identity": {"user:a@x"}, "analysisQuery.options.expandGroups": {"true"}},
		"expandRoles with access":    {"analysisQuery.accessSelector.roles": {"roles/viewer"}, "analysisQuery.options.expandRoles": {"true"}},
		"expandResources on folder":  {"analysisQuery.resourceSelector.fullResourceName": {"//cloudresourcemanager.googleapis.com/" + f.folder}, "analysisQuery.options.expandResources": {"true"}},
		"no selector":                {},
		"past access time":           {"analysisQuery.accessSelector.roles": {"roles/viewer"}, "analysisQuery.conditionContext.accessTime": {"2001-01-01T00:00:00Z"}},
	} {
		if code, b := f.call(t, http.MethodGet, "/v1/projects/an-proj:analyzeIamPolicy", v, ""); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s; want 400", name, code, b)
		}
	}
}

func TestAnalyzeImpersonationAndSavedQueries(t *testing.T) {
	f := newAnalysisFixture(t)
	// Who has roles/viewer's resourcemanager.projects.get, and through
	// which service account: bob may mint the robot's tokens.
	resp := f.analyze(t, "projects/an-proj", url.Values{"analysisQuery.accessSelector.permissions": {"resourcemanager.projects.get"},
		"analysisQuery.options.analyzeServiceAccountImpersonation": {"true"}})
	if len(resp.ServiceAccountImpersonationAnalysis) != 1 {
		t.Fatalf("impersonation analyses = %+v", resp.ServiceAccountImpersonationAnalysis)
	}
	found := false
	for _, r := range resp.ServiceAccountImpersonationAnalysis[0].AnalysisResults {
		for _, id := range r.IdentityList.Identities {
			found = found || id.Name == "user:bob@cloudburrow.local"
		}
	}
	if !found {
		t.Errorf("bob is not in the impersonation analysis: %+v", resp.ServiceAccountImpersonationAnalysis[0])
	}
	// Saved queries: create, get by number, list with a filter, use, patch, delete.
	code, b := f.call(t, http.MethodPost, "/v1/projects/an-proj/savedQueries", url.Values{"savedQueryId": {"viewers"}},
		`{"description":"who views","labels":{"team":"sec"},"content":{"iamPolicyAnalysisQuery":{"scope":"projects/an-proj","accessSelector":{"roles":["roles/viewer"]}}}}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d %s", code, b)
	}
	var sq struct{ Name string }
	_ = json.Unmarshal(b, &sq)
	if !strings.HasPrefix(sq.Name, "projects/") || strings.Contains(sq.Name, "an-proj") || !strings.HasSuffix(sq.Name, "/savedQueries/viewers") {
		t.Errorf("name = %q; want projects/{number}/savedQueries/viewers", sq.Name)
	}
	if code, _ := f.call(t, http.MethodPost, "/v1/projects/an-proj/savedQueries", url.Values{"savedQueryId": {"viewers"}}, `{}`); code != http.StatusConflict {
		t.Errorf("duplicate = %d; want 409", code)
	}
	if code, _ := f.call(t, http.MethodPost, "/v1/projects/an-proj/savedQueries", url.Values{"savedQueryId": {"Bad_ID"}}, `{}`); code != http.StatusBadRequest {
		t.Errorf("bad id = %d; want 400", code)
	}
	code, b = f.call(t, http.MethodGet, "/v1/projects/an-proj/savedQueries", url.Values{"filter": {"labels.team=sec"}}, "")
	if code != http.StatusOK || !strings.Contains(string(b), "viewers") {
		t.Errorf("list = %d %s", code, b)
	}
	resp = f.analyze(t, "projects/an-proj", url.Values{"savedAnalysisQuery": {sq.Name}})
	if resultFor(resp, "roles/viewer") == nil || len(resp.MainAnalysis.AnalysisResults) != 1 {
		t.Errorf("saved query analysis = %+v", resp.MainAnalysis.AnalysisResults)
	}
	code, b = f.call(t, http.MethodPatch, "/v1/"+sq.Name, url.Values{"updateMask": {"description"}}, `{"description":"changed"}`)
	if code != http.StatusOK || !strings.Contains(string(b), "changed") {
		t.Errorf("patch = %d %s", code, b)
	}
	if code, _ := f.call(t, http.MethodDelete, "/v1/"+sq.Name, nil, ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
	if code, _ := f.call(t, http.MethodGet, "/v1/"+sq.Name, nil, ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d", code)
	}
	// The long-running method needs an output config.
	if code, _ := f.call(t, http.MethodPost, "/v1/projects/an-proj:analyzeIamPolicyLongrunning", nil,
		`{"analysisQuery":{"accessSelector":{"roles":["roles/viewer"]}}}`); code != http.StatusBadRequest {
		t.Errorf("no output config = %d", code)
	}
	_ = time.Now
}
