//go:build compat

package compat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformFirestoreDocumentAndBigtableTable (#1331), in the emulators
// shard: a google_firestore_document and a google_bigtable_table apply
// through `cloudburrow terraform` with the official hashicorp/google
// provider, read back through the official Firestore and Bigtable clients,
// plan clean, take an in-place update (the document's fields, a second
// column family), plan clean again and destroy, all with egress blocked.
//
// The document goes over the documents REST API at firestore_custom_endpoint;
// the table over gRPC at BIGTABLE_EMULATOR_HOST, which the wrapper sets for
// the provider. The admin resources the emulators do not serve
// (google_firestore_database and _index, the Firestore Admin API;
// google_bigtable_instance, the instance admin API) fail locally, and
// google_spanner_* is refused by the wrapper: docs/compatibility.md.
func TestTerraformFirestoreDocumentAndBigtableTable(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if os.Getenv(EnvFirestore) == "" || os.Getenv(EnvBigtable) == "" {
		t.Skipf("%s and %s must both be set", EnvFirestore, EnvBigtable)
	}
	fs := firestoreClient(t, h)
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	ctx := h.Context()
	const instance = "tf-instance"
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), instance)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	flags := cliArgs()
	dir := t.TempDir()
	collection, docID, table := "tf-docs", "d-"+h.Project(), "tf-table"

	write := func(value string, families ...string) {
		t.Helper()
		var cf strings.Builder
		for _, f := range families {
			fmt.Fprintf(&cf, "  column_family { family = %q }\n", f)
		}
		module := googleProviderRequirement + fmt.Sprintf(`resource "google_firestore_document" "d" {
  project     = %q
  collection  = %q
  document_id = %q
  fields      = jsonencode({ name = { stringValue = %q } })
}
resource "google_bigtable_table" "t" {
  project             = %q
  instance_name       = %q
  name                = %q
  deletion_protection = "UNPROTECTED"
%s}
`, h.Project(), collection, docID, value, h.Project(), instance, table, cf.String())
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
		// The provider's Bigtable address comes from the wrapper, not from
		// this test's environment.
		cmd.Env = append(cmd.Env, "BIGTABLE_EMULATOR_HOST=")
		b, err := cmd.CombinedOutput()
		if left, _ := filepath.Glob(filepath.Join(dir, "cloudburrow_providers*")); len(left) != 0 {
			t.Fatalf("the wrapper left %v behind after %s", left, args[0])
		}
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
	check := func(when, value string, families ...string) {
		t.Helper()
		snap, err := fs.Collection(collection).Doc(docID).Get(ctx)
		if err != nil {
			t.Fatalf("the document %s: %v", when, err)
		}
		if got, _ := snap.DataAt("name"); got != value {
			t.Errorf("the document's name %s = %v, want %q", when, got, value)
		}
		info, err := admin.TableInfo(ctx, table)
		if err != nil {
			t.Fatalf("the table %s: %v", when, err)
		}
		got := append([]string(nil), info.Families...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(families, ",") {
			t.Errorf("the table's families %s = %v, want %v", when, got, families)
		}
	}

	write("one", "cf")
	useGoogleProviderLock(t, dir)
	run("init", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color")
	check("after apply", "one", "cf")
	cleanPlan("right after apply")

	write("two", "cf", "cf2")
	out, err := tf("plan", "-input=false", "-no-color")
	if err != nil || !strings.Contains(out, "0 to add, 2 to change, 0 to destroy") {
		t.Errorf("the update is not planned in place (%v):\n%s", err, lastLines(out, 20))
	}
	run("apply", "-auto-approve", "-input=false", "-no-color")
	check("after the update", "two", "cf", "cf2")
	cleanPlan("after the update")

	run("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := fs.Collection(collection).Doc(docID).Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("the document after destroy: %v, want NotFound", err)
	}
	if _, err := admin.TableInfo(ctx, table); status.Code(err) != codes.NotFound {
		t.Errorf("the table after destroy: %v, want NotFound", err)
	}
}

// TestTerraformRefusesSpanner (#1331): the wrapper cannot point the
// provider's Spanner resources at the instance, so it refuses to plan a
// module that declares one rather than let it reach Google, and leaves no
// provider file behind. validate, which calls no API, still runs.
func TestTerraformRefusesSpanner(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	dir := t.TempDir()
	module := googleProviderRequirement + fmt.Sprintf(`resource "google_spanner_instance" "i" {
  project      = %q
  name         = "tf-spanner"
  config       = "emulator-config"
  display_name = "tf"
  num_nodes    = 1
}
`, h.Project())
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cli, append(append(append([]string{"terraform"}, cliArgs()...), "--"), "plan", "-input=false", "-no-color")...)
	cmd.Dir, cmd.Env = dir, noGoogleEgress()
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "refusing to run terraform plan") ||
		!strings.Contains(string(out), "google_spanner_instance") {
		t.Errorf("plan with a Spanner instance = %v:\n%s; want the wrapper's refusal naming it", err, lastLines(string(out), 20))
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "cloudburrow_providers*")); len(left) != 0 {
		t.Errorf("the refusal left %v behind", left)
	}
}
