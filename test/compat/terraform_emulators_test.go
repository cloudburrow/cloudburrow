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
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
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
// google_bigtable_instance, the instance admin API) fail locally:
// docs/compatibility.md. Spanner is TestTerraformSpannerInstanceAndDatabase.
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

// TestTerraformSpannerInstanceAndDatabase (#1331), in the emulators shard:
// a google_spanner_instance and a google_spanner_database apply through
// `cloudburrow terraform` with the official hashicorp/google provider, over
// the emulator's REST API (spanner_custom_endpoint, Spanner's second port,
// which the call front in its pod serves), read back through the official
// Spanner admin clients over gRPC, plan clean, take an in-place update (a
// second table in the database's DDL), plan clean again and destroy, all
// with egress blocked.
//
// The emulator does not update instances: UpdateInstance answers
// UNIMPLEMENTED, "Cloud Spanner Emulator does not support updating
// instances", over REST as 501. So a change to the instance's display name
// or node count is planned in place and fails at apply, locally, naming
// that; the instance is left as it was and the old configuration plans clean.
func TestTerraformSpannerInstanceAndDatabase(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if os.Getenv(EnvSpanner) == "" || os.Getenv(EnvSpannerREST) == "" {
		t.Skipf("%s and %s must both be set", EnvSpanner, EnvSpannerREST)
	}
	addr := h.Endpoint(EnvSpanner)
	ctx := h.Context()
	ic, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	t.Cleanup(func() { ic.Close() })
	dc, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	t.Cleanup(func() { dc.Close() })
	flags := cliArgs()
	dir := t.TempDir()
	const instanceID, dbID = "tf-spanner", "tfdb"
	instanceName := fmt.Sprintf("projects/%s/instances/%s", h.Project(), instanceID)
	dbName := instanceName + "/databases/" + dbID
	ddl := []string{
		"CREATE TABLE Items (\n  Id INT64 NOT NULL,\n  Name STRING(MAX),\n) PRIMARY KEY(Id)",
		"CREATE TABLE Tags (\n  Id INT64 NOT NULL,\n) PRIMARY KEY(Id)",
	}

	write := func(display string, nodes int, statements int) {
		t.Helper()
		var d strings.Builder
		for _, st := range ddl[:statements] {
			fmt.Fprintf(&d, "    %q,\n", st)
		}
		module := googleProviderRequirement + fmt.Sprintf(`resource "google_spanner_instance" "i" {
  project      = %q
  name         = %q
  config       = "emulator-config"
  display_name = %q
  num_nodes    = %d
}
resource "google_spanner_database" "d" {
  project             = %q
  instance            = google_spanner_instance.i.name
  name                = %q
  deletion_protection = false
  ddl = [
%s  ]
}
`, h.Project(), instanceID, display, nodes, h.Project(), dbID, d.String())
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
	check := func(when, display string, nodes int, statements int) {
		t.Helper()
		in, err := ic.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: instanceName})
		if err != nil {
			t.Fatalf("the instance %s: %v", when, err)
		}
		if in.GetDisplayName() != display || int(in.GetNodeCount()) != nodes {
			t.Errorf("the instance %s = %q with %d nodes, want %q with %d", when, in.GetDisplayName(), in.GetNodeCount(), display, nodes)
		}
		if want := fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", h.Project()); in.GetConfig() != want {
			t.Errorf("the instance's config %s = %q, want %q", when, in.GetConfig(), want)
		}
		got, err := dc.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: dbName})
		if err != nil {
			t.Fatalf("the database %s: %v", when, err)
		}
		if strings.Join(got.GetStatements(), ";") != strings.Join(ddl[:statements], ";") {
			t.Errorf("the database's DDL %s = %q, want %q", when, got.GetStatements(), ddl[:statements])
		}
	}

	write("tf one", 1, 1)
	useGoogleProviderLock(t, dir)
	run("init", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color")
	check("after apply", "tf one", 1, 1)
	cleanPlan("right after apply")

	write("tf one", 1, 2)
	out, err := tf("plan", "-input=false", "-no-color")
	if err != nil || !strings.Contains(out, "0 to add, 1 to change, 0 to destroy") {
		t.Errorf("the update is not planned in place (%v):\n%s", err, lastLines(out, 20))
	}
	run("apply", "-auto-approve", "-input=false", "-no-color")
	check("after the update", "tf one", 1, 2)
	cleanPlan("after the update")

	// The emulator's refusal to update an instance, locally.
	write("tf two", 2, 2)
	out, err = tf("apply", "-auto-approve", "-input=false", "-no-color")
	if err == nil || !strings.Contains(out, "does not support updating instances") {
		t.Errorf("an instance update = %v, want the emulator's refusal:\n%s", err, lastLines(out, 20))
	}
	check("after the refused instance update", "tf one", 1, 2)
	write("tf one", 1, 2)
	cleanPlan("after the refused instance update")

	run("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := dc.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: dbName}); status.Code(err) != codes.NotFound {
		t.Errorf("the database after destroy: %v, want NotFound", err)
	}
	if _, err := ic.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: instanceName}); status.Code(err) != codes.NotFound {
		t.Errorf("the instance after destroy: %v, want NotFound", err)
	}
}
