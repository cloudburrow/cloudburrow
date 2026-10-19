//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The fourth batch of Terraform resources (#1304): each test applies with
// the official hashicorp/google provider through `cloudburrow terraform`,
// with egress blocked, reads back with an official client, plans clean,
// changes in place where the resource can, and destroys.

// covers: sql.sslCerts.insert, sql.sslCerts.get, sql.sslCerts.delete, sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.operations.get
//
// TestTerraformSQLSSLCertAndReplica (#1304): a PostgreSQL
// google_sql_database_instance, a read replica of it named by
// master_instance_name, and a google_sql_ssl_cert on the primary. The
// generated sqladmin client reads the replica back as a
// READ_REPLICA_INSTANCE of the primary and the certificate by its SHA-1
// fingerprint, with the common name and the PEM the provider holds; the plan
// is clean; a label on the replica changes in place; destroy removes the
// certificate, the replica and the primary.
func TestTerraformSQLSSLCertAndReplica(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	n := time.Now().UnixNano() % 1e6
	primary, replica := fmt.Sprintf("tf-pgp-%d", n), fmt.Sprintf("tf-pgr-%d", n)
	module := func(label string) string {
		return fmt.Sprintf(`
resource "google_sql_database_instance" "main" {
  project             = %[1]q
  name                = %[2]q
  region              = "us-central1"
  database_version    = "POSTGRES_17"
  deletion_protection = false
  settings {
    tier = "db-custom-1-3840"
  }
}
resource "google_sql_database_instance" "replica" {
  project              = %[1]q
  name                 = %[3]q
  region               = "us-central1"
  database_version     = "POSTGRES_17"
  master_instance_name = google_sql_database_instance.main.name
  deletion_protection  = false
  settings {
    tier        = "db-custom-1-3840"
    user_labels = { env = %[4]q }
  }
}
resource "google_sql_ssl_cert" "client" {
  project     = %[1]q
  instance    = google_sql_database_instance.main.name
  common_name = "tf-client"
}
output "fingerprint" {
  value = google_sql_ssl_cert.client.sha1_fingerprint
}
output "cert" {
  value = google_sql_ssl_cert.client.cert
  sensitive = true
}
`, m.project, primary, replica, label)
	}
	t.Cleanup(func() {
		for _, name := range []string{replica, primary} {
			if op, err := svc.Instances.Delete(m.project, name).Do(); err == nil {
				waitSQLOp(t, svc, m.project, op)
			}
		}
	})
	m.write(module("a"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	r, err := svc.Instances.Get(m.project, replica).Do()
	if err != nil {
		t.Fatalf("instances.get of the replica: %v", err)
	}
	if r.InstanceType != "READ_REPLICA_INSTANCE" || r.MasterInstanceName != m.project+":"+primary || r.State != "RUNNABLE" ||
		r.Settings == nil || r.Settings.UserLabels["env"] != "a" {
		t.Errorf("after apply the replica reads %s %s %s %+v", r.InstanceType, r.MasterInstanceName, r.State, r.Settings)
	}
	if p, err := svc.Instances.Get(m.project, primary).Do(); err != nil || fmt.Sprint(p.ReplicaNames) != fmt.Sprint([]string{replica}) {
		t.Errorf("the primary's replicaNames = %v, %v; want [%s]", p.ReplicaNames, err, replica)
	}
	fp := m.must("output", "-raw", "fingerprint")
	c, err := svc.SslCerts.Get(m.project, primary, fp).Do()
	if err != nil {
		t.Fatalf("sslCerts.get(%q): %v", fp, err)
	}
	if c.CommonName != "tf-client" || c.Instance != primary || strings.TrimSpace(c.Cert) != strings.TrimSpace(m.must("output", "-raw", "cert")) {
		t.Errorf("the certificate reads %+v; want tf-client on %s with the provider's PEM", c, primary)
	}
	m.planClean()

	m.write(module("b"))
	m.apply()
	if r, err = svc.Instances.Get(m.project, replica).Do(); err != nil || r.Settings.UserLabels["env"] != "b" ||
		r.MasterInstanceName != m.project+":"+primary {
		t.Errorf("after the label change the replica reads %+v, %v", r, err)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, name := range []string{replica, primary} {
		_, err = svc.Instances.Get(m.project, name).Do()
		wantHTTP(t, "instances.get of "+name+" after destroy", err, http.StatusNotFound)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/UpdateInstance, google.cloud.redis.v1.CloudRedis/DeleteInstance
//
// TestTerraformRedisMaintenancePolicy (#1304, #1386): a google_redis_instance
// with a maintenance_policy applies; the official Go client reads back the
// one weekly window, its day, its UTC start hour and its one-hour duration;
// the plan is clean; a new day and hour change in place; destroy removes the
// instance. CloudBurrow never schedules maintenance, so the instance has no
// maintenance_schedule.
func TestTerraformRedisMaintenancePolicy(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	id := fmt.Sprintf("tfmp-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + m.project + "/locations/us-central1/instances/" + id
	module := func(day string, hour int) string {
		return fmt.Sprintf(`
resource "google_redis_instance" "cache" {
  project        = %q
  region         = "us-central1"
  name           = %q
  tier           = "BASIC"
  memory_size_gb = 1
  maintenance_policy {
    description = "tf window"
    weekly_maintenance_window {
      day = %q
      start_time {
        hours   = %d
        minutes = 0
        seconds = 0
        nanos   = 0
      }
    }
  }
}
`, m.project, id, day, hour)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	check := func(when string, day string, hour int32) {
		t.Helper()
		inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, GetInstance: %v", when, err)
		}
		mp := inst.GetMaintenancePolicy()
		ws := mp.GetWeeklyMaintenanceWindow()
		if len(ws) != 1 || ws[0].GetDay().String() != day || ws[0].GetStartTime().GetHours() != hour ||
			ws[0].GetDuration().AsDuration() != time.Hour || mp.GetDescription() != "tf window" {
			t.Errorf("%s, the maintenance_policy reads %v", when, mp)
		}
		if inst.GetMaintenanceSchedule() != nil {
			t.Errorf("%s, maintenance_schedule = %v; want none, as nothing is scheduled", when, inst.GetMaintenanceSchedule())
		}
	}
	m.write(module("TUESDAY", 3))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "TUESDAY", 3)
	m.planClean()

	m.write(module("SATURDAY", 22))
	m.apply()
	check("after the change", "SATURDAY", 22)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after destroy = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/AddSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/GetSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DisableSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret
//
// TestTerraformSecretVersionDeletionPolicy (#1304): a
// google_secret_manager_secret_version's deletion_policy decides what its
// destroy does. With DISABLE the version is disabled, not destroyed; with
// ABANDON it is left enabled. Each is read back by the official client
// after a destroy of the version alone, the secret staying; the plan is
// clean after each apply; and destroying the module deletes the secret.
func TestTerraformSecretVersionDeletionPolicy(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h) // skips without CLOUDBURROW_TEST_SECRETS
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	secret := "projects/" + project + "/secrets/tf-version-policy"
	module := func(versions string) string {
		return fmt.Sprintf(`
resource "google_secret_manager_secret" "s" {
  project   = %q
  secret_id = "tf-version-policy"
  replication {
    auto {}
  }
}
%s`, project, versions)
	}
	version := func(label, policy string) string {
		return fmt.Sprintf(`
resource "google_secret_manager_secret_version" %q {
  secret          = google_secret_manager_secret.s.id
  secret_data     = "kept-by-%s"
  deletion_policy = %q
}
`, label, strings.ToLower(policy), policy)
	}
	state := func(when, name string, want secretmanagerpb.SecretVersion_State) {
		t.Helper()
		v, err := sm.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, GetSecretVersion(%s): %v", when, name, err)
		}
		if v.GetState() != want {
			t.Errorf("%s, %s is %v; want %v", when, name, v.GetState(), want)
		}
	}
	both := module(version("disable", "DISABLE") + version("abandon", "ABANDON"))
	m.write(both)
	m.must("init", "-input=false", "-no-color")
	m.apply()
	names := map[string]string{}
	for _, label := range []string{"disable", "abandon"} {
		out := m.must("state", "show", "-no-color", "google_secret_manager_secret_version."+label)
		for _, line := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(k) == "name" {
				names[label] = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		if !strings.HasPrefix(names[label], secret+"/versions/") {
			t.Fatalf("the %s version's name is %q", label, names[label])
		}
		state("after apply", names[label], secretmanagerpb.SecretVersion_ENABLED)
	}
	m.planClean()

	// The versions go, the secret stays.
	m.write(module(""))
	m.apply()
	state("after removing the DISABLE version", names["disable"], secretmanagerpb.SecretVersion_DISABLED)
	state("after removing the ABANDON version", names["abandon"], secretmanagerpb.SecretVersion_ENABLED)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secret}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
}

// covers: google.cloud.resourcemanager.v3.Folders/CreateFolder, google.cloud.resourcemanager.v3.Folders/GetFolder, google.cloud.resourcemanager.v3.Folders/DeleteFolder, google.cloud.resourcemanager.v3.Folders/GetIamPolicy, google.cloud.resourcemanager.v3.Folders/SetIamPolicy, google.cloud.resourcemanager.v3.Organizations/GetIamPolicy, cloudresourcemanager.organizations.getIamPolicy, cloudresourcemanager.organizations.setIamPolicy
//
// TestTerraformFolderAndOrganizationIAM (#1304, #1368): on a google_folder,
// google_folder_iam_member, _binding and _policy go through the same phases
// as the other stored-IAM resources (member and binding, none, policy,
// none), each read back with the official v3 FoldersClient. On the local
// organization, a google_organization_iam_member and _binding apply, read
// back with the v3 OrganizationsClient; the binding's members change in
// place; removing them leaves the organization's other bindings as they
// were. No google_organization_iam_policy: it is authoritative over the one
// organization every test shares. Stored, never enforced (ADR-0006).
func TestTerraformFolderAndOrganizationIAM(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	fc, oc, _ := foldersClients(t, h)
	ctx := h.Context()
	folder := folderLabel(h, "tfiam-")
	base := fmt.Sprintf(`
resource "google_folder" "f" {
  display_name        = %q
  parent              = %q
  deletion_protection = false
}
`, folder, localOrg)
	policyOf := func(get func() (*iampb.Policy, error)) map[string][]string {
		t.Helper()
		p, err := get()
		if err != nil {
			t.Fatalf("GetIamPolicy: %v", err)
		}
		out := map[string][]string{}
		for _, b := range p.GetBindings() {
			out[b.GetRole()] = b.GetMembers()
		}
		return out
	}
	var folderName string
	readFolder := func() map[string][]string {
		if folderName == "" {
			f, err := fc.SearchFolders(ctx, &rmpb.SearchFoldersRequest{Query: fmt.Sprintf("displayName=%q", folder)}).Next()
			if err != nil {
				t.Fatalf("SearchFolders(%q): %v", folder, err)
			}
			folderName = f.GetName()
		}
		return policyOf(func() (*iampb.Policy, error) {
			return fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: folderName})
		})
	}
	runTFIAMPhases(t, m, base, [2]string{"roles/resourcemanager.folderViewer", "roles/resourcemanager.folderEditor"}, []tfIAMTarget{{
		kind: "google_folder", label: "f", policyData: true, ref: map[string]string{"folder": "google_folder.f.name"}, read: readFolder,
	}})
	if f, err := fc.GetFolder(ctx, &rmpb.GetFolderRequest{Name: folderName}); err != nil || f.GetState().String() != "DELETE_REQUESTED" {
		t.Errorf("after destroy %s reads %v, %v; want DELETE_REQUESTED", folderName, f, err)
	}

	// The organization: member and binding only.
	readOrg := func() map[string][]string {
		return policyOf(func() (*iampb.Policy, error) {
			return oc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: localOrg})
		})
	}
	before := readOrg()
	org := strings.TrimPrefix(localOrg, "organizations/")
	const viewer, admin = "roles/resourcemanager.organizationViewer", "roles/resourcemanager.folderAdmin"
	who := "user:tf-" + strings.TrimPrefix(h.Project(), "cb-test-") + "@example.com"
	orgModule := func(bindingMembers ...string) string {
		quoted := make([]string, len(bindingMembers))
		for i, b := range bindingMembers {
			quoted[i] = fmt.Sprintf("%q", b)
		}
		return fmt.Sprintf(`
# Stored, never enforced (ADR-0006).
resource "google_organization_iam_member" "o" {
  org_id = %q
  role   = %q
  member = %q
}
resource "google_organization_iam_binding" "o" {
  org_id  = %q
  role    = %q
  members = [%s]
}
`, org, viewer, who, org, admin, strings.Join(quoted, ", "))
	}
	members := func(p map[string][]string, role string) string {
		ms := append([]string(nil), p[role]...)
		sort.Strings(ms)
		return strings.Join(ms, ",")
	}
	m.write(orgModule(tfIAMMember2))
	m.apply()
	t.Cleanup(func() {
		if t.Failed() {
			_, _ = m.tf("destroy", "-auto-approve", "-input=false", "-no-color")
		}
	})
	if p := readOrg(); !strings.Contains(members(p, viewer), who) || members(p, admin) != tfIAMMember2 {
		t.Errorf("after apply the organization's policy = %v", p)
	}
	m.planClean()
	m.write(orgModule(tfIAMMember, tfIAMMember2))
	m.apply()
	if p := readOrg(); members(p, admin) != tfIAMMember2+","+tfIAMMember {
		t.Errorf("after the binding's change the organization's policy = %v", p)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	after := readOrg()
	if members(after, viewer) != members(before, viewer) || len(after[admin]) != 0 {
		t.Errorf("after destroy the organization's policy = %v; want it as before, %v", after, before)
	}
}

// covers: sql.instances.insert, sql.instances.get, sql.instances.delete
//
// TestTerraformSQLSourceRepresentation (#1304, #1501, #1524): a MySQL
// google_sql_source_representation_instance, a record of a server outside
// Cloud SQL, with a dump_file_path, and a PostgreSQL one each apply, are
// read back as an ON_PREMISES_INSTANCE with their host and port (and the
// dump file), plan no changes, and are destroyed.
func TestTerraformSQLSourceRepresentation(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	m := newTFModule(t)
	n := time.Now().UnixNano() % 1e6
	my, pg := fmt.Sprintf("tf-extmy-%d", n), fmt.Sprintf("tf-ext-%d", n)
	m.write(fmt.Sprintf(`
resource "google_sql_source_representation_instance" "my" {
  project          = %q
  name             = %q
  region           = "us-central1"
  database_version = "MYSQL_8_0"
  host             = "10.0.0.6"
  port             = 3306
  username         = "repl"
  password         = "not-a-secret"
  dump_file_path   = "gs://a-bucket/source.sql.gz"
}

resource "google_sql_source_representation_instance" "pg" {
  project          = %q
  name             = %q
  region           = "us-central1"
  database_version = "POSTGRES_17"
  host             = "10.0.0.5"
  port             = 5432
  username         = "repl"
  password         = "not-a-secret"
}
`, m.project, my, m.project, pg))
	m.must("init", "-input=false", "-no-color")
	m.must("apply", "-auto-approve", "-input=false", "-no-color")
	for name, want := range map[string]string{my: "10.0.0.6:3306", pg: "10.0.0.5:5432"} {
		got, err := svc.Instances.Get(m.project, name).Do()
		if err != nil || got.InstanceType != "ON_PREMISES_INSTANCE" || got.OnPremisesConfiguration == nil ||
			got.OnPremisesConfiguration.HostPort != want || got.OnPremisesConfiguration.Password != "" {
			t.Fatalf("the source representation instance %s: %+v %v", name, got, err)
		}
		if name == my && got.OnPremisesConfiguration.DumpFilePath != "gs://a-bucket/source.sql.gz" {
			t.Errorf("the MySQL source's dumpFilePath: %q", got.OnPremisesConfiguration.DumpFilePath)
		}
	}
	if out := m.must("plan", "-detailed-exitcode", "-input=false", "-no-color"); !strings.Contains(out, "No changes") {
		t.Errorf("a plan after apply is not empty:\n%s", lastLines(out, 30))
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, name := range []string{my, pg} {
		_, err := svc.Instances.Get(m.project, name).Do()
		wantHTTP(t, "instances.get of the destroyed instance", err, http.StatusNotFound)
	}
}
