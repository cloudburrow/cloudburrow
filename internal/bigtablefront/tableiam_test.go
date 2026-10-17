package bigtablefront

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/iam"
	"google.golang.org/api/bigtableadmin/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Table IAM (#1486) through the official clients: the Go client's TableIAM
// handle over gRPC and the bigtableadmin REST client, as Terraform's
// google_bigtable_table_iam_* calls it. The policy goes with the table.
func TestTableIAM(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	h := ac.TableIAM("t")
	pol, err := h.Policy(ctx)
	if err != nil || len(pol.Roles()) != 0 {
		t.Fatalf("Policy = %v, %v", pol, err)
	}
	pol.Add("user:ada@example.com", iam.RoleName("roles/bigtable.reader"))
	if err := h.SetPolicy(ctx, pol); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	// The etag pol carries is now stale.
	if err := h.SetPolicy(ctx, pol); status.Code(err) != codes.Aborted {
		t.Errorf("a stale etag = %v, want Aborted", err)
	}
	if held, err := h.TestPermissions(ctx, []string{"bigtable.tables.readRows"}); err != nil || len(held) != 1 {
		t.Errorf("TestPermissions = %v, %v", held, err)
	}
	svc, err := bigtableadmin.NewService(ctx, option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	const table = "projects/p/instances/i/tables/t"
	got, err := svc.Projects.Instances.Tables.GetIamPolicy(table, &bigtableadmin.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "user:ada@example.com" {
		t.Errorf("REST GetIamPolicy = %+v, %v", got, err)
	}
	if _, err := ac.TableIAM("missing").Policy(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("a missing table = %v, want NotFound", err)
	}
	if _, err := ac.AuthorizedViewIAM("t", "v").Policy(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("a missing authorized view = %v, want NotFound", err)
	}
	if err := ac.DeleteTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if again, err := h.Policy(ctx); err != nil || len(again.Roles()) != 0 {
		t.Errorf("a table made again = %v, %v; want no bindings", again, err)
	}
}
