package spannerfront_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Instance and database IAM (#1486): stored, a stale etag ABORTED, a
// condition UNIMPLEMENTED, a missing resource NOT_FOUND, and the policy gone
// with the instance.
func TestIAMPoliciesAreStoredOnInstancesAndDatabases(t *testing.T) {
	addr, c := serve(t)
	ctx := context.Background()
	db := databasepb.NewDatabaseAdminClient(lastConn)
	inst, dbName := "projects/p/instances/i", "projects/p/instances/i/databases/d"
	for res, api := range map[string]struct {
		get func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error)
		set func(context.Context, *iampb.SetIamPolicyRequest) (*iampb.Policy, error)
	}{
		inst: {func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			return c.GetIamPolicy(ctx, r)
		},
			func(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
				return c.SetIamPolicy(ctx, r)
			}},
		dbName: {func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			return db.GetIamPolicy(ctx, r)
		},
			func(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
				return db.SetIamPolicy(ctx, r)
			}},
	} {
		empty, err := api.get(ctx, &iampb.GetIamPolicyRequest{Resource: res})
		if err != nil || len(empty.GetBindings()) != 0 {
			t.Fatalf("%s: empty = %v, %v", res, empty, err)
		}
		set, err := api.set(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: empty.GetEtag(),
			Bindings: []*iampb.Binding{{Role: "roles/spanner.databaseReader", Members: []string{"user:ada@example.com"}}}}})
		if err != nil || len(set.GetBindings()) != 1 {
			t.Fatalf("%s: set = %v, %v", res, set, err)
		}
		if _, err := api.set(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: empty.GetEtag()}}); status.Code(err) != codes.Aborted {
			t.Errorf("%s: stale etag = %v, want Aborted", res, err)
		}
		if _, err := api.set(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Version: 3, Bindings: []*iampb.Binding{{Role: "roles/viewer",
			Members: []string{"user:a@example.com"}, Condition: &expr.Expr{Expression: "true"}}}}}); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s: condition = %v, want Unimplemented", res, err)
		}
		if got, err := api.get(ctx, &iampb.GetIamPolicyRequest{Resource: res}); err != nil || len(got.GetBindings()) != 1 {
			t.Errorf("%s: read back = %v, %v", res, got, err)
		}
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/p/instances/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing instance = %v, want NotFound", err)
	}
	if _, err := db.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/p/instances/i/backups/b"}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing backup = %v, want NotFound", err)
	}
	perms, err := db.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: dbName, Permissions: []string{"spanner.databases.read"}})
	if err != nil || len(perms.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions = %v, %v", perms, err)
	}

	// REST, as gcloud and Terraform call it.
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/"+dbName+":getIamPolicy", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Bindings []struct{ Role string } }
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(got.Bindings) != 1 {
		t.Errorf("REST getIamPolicy = %d %+v", resp.StatusCode, got)
	}

	if _, err := c.DeleteInstance(ctx, &instancepb.DeleteInstanceRequest{Name: inst}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: inst}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted instance = %v, want NotFound", err)
	}
}
