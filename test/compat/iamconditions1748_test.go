//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btadmin "cloud.google.com/go/bigtable/admin/apiv2"
	"cloud.google.com/go/iam/apiv1/iampb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// IAM Conditions on the services #1745 left unit-tested (#1748): Bigtable
// and BigQuery, which Google's "Services that allow conditions" lists, run
// through their official clients, and Memorystore's IAM auth evaluating a
// conditional project binding.

// TestBigtableIAMConditions (#1748): a Bigtable instance and table take
// conditional role bindings, through the generated instance and table
// admin clients, with the version 3 rules conditionalPolicy checks.
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/SetIamPolicy, google.bigtable.admin.v2.BigtableInstanceAdmin/GetIamPolicy, google.bigtable.admin.v2.BigtableTableAdmin/SetIamPolicy, google.bigtable.admin.v2.BigtableTableAdmin/GetIamPolicy
func TestBigtableIAMConditions(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h) // skips without CLOUDBURROW_TEST_BIGTABLE
	ctx := h.Context()
	const id = "cb-cond-1748"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Conditions",
		ClusterId: id + "-c1", Zone: "europe-west1-b", NumNodes: 1}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	if err := ac.CreateTable(ctx, "cond"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	conn, err := grpc.NewClient(h.Endpoint(EnvBigtable), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ic, err := btadmin.NewBigtableInstanceAdminClient(ctx, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	tc, err := btadmin.NewBigtableTableAdminClient(ctx, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	inst := "projects/" + h.Project() + "/instances/" + id
	conditionalPolicy(t, ctx, "instance", ic, inst, "roles/bigtable.user", `resource.name.startsWith("`+inst+`/tables/prod-")`)
	conditionalPolicy(t, ctx, "table", tc, inst+"/tables/cond", "roles/bigtable.reader", `request.time < timestamp("2099-01-01T00:00:00Z")`)
}

// TestBigQueryTableIAMConditions (#1748): a table takes a conditional role
// binding through Google's generated BigQuery v2 client: refused 400 in a
// version 1 policy, stored and read back with its condition at version 3,
// and ROLE_withcond_HASH without it at version 1, which a version 1 write
// back keeps.
// covers: bigquery.tables.getIamPolicy, bigquery.tables.setIamPolicy
func TestBigQueryTableIAMConditions(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ds, tbl := seedOrders(t, h, c)
	ctx := h.Context()
	res := "projects/" + project + "/datasets/" + ds.DatasetID + "/tables/" + tbl.TableID
	e := `request.time < timestamp("2099-01-01T00:00:00Z")`
	binding := []*bq.Binding{{Role: "roles/bigquery.dataViewer", Members: []string{"user:ada@example.com"},
		Condition: &bq.Expr{Title: "until 2099", Description: "#1748", Expression: e}}}
	_, err := svc.Tables.SetIamPolicy(res, &bq.SetIamPolicyRequest{Policy: &bq.Policy{Version: 1, Bindings: binding}}).Context(ctx).Do()
	wantCode(t, "a condition in a version 1 policy", err, http.StatusBadRequest, "")
	set, err := svc.Tables.SetIamPolicy(res, &bq.SetIamPolicyRequest{Policy: &bq.Policy{Version: 3, Bindings: binding}}).Context(ctx).Do()
	if err != nil || set.Version != 3 {
		t.Fatalf("SetIamPolicy version 3 = %+v, %v", set, err)
	}
	v3, err := svc.Tables.GetIamPolicy(res, &bq.GetIamPolicyRequest{Options: &bq.GetPolicyOptions{RequestedPolicyVersion: 3}}).Context(ctx).Do()
	if err != nil || v3.Version != 3 || len(v3.Bindings) != 1 || v3.Bindings[0].Condition == nil ||
		v3.Bindings[0].Condition.Title != "until 2099" || v3.Bindings[0].Condition.Description != "#1748" || v3.Bindings[0].Condition.Expression != e {
		t.Fatalf("GetIamPolicy version 3 = %+v, %v", v3, err)
	}
	v1, err := svc.Tables.GetIamPolicy(res, &bq.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil || len(v1.Bindings) != 1 || !strings.HasPrefix(v1.Bindings[0].Role, "roles/bigquery.dataViewer_withcond_") || v1.Bindings[0].Condition != nil {
		t.Fatalf("GetIamPolicy version 1 = %+v, %v; want ROLE_withcond_HASH without the condition", v1, err)
	}
	if _, err := svc.Tables.SetIamPolicy(res, &bq.SetIamPolicyRequest{Policy: v1}).Context(ctx).Do(); err != nil {
		t.Fatalf("a version 1 write back = %v", err)
	}
	again, err := svc.Tables.GetIamPolicy(res, &bq.GetIamPolicyRequest{Options: &bq.GetPolicyOptions{RequestedPolicyVersion: 3}}).Context(ctx).Do()
	if err != nil || len(again.Bindings) != 1 || again.Bindings[0].Condition == nil || again.Bindings[0].Condition.Expression != e {
		t.Errorf("after a version 1 write back = %+v, %v; want the condition kept", again, err)
	}
}

// setConditionalProjectRole replaces member's bindings on project with one
// conditional binding of role, through the official Resource Manager
// client at version 3; the cleanup removes it.
func setConditionalProjectRole(t *testing.T, h *Harness, project, member, role, expression string) {
	t.Helper()
	ctx := h.Context()
	rm, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(h.Endpoint(EnvResourceManager))...)
	if err != nil {
		t.Fatal(err)
	}
	defer rm.Close()
	change := func(add bool) error {
		pol, err := rm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project,
			Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			return err
		}
		var bindings []*iampb.Binding
		for _, b := range pol.GetBindings() {
			var ms []string
			for _, m := range b.GetMembers() {
				if m != member {
					ms = append(ms, m)
				}
			}
			if len(ms) > 0 {
				b.Members = ms
				bindings = append(bindings, b)
			}
		}
		if add {
			bindings = append(bindings, &iampb.Binding{Role: role, Members: []string{member},
				Condition: &expr.Expr{Title: "scoped", Expression: expression}})
		}
		_, err = rm.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + project,
			Policy: &iampb.Policy{Version: 3, Etag: pol.GetEtag(), Bindings: bindings}})
		return err
	}
	if err := change(true); err != nil {
		t.Fatalf("SetIamPolicy with %s: %v", expression, err)
	}
	t.Cleanup(func() { _ = change(false) })
}

// TestMemorystoreIAMConditions (#1748): a Valkey instance's IAM auth
// evaluates the principal's conditional project binding of the connect
// role: a request.time condition that has expired refuses its token, one
// that holds takes it, and a resource.name condition grants nothing,
// because Memorystore is not among the services Google's "Resource
// attributes for IAM Conditions" lists.
// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance
func TestMemorystoreIAMConditions(t *testing.T) {
	h := New(t)
	vc, project := valkeyAdminClient(t, h)
	user := newIAMPrincipal(t, h, project, "vk-cond")
	member := "serviceAccount:" + user.email
	parent := "projects/" + project + "/locations/us-central1"
	vid := fmt.Sprintf("compat-vkcond-%d", time.Now().UnixNano()%1e8)
	op, err := vc.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: vid, Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}},
		AuthorizationMode:  memorystorepb.Instance_IAM_AUTH,
	}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := vc.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: parent + "/instances/" + vid}); err == nil {
			wctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_ = waitLRODone(wctx, op)
		}
	})
	inst, err := waitLRO(memorystoreWait(t), op)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	addr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	ctx := h.Context()
	// await waits for the nodes to follow the policy, then checks the
	// token is taken (want true) or refused.
	await := func(what string, want bool) {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for (authErr(ctx, addr, "", user.token) == nil) != want && time.Now().Before(deadline) {
			time.Sleep(time.Second)
		}
		if err := authErr(ctx, addr, "", user.token); (err == nil) != want {
			t.Errorf("%s: AUTH = %v; want taken %v", what, err, want)
		}
	}
	setConditionalProjectRole(t, h, project, member, valkeyConnectRole, `request.time < timestamp("2000-01-01T00:00:00Z")`)
	await("an expired request.time condition", false)
	setConditionalProjectRole(t, h, project, member, valkeyConnectRole, `request.time < timestamp("2099-01-01T00:00:00Z")`)
	await("a request.time condition that holds", true)
	setConditionalProjectRole(t, h, project, member, valkeyConnectRole, `resource.name.startsWith("projects/")`)
	await("a resource.name condition", false)
}
