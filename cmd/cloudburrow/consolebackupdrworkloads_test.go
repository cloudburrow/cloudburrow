package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"

	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
)

// TestConsoleBackupDRFilestore (#1524): Backup and DR's pages over the
// in-process service and a Filestore over memory, joined by the adapters up
// wires: Create Backup plan for Filestore (Database Logs refused), Schedule
// backup offering the project's instances, the instance's Vaulted backups
// page with its rows, Create on-demand backup, and Restore into a new
// instance whose share holds the backed-up data while the source keeps its
// change. Without AlloyDB its type is drawn disabled with the reason.
func TestConsoleBackupDRFilestore(t *testing.T) {
	ctx := context.Background()
	const project = "bdr-fs"
	be := &filestore.MemoryBackend{}
	fapi, err := filestore.NewServer("", filestore.Options{Backend: be})
	if err != nil {
		t.Fatal(err)
	}
	defer fapi.Close()
	fsvc := &filestoreService{api: fapi, rest: filestore.NewRESTHandler(fapi)}
	srv := backupdr.NewServer("127.0.0.1:0", nil, "")
	useBackupDRWorkloads(srv, nil, fsvc)
	fapi.SetBackupDR(fsvc.backupDR)
	api := backupDRProvider{bdr: srv, filestore: fsvc.REST}
	vaults, plans, vaulted := api, bdrPlansProvider{api: api}, bdrVaultedProvider{api: api}

	full := "projects/" + project + "/locations/us-central1-a/instances/files"
	op, err := fapi.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/us-central1-a", InstanceId: "files",
		Instance: &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024}},
			Networks: []*filestorepb.NetworkConfig{{Network: "default"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op
	fsWaitReady(t, fapi, full)
	key := fapi.ServerKey(full)
	be.Write(key, "vol1", "original")

	_, fields := plans.CreateForm()
	if why := fields[0].UnavailableOptions[backupdr.AlloyDBResourceType]; !strings.Contains(why, "alloydb in --services") {
		t.Errorf("AlloyDB's choice without AlloyDB: %q", why)
	}
	if _, ok := fields[0].UnavailableOptions[backupdr.FilestoreResourceType]; ok {
		t.Errorf("Filestore's choice is disabled while Filestore is served")
	}
	if _, err := vaults.Create(ctx, project, map[string]string{"id": "vault", "locationType": "region", "region": "us-central1",
		"retentionDays": "1", "access": "WITHIN_PROJECT", "encryption": "google"}); err != nil {
		t.Fatal(err)
	}
	vault := "projects/" + project + "/locations/us-central1/backupVaults/vault"
	rules := `[{"ruleId":"daily","recurrence":"DAILY","hourly":"","start":"1","end":"7","retention":"7"}]`
	plan := map[string]string{"id": "fsplan", "region": "us-central1", "vault": vault, "resourceType": backupdr.FilestoreResourceType,
		"rules": rules, "timeZone": "UTC", "logs": "true", "logDays": "7"}
	if _, err := plans.Create(ctx, project, plan); err == nil || !strings.Contains(err.Error(), "Cloud SQL and AlloyDB") {
		t.Errorf("a Filestore plan with Database Logs: %v", err)
	}
	plan["logs"] = ""
	if _, err := plans.Create(ctx, project, plan); err != nil {
		t.Fatal(err)
	}
	planName := "projects/" + project + "/locations/us-central1/backupPlans/fsplan"

	// Schedule backup offers the instance.
	var offered []string
	for _, f := range vaulted.CreateFormFor(ctx, project) {
		if f.Name == "fsinstance" {
			offered = f.Options
		}
	}
	if len(offered) != 1 || offered[0] != "files" {
		t.Fatalf("Schedule backup's Filestore instances: %v", offered)
	}
	if _, err := vaulted.Create(ctx, project, map[string]string{"resourceType": backupdr.FilestoreResourceType, "region": "us-central1",
		"fsinstance": "files", "plan": planName}); err != nil {
		t.Fatalf("Schedule backup: %v", err)
	}
	l, err := vaulted.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "files" || l.Items[0].Fields["Resource type"] != "Filestore" {
		t.Fatalf("Vaulted backups: %+v, %v", l.Items, err)
	}
	path := l.Items[0].Opens
	if err := vaulted.ActAt(ctx, project, path, "ondemand", map[string]string{"rule": "daily"}); err != nil {
		t.Fatal(err)
	}
	var backup string
	for deadline := time.Now().Add(5 * time.Second); backup == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		d, _ := vaulted.Detail(ctx, project, path)
		if len(d.Sections) == 1 && len(d.Sections[0].Listing.Items) == 1 && d.Sections[0].Listing.Items[0].Status == "Succeeded" {
			backup = d.Sections[0].Listing.Items[0].Name
		}
	}
	if backup == "" {
		d, _ := vaulted.Detail(ctx, project, path)
		t.Fatalf("no active backup: %+v", d.Sections)
	}
	be.Write(key, "vol1", "changed")
	acts := bdrActionIDs(vaulted.DetailActions(ctx, project, path))
	if acts != "ondemand,removeplan,restore" {
		t.Errorf("the instance's actions: %s, want no point-in-time restore", acts)
	}
	var restore map[string]string
	for _, a := range vaulted.DetailActions(ctx, project, path) {
		if a.ID == "restore" {
			restore = map[string]string{}
			for _, f := range a.Fields {
				restore[f.Name] = f.Default
			}
		}
	}
	if restore["tier"] != "ZONAL" || restore["capacity"] != "1024" || restore["share"] != "vol1" || restore["location"] != "us-central1-a" {
		t.Errorf("Restore's defaults: %v", restore)
	}
	restore["backup"], restore["name"], restore["capacity"] = backup, "files-copy", "512"
	if err := vaulted.ActAt(ctx, project, path, "restore", restore); err == nil {
		t.Errorf("a restore below the source's capacity was accepted")
	}
	restore["capacity"] = "1024"
	if err := vaulted.ActAt(ctx, project, path, "restore", restore); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	copyName := "projects/" + project + "/locations/us-central1-a/instances/files-copy"
	fsWaitReady(t, fapi, copyName)
	if got := be.Read(fapi.ServerKey(copyName), "vol1"); got != "original" {
		t.Errorf("the restored share: %q, want original", got)
	}
	if got := be.Read(key, "vol1"); got != "changed" {
		t.Errorf("the source share: %q, want changed", got)
	}
	if out, err := api.call(ctx, srv, http.MethodGet, "/v1/projects/"+project+"/locations/us-central1/backupPlanAssociations", nil); err != nil ||
		!strings.Contains(strings.Join(bdrItemNames(out, "backupPlanAssociations"), ","), "files-") {
		t.Errorf("the association: %v, %v", out, err)
	}
}

func bdrItemNames(out map[string]any, key string) []string {
	var names []string
	for _, x := range bdrItems(out, key) {
		names = append(names, str(x, "name"))
	}
	return names
}

// fsWaitReady waits for a Filestore instance to be READY.
func fsWaitReady(t *testing.T, api *filestore.Server, name string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if in, err := api.GetInstance(context.Background(), &filestorepb.GetInstanceRequest{Name: name}); err == nil && in.GetState() == filestorepb.Instance_READY {
			return
		}
	}
	t.Fatalf("instance %s never became READY", name)
}
