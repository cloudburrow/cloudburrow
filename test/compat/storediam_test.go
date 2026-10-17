//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/googleapis/gax-go/v2"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Stored IAM (#1486, ADR-0006 as amended by #1486): Spanner instances and
// databases, Bigtable tables, Resource Manager tag keys and values, and the
// audit configs of project policies, stored and never enforced. These tests
// drive them with the official clients, gcloud, Terraform and the console,
// and assert nothing about access.

// gaxIAMClient is a generated client's three IAM methods.
type gaxIAMClient interface {
	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest, ...gax.CallOption) (*iampb.Policy, error)
	SetIamPolicy(context.Context, *iampb.SetIamPolicyRequest, ...gax.CallOption) (*iampb.Policy, error)
	TestIamPermissions(context.Context, *iampb.TestIamPermissionsRequest, ...gax.CallOption) (*iampb.TestIamPermissionsResponse, error)
}

// gaxPolicy is the policyHandle of resource through a generated client.
func gaxPolicy(c gaxIAMClient, resource string) policyHandle {
	return policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			return c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			_, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: p})
			return err
		},
		test: func(ctx context.Context, perms []string) ([]string, error) {
			r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: resource, Permissions: perms})
			return r.GetPermissions(), err
		},
	}
}

// policyRoundTrip is ADR-0006 rule 6's SDK test on one resource: a set reads
// back with a new etag, a stale etag is ABORTED, a conditional binding is
// UNIMPLEMENTED and TestIamPermissions answers perm.
func policyRoundTrip(t *testing.T, ctx context.Context, what string, p policyHandle, role, perm string) {
	t.Helper()
	empty, err := p.get(ctx)
	if err != nil || len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
		t.Fatalf("%s: GetIamPolicy of a new resource = %v, %v", what, empty, err)
	}
	if err := p.set(ctx, &iampb.Policy{Etag: empty.GetEtag(), Bindings: []*iampb.Binding{{Role: role, Members: []string{"user:ada@example.com"}}}}); err != nil {
		t.Fatalf("%s: SetIamPolicy: %v", what, err)
	}
	got, err := p.get(ctx)
	if err != nil || rolesOf(got)[role] != "user:ada@example.com" || string(got.GetEtag()) == string(empty.GetEtag()) {
		t.Errorf("%s: read back %v, %v; want the binding with a new etag", what, got, err)
	}
	if err := p.set(ctx, &iampb.Policy{Etag: empty.GetEtag()}); status.Code(err) != codes.Aborted {
		t.Errorf("%s: a stale etag = %v; want Aborted", what, err)
	}
	if err := p.set(ctx, &iampb.Policy{Version: 3, Bindings: []*iampb.Binding{{Role: role, Members: []string{"user:ada@example.com"},
		Condition: &expr.Expr{Expression: "true"}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("%s: a conditional binding = %v; want Unimplemented", what, err)
	}
	if held, err := p.test(ctx, []string{perm}); err != nil || !slices.Equal(held, []string{perm}) {
		t.Errorf("%s: TestIamPermissions = %v, %v; want %s", what, held, err, perm)
	}
}

// --- Spanner -------------------------------------------------------------

// TestSpannerIAM: the official instance and database admin clients'
// GetIamPolicy, SetIamPolicy and TestIamPermissions on an instance and a
// database, answered by the Spanner front (the emulator has none); a missing
// database is NOT_FOUND, and a dropped database's policy goes with it.
// covers: google.spanner.admin.instance.v1.InstanceAdmin/GetIamPolicy, google.spanner.admin.instance.v1.InstanceAdmin/SetIamPolicy, google.spanner.admin.instance.v1.InstanceAdmin/TestIamPermissions
// covers: google.spanner.admin.database.v1.DatabaseAdmin/GetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/SetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/TestIamPermissions
func TestSpannerIAM(t *testing.T) {
	f := newSpannerAdmin(t, "cb-iam", "iamdb") // skips without CLOUDBURROW_TEST_SPANNER
	policyRoundTrip(t, f.ctx, "instance", gaxPolicy(f.inst, f.instance), "roles/spanner.viewer", "spanner.instances.get")
	policyRoundTrip(t, f.ctx, "database", gaxPolicy(f.db, f.database), "roles/spanner.databaseReader", "spanner.databases.read")
	if _, err := f.db.GetIamPolicy(f.ctx, &iampb.GetIamPolicyRequest{Resource: f.instance + "/databases/missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing database = %v; want NotFound", err)
	}
}

// TestTerraformSpannerIAM: google_spanner_instance_iam_* and
// google_spanner_database_iam_* member, binding and policy through
// `cloudburrow terraform` with egress blocked, on an instance and database
// the provider makes; each phase applied, planned clean and read back with
// the official clients, then destroyed.
// covers: google.spanner.admin.instance.v1.InstanceAdmin/GetIamPolicy, google.spanner.admin.instance.v1.InstanceAdmin/SetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/GetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/SetIamPolicy
func TestTerraformSpannerIAM(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvSpanner) == "" || os.Getenv(EnvSpannerREST) == "" {
		t.Skipf("%s and %s must both be set", EnvSpanner, EnvSpannerREST)
	}
	h.Endpoint(EnvSpannerREST)
	f := newSpannerAdmin(t, "tf-iam-fixture", "fixdb") // the clients
	m := newTFModule(t)
	const id, db = "tf-iam", "tfiamdb"
	inst := "projects/" + h.Project() + "/instances/" + id
	base := fmt.Sprintf(`
resource "google_spanner_instance" "i" {
  project      = %q
  name         = %q
  config       = "emulator-config"
  display_name = "IAM"
  num_nodes    = 1
}
resource "google_spanner_database" "d" {
  project             = google_spanner_instance.i.project
  instance            = google_spanner_instance.i.name
  name                = %q
  deletion_protection = false
}
`, h.Project(), id, db)
	read := func(c gaxIAMClient, res string) func() map[string][]string {
		return func() map[string][]string {
			t.Helper()
			p, err := c.GetIamPolicy(f.ctx, &iampb.GetIamPolicyRequest{Resource: res})
			if err != nil {
				t.Fatalf("GetIamPolicy %s: %v", res, err)
			}
			return policyMap(p)
		}
	}
	runTFIAMPhases(t, m, base, [2]string{"roles/spanner.viewer", "roles/spanner.databaseReader"}, []tfIAMTarget{
		{kind: "google_spanner_instance", label: "i", ref: map[string]string{"project": "google_spanner_instance.i.project", "instance": "google_spanner_instance.i.name"},
			read: read(f.inst, inst)},
		{kind: "google_spanner_database", label: "d", ref: map[string]string{"project": "google_spanner_database.d.project",
			"instance": "google_spanner_database.d.instance", "database": "google_spanner_database.d.name"},
			read: read(f.db, inst+"/databases/"+db)},
	})
}

// TestGcloudSpannerIAM: gcloud spanner instances and databases
// add-iam-policy-binding, configured by gcloud-setup alone (its spanner
// override, the admin REST API the front answers), read back with the
// official clients; a second add keeps the first.
// covers: google.spanner.admin.instance.v1.InstanceAdmin/GetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/GetIamPolicy
func TestGcloudSpannerIAM(t *testing.T) {
	f := newSpannerAdmin(t, "gc-iam", "gciamdb") // skips without CLOUDBURROW_TEST_SPANNER
	New(t).Endpoint(EnvSpannerREST)
	gc := gcloudAdminSession(t, f.project, "spanner")
	id := strings.TrimPrefix(f.instance, "projects/"+f.project+"/instances/")
	db := f.database[strings.LastIndex(f.database, "/")+1:]
	const dev, ops = "user:dev@example.com", "group:ops@example.com"
	gc("spanner", "instances", "add-iam-policy-binding", id, "--member="+dev, "--role=roles/spanner.viewer")
	gc("spanner", "instances", "add-iam-policy-binding", id, "--member="+ops, "--role=roles/spanner.admin")
	gc("spanner", "databases", "add-iam-policy-binding", db, "--instance="+id, "--member="+dev, "--role=roles/spanner.databaseReader")
	p, err := f.inst.GetIamPolicy(f.ctx, &iampb.GetIamPolicyRequest{Resource: f.instance})
	if r := rolesOf(p); err != nil || r["roles/spanner.viewer"] != dev || r["roles/spanner.admin"] != ops {
		t.Errorf("the instance's policy = %v, %v", r, err)
	}
	p, err = f.db.GetIamPolicy(f.ctx, &iampb.GetIamPolicyRequest{Resource: f.database})
	if r := rolesOf(p); err != nil || r["roles/spanner.databaseReader"] != dev {
		t.Errorf("the database's policy = %v, %v", r, err)
	}
}

// TestConsoleSpannerPermissions: the Permissions tab on a Spanner
// instance's page and a database's, read back with the official clients.
// covers: google.spanner.admin.instance.v1.InstanceAdmin/GetIamPolicy, google.spanner.admin.instance.v1.InstanceAdmin/SetIamPolicy, google.spanner.admin.instance.v1.InstanceAdmin/TestIamPermissions
// covers: google.spanner.admin.database.v1.DatabaseAdmin/GetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/SetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/TestIamPermissions
func TestConsoleSpannerPermissions(t *testing.T) {
	f := newSpannerAdmin(t, "cs-iam", "csiamdb") // skips without CLOUDBURROW_TEST_SPANNER
	h := New(t)
	addr := consoleAddr(t, h)
	id := strings.TrimPrefix(f.instance, "projects/"+f.project+"/instances/")
	db := f.database[strings.LastIndex(f.database, "/")+1:]
	consolePermissionsRoundTripIn(t, h, addr, "spanner", f.project, []string{id}, gaxPolicy(f.inst, f.instance), "roles/spanner.viewer", "spanner.instances.get")
	consolePermissionsRoundTripIn(t, h, addr, "spanner", f.project, []string{id, db}, gaxPolicy(f.db, f.database), "roles/spanner.databaseReader", "spanner.databases.read")
}

// --- Bigtable --------------------------------------------------------------

// bigtableTableHandle is the policyHandle of a table through the official
// admin client's TableIAM.
func bigtableTableHandle(ac *bigtable.AdminClient, table string) policyHandle {
	h := ac.TableIAM(table)
	return policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			p, err := h.Policy(ctx)
			if err != nil {
				return nil, err
			}
			return p.InternalProto, nil
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			return h.SetPolicy(ctx, &iam.Policy{InternalProto: p})
		},
		test: func(ctx context.Context, perms []string) ([]string, error) {
			return h.TestPermissions(ctx, perms)
		},
	}
}

// TestBigtableTableIAM: the official admin client's TableIAM on a table,
// answered by the Bigtable front (the emulator has no table IAM); an
// authorized view's is UNIMPLEMENTED, as the emulator keeps none, and a
// table deleted and made again starts with no policy.
// covers: google.bigtable.admin.v2.BigtableTableAdmin/GetIamPolicy, google.bigtable.admin.v2.BigtableTableAdmin/SetIamPolicy, google.bigtable.admin.v2.BigtableTableAdmin/TestIamPermissions, google.bigtable.admin.v2.BigtableTableAdmin/DeleteTable
func TestBigtableTableIAM(t *testing.T) {
	h := New(t)
	ac, _ := bigtableTable(t, h, "iam-table") // skips without CLOUDBURROW_TEST_BIGTABLE
	ctx := h.Context()
	policyRoundTrip(t, ctx, "table", bigtableTableHandle(ac, "iam-table"), "roles/bigtable.reader", "bigtable.tables.readRows")
	if _, err := ac.AuthorizedViewIAM("iam-table", "v").Policy(ctx); status.Code(err) != codes.Unimplemented {
		t.Errorf("an authorized view's policy = %v; want Unimplemented", err)
	}
	if err := ac.DeleteTable(ctx, "iam-table"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "iam-table"); err != nil {
		t.Fatal(err)
	}
	if p, err := ac.TableIAM("iam-table").Policy(ctx); err != nil || len(p.Roles()) != 0 {
		t.Errorf("a table made again reads %v, %v; want no bindings", p, err)
	}
}

// TestTerraformBigtableTableIAM: google_bigtable_table_iam_member, _binding
// and _policy through `cloudburrow terraform` (the bigtableadmin REST API
// the front serves) on a table the provider makes, read back with the
// official admin client; then destroyed.
// covers: google.bigtable.admin.v2.BigtableTableAdmin/GetIamPolicy, google.bigtable.admin.v2.BigtableTableAdmin/SetIamPolicy
func TestTerraformBigtableTableIAM(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h) // skips without CLOUDBURROW_TEST_BIGTABLE
	m := newTFModule(t)
	ctx := h.Context()
	const id, cluster, table = "tf-bt-iam", "tf-bt-iam-c1", "tfiam"
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	base := fmt.Sprintf(`
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
`, h.Project(), id, cluster, table)
	runTFIAMPhases(t, m, base, [2]string{"roles/bigtable.reader", "roles/bigtable.user"}, []tfIAMTarget{{
		kind: "google_bigtable_table", label: "t",
		ref: map[string]string{"project": "google_bigtable_table.t.project", "instance_name": "google_bigtable_table.t.instance_name", "table": "google_bigtable_table.t.name"},
		read: func() map[string][]string {
			t.Helper()
			p, err := ac.TableIAM(table).Policy(ctx)
			if err != nil {
				t.Fatalf("the table's IAM policy: %v", err)
			}
			return policyMap(p.InternalProto)
		},
	}})
}

// TestGcloudBigtableTableIAM: gcloud bigtable tables add-iam-policy-binding,
// configured by gcloud-setup alone (its bigtableadmin override), egress to
// Google blocked, read back with the official admin client.
// covers: google.bigtable.admin.v2.BigtableTableAdmin/GetIamPolicy, google.bigtable.admin.v2.BigtableTableAdmin/SetIamPolicy
func TestGcloudBigtableTableIAM(t *testing.T) {
	h := New(t)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	iac := bigtableInstanceAdmin(t, h) // skips without CLOUDBURROW_TEST_BIGTABLE
	ctx := h.Context()
	const id, table = "gc-bt-iam", "gciam"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "gcloud IAM",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
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
	if conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name)); !strings.Contains(string(conf), "bigtableadmin = http://") {
		t.Fatalf("the configuration has no bigtableadmin override:\n%s", conf)
	}
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
	const dev, ops = "user:dev@example.com", "group:ops@example.com"
	gc("bigtable", "tables", "add-iam-policy-binding", table, "--instance="+id, "--member="+dev, "--role=roles/bigtable.reader")
	gc("bigtable", "tables", "add-iam-policy-binding", table, "--instance="+id, "--member="+ops, "--role=roles/bigtable.user")
	p, err := ac.TableIAM(table).Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := rolesOf(p.InternalProto); r["roles/bigtable.reader"] != dev || r["roles/bigtable.user"] != ops {
		t.Errorf("the table's policy = %v", r)
	}
}

// TestConsoleBigtableTablePermissions: the Permissions tab on a table's page
// on the Bigtable screen (the default instance), read back with the official
// admin client.
// covers: google.bigtable.admin.v2.BigtableTableAdmin/GetIamPolicy, google.bigtable.admin.v2.BigtableTableAdmin/SetIamPolicy, google.bigtable.admin.v2.BigtableTableAdmin/TestIamPermissions
func TestConsoleBigtableTablePermissions(t *testing.T) {
	h := New(t)
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	addr := consoleAddr(t, h)
	ctx := h.Context()
	// The console's default instance (cmd/cloudburrow's bigtableInstance).
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), "cloudburrow")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	const table = "console-iam"
	if err := ac.CreateTable(ctx, table); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.DeleteTable(h.Context(), table) })
	consolePermissionsRoundTripIn(t, h, addr, "bigtable", h.Project(), []string{table}, bigtableTableHandle(ac, table), "roles/bigtable.reader", "bigtable.tables.readRows")
}

// --- Resource Manager tags -------------------------------------------------

// iamTag creates a tag key under the organization and a value under it,
// deleted at cleanup.
func iamTag(t *testing.T, h *Harness, c tagClients, prefix string) (*rmpb.TagKey, *rmpb.TagValue) {
	t.Helper()
	ctx := h.Context()
	op, err := c.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: localOrg, ShortName: v1ID(h, prefix)}})
	if err != nil {
		t.Fatal(err)
	}
	k, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vop, err := c.values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: k.GetName(), ShortName: "prod"}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := vop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := h.Context()
		if op, err := c.values.DeleteTagValue(ctx, &rmpb.DeleteTagValueRequest{Name: v.GetName()}); err == nil {
			_, _ = op.Wait(ctx)
		}
		if op, err := c.keys.DeleteTagKey(ctx, &rmpb.DeleteTagKeyRequest{Name: k.GetName()}); err == nil {
			_, _ = op.Wait(ctx)
		}
	})
	return k, v
}

// TestResourceManagerTagIAM: the official TagKeysClient's and
// TagValuesClient's GetIamPolicy, SetIamPolicy and TestIamPermissions; a
// permission that is not a tag's is not answered, and a deleted key's
// policy goes with it.
// covers: google.cloud.resourcemanager.v3.TagKeys/GetIamPolicy, google.cloud.resourcemanager.v3.TagKeys/SetIamPolicy, google.cloud.resourcemanager.v3.TagKeys/TestIamPermissions
// covers: google.cloud.resourcemanager.v3.TagValues/GetIamPolicy, google.cloud.resourcemanager.v3.TagValues/SetIamPolicy, google.cloud.resourcemanager.v3.TagValues/TestIamPermissions
func TestResourceManagerTagIAM(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := newTagClients(t, h)
	ctx := h.Context()
	k, v := iamTag(t, h, c, "iamk-")
	policyRoundTrip(t, ctx, "tag key", gaxPolicy(c.keys, k.GetName()), "roles/resourcemanager.tagUser", "resourcemanager.tagKeys.get")
	policyRoundTrip(t, ctx, "tag value", gaxPolicy(c.values, v.GetName()), "roles/resourcemanager.tagUser", "resourcemanager.tagValueBindings.create")
	r, err := c.keys.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: k.GetName(), Permissions: []string{"storage.buckets.get"}})
	if err != nil || len(r.GetPermissions()) != 0 {
		t.Errorf("a storage permission on a tag key = %v, %v; want none", r, err)
	}
}

// TestTerraformTagIAM: google_tags_tag_key_iam_* and google_tags_tag_value_iam_*
// member, binding and policy through `cloudburrow terraform`
// (tags_custom_endpoint), on a key and value the provider makes, read back
// with the official clients; then destroyed.
// covers: google.cloud.resourcemanager.v3.TagKeys/GetIamPolicy, google.cloud.resourcemanager.v3.TagKeys/SetIamPolicy, google.cloud.resourcemanager.v3.TagValues/GetIamPolicy, google.cloud.resourcemanager.v3.TagValues/SetIamPolicy
func TestTerraformTagIAM(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := newTagClients(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	short := v1ID(h, "tfik-")
	base := fmt.Sprintf(`
resource "google_tags_tag_key" "k" {
  parent     = %q
  short_name = %q
}
resource "google_tags_tag_value" "v" {
  parent     = google_tags_tag_key.k.id
  short_name = "prod"
}
`, localOrg, short)
	read := func(value bool) func() map[string][]string {
		return func() map[string][]string {
			t.Helper()
			k, err := c.keys.GetNamespacedTagKey(ctx, &rmpb.GetNamespacedTagKeyRequest{Name: strings.TrimPrefix(localOrg, "organizations/") + "/" + short})
			if err != nil {
				t.Fatalf("the key: %v", err)
			}
			var p *iampb.Policy
			if value {
				vl, err := c.values.GetNamespacedTagValue(ctx, &rmpb.GetNamespacedTagValueRequest{Name: k.GetNamespacedName() + "/prod"})
				if err != nil {
					t.Fatalf("the value: %v", err)
				}
				p, err = c.values.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: vl.GetName()})
			} else {
				p, err = c.keys.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: k.GetName()})
			}
			if err != nil {
				t.Fatalf("GetIamPolicy: %v", err)
			}
			return policyMap(p)
		}
	}
	runTFIAMPhases(t, m, base, [2]string{"roles/resourcemanager.tagUser", "roles/resourcemanager.tagViewer"}, []tfIAMTarget{
		{kind: "google_tags_tag_key", label: "k", ref: map[string]string{"tag_key": "google_tags_tag_key.k.name"}, read: read(false)},
		{kind: "google_tags_tag_value", label: "v", ref: map[string]string{"tag_value": "google_tags_tag_value.v.name"}, read: read(true)},
	})
}

// TestGcloudTagIAM: gcloud resource-manager tags keys and values
// add-iam-policy-binding, configured by gcloud-setup alone, read back with
// the official clients.
// covers: google.cloud.resourcemanager.v3.TagKeys/GetIamPolicy, google.cloud.resourcemanager.v3.TagValues/GetIamPolicy
func TestGcloudTagIAM(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := newTagClients(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	k, v := iamTag(t, h, c, "gcik-")
	const dev = "user:dev@example.com"
	g.must("resource-manager", "tags", "keys", "add-iam-policy-binding", k.GetName(), "--member="+dev, "--role=roles/resourcemanager.tagUser")
	g.must("resource-manager", "tags", "values", "add-iam-policy-binding", v.GetName(), "--member="+dev, "--role=roles/resourcemanager.tagViewer")
	if p, err := c.keys.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: k.GetName()}); err != nil || rolesOf(p)["roles/resourcemanager.tagUser"] != dev {
		t.Errorf("the key's policy = %v, %v", p, err)
	}
	if p, err := c.values.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: v.GetName()}); err != nil || rolesOf(p)["roles/resourcemanager.tagViewer"] != dev {
		t.Errorf("the value's policy = %v, %v", p, err)
	}
}

// TestConsoleTagPermissions: the Permissions tab on a tag key's page and a
// tag value's, read back with the official clients.
// covers: google.cloud.resourcemanager.v3.TagKeys/GetIamPolicy, google.cloud.resourcemanager.v3.TagKeys/SetIamPolicy, google.cloud.resourcemanager.v3.TagKeys/TestIamPermissions
// covers: google.cloud.resourcemanager.v3.TagValues/GetIamPolicy, google.cloud.resourcemanager.v3.TagValues/SetIamPolicy, google.cloud.resourcemanager.v3.TagValues/TestIamPermissions
func TestConsoleTagPermissions(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := newTagClients(t, h)
	addr := consoleAddr(t, h)
	k, v := iamTag(t, h, c, "csik-")
	kid, vid := strings.TrimPrefix(k.GetName(), "tagKeys/"), strings.TrimPrefix(v.GetName(), "tagValues/")
	consolePermissionsRoundTripIn(t, h, addr, "tags", h.Project(), []string{kid}, gaxPolicy(c.keys, k.GetName()), "roles/resourcemanager.tagUser", "resourcemanager.tagKeys.get")
	consolePermissionsRoundTripIn(t, h, addr, "tags", h.Project(), []string{kid, vid}, gaxPolicy(c.values, v.GetName()), "roles/resourcemanager.tagUser", "resourcemanager.tagValues.get")
}

// --- audit configs -----------------------------------------------------------

// TestResourceManagerAuditConfigs: audit configs on a project's policy
// through the official v3 client and v1 REST, stored as update_mask says:
// set with audit_configs in the mask, kept by a set whose mask leaves them
// out, cleared by one that names them with none. They configure no audit log.
// covers: google.cloud.resourcemanager.v3.Projects/GetIamPolicy, google.cloud.resourcemanager.v3.Projects/SetIamPolicy, cloudresourcemanager.projects.getIamPolicy
func TestResourceManagerAuditConfigs(t *testing.T) {
	h := New(t)
	c := rmProjects(t, h)
	ctx := h.Context()
	id := iamProject(t, h, "audit-")
	res := "projects/" + id
	audit := []*iampb.AuditConfig{{Service: "allServices", AuditLogConfigs: []*iampb.AuditLogConfig{
		{LogType: iampb.AuditLogConfig_DATA_READ, ExemptedMembers: []string{"user:ada@example.com"}}, {LogType: iampb.AuditLogConfig_ADMIN_READ}}}}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{AuditConfigs: audit,
		Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:ada@example.com"}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bindings", "etag", "audit_configs"}}}); err != nil {
		t.Fatalf("SetIamPolicy with audit configs: %v", err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
	if err != nil || len(got.GetAuditConfigs()) != 1 || got.GetAuditConfigs()[0].GetService() != "allServices" ||
		len(got.GetAuditConfigs()[0].GetAuditLogConfigs()) != 2 || got.GetAuditConfigs()[0].GetAuditLogConfigs()[0].GetExemptedMembers()[0] != "user:ada@example.com" {
		t.Fatalf("read back %v, %v", got, err)
	}
	// No mask is "bindings, etag": the audit configs stay.
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: got.GetEtag(), Bindings: got.GetBindings()}}); err != nil {
		t.Fatal(err)
	}
	v1, err := rmV1(t, h).Projects.GetIamPolicy(id, &crmv1.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil || len(v1.AuditConfigs) != 1 || len(v1.AuditConfigs[0].AuditLogConfigs) != 2 {
		t.Errorf("v1 getIamPolicy after a set without a mask = %+v, %v; want the audit configs kept", v1, err)
	}
	got, _ = c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: got.GetEtag(), Bindings: got.GetBindings()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bindings", "etag", "audit_configs"}}}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res}); err != nil || len(got.GetAuditConfigs()) != 0 {
		t.Errorf("after a set naming audit_configs with none: %v, %v", got, err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{AuditConfigs: []*iampb.AuditConfig{{}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"audit_configs"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an audit config with no service = %v; want InvalidArgument", err)
	}
}

// TestTerraformProjectIAMAuditConfig: google_project_iam_audit_config
// through `cloudburrow terraform` with egress blocked: applied, a plan with
// no diff, its exempted members changed, a plan with no diff, destroyed,
// each read back with the official v3 client.
// covers: cloudresourcemanager.projects.getIamPolicy, cloudresourcemanager.projects.setIamPolicy, google.cloud.resourcemanager.v3.Projects/GetIamPolicy
func TestTerraformProjectIAMAuditConfig(t *testing.T) {
	h := New(t)
	c := rmProjects(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	id := iamProject(t, h, "tfaud-")
	module := func(exempt string) string {
		return fmt.Sprintf(`
resource "google_project_iam_audit_config" "a" {
  project = %q
  service = "allServices"
  audit_log_config {
    log_type         = "DATA_READ"
    exempted_members = [%q]
  }
  audit_log_config {
    log_type = "ADMIN_READ"
  }
}
`, id, exempt)
	}
	check := func(when, exempt string) {
		t.Helper()
		p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + id})
		if err != nil {
			t.Fatalf("%s: GetIamPolicy: %v", when, err)
		}
		var got []string
		for _, a := range p.GetAuditConfigs() {
			for _, l := range a.GetAuditLogConfigs() {
				got = append(got, a.GetService()+"/"+l.GetLogType().String()+"/"+strings.Join(l.GetExemptedMembers(), ","))
			}
		}
		slices.Sort(got)
		want := []string{"allServices/ADMIN_READ/", "allServices/DATA_READ/" + exempt}
		if exempt == "" {
			want = nil
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: the audit configs = %v; want %v", when, got, want)
		}
	}
	m.write(module("user:ada@example.com"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "user:ada@example.com")
	m.planClean()
	m.write(module("group:ops@example.com"))
	m.apply()
	check("after the update", "group:ops@example.com")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	check("after destroy", "")
}
