//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	backupdr "cloud.google.com/go/backupdr/apiv1"
	"cloud.google.com/go/backupdr/apiv1/backupdrpb"
	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestBackupDRRegionalDiskRoundTrip (#2206), the official
// cloud.google.com/go/backupdr REST client with compute/apiv1: a regional
// disk attached to a VM holding a file, a backup plan for
// compute.googleapis.com/RegionDisk and its association (a zonal path
// refused), an on-demand backup (diskBackupProperties with the region and
// the replica zones), the file changed, and the backup restored into a new
// regional disk (regionDiskTargetEnvironment with its replica zones, a
// zone of another region refused) and into a zonal disk, both attached to
// the VM and holding the original file.
// covers: backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlanAssociations.create, backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.backupVaults.dataSources.backups.list, backupdr.projects.locations.backupVaults.dataSources.backups.restore, compute.regionDisks.insert, compute.regionDisks.get, compute.instances.attachDisk
func TestBackupDRRegionalDiskRoundTrip(t *testing.T) {
	h := New(t)
	bdrAddr, computeAddr := backupDRComputeFixture(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	bdr, err := backupdr.NewRESTClient(ctx, option.WithEndpoint("http://"+bdrAddr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	copts := []option.ClientOption{option.WithEndpoint("http://" + computeAddr), option.WithoutAuthentication()}
	insts, err := computeapi.NewInstancesRESTClient(ctx, copts...)
	if err != nil {
		t.Fatal(err)
	}
	rdisks, _ := computeapi.NewRegionDisksRESTClient(ctx, copts...)
	disks, _ := computeapi.NewDisksRESTClient(ctx, copts...)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	loc := "projects/" + p + "/locations/" + region
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vm, rd, rd2, zd := "br-vm-"+sfx, "br-rd-"+sfx, "br-rd2-"+sfx, "br-zd-"+sfx
	vault, plan, assoc := "br-vault-"+sfx, "br-plan-"+sfx, "br-a-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vm}); err == nil {
			_ = op.Wait(c)
		}
		for _, d := range []string{rd, rd2} {
			if op, err := rdisks.Delete(c, &computepb.DeleteRegionDiskRequest{Project: p, Region: region, Disk: d}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: zone, Disk: zd}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := bdr.DeleteBackupPlanAssociation(c, &backupdrpb.DeleteBackupPlanAssociationRequest{Name: loc + "/backupPlanAssociations/" + assoc}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := bdr.DeleteBackupPlan(c, &backupdrpb.DeleteBackupPlanRequest{Name: loc + "/backupPlans/" + plan}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := bdr.DeleteBackupVault(c, &backupdrpb.DeleteBackupVaultRequest{Name: loc + "/backupVaults/" + vault, Force: true}); err == nil {
			_ = op.Wait(c)
		}
	})

	sh := newVMShell(t, computeAddr, p)
	replicas := []string{"zones/" + zone, "zones/us-central1-c"}
	wait(rdisks.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: &computepb.Disk{
		Name: proto.String(rd), SizeGb: proto.Int64(10), ReplicaZones: replicas}}))
	wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name: proto.String(vm), MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}},
			{Source: proto.String("regions/" + region + "/disks/" + rd), DeviceName: proto.String("data")}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default"),
			AccessConfigs: []*computepb.AccessConfig{{Name: proto.String("External NAT")}}}},
		Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(sh.keyLine)}}},
	}}))
	run := func(cmd string) string {
		t.Helper()
		out, err := sh.run(zone, vm, cmd)
		if err != nil {
			t.Fatalf("ssh %q: %v %s", cmd, err, out)
		}
		return out
	}
	run("echo original | sudo tee /mnt/disks/data/note.txt && sync")

	vop, err := bdr.CreateBackupVault(ctx, &backupdrpb.CreateBackupVaultRequest{Parent: loc, BackupVaultId: vault,
		BackupVault: &backupdrpb.BackupVault{BackupMinimumEnforcedRetentionDuration: durationpb.New(24 * time.Hour)}})
	must(err)
	_, err = vop.Wait(ctx)
	must(err)
	pop, err := bdr.CreateBackupPlan(ctx, &backupdrpb.CreateBackupPlanRequest{Parent: loc, BackupPlanId: plan, BackupPlan: &backupdrpb.BackupPlan{
		BackupVault: loc + "/backupVaults/" + vault, ResourceType: "compute.googleapis.com/RegionDisk",
		BackupRules: []*backupdrpb.BackupRule{{RuleId: "daily", BackupRetentionDays: 7, BackupScheduleOneof: &backupdrpb.BackupRule_StandardSchedule{
			StandardSchedule: &backupdrpb.StandardSchedule{RecurrenceType: backupdrpb.StandardSchedule_DAILY, TimeZone: "UTC",
				BackupWindow: &backupdrpb.BackupWindow{StartHourOfDay: 1, EndHourOfDay: 7}}}}}}})
	must(err)
	if pl, err := pop.Wait(ctx); err != nil || pl.GetResourceType() != "compute.googleapis.com/RegionDisk" {
		t.Fatalf("the regional disk plan: %v %v", pl, err)
	}
	associate := func(id, res string) (*backupdrpb.BackupPlanAssociation, error) {
		op, err := bdr.CreateBackupPlanAssociation(ctx, &backupdrpb.CreateBackupPlanAssociationRequest{Parent: loc, BackupPlanAssociationId: id,
			BackupPlanAssociation: &backupdrpb.BackupPlanAssociation{BackupPlan: loc + "/backupPlans/" + plan,
				ResourceType: "compute.googleapis.com/RegionDisk", Resource: res}})
		if err != nil {
			return nil, err
		}
		return op.Wait(ctx)
	}
	if _, err := associate("br-bad-"+sfx, "projects/"+p+"/zones/"+zone+"/disks/"+rd); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a RegionDisk association with a zonal path: %v, want 400", err)
	}
	a, err := associate(assoc, "projects/"+p+"/regions/"+region+"/disks/"+rd)
	must(err)
	top, err := bdr.TriggerBackup(ctx, &backupdrpb.TriggerBackupRequest{Name: a.GetName(), RuleId: "daily"})
	must(err)
	_, err = top.Wait(ctx)
	must(err)
	b, err := bdr.ListBackups(ctx, &backupdrpb.ListBackupsRequest{Parent: a.GetDataSource()}).Next()
	if err != nil || b.GetState() != backupdrpb.Backup_ACTIVE {
		t.Fatalf("the regional disk's backup: %v %v", b, err)
	}
	if dp := b.GetDiskBackupProperties(); !strings.HasSuffix(dp.GetRegion(), "/regions/"+region) || len(dp.GetReplicaZones()) != 2 || dp.GetSizeGb() != 10 {
		t.Errorf("diskBackupProperties: %v", dp)
	}
	run("echo changed | sudo tee /mnt/disks/data/note.txt && sync")

	restore := func(env *backupdrpb.RegionDiskTargetEnvironment, zonal *backupdrpb.DiskTargetEnvironment, name string) error {
		req := &backupdrpb.RestoreBackupRequest{Name: b.GetName(), InstanceProperties: &backupdrpb.RestoreBackupRequest_DiskRestoreProperties{
			DiskRestoreProperties: &backupdrpb.DiskRestoreProperties{Name: proto.String(name), SizeGb: proto.Int64(10), Type: proto.String("pd-balanced")}}}
		if env != nil {
			req.TargetEnvironment = &backupdrpb.RestoreBackupRequest_RegionDiskTargetEnvironment{RegionDiskTargetEnvironment: env}
		} else {
			req.TargetEnvironment = &backupdrpb.RestoreBackupRequest_DiskTargetEnvironment{DiskTargetEnvironment: zonal}
		}
		op, err := bdr.RestoreBackup(ctx, req)
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	if err := restore(&backupdrpb.RegionDiskTargetEnvironment{Project: p, Region: region, ReplicaZones: []string{"zones/" + zone, "zones/us-east1-b"}}, nil, rd2); httpCodeOf(err) != http.StatusBadRequest {
		t.Errorf("a replica zone of another region: %v, want 400", err)
	}
	must(restore(&backupdrpb.RegionDiskTargetEnvironment{Project: p, Region: region,
		ReplicaZones: []string{"https://www.googleapis.com/compute/v1/projects/" + p + "/zones/" + zone, "zones/us-central1-b"}}, nil, rd2))
	if d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: rd2}); err != nil || len(d.GetReplicaZones()) != 2 {
		t.Fatalf("the restored regional disk: %v %v", d, err)
	}
	must(restore(nil, &backupdrpb.DiskTargetEnvironment{Project: p, Zone: zone}, zd))
	wait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: zone, Instance: vm,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("regions/" + region + "/disks/" + rd2), DeviceName: proto.String("restored")}}))
	wait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: zone, Instance: vm,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("zones/" + zone + "/disks/" + zd), DeviceName: proto.String("zonal")}}))
	if out := strings.Fields(run("cat /mnt/disks/restored/note.txt /mnt/disks/zonal/note.txt /mnt/disks/data/note.txt")); len(out) != 3 ||
		out[0] != "original" || out[1] != "original" || out[2] != "changed" {
		t.Fatalf("the restored disks and the source: %q, want original, original and changed", out)
	}
}
