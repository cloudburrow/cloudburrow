//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	asset "cloud.google.com/go/asset/apiv1"
	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cloud Asset Inventory's IAM policy analysis (#2112), through the official
// client (cloud.google.com/go/asset/apiv1, its REST transport, as the
// analysis is served on the Resource Manager port's JSON API) and gcloud
// asset: analysisQuery's options over a folder, a project in it, a service
// account, Cloud Identity groups and custom roles; saved queries; and
// analyzeIamPolicyLongrunning to the instance's Cloud Storage and BigQuery.

// assetFixture is a folder with a project, a service account in it, a
// nested group, and their policies.
type assetFixture struct {
	folder, project, sa, group, inner, member, custom string
}

func assetClient(t *testing.T, h *Harness, rmEndpoint string) *asset.Client {
	t.Helper()
	base := "http://" + strings.TrimPrefix(rmEndpoint, "http://")
	c, err := asset.NewRESTClient(h.Context(), option.WithEndpoint(base), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func newAssetFixture(t *testing.T, h *Harness, rmEndpoint, prefix string) assetFixture {
	t.Helper()
	ctx := h.Context()
	fc, _, pc := foldersClientsAt(t, h, rmOptionsAt(rmEndpoint))
	fop, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: "organizations/100000000001",
		DisplayName: folderLabel(h, prefix)}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	project := v1ID(h, prefix)
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: folder.GetName()}})
	if err == nil {
		_, err = waitLRO(ctx, op)
	}
	if err != nil {
		t.Fatalf("CreateProject %s: %v", project, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + project})
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	sa := newServiceAccount(t, h, project, "asa")
	// Groups: outer holds inner, which holds the member.
	svc := cloudIdentity(t, h, rmEndpoint)
	suffix := strings.ToLower(project)
	mk := func(email string) string {
		t.Helper()
		op, err := svc.Groups.Create(&ci.Group{GroupKey: &ci.EntityKey{Id: email}, Parent: "customers/C0cloudburrow",
			Labels: map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}}).InitialGroupConfig("EMPTY").Do()
		if err != nil {
			t.Fatalf("groups.create %s: %v", email, err)
		}
		var g ci.Group
		_ = json.Unmarshal(op.Response, &g)
		t.Cleanup(func() { _, _ = svc.Groups.Delete(g.Name).Do() })
		return g.Name
	}
	outerEmail, innerEmail, member := "outer-"+suffix+"@cloudburrow.local", "inner-"+suffix+"@cloudburrow.local", "ann-"+suffix+"@cloudburrow.local"
	outer, inner := mk(outerEmail), mk(innerEmail)
	for _, m := range []struct{ g, email string }{{outer, innerEmail}, {inner, member}} {
		if _, err := svc.Groups.Memberships.Create(m.g, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: m.email},
			Roles: []*ci.MembershipRole{{Name: "MEMBER"}}}).Do(); err != nil {
			t.Fatalf("memberships.create %s: %v", m.email, err)
		}
	}
	// A custom role of the project.
	admin := iamAdmin(t, h)
	role, err := admin.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: "projects/" + project, RoleId: "assetReader",
		Role: &adminpb.Role{Title: "Asset reader", IncludedPermissions: []string{"secretmanager.secrets.get"}}})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	undoFolder := bindOn(t, fc.GetIamPolicy, fc.SetIamPolicy, folder.GetName(),
		&iampb.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{"group:" + outerEmail}})
	t.Cleanup(undoFolder)
	bind := func(b *iampb.Binding) {
		t.Helper()
		t.Cleanup(bindOn(t, pc.GetIamPolicy, pc.SetIamPolicy, "projects/"+project, b))
	}
	bind(&iampb.Binding{Role: "roles/viewer", Members: []string{"serviceAccount:" + sa.GetEmail()}})
	bind(&iampb.Binding{Role: "roles/iam.serviceAccountTokenCreator", Members: []string{"user:bob-" + suffix + "@cloudburrow.local"}})
	bind(&iampb.Binding{Role: role.GetName(), Members: []string{"user:" + member}})
	bind(&iampb.Binding{Role: "roles/secretmanager.admin", Members: []string{"user:carol-" + suffix + "@cloudburrow.local"},
		Condition: &expr.Expr{Title: "until 2100", Expression: `request.time < timestamp("2100-01-01T00:00:00Z")`}})
	return assetFixture{folder: folder.GetName(), project: project, sa: sa.GetEmail(), group: outerEmail, inner: innerEmail,
		member: member, custom: role.GetName()}
}

func assetResult(resp *assetpb.AnalyzeIamPolicyResponse, role string) *assetpb.IamPolicyAnalysisResult {
	for _, r := range resp.GetMainAnalysis().GetAnalysisResults() {
		if r.GetIamBinding().GetRole() == role {
			return r
		}
	}
	return nil
}

// TestCloudAssetAnalyzeIamPolicyOptions: the scope is the scope and its
// descendants (a project scope does not read its folder's policy);
// expandGroups lists the nested groups' members with outputGroupEdges'
// membership edges; expandResources lists the folder's project and the
// project's service account with outputResourceEdges; the identity
// selector finds a member through nested groups and a custom role;
// expandRoles lists a custom role's permissions; a conditional binding is
// CONDITIONAL without an access time, TRUE before its end and left out
// after it; service account impersonation finds who may mint the
// account's tokens; and the documented argument errors.
func TestCloudAssetAnalyzeIamPolicyOptions(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	ctx := h.Context()
	f := newAssetFixture(t, h, rmEndpoint, "aio-")
	c := assetClient(t, h, rmEndpoint)
	analyze := func(q *assetpb.IamPolicyAnalysisQuery) *assetpb.AnalyzeIamPolicyResponse {
		t.Helper()
		resp, err := c.AnalyzeIamPolicy(ctx, &assetpb.AnalyzeIamPolicyRequest{AnalysisQuery: q})
		if err != nil {
			t.Fatalf("AnalyzeIamPolicy %v: %v", q, err)
		}
		return resp
	}
	proj := "//cloudresourcemanager.googleapis.com/projects/" + f.project
	folderFull := "//cloudresourcemanager.googleapis.com/" + f.folder
	saFull := "//iam.googleapis.com/projects/" + f.project + "/serviceAccounts/" + f.sa

	resp := analyze(&assetpb.IamPolicyAnalysisQuery{Scope: "projects/" + f.project,
		AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Roles: []string{"roles/secretmanager.secretAccessor"}}})
	if len(resp.GetMainAnalysis().GetAnalysisResults()) != 0 {
		t.Errorf("a project scope read its folder's policy: %v", resp.GetMainAnalysis().GetAnalysisResults())
	}

	resp = analyze(&assetpb.IamPolicyAnalysisQuery{Scope: f.folder,
		AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Permissions: []string{"secretmanager.versions.access"}},
		Options:        &assetpb.IamPolicyAnalysisQuery_Options{ExpandGroups: true, ExpandResources: true, OutputGroupEdges: true, OutputResourceEdges: true}})
	r := assetResult(resp, "roles/secretmanager.secretAccessor")
	if r == nil || r.GetAttachedResourceFullName() != folderFull {
		t.Fatalf("no folder binding: %v", resp.GetMainAnalysis())
	}
	ids := map[string]bool{}
	for _, id := range r.GetIdentityList().GetIdentities() {
		ids[id.GetName()] = true
	}
	if !ids["group:"+f.group] || !ids["group:"+f.inner] || !ids["user:"+f.member] {
		t.Errorf("expandGroups identities = %v", ids)
	}
	edges := map[string]bool{}
	for _, e := range r.GetIdentityList().GetGroupEdges() {
		edges[e.GetSourceNode()+">"+e.GetTargetNode()] = true
	}
	if !edges["group:"+f.group+">group:"+f.inner] || !edges["group:"+f.inner+">user:"+f.member] {
		t.Errorf("group edges = %v", edges)
	}
	res := map[string]bool{}
	for _, x := range r.GetAccessControlLists()[0].GetResources() {
		res[x.GetFullResourceName()] = true
	}
	if !res[folderFull] || !res[proj] || !res[saFull] {
		t.Errorf("expandResources resources = %v", res)
	}
	redges := map[string]bool{}
	for _, e := range r.GetAccessControlLists()[0].GetResourceEdges() {
		redges[e.GetSourceNode()+">"+e.GetTargetNode()] = true
	}
	if !redges[folderFull+">"+proj] || !redges[proj+">"+saFull] {
		t.Errorf("resource edges = %v", redges)
	}

	// The member, through nested groups and through a custom role.
	resp = analyze(&assetpb.IamPolicyAnalysisQuery{Scope: f.folder,
		IdentitySelector: &assetpb.IamPolicyAnalysisQuery_IdentitySelector{Identity: "user:" + f.member},
		Options:          &assetpb.IamPolicyAnalysisQuery_Options{OutputGroupEdges: true, ExpandRoles: true}})
	if r := assetResult(resp, "roles/secretmanager.secretAccessor"); r == nil || len(r.GetIdentityList().GetGroupEdges()) != 2 ||
		r.GetIdentityList().GetIdentities()[0].GetName() != "user:"+f.member {
		t.Errorf("identity through nested groups = %v", r)
	}
	if r := assetResult(resp, f.custom); r == nil || len(r.GetAccessControlLists()[0].GetAccesses()) != 1 ||
		r.GetAccessControlLists()[0].GetAccesses()[0].GetPermission() != "secretmanager.secrets.get" {
		t.Errorf("custom role with expandRoles = %v", r)
	}

	// Conditions over conditionContext.accessTime.
	cond := func(at *timestamppb.Timestamp) *assetpb.IamPolicyAnalysisResult {
		t.Helper()
		q := &assetpb.IamPolicyAnalysisQuery{Scope: "projects/" + f.project,
			AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Roles: []string{"roles/secretmanager.admin"}}}
		if at != nil {
			q.ConditionContext = &assetpb.IamPolicyAnalysisQuery_ConditionContext{TimeContext: &assetpb.IamPolicyAnalysisQuery_ConditionContext_AccessTime{AccessTime: at}}
		}
		return assetResult(analyze(q), "roles/secretmanager.admin")
	}
	if r := cond(nil); r == nil || r.GetAccessControlLists()[0].GetConditionEvaluation().GetEvaluationValue() != assetpb.ConditionEvaluation_CONDITIONAL {
		t.Errorf("without an access time = %v", r)
	}
	if r := cond(timestamppb.New(time.Date(2099, 2, 1, 0, 0, 0, 0, time.UTC))); r == nil ||
		r.GetAccessControlLists()[0].GetConditionEvaluation().GetEvaluationValue() != assetpb.ConditionEvaluation_TRUE {
		t.Errorf("at 2099 = %v", r)
	}
	if r := cond(timestamppb.New(time.Date(2101, 2, 1, 0, 0, 0, 0, time.UTC))); r != nil {
		t.Errorf("a false condition is included: %v", r)
	}

	// Service account impersonation: the account's viewer access, and bob
	// who may mint its tokens.
	resp = analyze(&assetpb.IamPolicyAnalysisQuery{Scope: "projects/" + f.project,
		AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Permissions: []string{"resourcemanager.projects.get"}},
		Options:        &assetpb.IamPolicyAnalysisQuery_Options{AnalyzeServiceAccountImpersonation: true}})
	found := false
	for _, an := range resp.GetServiceAccountImpersonationAnalysis() {
		for _, r := range an.GetAnalysisResults() {
			for _, id := range r.GetIdentityList().GetIdentities() {
				found = found || strings.HasPrefix(id.GetName(), "user:bob-")
			}
		}
	}
	if !found {
		t.Errorf("impersonation analysis = %v", resp.GetServiceAccountImpersonationAnalysis())
	}

	// The documented argument errors.
	for name, q := range map[string]*assetpb.IamPolicyAnalysisQuery{
		"no selector": {Scope: "projects/" + f.project},
		"expandGroups with an identity": {Scope: "projects/" + f.project,
			IdentitySelector: &assetpb.IamPolicyAnalysisQuery_IdentitySelector{Identity: "user:" + f.member},
			Options:          &assetpb.IamPolicyAnalysisQuery_Options{ExpandGroups: true}},
		"expandRoles with an access selector": {Scope: "projects/" + f.project,
			AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Roles: []string{"roles/viewer"}},
			Options:        &assetpb.IamPolicyAnalysisQuery_Options{ExpandRoles: true}},
		"expandResources on a folder": {Scope: f.folder,
			ResourceSelector: &assetpb.IamPolicyAnalysisQuery_ResourceSelector{FullResourceName: folderFull},
			Options:          &assetpb.IamPolicyAnalysisQuery_Options{ExpandResources: true}},
		"a past access time": {Scope: "projects/" + f.project,
			AccessSelector:   &assetpb.IamPolicyAnalysisQuery_AccessSelector{Roles: []string{"roles/viewer"}},
			ConditionContext: &assetpb.IamPolicyAnalysisQuery_ConditionContext{TimeContext: &assetpb.IamPolicyAnalysisQuery_ConditionContext_AccessTime{AccessTime: timestamppb.New(time.Now().Add(-time.Hour))}}},
	} {
		if _, err := c.AnalyzeIamPolicy(ctx, &assetpb.AnalyzeIamPolicyRequest{AnalysisQuery: q}); !isCode(err, 400) {
			t.Errorf("%s: err = %v; want INVALID_ARGUMENT", name, err)
		}
	}
}

// TestCloudAssetSavedQueries: create (by project ID, named by number),
// get, list with a filter, run through savedAnalysisQuery, update by mask
// and delete, and the documented errors.
func TestCloudAssetSavedQueries(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	ctx := h.Context()
	f := newAssetFixture(t, h, rmEndpoint, "asq-")
	c := assetClient(t, h, rmEndpoint)
	sq, err := c.CreateSavedQuery(ctx, &assetpb.CreateSavedQueryRequest{Parent: "projects/" + f.project, SavedQueryId: "viewers",
		SavedQuery: &assetpb.SavedQuery{Description: "who views", Labels: map[string]string{"team": "sec"},
			Content: &assetpb.SavedQuery_QueryContent{QueryContent: &assetpb.SavedQuery_QueryContent_IamPolicyAnalysisQuery{
				IamPolicyAnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: "projects/" + f.project,
					AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Roles: []string{"roles/viewer"}}}}}}})
	if err != nil {
		t.Fatalf("CreateSavedQuery: %v", err)
	}
	if strings.Contains(sq.GetName(), f.project) || !strings.HasSuffix(sq.GetName(), "/savedQueries/viewers") || sq.GetCreateTime() == nil {
		t.Errorf("saved query = %v; want projects/{number}/savedQueries/viewers", sq)
	}
	if _, err := c.CreateSavedQuery(ctx, &assetpb.CreateSavedQueryRequest{Parent: "projects/" + f.project, SavedQueryId: "viewers",
		SavedQuery: &assetpb.SavedQuery{}}); !isCode(err, 409) {
		t.Errorf("duplicate: err = %v; want ALREADY_EXISTS", err)
	}
	if _, err := c.CreateSavedQuery(ctx, &assetpb.CreateSavedQueryRequest{Parent: "projects/" + f.project, SavedQueryId: "No",
		SavedQuery: &assetpb.SavedQuery{}}); !isCode(err, 400) {
		t.Errorf("bad id: err = %v; want INVALID_ARGUMENT", err)
	}
	got, err := c.GetSavedQuery(ctx, &assetpb.GetSavedQueryRequest{Name: sq.GetName()})
	if err != nil || got.GetDescription() != "who views" {
		t.Errorf("GetSavedQuery = %v, %v", got, err)
	}
	it := c.ListSavedQueries(ctx, &assetpb.ListSavedQueriesRequest{Parent: "projects/" + f.project, Filter: "labels.team=sec"})
	n := 0
	for {
		x, err := it.Next()
		if err != nil {
			break
		}
		n++
		if x.GetName() != sq.GetName() {
			t.Errorf("listed %s", x.GetName())
		}
	}
	if n != 1 {
		t.Errorf("ListSavedQueries listed %d; want 1", n)
	}
	resp, err := c.AnalyzeIamPolicy(ctx, &assetpb.AnalyzeIamPolicyRequest{AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: "projects/" + f.project},
		SavedAnalysisQuery: sq.GetName()})
	if err != nil || assetResult(resp, "roles/viewer") == nil || len(resp.GetMainAnalysis().GetAnalysisResults()) != 1 {
		t.Errorf("analysis through the saved query = %v, %v", resp, err)
	}
	up, err := c.UpdateSavedQuery(ctx, &assetpb.UpdateSavedQueryRequest{SavedQuery: &assetpb.SavedQuery{Name: sq.GetName(), Description: "changed"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil || up.GetDescription() != "changed" || len(up.GetLabels()) != 1 {
		t.Errorf("UpdateSavedQuery = %v, %v", up, err)
	}
	if err := c.DeleteSavedQuery(ctx, &assetpb.DeleteSavedQueryRequest{Name: sq.GetName()}); err != nil {
		t.Errorf("DeleteSavedQuery: %v", err)
	}
	if _, err := c.GetSavedQuery(ctx, &assetpb.GetSavedQueryRequest{Name: sq.GetName()}); !isCode(err, 404) {
		t.Errorf("get after delete: err = %v; want NOT_FOUND", err)
	}
}

// TestCloudAssetAnalyzeIamPolicyLongrunningStorage: a bucket's own policy is
// analyzed under its project's folder; the analysis is written
// to the instance's Cloud Storage as an AnalyzeIamPolicyResponse, and the
// operation completes with AnalyzeIamPolicyLongrunningResponse.
func TestCloudAssetAnalyzeIamPolicyLongrunningStorage(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvMetadata)
	ctx := h.Context()
	f := newAssetFixture(t, h, rmEndpoint, "alg-")
	sc := instanceStorage(t, h)
	bucket := strings.ToLower(f.project) + "-analysis"
	if err := sc.Bucket(bucket).Create(ctx, f.project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.Bucket(bucket).Object("out.json").Delete(context.Background())
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	c := assetClient(t, h, rmEndpoint)
	// The project's buckets are under the scope: a bucket's own binding is
	// found from the folder.
	bh := sc.Bucket(bucket).IAM()
	bp, err := bh.Policy(ctx)
	if err != nil {
		t.Fatalf("bucket policy: %v", err)
	}
	bp.Add("user:"+f.member, "roles/storage.objectViewer")
	if err := bh.SetPolicy(ctx, bp); err != nil {
		t.Fatalf("set the bucket policy: %v", err)
	}
	resp, err := c.AnalyzeIamPolicy(ctx, &assetpb.AnalyzeIamPolicyRequest{AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: f.folder,
		IdentitySelector: &assetpb.IamPolicyAnalysisQuery_IdentitySelector{Identity: "user:" + f.member}}})
	if err != nil {
		t.Fatalf("AnalyzeIamPolicy: %v", err)
	}
	if r := assetResult(resp, "roles/storage.objectViewer"); r == nil || r.GetAttachedResourceFullName() != "//storage.googleapis.com/projects/_/buckets/"+bucket {
		t.Errorf("the bucket's binding = %v", r)
	}
	op, err := c.AnalyzeIamPolicyLongrunning(ctx, &assetpb.AnalyzeIamPolicyLongrunningRequest{
		AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: f.folder,
			AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Permissions: []string{"secretmanager.versions.access"}}},
		OutputConfig: &assetpb.IamPolicyAnalysisOutputConfig{Destination: &assetpb.IamPolicyAnalysisOutputConfig_GcsDestination_{
			GcsDestination: &assetpb.IamPolicyAnalysisOutputConfig_GcsDestination{Uri: "gs://" + bucket + "/out.json"}}}})
	if err != nil {
		t.Fatalf("AnalyzeIamPolicyLongrunning: %v", err)
	}
	if !strings.Contains(op.Name(), "/operations/AnalyzeIamPolicyLongrunning/") {
		t.Errorf("operation name = %q", op.Name())
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("operation: %v", err)
	}
	rd, err := sc.Bucket(bucket).Object("out.json").NewReader(ctx)
	if err != nil {
		t.Fatalf("read the result: %v", err)
	}
	b, _ := io.ReadAll(rd)
	_ = rd.Close()
	var out struct {
		MainAnalysis struct {
			AnalysisResults []struct {
				AttachedResourceFullName string
			}
		}
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.MainAnalysis.AnalysisResults) == 0 ||
		out.MainAnalysis.AnalysisResults[0].AttachedResourceFullName != "//cloudresourcemanager.googleapis.com/"+f.folder {
		t.Errorf("result object = %s (%v)", b, err)
	}
	// The output config is required, and the URI must be gs://bucket/object.
	for name, oc := range map[string]*assetpb.IamPolicyAnalysisOutputConfig{
		"no output": nil,
		"bad uri": {Destination: &assetpb.IamPolicyAnalysisOutputConfig_GcsDestination_{
			GcsDestination: &assetpb.IamPolicyAnalysisOutputConfig_GcsDestination{Uri: "gs://" + bucket}}},
	} {
		if _, err := c.AnalyzeIamPolicyLongrunning(ctx, &assetpb.AnalyzeIamPolicyLongrunningRequest{AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{
			Scope: f.folder, AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Roles: []string{"roles/viewer"}}}, OutputConfig: oc}); !isCode(err, 400) {
			t.Errorf("%s: err = %v; want INVALID_ARGUMENT", name, err)
		}
	}
}

// TestCloudAssetAnalyzeIamPolicyLongrunningBigQuery: the analysis is written
// to {prefix}_analysis and {prefix}_analysis_result in the instance's
// BigQuery, partitioned by requestTime; WRITE_EMPTY then fails on the
// filled tables, and a missing dataset is INVALID_ARGUMENT.
func TestCloudAssetAnalyzeIamPolicyLongrunningBigQuery(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvMetadata)
	bq, bqProject := bigqueryClient(t, h)
	ctx := h.Context()
	f := newAssetFixture(t, h, rmEndpoint, "alb-")
	ds := "asset_" + strings.ReplaceAll(strings.ToLower(f.project), "-", "_")
	if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = bq.Dataset(ds).DeleteWithContents(context.Background()) })
	c := assetClient(t, h, rmEndpoint)
	run := func(disposition string) error {
		t.Helper()
		op, err := c.AnalyzeIamPolicyLongrunning(ctx, &assetpb.AnalyzeIamPolicyLongrunningRequest{
			AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: f.folder,
				AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Permissions: []string{"secretmanager.versions.access"}},
				Options:        &assetpb.IamPolicyAnalysisQuery_Options{ExpandGroups: true}},
			OutputConfig: &assetpb.IamPolicyAnalysisOutputConfig{Destination: &assetpb.IamPolicyAnalysisOutputConfig_BigqueryDestination{
				BigqueryDestination: &assetpb.IamPolicyAnalysisOutputConfig_BigQueryDestination{Dataset: "projects/" + bqProject + "/datasets/" + ds,
					TablePrefix: "iam", PartitionKey: assetpb.IamPolicyAnalysisOutputConfig_BigQueryDestination_REQUEST_TIME,
					WriteDisposition: disposition}}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	if err := run(""); err != nil {
		t.Fatalf("AnalyzeIamPolicyLongrunning: %v", err)
	}
	md, err := bq.Dataset(ds).Table("iam_analysis_result").Metadata(ctx)
	if err != nil {
		t.Fatalf("iam_analysis_result: %v", err)
	}
	if md.TimePartitioning == nil || md.TimePartitioning.Field != "requestTime" {
		t.Errorf("partitioning = %+v; want requestTime", md.TimePartitioning)
	}
	rows := bqRows(t, bq, "SELECT r.analysisResult.attachedResourceFullName AS attached, id.name AS name FROM `"+bqProject+"."+ds+
		".iam_analysis_result` r, UNNEST(r.analysisResult.identityList.identities) id")
	names := map[string]bool{}
	for _, r := range rows {
		if r["attached"] == "//cloudresourcemanager.googleapis.com/"+f.folder {
			names[r["name"].(string)] = true
		}
	}
	if !names["user:"+f.member] {
		t.Errorf("identities = %v", names)
	}
	if n := bqRows(t, bq, "SELECT opName FROM `"+bqProject+"."+ds+".iam_analysis`"); len(n) != 1 ||
		!strings.Contains(n[0]["opName"].(string), "/operations/AnalyzeIamPolicyLongrunning/") {
		t.Errorf("iam_analysis rows = %v", n)
	}
	if err := run("WRITE_EMPTY"); err == nil {
		t.Errorf("WRITE_EMPTY on filled tables succeeded")
	}
	if err := run("WRITE_TRUNCATE"); err != nil {
		t.Errorf("WRITE_TRUNCATE: %v", err)
	}
	if n := bqRows(t, bq, "SELECT opName FROM `"+bqProject+"."+ds+".iam_analysis`"); len(n) != 1 {
		t.Errorf("after WRITE_TRUNCATE iam_analysis has %d rows; want 1", len(n))
	}
	_, err = c.AnalyzeIamPolicyLongrunning(ctx, &assetpb.AnalyzeIamPolicyLongrunningRequest{
		AnalysisQuery: &assetpb.IamPolicyAnalysisQuery{Scope: f.folder, AccessSelector: &assetpb.IamPolicyAnalysisQuery_AccessSelector{Roles: []string{"roles/viewer"}}},
		OutputConfig: &assetpb.IamPolicyAnalysisOutputConfig{Destination: &assetpb.IamPolicyAnalysisOutputConfig_BigqueryDestination{
			BigqueryDestination: &assetpb.IamPolicyAnalysisOutputConfig_BigQueryDestination{Dataset: "projects/" + bqProject + "/datasets/missing_ds", TablePrefix: "iam"}}}})
	if !isCode(err, 400) {
		t.Errorf("missing dataset: err = %v; want INVALID_ARGUMENT", err)
	}
}

// TestGcloudAssetAnalyzeIamPolicy: gcloud asset analyze-iam-policy, through
// gcloud-setup's cloudasset override, with --expand-groups and
// --output-group-edges, and gcloud asset saved-queries.
func TestGcloudAssetAnalyzeIamPolicy(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	f := newAssetFixture(t, h, rmEndpoint, "agc-")
	g := newGcloudSession(t, h)
	folderNum := strings.TrimPrefix(f.folder, "folders/")
	out := g.must("asset", "analyze-iam-policy", "--folder="+folderNum, "--permissions=secretmanager.versions.access",
		"--expand-groups", "--output-group-edges", "--show-response", "--format=json")
	if !strings.Contains(out, "user:"+f.member) || !strings.Contains(out, `"sourceNode": "group:`+f.inner+`"`) {
		t.Errorf("analyze-iam-policy --expand-groups = %s", out)
	}
	out = g.must("asset", "analyze-iam-policy", "--project="+f.project, "--identity=user:"+f.member, "--format=json")
	if !strings.Contains(out, f.custom) {
		t.Errorf("analyze-iam-policy --identity = %s", out)
	}
	g.must("asset", "saved-queries", "create", "gviewers", "--project="+f.project, "--description=who views",
		"--query-file-path="+writeTemp(t, `{"IamPolicyAnalysisQuery":{"scope":"projects/`+f.project+`","accessSelector":{"roles":["roles/viewer"]}}}`))
	if out := g.must("asset", "saved-queries", "list", "--project="+f.project, "--format=json"); !strings.Contains(out, "/savedQueries/gviewers") {
		t.Errorf("saved-queries list = %q", out)
	}
	g.must("asset", "saved-queries", "delete", "gviewers", "--project="+f.project)
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := t.TempDir() + "/query.json"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// bqRows are a query's rows by column.
func bqRows(t *testing.T, c *bigquery.Client, sql string) []map[string]bigquery.Value {
	t.Helper()
	it, err := c.Query(sql).Read(context.Background())
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var out []map[string]bigquery.Value
	for {
		row := map[string]bigquery.Value{}
		err := it.Next(&row)
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		out = append(out, row)
	}
}
