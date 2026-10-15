//go:build compat

package compat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformBigtableInstance (#1331), in the emulators shard: a
// google_bigtable_instance with one cluster, and a google_bigtable_table in
// it, apply through `cloudburrow terraform` with the official
// hashicorp/google provider, read back through the official instance admin
// and table admin clients, plan clean, take an in-place update (the display
// name, a label and the cluster's node count), plan clean again and destroy,
// all with egress blocked. The provider's instance admin client reads
// BIGTABLE_EMULATOR_HOST, which the wrapper sets, as its table clients do;
// the instance admin API is the Bigtable front's own (internal/bigtablefront).
func TestTerraformBigtableInstance(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id, cluster, table = "tf-bt-inst", "tf-bt-inst-c1", "tf-in-instance"
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	flags := cliArgs()
	dir := t.TempDir()

	write := func(display, env string, nodes int) {
		t.Helper()
		module := googleProviderRequirement + fmt.Sprintf(`resource "google_bigtable_instance" "i" {
  project             = %q
  name                = %q
  display_name        = %q
  deletion_protection = false
  labels              = { env = %q }
  cluster {
    cluster_id   = %q
    zone         = "us-central1-b"
    num_nodes    = %d
    storage_type = "SSD"
  }
}
resource "google_bigtable_table" "t" {
  project             = %q
  instance_name       = google_bigtable_instance.i.name
  name                = %q
  deletion_protection = "UNPROTECTED"
  column_family { family = "cf" }
}
`, h.Project(), id, display, env, cluster, nodes, h.Project(), table)
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(module), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tf := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), args...)...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if args[0] != "init" {
			cmd.Env = noGoogleEgress()
		}
		// The provider's Bigtable address comes from the wrapper.
		cmd.Env = append(cmd.Env, "BIGTABLE_EMULATOR_HOST=")
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	run := func(args ...string) {
		t.Helper()
		out, err := tf(args...)
		if err != nil {
			t.Fatalf("cloudburrow terraform %s: %v\n%s", strings.Join(args, " "), err, lastLines(out, 30))
		}
		t.Logf("terraform %s:\n%s", args[0], lastLines(out, 6))
	}
	cleanPlan := func(when string) {
		t.Helper()
		if out, err := tf("plan", "-detailed-exitcode", "-input=false", "-no-color"); err != nil {
			t.Errorf("plan %s is not clean (%v):\n%s", when, err, lastLines(out, 40))
		}
	}
	check := func(when, display, env string, nodes int32) {
		t.Helper()
		info, err := iac.InstanceInfo(ctx, id)
		if err != nil {
			t.Fatalf("the instance %s: %v", when, err)
		}
		if info.DisplayName != display || info.Labels["env"] != env || info.InstanceType != bigtable.PRODUCTION {
			t.Errorf("the instance %s = %+v", when, info)
		}
		c, err := iac.GetCluster(ctx, id, cluster)
		if err != nil {
			t.Fatalf("the cluster %s: %v", when, err)
		}
		if c.ServeNodes != int(nodes) || c.Zone != "us-central1-b" || c.StorageType != bigtable.SSD {
			t.Errorf("the cluster %s = %+v", when, c)
		}
		if _, err := admin.TableInfo(ctx, table); err != nil {
			t.Errorf("the table %s: %v", when, err)
		}
	}

	write("Terraform instance", "dev", 1)
	useGoogleProviderLock(t, dir)
	run("init", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color")
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	check("after apply", "Terraform instance", "dev", 1)
	cleanPlan("right after apply")

	write("Terraform renamed", "test", 2)
	out, err := tf("plan", "-input=false", "-no-color")
	if err != nil || !strings.Contains(out, "0 to add, 1 to change, 0 to destroy") {
		t.Errorf("the update is not planned in place (%v):\n%s", err, lastLines(out, 30))
	}
	run("apply", "-auto-approve", "-input=false", "-no-color")
	check("after the update", "Terraform renamed", "test", 2)
	cleanPlan("after the update")

	run("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := iac.InstanceInfo(ctx, id); status.Code(err) != codes.NotFound {
		t.Errorf("the instance after destroy: %v, want NotFound", err)
	}
	if _, err := admin.TableInfo(ctx, table); status.Code(err) != codes.NotFound {
		t.Errorf("the table after destroy: %v, want NotFound", err)
	}
}
