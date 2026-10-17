//go:build compat

package compat

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Terraform and gcloud on the Bigtable resources the front keeps (#1499),
// through the bigtableadmin REST API it serves beside gRPC.

// TestTerraformBigtableViews: google_bigtable_authorized_view,
// google_bigtable_logical_view and google_bigtable_materialized_view through
// `cloudburrow terraform`, read back with the official admin clients, plan
// clean, and destroyed.
// covers: google.bigtable.admin.v2.BigtableTableAdmin/CreateAuthorizedView, google.bigtable.admin.v2.BigtableTableAdmin/GetAuthorizedView, google.bigtable.admin.v2.BigtableTableAdmin/DeleteAuthorizedView
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateLogicalView, google.bigtable.admin.v2.BigtableInstanceAdmin/GetLogicalView, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteLogicalView
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateMaterializedView, google.bigtable.admin.v2.BigtableInstanceAdmin/GetMaterializedView, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteMaterializedView
func TestTerraformBigtableViews(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h) // skips without CLOUDBURROW_TEST_BIGTABLE
	m := newTFModule(t)
	ctx := h.Context()
	const id, cluster, table = "tf-bt-views", "tf-bt-views-c1", "tfviews"
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	m.write(fmt.Sprintf(`
resource "google_bigtable_instance" "i" {
  project             = %q
  name                = %q
  deletion_protection = false
  cluster {
    cluster_id   = %q
    zone         = "us-central1-b"
    num_nodes    = 1
    storage_type = "SSD"
  }
}
resource "google_bigtable_table" "t" {
  project             = google_bigtable_instance.i.project
  instance_name       = google_bigtable_instance.i.name
  name                = %q
  deletion_protection = "UNPROTECTED"
  column_family { family = "cf" }
}
resource "google_bigtable_authorized_view" "v" {
  project             = google_bigtable_table.t.project
  instance_name       = google_bigtable_table.t.instance_name
  table_name          = google_bigtable_table.t.name
  name                = "eu"
  deletion_protection = "UNPROTECTED"
  subset_view {
    row_prefixes = [%q]
    family_subsets {
      family_name        = "cf"
      qualifier_prefixes = [""]
    }
  }
}
resource "google_bigtable_logical_view" "l" {
  project             = google_bigtable_instance.i.project
  instance            = google_bigtable_instance.i.name
  logical_view_id     = "lv"
  query               = "SELECT _key FROM %s"
  deletion_protection = false
  depends_on          = [google_bigtable_table.t] # the query is compiled against the table (#1511)
}
resource "google_bigtable_materialized_view" "mv" {
  project              = google_bigtable_instance.i.project
  instance             = google_bigtable_instance.i.name
  materialized_view_id = "mv"
  query                = "SELECT _key, count(*) AS n FROM %s GROUP BY _key"
  deletion_protection  = false
  depends_on           = [google_bigtable_table.t]
}
`, h.Project(), id, cluster, table, base64.StdEncoding.EncodeToString([]byte("eu#")), table, table))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if info, err := ac.AuthorizedViewInfo(ctx, table, "eu"); err != nil || string(info.AuthorizedView.(*bigtable.SubsetViewInfo).RowPrefixes[0]) != "eu#" {
		t.Errorf("AuthorizedViewInfo = %+v, %v", info, err)
	}
	if lv, err := iac.LogicalViewInfo(ctx, id, "lv"); err != nil || lv.Query != "SELECT _key FROM "+table {
		t.Errorf("LogicalViewInfo = %+v, %v", lv, err)
	}
	if mv, err := iac.MaterializedViewInfo(ctx, id, "mv"); err != nil || !strings.Contains(mv.Query, "GROUP BY") {
		t.Errorf("MaterializedViewInfo = %+v, %v", mv, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := iac.LogicalViewInfo(ctx, id, "lv"); status.Code(err) != codes.NotFound {
		t.Errorf("LogicalViewInfo after destroy = %v, want NotFound", err)
	}
}

// TestGcloudBigtableBackups: gcloud bigtable backups create, list,
// describe and delete, and instances tables restore, configured by
// gcloud-setup alone, egress to Google blocked, read back with the official
// admin client.
// covers: google.bigtable.admin.v2.BigtableTableAdmin/CreateBackup, google.bigtable.admin.v2.BigtableTableAdmin/ListBackups, google.bigtable.admin.v2.BigtableTableAdmin/GetBackup, google.bigtable.admin.v2.BigtableTableAdmin/DeleteBackup, google.bigtable.admin.v2.BigtableTableAdmin/RestoreTable
func TestGcloudBigtableBackups(t *testing.T) {
	h := New(t)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	iac := bigtableInstanceAdmin(t, h) // skips without CLOUDBURROW_TEST_BIGTABLE
	ctx := h.Context()
	const id, cluster, table = "gc-bt-bk", "gc-bt-bk-c1", "gcbk"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "gcloud backups",
		ClusterId: cluster, Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	if err := ac.CreateTable(ctx, table); err != nil {
		t.Fatal(err)
	}
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	gc := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	gc("bigtable", "backups", "create", "nightly", "--instance="+id, "--cluster="+cluster, "--table="+table, "--retention-period=2d")
	info, err := ac.BackupInfo(ctx, cluster, "nightly")
	if err != nil || info.SourceTable != table || time.Until(info.ExpireTime) < 47*time.Hour {
		t.Fatalf("BackupInfo = %+v, %v", info, err)
	}
	if out := gc("bigtable", "backups", "list", "--instance="+id, "--cluster="+cluster, "--format=value(name)"); !strings.Contains(out, "nightly") {
		t.Errorf("gcloud bigtable backups list:\n%s", out)
	}
	if out := gc("bigtable", "backups", "describe", "nightly", "--instance="+id, "--cluster="+cluster, "--format=value(state)"); !strings.Contains(out, "READY") {
		t.Errorf("gcloud bigtable backups describe:\n%s", out)
	}
	gc("bigtable", "instances", "tables", "restore", "--source="+"projects/"+h.Project()+"/instances/"+id+"/clusters/"+cluster+"/backups/nightly",
		"--destination="+table+"-r", "--destination-instance="+id)
	if _, err := ac.TableInfo(ctx, table+"-r"); err != nil {
		t.Errorf("the restored table: %v", err)
	}
	gc("bigtable", "backups", "delete", "nightly", "--instance="+id, "--cluster="+cluster)
	if _, err := ac.BackupInfo(ctx, cluster, "nightly"); status.Code(err) != codes.NotFound {
		t.Errorf("BackupInfo after gcloud delete = %v, want NotFound", err)
	}
}
