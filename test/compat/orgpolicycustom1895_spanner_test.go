//go:build compat

package compat

import (
	"context"
	"testing"
	"time"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// spannerDatabaseDenied is the Spanner page's output for a CreateDatabase
// a custom constraint refuses.
const spannerDatabaseDenied = "Either caller is missing IAM permission spanner.databases.create on resource or the " +
	"CreateDatabaseRequest.create_statement field is malformed and the database name could not be identified " +
	"to verify Cloud IAM Conditions."

// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstance, google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstance, google.spanner.admin.database.v1.DatabaseAdmin/CreateDatabase, google.spanner.admin.database.v1.DatabaseAdmin/UpdateDatabase, google.spanner.admin.database.v1.DatabaseAdmin/CreateBackup, google.spanner.admin.database.v1.DatabaseAdmin/UpdateBackup
//
// TestOrgPolicyCustomConstraintsSpanner (#1895), with the official Spanner
// admin clients: an ALLOW on spanner.googleapis.com/Instance
// (resource.nodeCount <= 2) refuses a 3-node instance and growing one to
// 5 nodes, FAILED_PRECONDITION "Operation denied by custom org policies:
// [...]"; the page's example on spanner.googleapis.com/Database (DENY
// resource.name.contains('denied-database-name')) refuses CreateDatabase
// PERMISSION_DENIED with the page's message; a DENY on UPDATE refuses
// turning drop protection on; an ALLOW on spanner.googleapis.com/Backup
// reading its timestamps refuses a backup kept 60 days, creates one kept
// 7, and refuses extending it.
func TestOrgPolicyCustomConstraintsSpanner(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	ctx := h.Context()
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ocs-")
	ia, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ia.Close() })
	da, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = da.Close() })

	nodes := customName(h, "spannernodes")
	createCustom(t, h, c, nodes, "spanner.googleapis.com/Instance", "resource.nodeCount <= 2", ccAllow,
		"At most two nodes.", ccCreate, ccUpdate)
	dbName := customName(h, "spannerdatabase")
	createCustom(t, h, c, dbName, "spanner.googleapis.com/Database", "resource.name.contains('denied-database-name')", ccDeny,
		"Database names must not contain denied-database-name.", ccCreate)
	protect := customName(h, "spannerprotect")
	createCustom(t, h, c, protect, "spanner.googleapis.com/Database", "resource.enableDropProtection == true", ccDeny,
		"No drop protection.", ccUpdate)
	keep := customName(h, "spannerkeep")
	createCustom(t, h, c, keep, "spanner.googleapis.com/Backup", "resource.expireTime < resource.versionTime + duration('720h')", ccAllow,
		"Backups are kept 30 days at most.", ccCreate, ccUpdate)
	for _, n := range []string{nodes, dbName, protect, keep} {
		setOrgPolicy(t, h, c, "projects/"+project, n, opEnforce())
	}

	parent := "projects/" + project
	config := parent + "/instanceConfigs/emulator-config"
	_, err = ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: parent, InstanceId: "cc-big",
		Instance: &instancepb.Instance{Config: config, DisplayName: "cc big", NodeCount: 3}})
	wantFailedPrecondition(t, "a 3-node instance", err, customMessage(nodes, "At most two nodes."))
	op, err := ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: parent, InstanceId: "cc-small",
		Instance: &instancepb.Instance{Config: config, DisplayName: "cc small", NodeCount: 1}})
	if err != nil {
		t.Fatalf("a 1-node instance: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	inst := parent + "/instances/cc-small"
	t.Cleanup(func() { _ = ia.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: inst}) })
	_, err = ia.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: inst, NodeCount: 5},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"node_count"}}})
	wantFailedPrecondition(t, "growing it to 5 nodes", err, customMessage(nodes, "At most two nodes."))
	if _, err := ia.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: inst, DisplayName: "cc renamed"},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil {
		t.Errorf("renaming it: %v", err)
	}

	_, err = da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: inst, CreateStatement: "CREATE DATABASE `denied-database-name100`"})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || st.Message() != spannerDatabaseDenied {
		t.Errorf("a database named denied-database-name100 = %v, want PERMISSION_DENIED %q", err, spannerDatabaseDenied)
	}
	dop, err := da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: inst, CreateStatement: "CREATE DATABASE `cc-db`",
		ExtraStatements: []string{"CREATE TABLE T (K INT64) PRIMARY KEY (K)"}})
	if err != nil {
		t.Fatalf("an allowed database: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	db := inst + "/databases/cc-db"
	_, err = da.UpdateDatabase(ctx, &databasepb.UpdateDatabaseRequest{Database: &databasepb.Database{Name: db, EnableDropProtection: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enable_drop_protection"}}})
	wantFailedPrecondition(t, "turning drop protection on", err, customMessage(protect, "No drop protection."))

	now := time.Now()
	_, err = da.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: inst, BackupId: "cc-long",
		Backup: &databasepb.Backup{Database: db, ExpireTime: timestamppb.New(now.Add(60 * 24 * time.Hour))}})
	wantFailedPrecondition(t, "a backup kept 60 days", err, customMessage(keep, "Backups are kept 30 days at most."))
	bop, err := da.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: inst, BackupId: "cc-short",
		Backup: &databasepb.Backup{Database: db, ExpireTime: timestamppb.New(now.Add(7 * 24 * time.Hour))}})
	if err != nil {
		t.Fatalf("a backup kept 7 days: %v", err)
	}
	if _, err := bop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	backup := inst + "/backups/cc-short"
	t.Cleanup(func() { _ = da.DeleteBackup(context.Background(), &databasepb.DeleteBackupRequest{Name: backup}) })
	_, err = da.UpdateBackup(ctx, &databasepb.UpdateBackupRequest{Backup: &databasepb.Backup{Name: backup,
		ExpireTime: timestamppb.New(now.Add(90 * 24 * time.Hour))}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
	wantFailedPrecondition(t, "keeping it 90 days", err, customMessage(keep, "Backups are kept 30 days at most."))
}
