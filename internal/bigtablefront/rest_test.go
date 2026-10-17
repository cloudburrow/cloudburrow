package bigtablefront

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"google.golang.org/api/bigtableadmin/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestRESTAdmin drives the admin REST API (#1440) through the official
// bigtableadmin REST client, as Terraform's google_bigtable_app_profile and
// google_bigtable_instance_iam_* do, and reads the app profile back through
// the official gRPC instance admin client: both reach the same state.
func TestRESTAdmin(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "rest-inst", DisplayName: "REST",
		ClusterId: "rest-inst-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	svc, err := bigtableadmin.NewService(ctx, option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	const inst = "projects/p/instances/rest-inst"

	p, err := svc.Projects.Instances.AppProfiles.Create(inst, &bigtableadmin.AppProfile{Description: "from REST",
		SingleClusterRouting: &bigtableadmin.SingleClusterRouting{ClusterId: "rest-inst-c1", AllowTransactionalWrites: true}}).
		AppProfileId("rest-profile").IgnoreWarnings(true).Context(ctx).Do()
	if err != nil {
		t.Fatalf("AppProfiles.Create: %v", err)
	}
	if p.Name != inst+"/appProfiles/rest-profile" || p.Etag == "" || p.SingleClusterRouting.ClusterId != "rest-inst-c1" {
		t.Errorf("created %+v", p)
	}
	list, err := svc.Projects.Instances.AppProfiles.List(inst).Context(ctx).Do()
	if err != nil || len(list.AppProfiles) != 2 { // default and rest-profile
		t.Fatalf("AppProfiles.List = %v, %v", list, err)
	}
	op, err := svc.Projects.Instances.AppProfiles.Patch(p.Name, &bigtableadmin.AppProfile{Description: "patched",
		MultiClusterRoutingUseAny: &bigtableadmin.MultiClusterRoutingUseAny{}}).
		UpdateMask("description,multiClusterRoutingUseAny").IgnoreWarnings(true).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("AppProfiles.Patch = %+v, %v", op, err)
	}
	if got, err := svc.Operations.Get(op.Name).Context(ctx).Do(); err != nil || !got.Done {
		t.Errorf("Operations.Get(%s) = %+v, %v", op.Name, got, err)
	}
	ap, err := iac.GetAppProfile(ctx, "rest-inst", "rest-profile")
	if err != nil {
		t.Fatalf("GetAppProfile over gRPC: %v", err)
	}
	if ap.Description != "patched" || ap.GetMultiClusterRoutingUseAny() == nil {
		t.Errorf("over gRPC the patched profile is %v", ap)
	}

	pol, err := svc.Projects.Instances.SetIamPolicy(inst, &bigtableadmin.SetIamPolicyRequest{Policy: &bigtableadmin.Policy{
		Bindings: []*bigtableadmin.Binding{{Role: "roles/bigtable.user", Members: []string{"user:a@example.com"}}}}}).Context(ctx).Do()
	if err != nil || pol.Etag == "" {
		t.Fatalf("SetIamPolicy = %+v, %v", pol, err)
	}
	got, err := svc.Projects.Instances.GetIamPolicy(inst, &bigtableadmin.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "user:a@example.com" {
		t.Fatalf("GetIamPolicy = %+v, %v", got, err)
	}
	h, err := iac.InstanceIAM("rest-inst").Policy(ctx)
	if err != nil || !h.HasRole("user:a@example.com", "roles/bigtable.user") {
		t.Errorf("over gRPC the policy is %v, %v", h, err)
	}

	if _, err := svc.Projects.Instances.AppProfiles.Delete(p.Name).IgnoreWarnings(true).Context(ctx).Do(); err != nil {
		t.Fatalf("AppProfiles.Delete: %v", err)
	}
	if _, err := svc.Projects.Instances.AppProfiles.Get(p.Name).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("Get after delete: %v, want 404", err)
	}

	// Table admin calls go to the emulator; the Refused ones answer 501.
	if _, err := svc.Projects.Instances.Tables.Create(inst, &bigtableadmin.CreateTableRequest{TableId: "rest-table",
		Table: &bigtableadmin.Table{ColumnFamilies: map[string]bigtableadmin.ColumnFamily{"cf": {}}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("Tables.Create: %v", err)
	}
	if tb, err := svc.Projects.Instances.Tables.Get(inst + "/tables/rest-table").Context(ctx).Do(); err != nil || len(tb.ColumnFamilies) != 1 {
		t.Errorf("Tables.Get = %+v, %v", tb, err)
	}
	if l, err := svc.Projects.Instances.Clusters.Backups.List(inst + "/clusters/rest-inst-c1").Context(ctx).Do(); err != nil || len(l.Backups) != 0 {
		t.Errorf("Backups.List = %+v, %v; want none", l, err)
	}
	if _, err := svc.Projects.Instances.Tables.Undelete(inst+"/tables/rest-table", &bigtableadmin.UndeleteTableRequest{}).Context(ctx).Do(); httpCode(err) != http.StatusNotImplemented {
		t.Errorf("Tables.Undelete: %v, want 501", err)
	}
	resp, err := http.Get("http://" + addr + "/v2/projects/p/nowhere")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unbound path answered %d, want 404", resp.StatusCode)
	}
}

func httpCode(err error) int {
	var e *googleapi.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}
