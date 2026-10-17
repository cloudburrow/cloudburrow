package resourcemanager

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv2 "google.golang.org/api/cloudresourcemanager/v2"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Project, folder and organization IAM (#1368): stored, never enforced,
// with internal/iampolicy's etags and refusals, over gRPC, v1, v2 and v3.
func TestProjectIAMThroughTheOfficialClients(t *testing.T) {
	reg, srv, c := startAPI(t)
	ctx := context.Background()
	if _, err := reg.Create(Project{ProjectID: "iam-project"}); err != nil {
		t.Fatal(err)
	}
	const res = "projects/iam-project"
	empty, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
	if err != nil || len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
		t.Fatalf("GetIamPolicy of a new project = %v, %v", empty, err)
	}
	set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{
		Etag: empty.GetEtag(), Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:ada@example.com"}}}}})
	if err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: empty.GetEtag()}}); status.Code(err) != codes.Aborted {
		t.Errorf("SetIamPolicy with a stale etag = %v, want Aborted", err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Version: 3,
		Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}, Condition: &expr.Expr{Expression: "true"}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a conditional binding = %v, want Unimplemented", err)
	}
	// Terraform's mask names auditConfigs on every set; with none, it is a no-op.
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: set.GetEtag(),
		Bindings: set.GetBindings()}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bindings", "etag", "auditConfigs"}}}); err != nil {
		t.Errorf("SetIamPolicy with Terraform's mask: %v", err)
	}
	// Audit configs are stored (#1486), and the mask without bindings keeps them.
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{
		AuditConfigs: []*iampb.AuditConfig{{Service: "allServices", AuditLogConfigs: []*iampb.AuditLogConfig{{LogType: iampb.AuditLogConfig_DATA_READ}}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"auditConfigs"}}}); err != nil {
		t.Errorf("audit configs: %v", err)
	}
	if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res}); err != nil || len(got.GetAuditConfigs()) != 1 ||
		got.GetAuditConfigs()[0].GetAuditLogConfigs()[0].GetLogType() != iampb.AuditLogConfig_DATA_READ {
		t.Errorf("audit configs read back = %v, %v", got, err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != "user:ada@example.com" {
		t.Errorf("GetIamPolicy after a set = %v, %v", got, err)
	}
	tp, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: res, Permissions: []string{
		"storage.buckets.create", "resourcemanager.projects.get", "resourcemanager.folders.get", "not a permission"}})
	if err != nil || len(tp.GetPermissions()) != 2 || tp.GetPermissions()[0] != "storage.buckets.create" {
		t.Errorf("TestIamPermissions = %v, %v; want the two a project can hold", tp, err)
	}

	// v1, which gcloud projects and Terraform call: the same policy.
	v1, err := crmv1.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	p1, err := v1.Projects.GetIamPolicy("iam-project", &crmv1.GetIamPolicyRequest{
		Options: &crmv1.GetPolicyOptions{RequestedPolicyVersion: 3}}).Context(ctx).Do()
	if err != nil || len(p1.Bindings) != 1 || p1.Bindings[0].Role != "roles/viewer" {
		t.Fatalf("v1 getIamPolicy = %+v, %v", p1, err)
	}
	p1.Bindings = append(p1.Bindings, &crmv1.Binding{Role: "roles/editor", Members: []string{"group:ops@example.com"}})
	if _, err := v1.Projects.SetIamPolicy("iam-project", &crmv1.SetIamPolicyRequest{Policy: p1,
		UpdateMask: "bindings,etag,auditConfigs"}).Context(ctx).Do(); err != nil {
		t.Fatalf("v1 setIamPolicy: %v", err)
	}
	if _, err := v1.Projects.SetIamPolicy("iam-project", &crmv1.SetIamPolicyRequest{Policy: p1}).Context(ctx).Do(); code(err) != http.StatusConflict {
		t.Errorf("v1 setIamPolicy with a stale etag = %v, want 409", err)
	}
	if r, err := v1.Projects.TestIamPermissions("iam-project", &crmv1.TestIamPermissionsRequest{
		Permissions: []string{"pubsub.topics.create"}}).Context(ctx).Do(); err != nil || len(r.Permissions) != 1 {
		t.Errorf("v1 testIamPermissions = %+v, %v", r, err)
	}
	if got, _ := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res}); len(got.GetBindings()) != 2 {
		t.Errorf("v3 after a v1 set = %v; want both bindings", got)
	}
	// By number, as v3 names a project too.
	if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + projectNumberString("iam-project")}); err != nil || len(got.GetBindings()) != 2 {
		t.Errorf("GetIamPolicy by number = %v, %v", got, err)
	}

	// A deleted project keeps its policy until it is purged, and refuses a set.
	now := time.Now()
	reg.now = func() time.Time { return now }
	if _, err := reg.Delete("iam-project"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res}); err != nil || len(got.GetBindings()) != 2 {
		t.Errorf("GetIamPolicy of a deleted project = %v, %v", got, err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SetIamPolicy of a deleted project = %v, want FailedPrecondition", err)
	}
	now = now.Add(RetentionPeriod + time.Hour)
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy of a purged project = %v, want NotFound", err)
	}
	if _, err := reg.Create(Project{ProjectID: "iam-project"}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res}); err != nil || len(got.GetBindings()) != 0 {
		t.Errorf("a project made again under a purged ID has policy %v, %v; want none", got, err)
	}
	if keys, _ := reg.st.List(iamPrefix); len(keys) != 0 {
		t.Errorf("stored policies after the purge = %v", keys)
	}
}

func projectNumberString(id string) string { return strconv.FormatInt(projectNumber(id), 10) }

// Folders over v3 and v2, the organization over v3 and v1; ResetIAM clears
// them all, and a project's alone with a project.
func TestFolderAndOrganizationIAM(t *testing.T) {
	reg, srv, _ := startAPI(t)
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	fc, err := resourcemanager.NewFoldersClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()
	oc, err := resourcemanager.NewOrganizationsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer oc.Close()
	f, err := reg.CreateFolder(OrganizationName, "Team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(Project{ProjectID: "reset-project"}); err != nil {
		t.Fatal(err)
	}
	b := []*iampb.Binding{{Role: "roles/browser", Members: []string{"user:ada@example.com"}}}
	if _, err := fc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: f.Name, Policy: &iampb.Policy{Bindings: b}}); err != nil {
		t.Fatalf("folder SetIamPolicy: %v", err)
	}
	if _, err := oc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: OrganizationName, Policy: &iampb.Policy{Bindings: b}}); err != nil {
		t.Fatalf("organization SetIamPolicy: %v", err)
	}
	if _, err := reg.SetIAMPolicy("projects/reset-project", &iampb.SetIamPolicyRequest{Policy: &iampb.Policy{Bindings: b}}); err != nil {
		t.Fatal(err)
	}
	if _, err := fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "folders/999"}); status.Code(err) != codes.NotFound {
		t.Errorf("an absent folder's policy = %v, want NotFound", err)
	}
	if r, err := fc.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: f.Name,
		Permissions: []string{"resourcemanager.folders.get", "resourcemanager.organizations.get"}}); err != nil || len(r.GetPermissions()) != 1 {
		t.Errorf("folder TestIamPermissions = %v, %v", r, err)
	}
	if r, err := oc.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: OrganizationName,
		Permissions: []string{"resourcemanager.organizations.get"}}); err != nil || len(r.GetPermissions()) != 1 {
		t.Errorf("organization TestIamPermissions = %v, %v", r, err)
	}

	v2, err := crmv2.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if p, err := v2.Folders.GetIamPolicy(f.Name, &crmv2.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil || len(p.Bindings) != 1 {
		t.Errorf("v2 folders getIamPolicy = %+v, %v", p, err)
	}
	v1, err := crmv1.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if p, err := v1.Organizations.GetIamPolicy(OrganizationName, &crmv1.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil || len(p.Bindings) != 1 {
		t.Errorf("v1 organizations getIamPolicy = %+v, %v", p, err)
	}

	if err := reg.ResetIAM("reset-project"); err != nil {
		t.Fatal(err)
	}
	if p, _ := reg.IAMPolicy("projects/reset-project"); p != nil {
		t.Errorf("after a project reset its policy is %+v", p)
	}
	if p, _ := reg.IAMPolicy(f.Name); p == nil {
		t.Error("a project reset cleared the folder's policy")
	}
	if err := reg.ResetIAM(""); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{f.Name, OrganizationName} {
		if p, _ := reg.IAMPolicy(r); p != nil {
			t.Errorf("after a reset %s's policy is %+v", r, p)
		}
	}
	if _, err := reg.Folder(f.Name); err != nil {
		t.Errorf("a reset removed the folder: %v", err)
	}
}
