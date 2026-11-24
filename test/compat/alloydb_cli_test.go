//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/grpc/codes"
)

// TestGcloudAlloyDB (#2191): gcloud alloydb, through gcloud-setup's
// alloydb override alone, creates a cluster and its primary instance,
// lists and describes them, creates a user and an on-demand backup, and
// deletes everything; the official client reads back what gcloud made.
func TestGcloudAlloyDB(t *testing.T) {
	h := New(t)
	c := alloyDBClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_ALLOYDB
	ctx := alloyDBWait(t)
	g := newGcloudSession(t, h)
	run := func(a ...string) string {
		t.Helper()
		out, err := g.run(nil, append(append([]string{"alloydb"}, a...), "--project", g.project, "--quiet")...)
		if err != nil {
			t.Fatalf("gcloud alloydb %s: %v\n%s", strings.Join(a, " "), err, out)
		}
		return out
	}
	id := fmt.Sprintf("gc-%d", time.Now().UnixNano()%1e6)
	name := "projects/" + g.project + "/locations/us-central1/clusters/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(h.Context(), &alloydbpb.DeleteClusterRequest{Name: name, Force: true}); err == nil {
			_ = op.Wait(alloyDBWait(t))
		}
	})
	run("clusters", "create", id, "--region", "us-central1", "--password", "burrow-pw", "--network", "default")
	cl, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name})
	if err != nil || cl.GetState() != alloydbpb.Cluster_READY || !strings.HasSuffix(cl.GetNetworkConfig().GetNetwork(), "/networks/default") {
		t.Fatalf("the cluster gcloud created: %v, %v", cl, err)
	}
	if out := run("clusters", "list", "--region", "us-central1", "--format", "value(name)"); !strings.Contains(out, id) {
		t.Errorf("gcloud alloydb clusters list: %s", out)
	}
	run("instances", "create", "primary", "--cluster", id, "--region", "us-central1", "--instance-type", "PRIMARY", "--cpu-count", "2")
	if out := run("instances", "describe", "primary", "--cluster", id, "--region", "us-central1", "--format", "value(state,instanceType)"); !strings.Contains(out, "READY") || !strings.Contains(out, "PRIMARY") {
		t.Errorf("gcloud alloydb instances describe: %s", out)
	}
	run("users", "create", "app", "--cluster", id, "--region", "us-central1", "--password", "app-pw", "--db-roles", "alloydbsuperuser")
	if out := run("users", "list", "--cluster", id, "--region", "us-central1", "--format", "value(name)"); !strings.Contains(out, "/users/app") {
		t.Errorf("gcloud alloydb users list: %s", out)
	}
	run("backups", "create", id+"-b", "--cluster", id, "--region", "us-central1")
	b, err := c.GetBackup(ctx, &alloydbpb.GetBackupRequest{Name: "projects/" + g.project + "/locations/us-central1/backups/" + id + "-b"})
	if err != nil || b.GetState() != alloydbpb.Backup_READY {
		t.Errorf("the backup gcloud created: %v, %v", b, err)
	}
	run("backups", "delete", id+"-b", "--region", "us-central1")
	run("users", "delete", "app", "--cluster", id, "--region", "us-central1")
	run("instances", "delete", "primary", "--cluster", id, "--region", "us-central1")
	run("clusters", "delete", id, "--region", "us-central1")
	if _, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name}); alloyDBCode(err) != codes.NotFound {
		t.Errorf("after gcloud delete: %v, want NOT_FOUND", err)
	}
}

// TestTerraformAlloyDB (#2191): google_alloydb_cluster,
// google_alloydb_instance, google_alloydb_user and google_alloydb_backup
// apply against this instance through alloydb_custom_endpoint, plan
// clean, update the instance's flags in place and destroy; the official
// client reads back what Terraform made.
func TestTerraformAlloyDB(t *testing.T) {
	h := New(t)
	c := alloyDBClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_ALLOYDB
	ctx := alloyDBWait(t)
	m := newTFModule(t)
	id := fmt.Sprintf("tf-%d", time.Now().UnixNano()%1e6)
	conf := func(statementTimeout string) string {
		return fmt.Sprintf(`
resource "google_alloydb_cluster" "c" {
  project    = %[1]q
  cluster_id = %[2]q
  location   = "us-central1"
  network_config {
    network = "projects/%[1]s/global/networks/default"
  }
  initial_user {
    password = "burrow-pw"
  }
  deletion_protection = false
}

resource "google_alloydb_instance" "primary" {
  cluster       = google_alloydb_cluster.c.name
  instance_id   = "primary"
  instance_type = "PRIMARY"
  machine_config {
    cpu_count = 2
  }
  database_flags = {
    statement_timeout = %[3]q
  }
}

resource "google_alloydb_user" "app" {
  cluster        = google_alloydb_cluster.c.name
  user_id        = "app"
  user_type      = "ALLOYDB_BUILT_IN"
  password       = "app-pw"
  database_roles = ["alloydbsuperuser"]
  depends_on     = [google_alloydb_instance.primary]
}

resource "google_alloydb_backup" "b" {
  project      = %[1]q
  location     = "us-central1"
  backup_id    = "%[2]s-b"
  cluster_name = google_alloydb_cluster.c.name
  depends_on   = [google_alloydb_instance.primary]
}
`, m.project, id, statementTimeout)
	}
	m.write(conf("30000"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/locations/us-central1/clusters/" + id
	in, err := c.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: name + "/instances/primary"})
	if err != nil || in.GetState() != alloydbpb.Instance_READY || in.GetDatabaseFlags()["statement_timeout"] != "30000" {
		t.Fatalf("the instance Terraform made: %v, %v", in, err)
	}
	if u, err := c.GetUser(ctx, &alloydbpb.GetUserRequest{Name: name + "/users/app"}); err != nil || len(u.GetDatabaseRoles()) != 1 {
		t.Fatalf("the user Terraform made: %v, %v", u, err)
	}
	m.planClean()
	m.write(conf("60000"))
	m.apply()
	if in, err := c.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: name + "/instances/primary"}); err != nil || in.GetDatabaseFlags()["statement_timeout"] != "60000" {
		t.Fatalf("after the update: %v, %v", in, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name}); alloyDBCode(err) != codes.NotFound {
		t.Errorf("after destroy: %v; want NOT_FOUND", err)
	}
}
