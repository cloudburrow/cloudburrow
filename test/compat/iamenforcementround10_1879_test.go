//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/datastore"
	dsadmin "cloud.google.com/go/datastore/admin/apiv1"
	"cloud.google.com/go/datastore/admin/apiv1/adminpb"
	"cloud.google.com/go/firestore"
	fsapi "cloud.google.com/go/firestore/apiv1"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// connAs is a raw gRPC connection to addr calling with token ("" for the
// instance's own account), for the generated clients of methods the
// handwritten ones do not offer (operations, change streams, Write).
func connAs(t *testing.T, addr, token string) *grpc.ClientConn {
	t.Helper()
	dial := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if token != "" {
		dial = append(dial, grpc.WithPerRPCCredentials(bearerCreds(token)))
	}
	if stripScheme(addr) == "" {
		t.Fatal("no endpoint")
	}
	conn, err := grpc.NewClient("passthrough:///"+stripScheme(addr), dial...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// covers: google.datastore.v1.Datastore/Lookup, google.datastore.v1.Datastore/RunQuery, google.datastore.v1.Datastore/Commit, google.datastore.v1.Datastore/AllocateIds, google.datastore.admin.v1.DatastoreAdmin/ListIndexes
//
// TestIAMEnforcementDatastore (#1879), with the official Datastore and
// Datastore Admin clients over gRPC and a minted token: as Datastore's
// "Required permissions for API methods" lists them, a lookup needs
// datastore.entities.get, a query entities.get and list (a keys-only one
// list), an upsert entities.create and update, AllocateIds
// entities.allocateIds, and the Admin API's index list datastore.schemas.list,
// each on the database, refused PERMISSION_DENIED "Missing or insufficient
// permissions." until Cloud Datastore Viewer, then User, is granted on the
// project; the instance's own account is never refused.
func TestIAMEnforcementDatastore(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvDatastore)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieds10-")
	p := newIAMPrincipal(t, h, project, "ieds10")
	owner, err := datastore.NewClient(ctx, project, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	type widget struct{ Size int }
	k := datastore.NameKey("Widget", "one", nil)
	if _, err := owner.Put(ctx, k, &widget{3}); err != nil {
		t.Fatalf("the instance's own account: Put = %v", err)
	}
	as, err := datastore.NewClient(ctx, project, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	adm, err := dsadmin.NewDatastoreAdminClient(ctx, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	const msg = "Missing or insufficient permissions."
	get := func() error { var w widget; return as.Get(ctx, k, &w) }
	query := func() error {
		var ws []widget
		_, err := as.GetAll(ctx, datastore.NewQuery("Widget"), &ws)
		return err
	}
	keys := func() error { _, err := as.GetAll(ctx, datastore.NewQuery("Widget").KeysOnly(), nil); return err }
	put := func() error { _, err := as.Put(ctx, datastore.NameKey("Widget", "two", nil), &widget{9}); return err }
	alloc := func() error {
		_, err := as.AllocateIDs(ctx, []*datastore.Key{datastore.IncompleteKey("Widget", nil)})
		return err
	}
	indexes := func() error {
		_, err := adm.ListIndexes(ctx, &adminpb.ListIndexesRequest{ProjectId: project}).Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	wantDenied(t, "Lookup without a role", get(), msg, "", false)
	wantDenied(t, "RunQuery without a role", query(), msg, "", false)
	wantDenied(t, "a keys-only RunQuery without a role", keys(), msg, "", false)
	wantDenied(t, "Commit without a role", put(), msg, "", false)
	wantDenied(t, "AllocateIds without a role", alloc(), msg, "", false)
	wantDenied(t, "ListIndexes without a role", indexes(), msg, "", false)
	p.grant("roles/datastore.viewer")
	t.Cleanup(func() { p.revoke("roles/datastore.viewer") })
	waitGranted(t, "Lookup as a Cloud Datastore Viewer", get)
	for what, call := range map[string]func() error{"RunQuery": query, "a keys-only RunQuery": keys, "ListIndexes": indexes} {
		if err := call(); err != nil {
			t.Errorf("%s as a Cloud Datastore Viewer: %v", what, err)
		}
	}
	wantDenied(t, "Commit as a Cloud Datastore Viewer", put(), msg, "", false)
	wantDenied(t, "AllocateIds as a Cloud Datastore Viewer", alloc(), msg, "", false)
	p.grant("roles/datastore.user")
	t.Cleanup(func() { p.revoke("roles/datastore.user") })
	waitGranted(t, "Commit as a Cloud Datastore User", put)
	if err := alloc(); err != nil {
		t.Errorf("AllocateIds as a Cloud Datastore User: %v", err)
	}
}

// covers: google.firestore.v1.Firestore/Write, google.firestore.v1.Firestore/ExecutePipeline
//
// TestIAMEnforcementFirestoreStreams (#1879), with the official Firestore
// clients and a minted token: a Write stream's every message is checked
// (its first, naming the database, needs nothing; a later one carrying a
// set needs datastore.entities.create and update, and a refused one ends
// the stream PERMISSION_DENIED "Missing or insufficient permissions."
// without the write reaching the database), and ExecutePipeline needs
// its collection stage's datastore.entities.get and list, until Cloud
// Datastore Viewer, then User, is granted on the project.
func TestIAMEnforcementFirestoreStreams(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvFirestore)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iefw10-")
	p := newIAMPrincipal(t, h, project, "iefw10")
	db := "projects/" + project + "/databases/(default)"
	owner, err := firestore.NewClient(ctx, project, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.Collection("widgets").Doc("one").Set(ctx, map[string]any{"size": 3}); err != nil {
		t.Fatalf("the instance's own account: Set = %v", err)
	}
	gc, err := fsapi.NewClient(ctx, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gc.Close() })
	const msg = "Missing or insufficient permissions."
	// write opens a Write stream, then sends one set of doc on it.
	write := func(doc string) error {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		// The emulator, like Google, needs the resource prefix on the
		// stream (TestFirestoreWriteStream).
		sctx = metadata.AppendToOutgoingContext(sctx, "google-cloud-resource-prefix", db)
		st, err := gc.Write(sctx)
		if err != nil {
			return err
		}
		if err := st.Send(&firestorepb.WriteRequest{Database: db}); err != nil {
			return err
		}
		first, err := st.Recv()
		if err != nil {
			return fmt.Errorf("the stream's first answer: %w", err)
		}
		if err := st.Send(&firestorepb.WriteRequest{StreamToken: first.GetStreamToken(), Writes: []*firestorepb.Write{{
			Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{Name: db + "/documents/widgets/" + doc,
				Fields: map[string]*firestorepb.Value{"size": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 9}}}}}}}}); err != nil {
			return err
		}
		if _, err := st.Recv(); err != nil {
			return err
		}
		return st.CloseSend()
	}
	wantDenied(t, "a Write stream's set without a role", write("denied"), msg, "", false)
	if _, err := owner.Collection("widgets").Doc("denied").Get(ctx); status.Code(err) != codes.NotFound {
		t.Fatalf("the refused write reached the database: Get = %v", err)
	}
	as, err := firestore.NewClient(ctx, project, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	pipeline := func() error {
		_, err := as.Pipeline().Collection("widgets").Limit(5).Execute(ctx).Results().GetAll()
		return err
	}
	wantDenied(t, "ExecutePipeline without a role", pipeline(), msg, "", false)
	p.grant("roles/datastore.viewer")
	t.Cleanup(func() { p.revoke("roles/datastore.viewer") })
	waitPast(t, "ExecutePipeline as a Cloud Datastore Viewer", pipeline)
	wantDenied(t, "a Write stream's set as a Cloud Datastore Viewer", write("viewer"), msg, "", false)
	p.grant("roles/datastore.user")
	t.Cleanup(func() { p.revoke("roles/datastore.user") })
	waitGranted(t, "a Write stream's set as a Cloud Datastore User", func() error { return write("user") })
	if _, err := owner.Collection("widgets").Doc("user").Get(ctx); err != nil {
		t.Errorf("the granted write: Get = %v", err)
	}
}

// covers: google.spanner.admin.database.v1.DatabaseAdmin/ListDatabaseOperations, google.spanner.admin.database.v1.DatabaseAdmin/ListBackupSchedules, google.spanner.admin.database.v1.DatabaseAdmin/TestIamPermissions
//
// TestIAMEnforcementSpannerOperations (#1879), with the official Spanner
// admin client and the generated Operations client over gRPC and a minted
// token: ListDatabaseOperations needs spanner.databaseOperations.list on
// the instance, as its reference names, and a database operation's
// GetOperation spanner.databaseOperations.get on the database, refused
// "Caller is missing IAM permission P on resource R." until Cloud Spanner
// Viewer is granted on the project; ListBackupSchedules needs
// spanner.backupSchedules.list on the database; TestIamPermissions answers
// what the caller holds (nothing, then a Viewer's
// spanner.databaseOperations.get and not spanner.databases.select).
func TestIAMEnforcementSpannerOperations(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	project := iamProject(t, h, "iesp10-")
	p := newIAMPrincipal(t, h, project, "iesp10")
	ia, err := instance.NewInstanceAdminClient(ctx, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ia.Close() })
	inst := "projects/" + project + "/instances/iamten-i"
	op, err := ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + project, InstanceId: "iamten-i",
		Instance: &instancepb.Instance{Config: "projects/" + project + "/instanceConfigs/emulator-config", DisplayName: "iam10", NodeCount: 1}})
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
	dop, err := da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: inst, CreateStatement: "CREATE DATABASE db"})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db := inst + "/databases/db"
	pa, err := database.NewDatabaseAdminClient(ctx, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pa.Close() })
	ops := longrunningpb.NewOperationsClient(connAs(t, addr, p.token))
	list := func() error {
		_, err := pa.ListDatabaseOperations(ctx, &databasepb.ListDatabaseOperationsRequest{Parent: inst}).Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	get := func() error {
		_, err := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: dop.Name()})
		return err
	}
	wantDenied(t, "ListDatabaseOperations without a role", list(),
		"Caller is missing IAM permission spanner.databaseOperations.list on resource "+inst+".", "", false)
	wantDenied(t, "GetOperation without a role", get(),
		"Caller is missing IAM permission spanner.databaseOperations.get on resource "+db+".", "", false)
	schedules := func() error {
		_, err := pa.ListBackupSchedules(ctx, &databasepb.ListBackupSchedulesRequest{Parent: db}).Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	wantDenied(t, "ListBackupSchedules without a role", schedules(),
		"Caller is missing IAM permission spanner.backupSchedules.list on resource "+db+".", "", false)
	held := func() string {
		r, err := pa.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: db,
			Permissions: []string{"spanner.databases.select", "spanner.databaseOperations.get"}})
		if err != nil {
			t.Fatalf("TestIamPermissions: %v", err)
		}
		return strings.Join(r.GetPermissions(), ",")
	}
	if got := held(); got != "" {
		t.Errorf("TestIamPermissions without a role = %q, want none", got)
	}
	p.grant("roles/spanner.viewer")
	t.Cleanup(func() { p.revoke("roles/spanner.viewer") })
	waitGranted(t, "ListDatabaseOperations as a Cloud Spanner Viewer", list)
	if err := get(); err != nil {
		t.Errorf("GetOperation as a Cloud Spanner Viewer: %v", err)
	}
	waitPast(t, "ListBackupSchedules as a Cloud Spanner Viewer", schedules)
	// Viewer holds databaseOperations.get, not databases.select.
	if got := held(); got != "spanner.databaseOperations.get" {
		t.Errorf("TestIamPermissions as a Cloud Spanner Viewer = %q, want spanner.databaseOperations.get", got)
	}
	// The instance's own account.
	if _, err := longrunningpb.NewOperationsClient(connAs(t, addr, "")).GetOperation(ctx,
		&longrunningpb.GetOperationRequest{Name: dop.Name()}); err != nil {
		t.Errorf("GetOperation as the instance's own account: %v", err)
	}
}

// covers: google.bigtable.v2.Bigtable/GenerateInitialChangeStreamPartitions, google.bigtable.admin.v2.BigtableTableAdmin/ListSchemaBundles, google.bigtable.admin.v2.BigtableTableAdmin/TestIamPermissions
//
// TestIAMEnforcementBigtableAdmin (#1879), with the generated Bigtable data,
// admin and Operations clients over gRPC and a minted token: a change
// stream's GenerateInitialChangeStreamPartitions needs
// bigtable.tables.readRows on the table, ListSchemaBundles
// bigtable.schemaBundles.list on the table (each as the reference names),
// and an instance creation's GetOperation the instance's bigtable.instances.get,
// refused "Permission 'P' denied on resource 'R' (or it may not exist)."
// until Bigtable Reader is granted on the project, whose TestIamPermissions
// on the table then answers readRows and not mutateRows; the instance's
// own account is never refused.
func TestIAMEnforcementBigtableAdmin(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvBigtable)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iebt10-")
	p := newIAMPrincipal(t, h, project, "iebt10")
	const instID, tableID = "cb-instance", "w10"
	inst := "projects/" + project + "/instances/" + instID
	table := inst + "/tables/" + tableID
	oc := connAs(t, addr, "")
	ta := btapb.NewBigtableTableAdminClient(oc)
	if _, err := ta.CreateTable(ctx, &btapb.CreateTableRequest{Parent: inst, TableId: tableID, Table: &btapb.Table{
		ColumnFamilies: map[string]*btapb.ColumnFamily{"f": {}}}}); err != nil {
		t.Fatalf("the instance's own account: CreateTable = %v", err)
	}
	t.Cleanup(func() { _, _ = ta.DeleteTable(context.Background(), &btapb.DeleteTableRequest{Name: table}) })
	// An instance of the instance admin API, whose creation is an
	// operation of the instance.
	ia := btapb.NewBigtableInstanceAdminClient(oc)
	opInst := "projects/" + project + "/instances/iamten-i"
	op, err := ia.CreateInstance(ctx, &btapb.CreateInstanceRequest{Parent: "projects/" + project, InstanceId: "iamten-i",
		Instance: &btapb.Instance{DisplayName: "iam10", Type: btapb.Instance_PRODUCTION},
		Clusters: map[string]*btapb.Cluster{"iamten-c1": {Location: "projects/" + project + "/locations/us-central1-b", ServeNodes: 1,
			DefaultStorageType: btapb.StorageType_SSD}}})
	if err != nil {
		t.Fatalf("the instance's own account: CreateInstance = %v", err)
	}
	t.Cleanup(func() { _, _ = ia.DeleteInstance(context.Background(), &btapb.DeleteInstanceRequest{Name: opInst}) })
	pc := connAs(t, addr, p.token)
	data, admin, ops := btpb.NewBigtableClient(pc), btapb.NewBigtableTableAdminClient(pc), longrunningpb.NewOperationsClient(pc)
	partitions := func() error {
		st, err := data.GenerateInitialChangeStreamPartitions(ctx, &btpb.GenerateInitialChangeStreamPartitionsRequest{TableName: table})
		if err != nil {
			return err
		}
		for {
			if _, err := st.Recv(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}
	bundles := func() error {
		_, err := admin.ListSchemaBundles(ctx, &btapb.ListSchemaBundlesRequest{Parent: table})
		return err
	}
	getOp := func() error {
		_, err := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()})
		return err
	}
	denied := func(perm, res string) string {
		return fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", perm, res)
	}
	wantDeniedText(t, "GenerateInitialChangeStreamPartitions without a role", partitions(), denied("bigtable.tables.readRows", table))
	wantDeniedText(t, "ListSchemaBundles without a role", bundles(), denied("bigtable.schemaBundles.list", table))
	wantDeniedText(t, "GetOperation without a role", getOp(), denied("bigtable.instances.get", opInst))
	p.grant("roles/bigtable.reader")
	t.Cleanup(func() { p.revoke("roles/bigtable.reader") })
	waitPast(t, "GenerateInitialChangeStreamPartitions as a Bigtable Reader", partitions)
	waitPast(t, "ListSchemaBundles as a Bigtable Reader", bundles)
	if err := getOp(); err != nil {
		t.Errorf("GetOperation as a Bigtable Reader: %v", err)
	}
	// Bigtable Reader reads rows but does not write them.
	r, err := admin.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: table,
		Permissions: []string{"bigtable.tables.readRows", "bigtable.tables.mutateRows"}})
	if err != nil || strings.Join(r.GetPermissions(), ",") != "bigtable.tables.readRows" {
		t.Errorf("TestIamPermissions as a Bigtable Reader = %v, %v; want bigtable.tables.readRows", r.GetPermissions(), err)
	}
	if _, err := longrunningpb.NewOperationsClient(oc).GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()}); err != nil {
		t.Errorf("GetOperation as the instance's own account: %v", err)
	}
}
