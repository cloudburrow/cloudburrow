//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	backupdr "cloud.google.com/go/backupdr/apiv1"
	"cloud.google.com/go/backupdr/apiv1/backupdrpb"
	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	bdrv1 "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// backupDRComputeFixture is the endpoints the Compute Engine backup tests
// use, asked for before anything runs so a shard without them skips: Backup
// and DR, and Compute Engine (SQL_COMPUTE in the sql shard).
func backupDRComputeFixture(t *testing.T, h *Harness) (bdrAddr, computeAddr string) {
	t.Helper()
	bdrAddr = h.Endpoint(EnvBackupDR)
	computeAddr = runShardEndpoint(h, envSQLCompute, EnvCompute)
	if bdrAddr == "" || computeAddr == "" {
		t.Fatal("no Backup and DR or Compute Engine endpoint")
	}
	return bdrAddr, computeAddr
}

// TestBackupDRComputeVMAndDiskRoundTrip (#2075), the official
// cloud.google.com/go/backupdr REST client with compute/apiv1: a VM with a
// data disk holding a file, a backup plan for compute.googleapis.com/Instance
// and one for compute.googleapis.com/Disk, their associations, an on-demand
// backup of each, the file changed, and then:
//   - the VM's backup restored to a new VM (another name and machine type),
//     whose data disk holds the original file, read over SSH;
//   - the disk's backup restored to a new disk, attached to the first VM,
//     holding the original file;
//   - one disk of the VM's backup restored by its device name.
//
// The backups carry computeInstanceBackupProperties and
// diskBackupProperties, and the project's snapshots.list does not show the
// vault's snapshots.
// covers: backupdr.projects.locations.backupVaults.dataSources.backups.restore, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlanAssociations.create, backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.backupVaults.dataSources.backups.list, backupdr.projects.locations.backupVaults.dataSources.get, compute.instances.insert, compute.instances.get, compute.instances.attachDisk, compute.disks.get, compute.snapshots.list
func TestBackupDRComputeVMAndDiskRoundTrip(t *testing.T) {
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
	disks, _ := computeapi.NewDisksRESTClient(ctx, copts...)
	snaps, _ := computeapi.NewSnapshotsRESTClient(ctx, copts...)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	loc := "projects/" + p + "/locations/" + region
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vmName, dataName, vm2, rdisk, rdisk2 := "bv-vm-"+sfx, "bv-data-"+sfx, "bv-vm2-"+sfx, "bv-rdisk-"+sfx, "bv-rdisk2-"+sfx
	vault, iplan, dplan := "bv-vault-"+sfx, "bv-iplan-"+sfx, "bv-dplan-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, n := range []string{vmName, vm2} {
			if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: n}); err == nil {
				_ = op.Wait(c)
			}
		}
		for _, d := range []string{dataName, rdisk, rdisk2} {
			if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: zone, Disk: d}); err == nil {
				_ = op.Wait(c)
			}
		}
		for _, a := range []string{"bv-ia-" + sfx, "bv-da-" + sfx} {
			if op, err := bdr.DeleteBackupPlanAssociation(c, &backupdrpb.DeleteBackupPlanAssociationRequest{Name: loc + "/backupPlanAssociations/" + a}); err == nil {
				_ = op.Wait(c)
			}
		}
		for _, pl := range []string{iplan, dplan} {
			if op, err := bdr.DeleteBackupPlan(c, &backupdrpb.DeleteBackupPlanRequest{Name: loc + "/backupPlans/" + pl}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := bdr.DeleteBackupVault(c, &backupdrpb.DeleteBackupVaultRequest{Name: loc + "/backupVaults/" + vault, Force: true}); err == nil {
			_ = op.Wait(c)
		}
	})

	// A VM with a data disk, and SSH into it.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: zone, DiskResource: &computepb.Disk{
		Name: proto.String(dataName), SizeGb: proto.Int64(10)}}))
	newVM := func(name string, extra ...*computepb.AttachedDisk) {
		t.Helper()
		wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
			Name:        proto.String(name),
			MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
			Disks: append([]*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}}, extra...),
			NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default"),
				AccessConfigs: []*computepb.AccessConfig{{Name: proto.String("External NAT")}}}},
			Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}},
		}}))
	}
	newVM(vmName, &computepb.AttachedDisk{Source: proto.String("zones/" + zone + "/disks/" + dataName), DeviceName: proto.String("data")})
	run := func(vm, cmd string) string {
		t.Helper()
		var out string
		deadline := time.Now().Add(3 * time.Minute)
		for {
			c, err := ssh.Dial("tcp", vmSSHAddress(t, computeAddr, p, zone, vm), &ssh.ClientConfig{User: "tester",
				Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
			if err == nil {
				s, err2 := c.NewSession()
				if err2 == nil {
					b, err3 := s.CombinedOutput(cmd)
					out, err = string(b), err3
				} else {
					err = err2
				}
				c.Close()
				if err == nil {
					return out
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("ssh %s %q: %v: %s", vm, cmd, err, out)
			}
			time.Sleep(2 * time.Second)
		}
	}
	run(vmName, "echo original | sudo tee /mnt/disks/data/note.txt && sync")

	// The vault, the two plans and their associations.
	waitBDR := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	vop, err := bdr.CreateBackupVault(ctx, &backupdrpb.CreateBackupVaultRequest{Parent: loc, BackupVaultId: vault,
		BackupVault: &backupdrpb.BackupVault{BackupMinimumEnforcedRetentionDuration: durationpb.New(24 * time.Hour)}})
	waitBDR(err)
	_, err = vop.Wait(ctx)
	waitBDR(err)
	daily := []*backupdrpb.BackupRule{{RuleId: "daily", BackupRetentionDays: 7, BackupScheduleOneof: &backupdrpb.BackupRule_StandardSchedule{
		StandardSchedule: &backupdrpb.StandardSchedule{RecurrenceType: backupdrpb.StandardSchedule_DAILY, TimeZone: "UTC",
			BackupWindow: &backupdrpb.BackupWindow{StartHourOfDay: 1, EndHourOfDay: 5}}}}}
	for id, typ := range map[string]string{iplan: "compute.googleapis.com/Instance", dplan: "compute.googleapis.com/Disk"} {
		op, err := bdr.CreateBackupPlan(ctx, &backupdrpb.CreateBackupPlanRequest{Parent: loc, BackupPlanId: id, BackupPlan: &backupdrpb.BackupPlan{
			BackupVault: loc + "/backupVaults/" + vault, ResourceType: typ, BackupRules: daily}})
		waitBDR(err)
		pl, err := op.Wait(ctx)
		waitBDR(err)
		if pl.GetResourceType() != typ {
			t.Fatalf("plan %s: %v", id, pl)
		}
	}
	associate := func(id, plan, typ, res string) *backupdrpb.BackupPlanAssociation {
		t.Helper()
		op, err := bdr.CreateBackupPlanAssociation(ctx, &backupdrpb.CreateBackupPlanAssociationRequest{Parent: loc, BackupPlanAssociationId: id,
			BackupPlanAssociation: &backupdrpb.BackupPlanAssociation{BackupPlan: loc + "/backupPlans/" + plan, ResourceType: typ, Resource: res}})
		waitBDR(err)
		a, err := op.Wait(ctx)
		waitBDR(err)
		return a
	}
	ia := associate("bv-ia-"+sfx, iplan, "compute.googleapis.com/Instance", "projects/"+p+"/zones/"+zone+"/instances/"+vmName)
	da := associate("bv-da-"+sfx, dplan, "compute.googleapis.com/Disk", "projects/"+p+"/zones/"+zone+"/disks/"+dataName)
	backup := func(a *backupdrpb.BackupPlanAssociation) *backupdrpb.Backup {
		t.Helper()
		op, err := bdr.TriggerBackup(ctx, &backupdrpb.TriggerBackupRequest{Name: a.GetName(), RuleId: "daily"})
		waitBDR(err)
		_, err = op.Wait(ctx)
		waitBDR(err)
		it := bdr.ListBackups(ctx, &backupdrpb.ListBackupsRequest{Parent: a.GetDataSource()})
		b, err := it.Next()
		if err != nil || b.GetState() != backupdrpb.Backup_ACTIVE {
			t.Fatalf("backup of %s: %v %v", a.GetResource(), b, err)
		}
		return b
	}
	vb, db := backup(ia), backup(da)
	if ip := vb.GetComputeInstanceBackupProperties(); !strings.HasSuffix(ip.GetMachineType(), "/e2-small") || len(ip.GetDisk()) != 2 {
		t.Errorf("computeInstanceBackupProperties: %v", ip)
	}
	if dp := db.GetDiskBackupProperties(); dp.GetSizeGb() != 10 {
		t.Errorf("diskBackupProperties: %v", dp)
	}
	if ds, err := bdr.GetDataSource(ctx, &backupdrpb.GetDataSourceRequest{Name: ia.GetDataSource()}); err != nil ||
		ds.GetDataSourceGcpResource().GetComputeInstanceDatasourceProperties().GetTotalDiskCount() != 2 {
		t.Errorf("data source: %v %v", ds, err)
	}
	for it := snaps.List(ctx, &computepb.ListSnapshotsRequest{Project: p}); ; {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(s.GetName(), "bdr-") {
			t.Errorf("snapshots.list shows the vault's snapshot %s", s.GetName())
		}
	}

	// The file changes after the backups.
	run(vmName, "echo changed | sudo tee /mnt/disks/data/note.txt && sync")

	restore := func(req *backupdrpb.RestoreBackupRequest) *backupdrpb.RestoreBackupResponse {
		t.Helper()
		op, err := bdr.RestoreBackup(ctx, req)
		waitBDR(err)
		resp, err := op.Wait(ctx)
		waitBDR(err)
		return resp
	}
	resp := restore(&backupdrpb.RestoreBackupRequest{Name: vb.GetName(),
		TargetEnvironment: &backupdrpb.RestoreBackupRequest_ComputeInstanceTargetEnvironment{ComputeInstanceTargetEnvironment: &backupdrpb.ComputeInstanceTargetEnvironment{Project: p, Zone: zone}},
		InstanceProperties: &backupdrpb.RestoreBackupRequest_ComputeInstanceRestoreProperties{ComputeInstanceRestoreProperties: &backupdrpb.ComputeInstanceRestoreProperties{
			Name: proto.String(vm2), MachineType: proto.String("zones/" + zone + "/machineTypes/e2-medium")}}})
	if got := resp.GetTargetResource().GetGcpResource().GetGcpResourcename(); got != "//compute.googleapis.com/projects/"+p+"/zones/"+zone+"/instances/"+vm2 {
		t.Errorf("restore target: %s", got)
	}
	if i, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vm2}); err != nil ||
		!strings.HasSuffix(i.GetMachineType(), "/e2-medium") || len(i.GetDisks()) != 2 {
		t.Fatalf("restored VM: %v %v", i, err)
	}
	if out := run(vm2, "cat /mnt/disks/data/note.txt"); strings.TrimSpace(out) != "original" {
		t.Fatalf("the restored VM's data disk holds %q, want the backed up file", out)
	}

	restore(&backupdrpb.RestoreBackupRequest{Name: db.GetName(),
		TargetEnvironment:  &backupdrpb.RestoreBackupRequest_DiskTargetEnvironment{DiskTargetEnvironment: &backupdrpb.DiskTargetEnvironment{Project: p, Zone: zone}},
		InstanceProperties: &backupdrpb.RestoreBackupRequest_DiskRestoreProperties{DiskRestoreProperties: &backupdrpb.DiskRestoreProperties{Name: proto.String(rdisk)}}})
	// One disk of the VM's backup, by its device name: instanceBackupSource,
	// which the discovery client (google.golang.org/api/backupdr/v1) carries.
	dsvc := backupDRService(t, h)
	dop, err := dsvc.Projects.Locations.BackupVaults.DataSources.Backups.Restore(vb.GetName(), &bdrv1.RestoreBackupRequest{
		DiskTargetEnvironment: &bdrv1.DiskTargetEnvironment{Project: p, Zone: zone},
		DiskRestoreProperties: &bdrv1.DiskRestoreProperties{Name: rdisk2, SizeGb: 20,
			InstanceBackupSource: &bdrv1.RestoreDiskFromInstanceOptions{SourceDeviceName: "data"}}}).Do()
	backupDRWaiter(t, dsvc)(dop, err)
	if d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zone, Disk: rdisk2}); err != nil || d.GetSizeGb() != 20 {
		t.Errorf("the disk restored from the VM's backup: %v %v", d, err)
	}
	wait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: zone, Instance: vmName,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("zones/" + zone + "/disks/" + rdisk), DeviceName: proto.String("restored")}}))
	if out := run(vmName, "cat /mnt/disks/restored/note.txt /mnt/disks/data/note.txt"); strings.Fields(out)[0] != "original" || strings.Fields(out)[1] != "changed" {
		t.Fatalf("the restored disk and the source: %q, want original and changed", out)
	}

	// Refused as Google refuses them.
	_, err = bdr.RestoreBackup(ctx, &backupdrpb.RestoreBackupRequest{Name: db.GetName(),
		TargetEnvironment: &backupdrpb.RestoreBackupRequest_ComputeInstanceTargetEnvironment{ComputeInstanceTargetEnvironment: &backupdrpb.ComputeInstanceTargetEnvironment{Project: p, Zone: zone}}})
	if code := httpCodeOf(err); code != http.StatusBadRequest {
		t.Errorf("a disk's backup into a VM: %v, want 400", err)
	}
}

// httpCodeOf is the HTTP status of a REST client's error, 0 if none.
func httpCodeOf(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// TestGcloudBackupDRRestoreDisk (#2075): `gcloud backup-dr backups restore
// disk BACKUP --name --target-zone --target-project`, through the
// configuration `cloudburrow gcloud-setup` writes with egress to Google
// blocked, makes the new disk from a disk's backup, its size and type the
// backup's.
// covers: backupdr.projects.locations.backupVaults.dataSources.backups.restore, backupdr.projects.locations.operations.get
func TestGcloudBackupDRRestoreDisk(t *testing.T) {
	h := New(t)
	_, computeAddr := backupDRComputeFixture(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	disks, err := computeapi.NewDisksRESTClient(ctx, option.WithEndpoint("http://"+computeAddr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	bdr := backupDRService(t, h)
	wait := backupDRWaiter(t, bdr)
	p := g.project
	const zone = "us-central1-a"
	loc := "projects/" + p + "/locations/us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	src, restored, vault := "gr-src-"+sfx, "gr-restored-"+sfx, "gr-vault-"+sfx
	op, err := disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: zone, DiskResource: &computepb.Disk{
		Name: proto.String(src), SizeGb: proto.Int64(12), Type: proto.String("zones/" + zone + "/diskTypes/pd-ssd")}})
	if err != nil || op.Wait(ctx) != nil {
		t.Fatalf("disk: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, d := range []string{src, restored} {
			if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: zone, Disk: d}); err == nil {
				_ = op.Wait(c)
			}
		}
		_, _ = bdr.Projects.Locations.BackupPlanAssociations.Delete(loc + "/backupPlanAssociations/gr-a-" + sfx).Do()
		_, _ = bdr.Projects.Locations.BackupPlans.Delete(loc + "/backupPlans/gr-p-" + sfx).Do()
		_, _ = bdr.Projects.Locations.BackupVaults.Delete(loc + "/backupVaults/" + vault).Force(true).Do()
	})
	wait(bdr.Projects.Locations.BackupVaults.Create(loc, &bdrv1.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).BackupVaultId(vault).Do())
	wait(bdr.Projects.Locations.BackupPlans.Create(loc, &bdrv1.BackupPlan{BackupVault: loc + "/backupVaults/" + vault,
		ResourceType: "compute.googleapis.com/Disk", BackupRules: dailyRule()}).BackupPlanId("gr-p-" + sfx).Do())
	wait(bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &bdrv1.BackupPlanAssociation{BackupPlan: loc + "/backupPlans/gr-p-" + sfx,
		ResourceType: "compute.googleapis.com/Disk", Resource: "projects/" + p + "/zones/" + zone + "/disks/" + src}).BackupPlanAssociationId("gr-a-" + sfx).Do())
	wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(loc+"/backupPlanAssociations/gr-a-"+sfx, &bdrv1.TriggerBackupRequest{RuleId: "daily"}).Do())
	a, err := bdr.Projects.Locations.BackupPlanAssociations.Get(loc + "/backupPlanAssociations/gr-a-" + sfx).Do()
	if err != nil {
		t.Fatal(err)
	}
	bs, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(a.DataSource).Do()
	if err != nil || len(bs.Backups) != 1 {
		t.Fatalf("backups: %v %v", bs, err)
	}
	out, err := g.run(nil, "backup-dr", "backups", "restore", "disk", bs.Backups[0].Name, "--name", restored,
		"--target-zone", zone, "--target-project", p, "--quiet")
	if err != nil {
		t.Fatalf("gcloud backup-dr backups restore disk: %v\n%s", err, out)
	}
	d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zone, Disk: restored})
	if err != nil || d.GetSizeGb() != 12 || !strings.HasSuffix(d.GetType(), "/pd-ssd") {
		t.Fatalf("the restored disk: %v %v", d, err)
	}
}
