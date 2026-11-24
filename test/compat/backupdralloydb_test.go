//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/alloydb/apiv1/alloydbpb"
	backupdr "cloud.google.com/go/backupdr/apiv1"
	"cloud.google.com/go/backupdr/apiv1/backupdrpb"
	adbrest "google.golang.org/api/alloydb/v1"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// alloyDBREST is google.golang.org/api/alloydb/v1 at the local endpoint:
// the published alloydbpb has no backupdrBackupSource, backupdrPitrSource
// or backupdrInfo, which AlloyDB's REST discovery document has.
func alloyDBREST(t *testing.T, h *Harness) *adbrest.Service {
	t.Helper()
	addr := h.Endpoint(EnvAlloyDB)
	if addr == "" {
		t.Fatal("no AlloyDB endpoint")
	}
	svc, err := adbrest.NewService(h.Context(), option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// alloyDBRESTWait waits for an alloydb/v1 operation.
func alloyDBRESTWait(ctx context.Context, t *testing.T, svc *adbrest.Service, op *adbrest.Operation, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	for !op.Done {
		select {
		case <-ctx.Done():
			t.Fatalf("operation %s: %v", op.Name, ctx.Err())
		case <-time.After(time.Second):
		}
		if op, err = svc.Projects.Locations.Operations.Get(op.Name).Context(ctx).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if op.Error != nil {
		t.Fatalf("operation %s: %s", op.Name, op.Error.Message)
	}
}

// TestBackupDRAlloyDBRoundTrip (#1524), the official
// cloud.google.com/go/backupdr REST client with AlloyDB's clients: a cluster
// with a primary holding two rows, a backup plan for
// alloydb.googleapis.com/Cluster with log retention, its association (the
// cluster's own automated and continuous backups turned off, and
// backupdrInfo naming the association), an on-demand backup with
// alloyDbBackupProperties, more rows written with the time noted between
// them, and then:
//   - the backup restored with clusters.restore backupdrBackupSource into a
//     new cluster, whose primary holds the backup's two rows;
//   - a point-in-time restore with backupdrPitrSource into another,
//     holding the noted time's three rows.
//
// The documented refusals are checked on the way: a Filestore plan with
// log retention, a five-hour window, a second backup job while one runs,
// a plan in another vault, backups.restore of the cluster's backup, both
// sources at once. Removing the association gives the cluster back its
// backup settings and keeps the backup.
// covers: backupdr.projects.locations.backupPlanAssociations.create, backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.backupPlanAssociations.patch, backupdr.projects.locations.backupPlanAssociations.delete, backupdr.projects.locations.backupVaults.dataSources.get, backupdr.projects.locations.backupVaults.dataSources.backups.list, alloydb.projects.locations.clusters.restore, alloydb.projects.locations.clusters.get
func TestBackupDRAlloyDBRoundTrip(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvBackupDR) == "" {
		t.Fatal("no Backup and DR endpoint")
	}
	c := alloyDBClients(t, h)["grpc"]
	rest := alloyDBREST(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	bdr, err := backupdr.NewRESTClient(ctx, option.WithEndpoint("http://"+h.Endpoint(EnvBackupDR)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bdr.Close() })
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"
	network := "projects/" + project + "/global/networks/default"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	id := "bda-" + sfx
	name := loc + "/clusters/" + id
	vault, vault2, plan, plan2, assoc := "bda-v-"+sfx, "bda-v2-"+sfx, "bda-p-"+sfx, "bda-p2-"+sfx, "bda-a-"+sfx
	created := []string{name}
	t.Cleanup(func() {
		c0 := context.Background()
		if op, err := bdr.DeleteBackupPlanAssociation(c0, &backupdrpb.DeleteBackupPlanAssociationRequest{Name: loc + "/backupPlanAssociations/" + assoc}); err == nil {
			_ = op.Wait(c0)
		}
		for _, n := range created {
			if op, err := c.DeleteCluster(c0, &alloydbpb.DeleteClusterRequest{Name: n, Force: true}); err == nil {
				_ = op.Wait(c0)
			}
		}
		for _, pl := range []string{plan, plan2} {
			if op, err := bdr.DeleteBackupPlan(c0, &backupdrpb.DeleteBackupPlanRequest{Name: loc + "/backupPlans/" + pl}); err == nil {
				_ = op.Wait(c0)
			}
		}
		for _, v := range []string{vault, vault2} {
			if op, err := bdr.DeleteBackupVault(c0, &backupdrpb.DeleteBackupVaultRequest{Name: loc + "/backupVaults/" + v, Force: true}); err == nil {
				_ = op.Wait(c0)
			}
		}
	})

	// The cluster, its primary and two rows.
	op, err := c.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: loc, ClusterId: id, Cluster: &alloydbpb.Cluster{
		NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network},
		InitialUser:   &alloydbpb.UserPassword{User: "postgres", Password: "burrow-pw"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatal(err)
	}
	iop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: "primary", Instance: &alloydbpb.Instance{
		InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := waitLRO(ctx, iop)
	if err != nil {
		t.Fatal(err)
	}
	d := alloyDBDialer(t, h)
	pg := alloyDBConn(ctx, t, d, primary.GetName(), "postgres", "burrow-pw")
	for _, q := range []string{"CREATE TABLE orders (id int PRIMARY KEY, item text)", "INSERT INTO orders VALUES (1, 'burrow'), (2, 'tunnel')"} {
		if _, err := pg.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// The vaults and plans.
	for _, v := range []string{vault, vault2} {
		vop, err := bdr.CreateBackupVault(ctx, &backupdrpb.CreateBackupVaultRequest{Parent: loc, BackupVaultId: v,
			BackupVault: &backupdrpb.BackupVault{BackupMinimumEnforcedRetentionDuration: durationpb.New(24 * time.Hour)}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rule := func(start, end int32) []*backupdrpb.BackupRule {
		return []*backupdrpb.BackupRule{{RuleId: "daily", BackupRetentionDays: 7, BackupScheduleOneof: &backupdrpb.BackupRule_StandardSchedule{
			StandardSchedule: &backupdrpb.StandardSchedule{RecurrenceType: backupdrpb.StandardSchedule_DAILY, TimeZone: "UTC",
				BackupWindow: &backupdrpb.BackupWindow{StartHourOfDay: start, EndHourOfDay: end}}}}}
	}
	ws, we := idleBackupWindow()
	newPlan := func(id, v, typ string, logDays int64, rules []*backupdrpb.BackupRule) error {
		op, err := bdr.CreateBackupPlan(ctx, &backupdrpb.CreateBackupPlanRequest{Parent: loc, BackupPlanId: id, BackupPlan: &backupdrpb.BackupPlan{
			BackupVault: loc + "/backupVaults/" + v, ResourceType: typ, BackupRules: rules, LogRetentionDays: logDays}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	if err := newPlan("bda-bad-"+sfx, vault, "file.googleapis.com/Instance", 7, rule(int32(ws), int32(we))); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a Filestore plan with log retention: %v, want 400", err)
	}
	if err := newPlan("bda-bad-"+sfx, vault, "alloydb.googleapis.com/Cluster", 7, rule(1, 6)); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a five-hour backup window: %v, want 400", err)
	}
	if err := newPlan(plan, vault, "alloydb.googleapis.com/Cluster", 7, rule(int32(ws), int32(we))); err != nil {
		t.Fatal(err)
	}
	if err := newPlan(plan2, vault2, "alloydb.googleapis.com/Cluster", 7, rule(int32(ws), int32(we))); err != nil {
		t.Fatal(err)
	}

	// The association: the cluster's own backups are off, and backupdrInfo
	// names the association.
	aop, err := bdr.CreateBackupPlanAssociation(ctx, &backupdrpb.CreateBackupPlanAssociationRequest{Parent: loc, BackupPlanAssociationId: assoc,
		BackupPlanAssociation: &backupdrpb.BackupPlanAssociation{BackupPlan: loc + "/backupPlans/" + plan,
			ResourceType: "alloydb.googleapis.com/Cluster", Resource: name}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := aop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.GetAlloydbClusterBackupPlanAssociationProperties().GetClusterUid() == "" {
		t.Errorf("alloydbClusterBackupPlanAssociationProperties: %v", a)
	}
	cl, err := rest.Projects.Locations.Clusters.Get(name).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if cl.BackupdrInfo == nil || cl.BackupdrInfo.CurrentWindow == nil || cl.BackupdrInfo.CurrentWindow.BackupPlanAssociation != a.GetName() ||
		cl.BackupdrInfo.CurrentWindow.DataSource != a.GetDataSource() || cl.BackupdrInfo.CurrentWindow.LogRetentionPeriod != "604800s" {
		t.Errorf("backupdrInfo after the association: %+v", cl.BackupdrInfo)
	}
	if cl.AutomatedBackupPolicy != nil && cl.AutomatedBackupPolicy.Enabled {
		t.Errorf("the cluster's automated backups are still on: %+v", cl.AutomatedBackupPolicy)
	}
	ds, err := bdr.GetDataSource(ctx, &backupdrpb.GetDataSourceRequest{Name: a.GetDataSource()})
	if err != nil || ds.GetDataSourceGcpResource().GetGcpResourcename() != "//alloydb.googleapis.com/"+name ||
		ds.GetDataSourceGcpResource().GetAlloyDbClusterDatasourceProperties().GetName() != name {
		t.Errorf("the data source: %v, %v", ds, err)
	}

	// An on-demand backup; a second job while it runs is refused.
	top, err := bdr.TriggerBackup(ctx, &backupdrpb.TriggerBackupRequest{Name: a.GetName(), RuleId: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bdr.TriggerBackup(ctx, &backupdrpb.TriggerBackupRequest{Name: a.GetName(), RuleId: "daily"}); err == nil ||
		!strings.Contains(err.Error(), "Unable to queue the operation") {
		t.Errorf("a second backup job: %v", err)
	}
	if _, err := top.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := bdr.ListBackups(ctx, &backupdrpb.ListBackupsRequest{Parent: a.GetDataSource()}).Next()
	if err != nil || b.GetState() != backupdrpb.Backup_ACTIVE || b.GetAlloyDbBackupProperties().GetStoredBytes() == 0 ||
		!strings.HasPrefix(b.GetAlloyDbBackupProperties().GetDatabaseVersion(), "POSTGRES_") {
		t.Fatalf("the backup: %v, %v", b, err)
	}

	// More rows, the time noted between them.
	if _, err := pg.Exec(ctx, "INSERT INTO orders VALUES (3, 'den')"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	mid := time.Now()
	time.Sleep(2 * time.Second)
	if _, err := pg.Exec(ctx, "INSERT INTO orders VALUES (4, 'warren')"); err != nil {
		t.Fatal(err)
	}

	// backups.restore does not restore it; AlloyDB does.
	if _, err := bdr.RestoreBackup(ctx, &backupdrpb.RestoreBackupRequest{Name: b.GetName()}); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("backups.restore of an AlloyDB backup: %v, want 400", err)
	}
	restore := func(rid string, req *adbrest.RestoreClusterRequest) string {
		t.Helper()
		req.ClusterId = rid
		req.Cluster = &adbrest.Cluster{NetworkConfig: &adbrest.NetworkConfig{Network: network}}
		op, err := rest.Projects.Locations.Clusters.Restore(loc, req).Context(ctx).Do()
		created = append(created, loc+"/clusters/"+rid)
		alloyDBRESTWait(ctx, t, rest, op, err)
		iop, err := c.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: loc + "/clusters/" + rid, InstanceId: "primary", Instance: &alloydbpb.Instance{
			InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := waitLRO(ctx, iop); err != nil {
			t.Fatalf("the restored cluster %s's primary: %v", rid, err)
		}
		return loc + "/clusters/" + rid
	}
	_, err = rest.Projects.Locations.Clusters.Restore(loc, &adbrest.RestoreClusterRequest{ClusterId: id + "-x",
		Cluster:              &adbrest.Cluster{NetworkConfig: &adbrest.NetworkConfig{Network: network}},
		BackupdrBackupSource: &adbrest.BackupDrBackupSource{Backup: b.GetName()},
		BackupdrPitrSource:   &adbrest.BackupDrPitrSource{DataSource: a.GetDataSource(), PointInTime: mid.UTC().Format(time.RFC3339Nano)}}).Context(ctx).Do()
	if httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a restore with two sources: %v, want 400", err)
	}
	fromBackup := restore(id+"-r", &adbrest.RestoreClusterRequest{BackupdrBackupSource: &adbrest.BackupDrBackupSource{Backup: b.GetName()}})
	alloyDBExpectRows(ctx, t, d, fromBackup+"/instances/primary", 2, false)
	if rc, err := rest.Projects.Locations.Clusters.Get(fromBackup).Context(ctx).Do(); err != nil || rc.BackupdrBackupSource == nil ||
		rc.BackupdrBackupSource.Backup != b.GetName() {
		t.Errorf("the restored cluster's backupdrBackupSource: %+v, %v", rc, err)
	}
	pitr := restore(id+"-p", &adbrest.RestoreClusterRequest{BackupdrPitrSource: &adbrest.BackupDrPitrSource{
		DataSource: a.GetDataSource(), PointInTime: mid.UTC().Format(time.RFC3339Nano)}})
	alloyDBExpectRows(ctx, t, d, pitr+"/instances/primary", 3, false)

	// A plan in another vault is refused; removing the association gives
	// the cluster back its settings and keeps the backup.
	pop, err := bdr.UpdateBackupPlanAssociation(ctx, &backupdrpb.UpdateBackupPlanAssociationRequest{
		BackupPlanAssociation: &backupdrpb.BackupPlanAssociation{Name: a.GetName(), BackupPlan: loc + "/backupPlans/" + plan2,
			ResourceType: "alloydb.googleapis.com/Cluster", Resource: name},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"backup_plan"}}})
	if err == nil {
		_, err = pop.Wait(ctx)
	}
	if httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a plan in another vault: %v, want 400", err)
	}
	dop, err := bdr.DeleteBackupPlanAssociation(ctx, &backupdrpb.DeleteBackupPlanAssociationRequest{Name: a.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		cl, err = rest.Projects.Locations.Clusters.Get(name).Context(ctx).Do()
		if err == nil && cl.BackupdrInfo != nil && cl.BackupdrInfo.CurrentWindow == nil && len(cl.BackupdrInfo.PreviousWindows) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || cl.BackupdrInfo == nil || cl.BackupdrInfo.CurrentWindow != nil || len(cl.BackupdrInfo.PreviousWindows) != 1 {
		t.Errorf("backupdrInfo after the association is removed: %+v, %v", cl.BackupdrInfo, err)
	}
	if _, err := bdr.GetBackup(ctx, &backupdrpb.GetBackupRequest{Name: b.GetName()}); err != nil {
		t.Errorf("the backup after the association is removed: %v", err)
	}
}
