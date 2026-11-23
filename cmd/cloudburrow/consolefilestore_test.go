package main

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/filestore/apiv1/filestorepb"

	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
)

// TestConsoleFilestore (#2192): the Filestore pages' providers over a
// server with an in-memory backend: Create instance makes a Zonal NFSv4.1
// instance; its page shows the mount command; Edit grows it; Create
// snapshot, Revert and Create backup act on it; the Backups page restores
// the backup to a new instance and deletes it; the Snapshots page deletes
// the snapshot; Delete removes the instance. A Basic HDD instance offers
// no snapshot or revert and restores in place.
func TestConsoleFilestore(t *testing.T) {
	ctx := context.Background()
	const project = "fs-console"
	be := &filestore.MemoryBackend{}
	api, err := filestore.NewServer("", filestore.Options{Backend: be})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	svc := &filestoreService{api: api}
	inst, backups, snaps := filestoreProvider{svc: svc}, filestoreBackupsProvider{svc: svc}, filestoreSnapshotsProvider{svc: svc}

	form := func(over map[string]string) map[string]string {
		v := map[string]string{}
		_, fields := inst.CreateForm()
		for _, f := range fields {
			v[f.Name] = f.Default
		}
		for k, x := range over {
			v[k] = x
		}
		return v
	}
	name, err := inst.Create(ctx, project, form(map[string]string{"instanceId": "files", "tier": "ZONAL", "protocol": "NFS_V4_1", "labels": `{"team":"a"}`}))
	if err != nil || name != "us-central1-a/files" {
		t.Fatalf("Create instance: %q, %v", name, err)
	}
	if _, err := inst.Create(ctx, project, form(map[string]string{"instanceId": "small", "tier": "BASIC_SSD", "capacityGb": "1024"})); err == nil {
		t.Fatal("a 1 TiB Basic SSD instance was created")
	}
	l, err := inst.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != name {
		t.Fatalf("Instances: %+v, %v", l, err)
	}
	d, err := inst.Detail(ctx, project, []string{name})
	if err != nil || !strings.Contains(appHubPageText(d), "mount") {
		t.Fatalf("the instance's page has no mount command: %+v, %v", d, err)
	}
	ids := func(path string) map[string]bool {
		out := map[string]bool{}
		for _, a := range inst.DetailActions(ctx, project, []string{path}) {
			out[a.ID] = true
		}
		return out
	}
	if got := ids(name); !got["snapshot"] || !got["backup"] || !got["edit"] || !got["delete"] {
		t.Fatalf("a Zonal instance's actions: %v", got)
	}
	if err := inst.ActAt(ctx, project, []string{name}, "edit", map[string]string{"capacityGb": "1536", "labels": `{"team":"b"}`, "description": "edited"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	full := "projects/" + project + "/locations/us-central1-a/instances/files"
	if got, _ := api.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: full}); got.GetFileShares()[0].GetCapacityGb() != 1536 || got.GetLabels()["team"] != "b" {
		t.Fatalf("the edited instance: %v", got)
	}
	key := api.ServerKey(full)
	be.Write(key, "vol1", "one")
	if err := inst.ActAt(ctx, project, []string{name}, "snapshot", map[string]string{"snapshotId": "snap1"}); err != nil {
		t.Fatalf("Create snapshot: %v", err)
	}
	be.Write(key, "vol1", "two")
	if err := inst.ActAt(ctx, project, []string{name}, "backup", map[string]string{"backupId": "bk1", "region": "us-central1"}); err != nil {
		t.Fatalf("Create backup: %v", err)
	}
	if !ids(name)["revert"] {
		t.Fatal("no Revert with a snapshot")
	}
	if err := inst.ActAt(ctx, project, []string{name}, "revert", map[string]string{"snapshotId": "snap1"}); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := be.Read(key, "vol1"); got != "one" {
		t.Fatalf("after Revert: %q", got)
	}

	bl, err := backups.List(ctx, project)
	if err != nil || len(bl.Items) != 1 || bl.Items[0].Name != "us-central1/bk1" {
		t.Fatalf("Backups: %+v, %v", bl, err)
	}
	var restore map[string]string
	for _, a := range backups.DetailActions(ctx, project, []string{"us-central1/bk1"}) {
		if a.ID == "restore" {
			restore = map[string]string{}
			for _, f := range a.Fields {
				restore[f.Name] = f.Default
			}
		}
	}
	if restore == nil || restore["capacityGb"] != "1536" || restore["tier"] != "ZONAL" || restore["protocol"] != "NFS_V4_1" {
		t.Fatalf("Restore's form: %v", restore)
	}
	restore["instanceId"], restore["zone"] = "restored", "us-central1-b"
	if err := backups.ActAt(ctx, project, []string{"us-central1/bk1"}, "restore", restore); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := be.Read(api.ServerKey("projects/"+project+"/locations/us-central1-b/instances/restored"), "vol1"); got != "two" {
		t.Fatalf("the restored instance's files: %q", got)
	}
	if err := backups.ActAt(ctx, project, []string{"us-central1/bk1"}, "edit", map[string]string{"description": "kept", "labels": `{"k":"v"}`}); err != nil {
		t.Fatalf("Edit backup: %v", err)
	}

	sl, err := snaps.List(ctx, project)
	if err != nil || len(sl.Items) != 1 || sl.Items[0].Name != "us-central1-a/files/snap1" {
		t.Fatalf("Snapshots: %+v, %v", sl, err)
	}
	if err := inst.ActAt(ctx, project, []string{name}, "delete", nil); err == nil {
		t.Fatal("an instance with a snapshot was deleted without force")
	}
	if err := snaps.Act(ctx, project, "us-central1-a/files/snap1", "delete"); err != nil {
		t.Fatalf("Delete snapshot: %v", err)
	}
	if err := inst.ActAt(ctx, project, []string{name}, "delete", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := backups.ActAt(ctx, project, []string{"us-central1/bk1"}, "delete", nil); err != nil {
		t.Fatalf("Delete backup: %v", err)
	}

	basic, err := inst.Create(ctx, project, form(map[string]string{"instanceId": "basic", "shareName": "share1"}))
	if err != nil {
		t.Fatalf("Create a Basic HDD instance: %v", err)
	}
	if got := ids(basic); got["snapshot"] || got["revert"] {
		t.Fatalf("a Basic instance's actions: %v", got)
	}
	if err := inst.ActAt(ctx, project, []string{basic}, "backup", map[string]string{"backupId": "bb", "region": "us-central1"}); err != nil {
		t.Fatalf("Create backup of a Basic instance: %v", err)
	}
	if !ids(basic)["restore"] {
		t.Fatal("a Basic instance with a backup offers no Restore")
	}
	if err := inst.ActAt(ctx, project, []string{basic}, "restore", map[string]string{"backup": "us-central1/bb"}); err != nil {
		t.Fatalf("Restore in place: %v", err)
	}
}
