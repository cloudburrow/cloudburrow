//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	backupdr "cloud.google.com/go/backupdr/apiv1"
	"cloud.google.com/go/backupdr/apiv1/backupdrpb"
	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/filestore/apiv1/filestorepb"
	fileapi "google.golang.org/api/file/v1"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/durationpb"
)

// envFilestoreBackupDR is the Backup and DR Service in the served and gke
// shards (#1524), beside Filestore, so the Backup and DR suites stay in sql.
const envFilestoreBackupDR = "CLOUDBURROW_TEST_FILESTORE_BACKUPDR"

// TestGKEFilestoreBackupDRRoundTrip (#1524), the official
// cloud.google.com/go/backupdr REST client with Filestore's: a Zonal
// instance whose share a Kubernetes Engine pod writes a file to, a backup
// plan for file.googleapis.com/Instance and its association (the instance
// named by its zone, as Google's guide writes it), an on-demand backup with
// filestoreInstanceBackupProperties, the file changed, and the backup
// restored into a new instance by file/v1's fileShares[].sourceBackupdrBackup:
// a pod mounting the new share reads the original file, and the source still
// holds the changed one. The documented refusals are checked on the way: a
// Basic HDD instance's association, a restore below the source's capacity,
// a restore to a Basic tier (Filestore's own capacity rules allow both), backups.restore of the instance's backup.
// covers: backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlanAssociations.create, backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.backupVaults.dataSources.get, backupdr.projects.locations.backupVaults.dataSources.backups.list, google.cloud.filestore.v1.CloudFilestoreManager/CreateInstance
func TestGKEFilestoreBackupDRRoundTrip(t *testing.T) {
	h := New(t)
	bdrAddr := runShardEndpoint(h, envFilestoreBackupDR, EnvBackupDR)
	fg, _ := filestoreClients(t, h)
	cg, _ := gkeClients(t, h)
	if bdrAddr == "" {
		t.Fatal("no Backup and DR endpoint")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	bdr, err := backupdr.NewRESTClient(ctx, option.WithEndpoint("http://"+bdrAddr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bdr.Close() })
	files, err := fileapi.NewService(ctx, option.WithEndpoint("http://"+h.Endpoint(EnvFilestore)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	project := h.Project()
	zone := "projects/" + project + "/locations/us-central1-a"
	loc := "projects/" + project + "/locations/us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vault, plan, assoc := "bdf-v-"+sfx, "bdf-p-"+sfx, "bdf-a-"+sfx
	restoredID := "ct-fs-bdr-r" + sfx
	t.Cleanup(func() {
		c0 := context.Background()
		if op, err := fg.DeleteInstance(c0, &filestorepb.DeleteInstanceRequest{Name: zone + "/instances/" + restoredID, Force: true}); err == nil {
			_ = op.Wait(c0)
		}
		if op, err := bdr.DeleteBackupPlanAssociation(c0, &backupdrpb.DeleteBackupPlanAssociationRequest{Name: loc + "/backupPlanAssociations/" + assoc}); err == nil {
			_ = op.Wait(c0)
		}
		if op, err := bdr.DeleteBackupPlan(c0, &backupdrpb.DeleteBackupPlanRequest{Name: loc + "/backupPlans/" + plan}); err == nil {
			_ = op.Wait(c0)
		}
		if op, err := bdr.DeleteBackupVault(c0, &backupdrpb.DeleteBackupVaultRequest{Name: loc + "/backupVaults/" + vault, Force: true}); err == nil {
			_ = op.Wait(c0)
		}
	})

	// The instances first, so their cleanups run after the cluster's.
	zonal := fsInstance(t, fg, zone, "ct-fs-bdr"+sfx, &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, Protocol: filestorepb.Instance_NFS_V4_1,
		FileShares: fsShare("vol1", 1280), Networks: fsNetwork})
	basic := fsInstance(t, fg, zone, "ct-fs-bdr3"+sfx, &filestorepb.Instance{Tier: filestorepb.Instance_BASIC_HDD,
		FileShares: fsShare("share1", 1024), Networks: fsNetwork})

	cname := zone + "/clusters/ct-gke-fs-bdr"
	op, err := cg.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: zone, Cluster: &containerpb.Cluster{Name: "ct-gke-fs-bdr", InitialNodeCount: 1,
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := cg.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: cname}); err == nil {
			gkeWait(t, cg, project, "us-central1-a", op)
		}
	})
	gkeWait(t, cg, project, "us-central1-a", op)
	c, err := cg.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cname})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	kc := gkeKubeconfig(t, c)
	pods := 0
	// sh runs script in a pod mounting in's share at /mnt/share.
	sh := func(in *filestorepb.Instance, script string) string {
		t.Helper()
		pods++
		name := fmt.Sprintf("ct-fs-bdr-%d", pods)
		if out, err := kubectlRun(kc, sharePod(name, in), time.Minute, "apply", "-f", "-"); err != nil {
			t.Fatalf("kubectl apply: %v\n%s", err, out)
		}
		waitPodRunning(t, kc, name)
		out, err := kubectlRun(kc, "", 2*time.Minute, "exec", name, "--", "sh", "-c", script)
		if err != nil {
			t.Fatalf("kubectl exec %q: %v\n%s", script, err, out)
		}
		if del, err := kubectlRun(kc, "", 2*time.Minute, "delete", "pod", name, "--wait=true", "--timeout=2m"); err != nil {
			t.Fatalf("kubectl delete pod: %v\n%s", err, del)
		}
		return out
	}
	if got := sh(zonal, "echo original > /mnt/share/note.txt && sync && cat /mnt/share/note.txt"); got != "original" {
		t.Fatalf("the write: %q", got)
	}

	// The vault, the plan, and the associations.
	vop, err := bdr.CreateBackupVault(ctx, &backupdrpb.CreateBackupVaultRequest{Parent: loc, BackupVaultId: vault,
		BackupVault: &backupdrpb.BackupVault{BackupMinimumEnforcedRetentionDuration: durationpb.New(24 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	ws, we := idleBackupWindow()
	pop, err := bdr.CreateBackupPlan(ctx, &backupdrpb.CreateBackupPlanRequest{Parent: loc, BackupPlanId: plan, BackupPlan: &backupdrpb.BackupPlan{
		BackupVault: loc + "/backupVaults/" + vault, ResourceType: "file.googleapis.com/Instance",
		BackupRules: []*backupdrpb.BackupRule{{RuleId: "daily", BackupRetentionDays: 7, BackupScheduleOneof: &backupdrpb.BackupRule_StandardSchedule{
			StandardSchedule: &backupdrpb.StandardSchedule{RecurrenceType: backupdrpb.StandardSchedule_DAILY, TimeZone: "UTC",
				BackupWindow: &backupdrpb.BackupWindow{StartHourOfDay: int32(ws), EndHourOfDay: int32(we)}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	associate := func(id, res string) (*backupdrpb.BackupPlanAssociation, error) {
		op, err := bdr.CreateBackupPlanAssociation(ctx, &backupdrpb.CreateBackupPlanAssociationRequest{Parent: loc, BackupPlanAssociationId: id,
			BackupPlanAssociation: &backupdrpb.BackupPlanAssociation{BackupPlan: loc + "/backupPlans/" + plan,
				ResourceType: "file.googleapis.com/Instance", Resource: res}})
		if err != nil {
			return nil, err
		}
		return op.Wait(ctx)
	}
	if _, err := associate("bdf-basic-"+sfx, basic.GetName()); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a Basic HDD instance's association: %v, want 400", err)
	}
	a, err := associate(assoc, strings.Replace(zonal.GetName(), "/locations/", "/zones/", 1))
	if err != nil {
		t.Fatal(err)
	}
	if a.GetResource() != zonal.GetName() {
		t.Errorf("the association's resource: %s, want %s", a.GetResource(), zonal.GetName())
	}
	ds, err := bdr.GetDataSource(ctx, &backupdrpb.GetDataSourceRequest{Name: a.GetDataSource()})
	if err != nil || ds.GetDataSourceGcpResource().GetGcpResourcename() != "//file.googleapis.com/"+zonal.GetName() ||
		ds.GetDataSourceGcpResource().GetType() != "file.googleapis.com/Instance" {
		t.Errorf("the data source: %v, %v", ds, err)
	}

	// An on-demand backup, then the file changed.
	top, err := bdr.TriggerBackup(ctx, &backupdrpb.TriggerBackupRequest{Name: a.GetName(), RuleId: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := top.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := bdr.ListBackups(ctx, &backupdrpb.ListBackupsRequest{Parent: a.GetDataSource()}).Next()
	if err != nil || b.GetState() != backupdrpb.Backup_ACTIVE {
		t.Fatalf("the backup: %v, %v", b, err)
	}
	if got := sh(zonal, "echo changed > /mnt/share/note.txt && sync && cat /mnt/share/note.txt"); got != "changed" {
		t.Fatalf("the change: %q", got)
	}
	if _, err := bdr.RestoreBackup(ctx, &backupdrpb.RestoreBackupRequest{Name: b.GetName()}); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("backups.restore of a Filestore backup: %v, want 400", err)
	}

	// Restored into a new instance by fileShares[].sourceBackupdrBackup.
	create := func(id, tier string, gb int64) (*fileapi.Operation, error) {
		return files.Projects.Locations.Instances.Create(zone, &fileapi.Instance{Tier: tier, Protocol: "NFS_V4_1",
			FileShares: []*fileapi.FileShareConfig{{Name: "vol1", CapacityGb: gb, SourceBackupdrBackup: b.GetName()}},
			Networks:   []*fileapi.NetworkConfig{{Network: "default"}}}).InstanceId(id).Context(ctx).Do()
	}
	if _, err := create(restoredID+"s", "ZONAL", 1024); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a restore below the source's capacity: %v, want 400", err)
	}
	if _, err := files.Projects.Locations.Instances.Create(zone, &fileapi.Instance{Tier: "BASIC_HDD",
		FileShares: []*fileapi.FileShareConfig{{Name: "vol1", CapacityGb: 1024, SourceBackupdrBackup: b.GetName()}},
		Networks:   []*fileapi.NetworkConfig{{Network: "default"}}}).InstanceId(restoredID + "b").Context(ctx).Do(); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a restore to Basic HDD: %v, want 400", err)
	}
	cop, err := create(restoredID, "ZONAL", 1280)
	if err != nil {
		t.Fatal(err)
	}
	for !cop.Done {
		time.Sleep(time.Second)
		if cop, err = files.Projects.Locations.Operations.Get(cop.Name).Context(ctx).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if cop.Error != nil {
		t.Fatalf("the restore: %s", cop.Error.Message)
	}
	got, err := files.Projects.Locations.Instances.Get(zone + "/instances/" + restoredID).Context(ctx).Do()
	if err != nil || got.FileShares[0].SourceBackupdrBackup != b.GetName() {
		t.Fatalf("the restored instance: %+v, %v", got, err)
	}
	restored, err := fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: got.Name})
	if err != nil {
		t.Fatal(err)
	}
	if got := sh(restored, "cat /mnt/share/note.txt"); got != "original" {
		t.Errorf("the restored share: %q, want original", got)
	}
	if got := sh(zonal, "cat /mnt/share/note.txt"); got != "changed" {
		t.Errorf("the source share after the restore: %q, want changed", got)
	}
}
