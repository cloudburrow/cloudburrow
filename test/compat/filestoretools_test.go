//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gcloudFilestore is an isolated gcloud configured by `cloudburrow
// gcloud-setup`, whose file override is the harness's Filestore endpoint.
func gcloudFilestore(t *testing.T, h *Harness) (func(args ...string) (string, error), string) {
	t.Helper()
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	h.Endpoint(EnvFilestore) // skips without CLOUDBURROW_TEST_FILESTORE
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, cliArgs()...)...).Output()
	var st struct{ Project string }
	if err := json.Unmarshal(out, &st); err != nil || st.Project == "" {
		t.Fatalf("status gave no project: %s", out)
	}
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	if conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name)); !strings.Contains(string(conf), "file = http://") {
		t.Fatalf("the configuration has no file override:\n%s", conf)
	}
	return func(args ...string) (string, error) {
		cmd := exec.Command(gcloud, append(args, "--quiet", "--project", st.Project)...)
		cmd.Env = append(gcloudTargetEnv(os.Environ()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}, st.Project
}

// TestGcloudFilestore (#2192): gcloud filestore, configured by `cloudburrow
// gcloud-setup` alone, creates a Zonal instance in its reserved range,
// describes, lists and updates it, takes a snapshot and reverts to it,
// makes a backup and restores a Basic HDD instance from one, and deletes
// them; the official client checks each step.
//
// covers: google.cloud.filestore.v1.CloudFilestoreManager/CreateInstance, google.cloud.filestore.v1.CloudFilestoreManager/GetInstance, google.cloud.filestore.v1.CloudFilestoreManager/ListInstances, google.cloud.filestore.v1.CloudFilestoreManager/UpdateInstance, google.cloud.filestore.v1.CloudFilestoreManager/DeleteInstance, google.cloud.filestore.v1.CloudFilestoreManager/CreateSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/DeleteSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/RevertInstance, google.cloud.filestore.v1.CloudFilestoreManager/CreateBackup, google.cloud.filestore.v1.CloudFilestoreManager/DeleteBackup, google.cloud.filestore.v1.CloudFilestoreManager/RestoreInstance
func TestGcloudFilestore(t *testing.T) {
	h := New(t)
	run, project := gcloudFilestore(t, h)
	fg, _ := filestoreClients(t, h)
	ctx := fsCtx(t)
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	zi, bi := "gc-fs-z"+suffix, "gc-fs-b"+suffix
	zone := "projects/" + project + "/locations/us-central1-a"
	t.Cleanup(func() {
		for _, id := range []string{zi, bi} {
			_, _ = run("filestore", "instances", "delete", id, "--zone=us-central1-a", "--force")
		}
		_, _ = run("filestore", "backups", "delete", "gc-bk"+suffix, "--region=us-central1")
	})
	gc("filestore", "instances", "create", zi, "--zone=us-central1-a", "--tier=ZONAL", "--protocol=NFS_V4_1",
		"--file-share=name=vol1,capacity=1TiB", "--network=name=default,reserved-ip-range=10.124.0.0/26", "--labels=env=a")
	in, err := fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: zone + "/instances/" + zi})
	if err != nil || in.GetTier() != filestorepb.Instance_ZONAL || in.GetProtocol() != filestorepb.Instance_NFS_V4_1 ||
		in.GetNetworks()[0].GetIpAddresses()[0] != "10.124.0.2" || in.GetLabels()["env"] != "a" {
		t.Fatalf("after create, GetInstance = %v, %v", in, err)
	}
	var desc struct {
		Networks []struct {
			IPAddresses     []string `json:"ipAddresses"`
			ReservedIPRange string   `json:"reservedIpRange"`
		} `json:"networks"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(gc("filestore", "instances", "describe", zi, "--zone=us-central1-a", "--format=json")), &desc); err != nil ||
		desc.State != "READY" || desc.Networks[0].IPAddresses[0] != "10.124.0.2" || desc.Networks[0].ReservedIPRange != "10.124.0.0/26" {
		t.Fatalf("describe = %+v, %v", desc, err)
	}
	if out := gc("filestore", "instances", "list", "--zone=us-central1-a", "--format=value(name)"); !strings.Contains(out, zi) {
		t.Fatalf("list lacks %s:\n%s", zi, out)
	}
	gc("filestore", "instances", "update", zi, "--zone=us-central1-a", "--description=updated", "--update-labels=env=b")
	if in, err = fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: zone + "/instances/" + zi}); err != nil || in.GetDescription() != "updated" || in.GetLabels()["env"] != "b" {
		t.Fatalf("after update, GetInstance = %v, %v", in, err)
	}
	gc("filestore", "instances", "snapshots", "create", "gc-snap", "--instance="+zi, "--instance-location=us-central1-a")
	if _, err := fg.GetSnapshot(ctx, &filestorepb.GetSnapshotRequest{Name: zone + "/instances/" + zi + "/snapshots/gc-snap"}); err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	gc("filestore", "instances", "revert", zi, "--location=us-central1-a", "--target-snapshot=gc-snap")
	gc("filestore", "backups", "create", "gc-bk"+suffix, "--region=us-central1", "--instance="+zi, "--instance-zone=us-central1-a", "--file-share=vol1")
	if b, err := fg.GetBackup(ctx, &filestorepb.GetBackupRequest{Name: "projects/" + project + "/locations/us-central1/backups/gc-bk" + suffix}); err != nil || b.GetState() != filestorepb.Backup_READY {
		t.Fatalf("GetBackup = %v, %v", b, err)
	}
	// A Basic HDD instance, backed up and restored in place.
	gc("filestore", "instances", "create", bi, "--zone=us-central1-a", "--tier=BASIC_HDD", "--file-share=name=share1,capacity=1TiB", "--network=name=default")
	gc("filestore", "backups", "create", "gc-bb"+suffix, "--region=us-central1", "--instance="+bi, "--instance-zone=us-central1-a", "--file-share=share1")
	gc("filestore", "instances", "restore", bi, "--zone=us-central1-a", "--file-share=share1", "--source-backup=gc-bb"+suffix, "--source-backup-region=us-central1")
	if in, err = fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: zone + "/instances/" + bi}); err != nil || !strings.HasSuffix(in.GetFileShares()[0].GetSourceBackup(), "/backups/gc-bb"+suffix) {
		t.Fatalf("after restore, GetInstance = %v, %v", in, err)
	}
	gc("filestore", "backups", "delete", "gc-bb"+suffix, "--region=us-central1")
	gc("filestore", "instances", "snapshots", "delete", "gc-snap", "--instance="+zi, "--instance-location=us-central1-a")
	gc("filestore", "instances", "delete", zi, "--zone=us-central1-a")
	gc("filestore", "instances", "delete", bi, "--zone=us-central1-a")
	gc("filestore", "backups", "delete", "gc-bk"+suffix, "--region=us-central1")
	for _, id := range []string{zi, bi} {
		if _, err := fg.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: zone + "/instances/" + id}); status.Code(err) != codes.NotFound {
			t.Errorf("GetInstance %s after delete = %v, want NOT_FOUND", id, err)
		}
	}
}

// TestTerraformFilestore (#2192): a google_filestore_instance (Zonal, in a
// reserved range), a google_filestore_snapshot of it and a
// google_filestore_backup of its share apply with the official
// hashicorp/google provider through `cloudburrow terraform`
// (filestore_custom_endpoint), the second plan is empty, a label and the
// capacity change in place, the plan after it is empty, and destroy
// removes them.
//
// covers: google.cloud.filestore.v1.CloudFilestoreManager/CreateInstance, google.cloud.filestore.v1.CloudFilestoreManager/GetInstance, google.cloud.filestore.v1.CloudFilestoreManager/UpdateInstance, google.cloud.filestore.v1.CloudFilestoreManager/DeleteInstance, google.cloud.filestore.v1.CloudFilestoreManager/CreateSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/GetSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/DeleteSnapshot, google.cloud.filestore.v1.CloudFilestoreManager/CreateBackup, google.cloud.filestore.v1.CloudFilestoreManager/GetBackup, google.cloud.filestore.v1.CloudFilestoreManager/DeleteBackup
func TestTerraformFilestore(t *testing.T) {
	h := New(t)
	fg, _ := filestoreClients(t, h)
	m := newTFModule(t)
	id := fmt.Sprintf("tf-fs-%d", time.Now().UnixNano()%1e6)
	zone := "projects/" + m.project + "/locations/us-central1-a"
	module := func(env string, capacity int) string {
		return fmt.Sprintf(`
resource "google_filestore_instance" "i" {
  project  = %q
  name     = %q
  location = "us-central1-a"
  tier     = "ZONAL"
  protocol = "NFS_V4_1"
  labels   = { env = %q }
  file_shares {
    name        = "vol1"
    capacity_gb = %d
  }
  networks {
    network           = "default"
    modes             = ["MODE_IPV4"]
    reserved_ip_range = "10.125.0.0/26"
  }
}
resource "google_filestore_snapshot" "s" {
  project  = %q
  name     = "tf-snap"
  instance = google_filestore_instance.i.name
  location = "us-central1-a"
}
resource "google_filestore_backup" "b" {
  project           = %q
  name              = "%s-bk"
  location          = "us-central1"
  source_instance   = google_filestore_instance.i.id
  source_file_share = "vol1"
}
output "ip" {
  value = google_filestore_instance.i.networks[0].ip_addresses[0]
}
`, m.project, id, env, capacity, m.project, m.project, id)
	}
	t.Cleanup(func() {
		if op, err := fg.DeleteInstance(h.Context(), &filestorepb.DeleteInstanceRequest{Name: zone + "/instances/" + id, Force: true}); err == nil {
			_ = op.Wait(h.Context())
		}
		if op, err := fg.DeleteBackup(h.Context(), &filestorepb.DeleteBackupRequest{Name: "projects/" + m.project + "/locations/us-central1/backups/" + id + "-bk"}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	m.write(module("a", 1024))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	in, err := fg.GetInstance(h.Context(), &filestorepb.GetInstanceRequest{Name: zone + "/instances/" + id})
	if err != nil || in.GetState() != filestorepb.Instance_READY || in.GetLabels()["env"] != "a" {
		t.Fatalf("after apply, GetInstance = %v, %v", in, err)
	}
	if ip := strings.TrimSpace(m.stdout("output", "-raw", "ip")); ip != "10.125.0.2" {
		t.Errorf("ip output = %q, want 10.125.0.2", ip)
	}
	m.planClean()
	m.write(module("b", 1280))
	m.apply()
	if in, err = fg.GetInstance(h.Context(), &filestorepb.GetInstanceRequest{Name: zone + "/instances/" + id}); err != nil ||
		in.GetLabels()["env"] != "b" || in.GetFileShares()[0].GetCapacityGb() != 1280 {
		t.Errorf("after the update, GetInstance = %v, %v", in, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := fg.GetInstance(h.Context(), &filestorepb.GetInstanceRequest{Name: zone + "/instances/" + id}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after destroy = %v, want NOT_FOUND", err)
	}
}
