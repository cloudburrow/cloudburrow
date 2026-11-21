//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/firestore"
	fsadmin "cloud.google.com/go/firestore/apiv1/admin"
	"cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// pastGRPCDenial fails when err is PERMISSION_DENIED: the call passed its
// IAM checks (it may still fail for another reason).
func pastGRPCDenial(t *testing.T, what string, err error) {
	t.Helper()
	if status.Code(err) == codes.PermissionDenied {
		t.Fatalf("%s = %v; want past its IAM checks", what, err)
	}
}

// waitPast retries call until it is no longer PERMISSION_DENIED.
func waitPast(t *testing.T, what string, call func() error) {
	t.Helper()
	waitGranted(t, what, func() error {
		if err := call(); status.Code(err) == codes.PermissionDenied {
			return err
		}
		return nil
	})
}

// covers: google.firestore.v1.Firestore/GetDocument, google.firestore.v1.Firestore/Commit, google.firestore.v1.Firestore/RunQuery, google.firestore.admin.v1.FirestoreAdmin/ListIndexes
//
// TestIAMEnforcementFirestore (#1879), with the official Firestore and
// Firestore Admin clients over gRPC and an access token minted through
// IAM Credentials: as Firestore's "Required permissions for API methods"
// lists them, a document get needs datastore.entities.get, a query
// datastore.entities.get and list, a set (an update with no precondition)
// datastore.entities.create and update, and the Admin API's index list
// datastore.indexes.list, each on the database, refused PERMISSION_DENIED
// "Missing or insufficient permissions." until Cloud Datastore Viewer,
// then User, is granted on the project; the instance's own account
// (no token, and the emulator's "owner" token) is never refused.
func TestIAMEnforcementFirestore(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvFirestore)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iefs9-")
	p := newIAMPrincipal(t, h, project, "iefs9")
	owner, err := firestore.NewClient(ctx, project, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.Collection("widgets").Doc("one").Set(ctx, map[string]any{"size": 3}); err != nil {
		t.Fatalf("the instance's own account: Set = %v", err)
	}
	as, err := firestore.NewClient(ctx, project, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	adm, err := fsadmin.NewFirestoreAdminClient(ctx, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	const msg = "Missing or insufficient permissions."
	doc := as.Collection("widgets").Doc("one")
	get := func() error { _, err := doc.Get(ctx); return err }
	query := func() error {
		_, err := as.Collection("widgets").Where("size", ">", 1).Documents(ctx).GetAll()
		return err
	}
	set := func() error {
		_, err := as.Collection("widgets").Doc("two").Set(ctx, map[string]any{"size": 9})
		return err
	}
	indexes := func() error {
		_, err := adm.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: "projects/" + project + "/databases/(default)/collectionGroups/widgets"}).Next()
		if err != nil && err.Error() == "no more items in iterator" {
			return nil
		}
		return err
	}
	wantDenied(t, "GetDocument without a role", get(), msg, "", false)
	wantDenied(t, "RunQuery without a role", query(), msg, "", false)
	wantDenied(t, "Commit without a role", set(), msg, "", false)
	wantDenied(t, "ListIndexes without a role", indexes(), msg, "", false)
	p.grant("roles/datastore.viewer")
	t.Cleanup(func() { p.revoke("roles/datastore.viewer") })
	waitGranted(t, "GetDocument as a Cloud Datastore Viewer", get)
	if err := query(); err != nil {
		t.Errorf("RunQuery as a Cloud Datastore Viewer: %v", err)
	}
	waitPast(t, "ListIndexes as a Cloud Datastore Viewer", indexes)
	wantDenied(t, "Commit as a Cloud Datastore Viewer", set(), msg, "", false)
	p.grant("roles/datastore.user")
	t.Cleanup(func() { p.revoke("roles/datastore.user") })
	waitGranted(t, "Commit as a Cloud Datastore User", set)
	// The Google client's emulator token, "owner", is the instance's own
	// account.
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	emu, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = emu.Close() })
	if _, err := emu.Collection("widgets").Doc("one").Get(ctx); err != nil {
		t.Errorf("FIRESTORE_EMULATOR_HOST's client: %v", err)
	}
}

// spannerAs is a Spanner data client on db calling with token, with the
// client's built-in metrics off (they export to Cloud Monitoring).
func spannerAs(t *testing.T, h *Harness, addr, db, token string) *spanner.Client {
	t.Helper()
	c, err := spanner.NewClientWithConfig(h.Context(), db, spanner.ClientConfig{DisableNativeMetrics: true}, grpcAs(addr, token)...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// covers: google.spanner.v1.Spanner/CreateSession, google.spanner.v1.Spanner/ExecuteStreamingSql, google.spanner.v1.Spanner/Commit, google.spanner.admin.database.v1.DatabaseAdmin/GetDatabaseDdl, google.spanner.admin.database.v1.DatabaseAdmin/SetIamPolicy
//
// TestIAMEnforcementSpanner (#1879), with the official Spanner clients over
// gRPC and a minted token: as each method's reference names, a query needs
// spanner.databases.select (and its session spanner.sessions.create), a
// write spanner.databases.write and GetDatabaseDdl spanner.databases.getDdl,
// on the database, refused PERMISSION_DENIED "Caller is missing IAM
// permission P on resource R." until Cloud Spanner Database Reader, then
// User, is granted on the database's own policy (its project's grants
// nothing); the instance's own account is never refused.
func TestIAMEnforcementSpanner(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	t.Setenv("SPANNER_DISABLE_BUILTIN_METRICS", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	project := iamProject(t, h, "iesp9-")
	p := newIAMPrincipal(t, h, project, "iesp9")
	ia, err := instance.NewInstanceAdminClient(ctx, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ia.Close() })
	inst := "projects/" + project + "/instances/iam9"
	op, err := ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + project, InstanceId: "iam9",
		Instance: &instancepb.Instance{Config: "projects/" + project + "/instanceConfigs/emulator-config", DisplayName: "iam9", NodeCount: 1}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("the instance's own account: CreateInstance = %v", err)
	}
	t.Cleanup(func() { _ = ia.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: inst}) })
	da, err := database.NewDatabaseAdminClient(ctx, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = da.Close() })
	dop, err := da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: inst, CreateStatement: "CREATE DATABASE db",
		ExtraStatements: []string{"CREATE TABLE W (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)"}})
	if err == nil {
		_, err = dop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db := inst + "/databases/db"
	if _, err := spannerAs(t, h, addr, db, "").Apply(ctx, []*spanner.Mutation{spanner.Insert("W", []string{"Id", "Name"}, []any{int64(1), "one"})}); err != nil {
		t.Fatalf("the instance's own account: Apply = %v", err)
	}
	as := spannerAs(t, h, addr, db, p.token)
	query := func() error {
		qctx, qcancel := context.WithTimeout(ctx, 20*time.Second)
		defer qcancel()
		it := as.Single().Query(qctx, spanner.Statement{SQL: "SELECT Name FROM W"})
		defer it.Stop()
		_, err := it.Next()
		return err
	}
	write := func() error {
		wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
		defer wcancel()
		_, err := as.Apply(wctx, []*spanner.Mutation{spanner.InsertOrUpdate("W", []string{"Id", "Name"}, []any{int64(2), "two"})})
		return err
	}
	pa, err := database.NewDatabaseAdminClient(ctx, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pa.Close() })
	ddl := func() error {
		_, err := pa.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: db})
		return err
	}
	if err := query(); status.Code(spanner.ToSpannerError(err)) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), "Caller is missing IAM permission spanner.") {
		t.Fatalf("a query without a role = %v; want PERMISSION_DENIED", err)
	}
	wantDenied(t, "GetDatabaseDdl without a role", ddl(), "Caller is missing IAM permission spanner.databases.getDdl on resource "+db+".", "", false)
	setDB := func(role string) {
		pol, err := da.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: db})
		if err != nil {
			t.Fatal(err)
		}
		pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: role, Members: []string{"serviceAccount:" + p.email}})
		if _, err := da.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: db, Policy: pol}); err != nil {
			t.Fatalf("SetIamPolicy on the database: %v", err)
		}
	}
	setDB("roles/spanner.databaseReader")
	waitGranted(t, "a query as a Database Reader on the database", query)
	if err := ddl(); err != nil {
		t.Errorf("GetDatabaseDdl as a Database Reader: %v", err)
	}
	if err := write(); status.Code(spanner.ToSpannerError(err)) != codes.PermissionDenied {
		t.Fatalf("a write as a Database Reader = %v; want PERMISSION_DENIED", err)
	}
	setDB("roles/spanner.databaseUser")
	waitGranted(t, "a write as a Database User on the database", write)
}

// bigtableAs are client options on a connection to addr calling with
// token: under BIGTABLE_EMULATOR_HOST the Bigtable client dials its own
// connection, without the caller's dial options, so the test gives it one.
func bigtableAs(t *testing.T, addr, token string) []option.ClientOption {
	t.Helper()
	dial := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if token != "" {
		dial = append(dial, grpc.WithPerRPCCredentials(bearerCreds(token)))
	}
	conn, err := grpc.NewClient("passthrough:///"+stripScheme(addr), dial...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return []option.ClientOption{option.WithGRPCConn(conn)}
}

// wantDeniedText checks err is PERMISSION_DENIED with msg (the Bigtable
// client hands its caller the status without its details).
func wantDeniedText(t *testing.T, what string, err error, msg string) {
	t.Helper()
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || st.Message() != msg {
		t.Fatalf("%s = %v; want PERMISSION_DENIED %q", what, err, msg)
	}
}

// covers: google.bigtable.v2.Bigtable/ReadRows, google.bigtable.v2.Bigtable/MutateRow, google.bigtable.admin.v2.BigtableTableAdmin/GetTable
//
// TestIAMEnforcementBigtable (#1879), with the official Bigtable clients
// over gRPC and a minted token: as Bigtable's data and admin references
// name, ReadRows needs bigtable.tables.readRows, MutateRow
// bigtable.tables.mutateRows and GetTable bigtable.tables.get, on the
// table, refused PERMISSION_DENIED "Permission 'P' denied on resource 'R'
// (or it may not exist)." (the front attaches ErrorInfo, which the
// client does not hand on) until Bigtable Reader, then User,
// is granted on the project; the instance's own account is never refused.
func TestIAMEnforcementBigtable(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvBigtable)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := h.Context()
	project := iamProject(t, h, "iebt9-")
	p := newIAMPrincipal(t, h, project, "iebt9")
	const inst, table = "cb-instance", "w9"
	admin, err := bigtable.NewAdminClient(ctx, project, inst, bigtableAs(t, addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.CreateTable(ctx, table); err != nil {
		t.Fatalf("the instance's own account: CreateTable = %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), table) })
	if err := admin.CreateColumnFamily(ctx, table, "f"); err != nil {
		t.Fatal(err)
	}
	cfg := bigtable.ClientConfig{MetricsProvider: bigtable.NoopMetricsProvider{}}
	oc, err := bigtable.NewClientWithConfig(ctx, project, inst, cfg, bigtableAs(t, addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oc.Close() })
	mut := bigtable.NewMutation()
	mut.Set("f", "c", bigtable.Now(), []byte("v"))
	if err := oc.Open(table).Apply(ctx, "r1", mut); err != nil {
		t.Fatalf("the instance's own account: MutateRow = %v", err)
	}
	pc, err := bigtable.NewClientWithConfig(ctx, project, inst, cfg, bigtableAs(t, addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	padm, err := bigtable.NewAdminClient(ctx, project, inst, bigtableAs(t, addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = padm.Close() })
	res := "projects/" + project + "/instances/" + inst + "/tables/" + table
	denied := func(perm string) string {
		return fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", perm, res)
	}
	read := func() error { _, err := pc.Open(table).ReadRow(ctx, "r1"); return err }
	write := func() error { return pc.Open(table).Apply(ctx, "r2", mut) }
	info := func() error { _, err := padm.TableInfo(ctx, table); return err }
	wantDeniedText(t, "ReadRows without a role", read(), denied("bigtable.tables.readRows"))
	wantDeniedText(t, "MutateRow without a role", write(), denied("bigtable.tables.mutateRows"))
	wantDeniedText(t, "GetTable without a role", info(), denied("bigtable.tables.get"))
	p.grant("roles/bigtable.reader")
	t.Cleanup(func() { p.revoke("roles/bigtable.reader") })
	waitGranted(t, "ReadRows as a Bigtable Reader", read)
	if err := info(); err != nil {
		t.Errorf("GetTable as a Bigtable Reader: %v", err)
	}
	wantDeniedText(t, "MutateRow as a Bigtable Reader", write(), denied("bigtable.tables.mutateRows"))
	p.grant("roles/bigtable.user")
	t.Cleanup(func() { p.revoke("roles/bigtable.user") })
	waitGranted(t, "MutateRow as a Bigtable User", write)
}

// covers: bigquery.jobs.query, bigqueryconnection.projects.locations.connections.setIamPolicy
//
// TestIAMEnforcementBigQueryMLRemote (#1879), with the official BigQuery,
// Connection and Resource Manager clients: ML.GENERATE_TEXT over a remote
// model, run by a caller with a minted token, is checked as Google
// documents it ("Generate text", Required permissions; "CREATE MODEL …
// REMOTE"): the caller's BigQuery permissions (bigquery.jobs.create, then
// bigquery.models.getData on the model, then bigquery.connections.use on
// the model's connection, each a 403 naming it), and the connection's
// service account roles/aiplatform.user on the project (400 naming the
// account without it). The instance's own account runs it.
func TestIAMEnforcementBigQueryMLRemote(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvLocalAI)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	c, _ := bigqueryClient(t, h)
	cc, project := remoteConnectionClient(t, h)
	pc := remoteProjectsClient(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_iam9"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() {
		_ = cc.DeleteConnection(context.Background(), &connectionpb.DeleteConnectionRequest{Name: cn.Name})
	})
	sa := cn.GetCloudResource().GetServiceAccountId()
	setConn := func(members ...string) {
		if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
			Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionUser", Members: members}}}}); err != nil {
			t.Fatalf("SetIamPolicy on the connection: %v", err)
		}
	}
	setConn(callerMember(project))
	if err := editProjectMember(h, pc, project, "roles/aiplatform.user", "serviceAccount:"+sa, true); err != nil {
		t.Fatal(err)
	}
	aiGranted := true
	t.Cleanup(func() {
		if aiGranted {
			_ = editProjectMember(h, pc, project, "roles/aiplatform.user", "serviceAccount:"+sa, false)
		}
	})
	ds := mlDataset(t, h, c, map[string]string{"prompts": "SELECT 'hi' AS prompt"})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.m` REMOTE WITH CONNECTION `%s.us.%s` OPTIONS(ENDPOINT = '%s')", ds, project, id, localModel)); err != nil {
		t.Fatalf("CREATE MODEL as the instance's own account: %v", err)
	}
	q := fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.m`, TABLE `%s.prompts`)", ds, ds)
	if err := mlRun(h, c, q); err != nil {
		t.Fatalf("ML.GENERATE_TEXT as the instance's own account: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "iebq9")
	tc, err := bigquery.NewClient(ctx, project, option.WithEndpoint(endpoint),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	// The host pushes BigQuery's roles every few seconds (#1767), so a
	// grant is waited for.
	want := func(what, perm string) {
		t.Helper()
		var err error
		for i := 0; i < 30; i++ {
			if err = mlRun(h, tc, q); err != nil && strings.Contains(err.Error(), perm) {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("ML.GENERATE_TEXT %s = %v; want a refusal naming %s", what, err, perm)
	}
	want("without a role", "bigquery.jobs.create")
	p.grant("roles/bigquery.jobUser")
	t.Cleanup(func() { p.revoke("roles/bigquery.jobUser") })
	want("as a Job User", "bigquery.models.getData")
	p.grant("roles/bigquery.dataViewer")
	t.Cleanup(func() { p.revoke("roles/bigquery.dataViewer") })
	want("without bigquery.connections.use", "bigquery.connections.use")
	setConn(callerMember(project), "serviceAccount:"+p.email)
	waitGranted(t, "ML.GENERATE_TEXT with every grant", func() error { return mlRun(h, tc, q) })
	if err := editProjectMember(h, pc, project, "roles/aiplatform.user", "serviceAccount:"+sa, false); err != nil {
		t.Fatal(err)
	}
	aiGranted = false
	want("with the connection's service account not a Vertex AI User", sa)
}
