//go:build browser

package browser

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

const envBDRCompute = "CLOUDBURROW_TEST_COMPUTE"

// TestBackupDRComputeRestoresThroughThePages (#2075), in the sql shard
// (Backup and DR beside Compute Engine): a VM with a data disk holding a
// file, protected through the API by a Compute Engine plan and its data
// disk by a Disk plan, each backed up; the file is then changed. On
// Google's pages:
//   - Vaulted backups' Filter table > Resource type Disk lists the disk,
//     Compute Engine the VM;
//   - the disk's page: Restore, the restore project and zone, the name
//     pre-filled with "-restore-" and a time, Restore; the new disk,
//     attached to the VM, holds the backed-up file;
//   - the VM's page: Restore, Restore scope Entire instance, a name, a
//     machine type, Restore; the new VM's data disk holds the backed-up
//     file, read over SSH;
//   - Create Backup plan's and Schedule backup's Resource Type offer
//     Compute Engine and Disk.
//
// Each screen is audited for WCAG 2.1 AA.
func TestBackupDRComputeRestoresThroughThePages(t *testing.T) {
	needService(t, "backupdr")
	needService(t, "compute-instances")
	bdr, err := backupdr.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envBackupDR)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	caddr := endpoint(t, envBDRCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	copts := []option.ClientOption{option.WithEndpoint("http://" + caddr), option.WithoutAuthentication()}
	insts, err := computeapi.NewInstancesRESTClient(ctx, copts...)
	if err != nil {
		t.Fatal(err)
	}
	disks, _ := computeapi.NewDisksRESTClient(ctx, copts...)
	project := servedProject(t)
	const zone = "us-central1-a"
	loc := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	vm, data, vm2, vault := fmt.Sprintf("bb-vm-%d", n), fmt.Sprintf("bb-data-%d", n), fmt.Sprintf("bb-vm2-%d", n), fmt.Sprintf("bb-vault-%d", n)
	rdisk := ""
	cwait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(what string, op *backupdr.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for end := time.Now().Add(10 * time.Minute); !op.Done && time.Now().Before(end); time.Sleep(time.Second) {
			if op, err = bdr.Projects.Locations.Operations.Get(op.Name).Do(); err != nil {
				t.Fatalf("%s: operations.get: %v", what, err)
			}
		}
		if !op.Done || op.Error != nil {
			t.Fatalf("%s: %+v", what, op)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, i := range []string{vm, vm2} {
			if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: i}); err == nil {
				_ = op.Wait(c)
			}
		}
		for _, d := range []string{data, rdisk} {
			if d == "" {
				continue
			}
			if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: project, Zone: zone, Disk: d}); err == nil {
				_ = op.Wait(c)
			}
		}
		for _, a := range []string{"vm", "disk"} {
			_, _ = bdr.Projects.Locations.BackupPlanAssociations.Delete(fmt.Sprintf("%s/backupPlanAssociations/bb-%s-%d", loc, a, n)).Do()
			_, _ = bdr.Projects.Locations.BackupPlans.Delete(fmt.Sprintf("%s/backupPlans/bb-%s-%d", loc, a, n)).Do()
		}
		_, _ = bdr.Projects.Locations.BackupVaults.Delete(loc + "/backupVaults/" + vault).Force(true).IgnoreBackupPlanReferences(true).Do()
	})

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"
	cwait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: project, Zone: zone, DiskResource: &computepb.Disk{
		Name: proto.String(data), SizeGb: proto.Int64(10)}}))
	cwait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: project, Zone: zone, InstanceResource: &computepb.Instance{
		Name: proto.String(vm), MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}},
			{Source: proto.String("zones/" + zone + "/disks/" + data), DeviceName: proto.String("data")}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default"),
			AccessConfigs: []*computepb.AccessConfig{{Name: proto.String("External NAT")}}}},
		Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}}}}))
	run := func(name, cmd string) string {
		t.Helper()
		var out string
		deadline := time.Now().Add(3 * time.Minute)
		for {
			resp, err := http.Get("http://" + caddr + "/compute/v1/projects/" + project + "/zones/" + zone + "/instances/" + name)
			var v struct {
				SSH string `json:"cloudburrowSshAddress"`
			}
			if err == nil {
				_ = json.NewDecoder(resp.Body).Decode(&v)
				resp.Body.Close()
			}
			if v.SSH != "" {
				c, err := ssh.Dial("tcp", v.SSH, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
					HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
				if err == nil {
					if s, err := c.NewSession(); err == nil {
						b, err := s.CombinedOutput(cmd)
						out = string(b)
						c.Close()
						if err == nil {
							return out
						}
					} else {
						c.Close()
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("ssh %s %q: %s", name, cmd, out)
			}
			time.Sleep(2 * time.Second)
		}
	}
	run(vm, "echo original | sudo tee /mnt/disks/data/note.txt && sync")

	op, err := bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).BackupVaultId(vault).Do()
	wait("backupVaults.create", op, err)
	dsOf := map[string]string{}
	for kind, res := range map[string][2]string{"vm": {"compute.googleapis.com/Instance", "projects/" + project + "/zones/" + zone + "/instances/" + vm},
		"disk": {"compute.googleapis.com/Disk", "projects/" + project + "/zones/" + zone + "/disks/" + data}} {
		plan, assoc := fmt.Sprintf("bb-%s-%d", kind, n), fmt.Sprintf("bb-%s-%d", kind, n)
		op, err = bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: loc + "/backupVaults/" + vault, ResourceType: res[0],
			BackupRules: []*backupdr.BackupRule{{RuleId: "daily", BackupRetentionDays: 7, StandardSchedule: &backupdr.StandardSchedule{
				RecurrenceType: "DAILY", TimeZone: "UTC", BackupWindow: &backupdr.BackupWindow{StartHourOfDay: 1, EndHourOfDay: 5}}}}}).BackupPlanId(plan).Do()
		wait("backupPlans.create", op, err)
		op, err = bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{BackupPlan: loc + "/backupPlans/" + plan,
			ResourceType: res[0], Resource: res[1]}).BackupPlanAssociationId(assoc).Do()
		wait("backupPlanAssociations.create", op, err)
		op, err = bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(loc+"/backupPlanAssociations/"+assoc, &backupdr.TriggerBackupRequest{RuleId: "daily"}).Do()
		wait("triggerBackup", op, err)
		a, err := bdr.Projects.Locations.BackupPlanAssociations.Get(loc + "/backupPlanAssociations/" + assoc).Do()
		if err != nil {
			t.Fatal(err)
		}
		dsOf[kind] = a.DataSource[strings.LastIndex(a.DataSource, "/")+1:]
	}
	run(vm, "echo changed | sudo tee /mnt/disks/data/note.txt && sync")

	p := open(t)
	list := "/backupdr-vaulted-backups?project=" + project
	p.navigate(list + "&type=compute.googleapis.com/Disk")
	p.waitFor(rowReady(data) + " && !(" + rowReady(vm) + ")")
	p.navigate(list + "&type=compute.googleapis.com/Instance")
	p.waitFor(rowReady(vm))

	restoreOn := func(kind string, fields map[string]string) {
		t.Helper()
		page := "/backupdr-vaulted-backups/" + vault + "/" + dsOf[kind] + "?project=" + project
		p.navigate(page)
		p.waitFor(settled + ` && [...document.querySelectorAll("#view tbody tr")].length > 0`)
		p.eval(`document.querySelector("#view tbody tr .overflow-trigger").click()`, nil)
		p.clickText(".overflow-item", "Restore")
		p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
		for id, v := range fields {
			setField(p, ".modal.is-open", "f-"+id, v)
		}
		p.clickText(".modal.is-open button", "Restore")
		p.confirmByName(p.confirmWord())
		p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	}

	// The disk: its name pre-filled, Google's "-restore-" and a time.
	page := "/backupdr-vaulted-backups/" + vault + "/" + dsOf["disk"] + "?project=" + project
	p.navigate(page)
	p.waitFor(settled + ` && [...document.querySelectorAll("#view tbody tr")].length > 0`)
	p.eval(`document.querySelector("#view tbody tr .overflow-trigger").click()`, nil)
	p.clickText(".overflow-item", "Restore")
	p.waitFor(`document.querySelector(".modal.is-open #f-name") !== null`)
	p.eval(`document.querySelector(".modal.is-open #f-name").value`, &rdisk)
	if !strings.HasPrefix(rdisk, data+"-restore-") {
		t.Errorf("the restored disk's name is pre-filled %q, want %s-restore- and a time", rdisk, data)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	// The dialog is opened again, its time newer: keep the name read.
	restoreOn("disk", map[string]string{"zone": zone, "name": rdisk})
	var d *computepb.Disk
	for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(2 * time.Second) {
		if d, err = disks.Get(ctx, &computepb.GetDiskRequest{Project: project, Zone: zone, Disk: rdisk}); err == nil && d.GetStatus() == "READY" {
			break
		}
	}
	if d.GetStatus() != "READY" {
		t.Fatalf("the restored disk %s: %v %v", rdisk, d, err)
	}
	cwait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: project, Zone: zone, Instance: vm,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("zones/" + zone + "/disks/" + rdisk), DeviceName: proto.String("restored")}}))
	if out := run(vm, "cat /mnt/disks/restored/note.txt"); strings.TrimSpace(out) != "original" {
		t.Errorf("the disk restored through the page holds %q, want the backed-up file", out)
	}

	// The VM: Entire instance, a name and a machine type.
	restoreOn("vm", map[string]string{"scope": "instance", "name": vm2, "zone": zone, "machineType": "e2-medium"})
	var i *computepb.Instance
	for end := time.Now().Add(5 * time.Minute); time.Now().Before(end); time.Sleep(2 * time.Second) {
		if i, err = insts.Get(ctx, &computepb.GetInstanceRequest{Project: project, Zone: zone, Instance: vm2}); err == nil && i.GetStatus() == "RUNNING" {
			break
		}
	}
	if i.GetStatus() != "RUNNING" || !strings.HasSuffix(i.GetMachineType(), "/e2-medium") {
		t.Fatalf("the VM restored through the page: %v %v", i, err)
	}
	if out := run(vm2, "cat /mnt/disks/data/note.txt"); strings.TrimSpace(out) != "original" {
		t.Errorf("the restored VM's data disk holds %q, want the backed-up file", out)
	}

	// Resource Type offers Compute Engine and Disk, enabled.
	var enabled string
	p.navigate("/backupdr-plans/create?project=" + project)
	// Resource Type's radios are named f-resourceType-choice; the hidden
	// input named resourceType holds the choice.
	radios := `[...document.querySelectorAll('input[type="radio"][name$="resourceType-choice"]')]`
	p.waitFor(radios + `.length > 0`)
	p.eval(radios+`.filter((r) => !r.disabled).map((r) => r.value).join(",")`, &enabled)
	if !strings.Contains(enabled, "compute.googleapis.com/Instance") || !strings.Contains(enabled, "compute.googleapis.com/Disk") {
		t.Errorf("Create Backup plan's enabled Resource Types: %s", enabled)
	}

	diskPage := "/backupdr-vaulted-backups/" + vault + "/" + dsOf["disk"] + "?project=" + project
	vmPage := "/backupdr-vaulted-backups/" + vault + "/" + dsOf["vm"] + "?project=" + project
	openRestore := `document.querySelector("#view tbody tr .overflow-trigger").click();
		[...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === "Restore").click()`
	auditScreens(t, []a11yPage{
		{"Vaulted backups, Resource type Disk", list + "&type=compute.googleapis.com/Disk", rowReady(data), ""},
		{"a disk's vaulted backups", diskPage, settled, ""},
		{"Restore a disk dialog", diskPage, settled + ` && document.querySelector("#view tbody tr") !== null`, openRestore},
		{"a VM's vaulted backups", vmPage, settled, ""},
		{"Restore a VM dialog", vmPage, settled + ` && document.querySelector("#view tbody tr") !== null`, openRestore},
	})
}
