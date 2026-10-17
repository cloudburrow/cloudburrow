//go:build compat

package compat

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	vlogging "cloud.google.com/go/logging/apiv2"
)

// #1304's eleventh batch of google provider resources and data sources,
// over APIs built since the tenth: Logging saved queries and log scopes,
// Bigtable schema bundles, Spanner instance partitions and regional
// secrets. Each goes through `cloudburrow terraform` with hashicorp/google
// and egress blocked, is read back through an official SDK, planned clean,
// changed in place, planned clean again and destroyed. Each test asks for
// the endpoints of the services it uses first, so it skips in a shard
// without them.

// TestTerraformLoggingSavedQueryAndLogScope (#1304): a
// google_logging_saved_query (a logging query with a summary field) and a
// google_logging_log_scope over the project apply; Google's generated
// logging/v2 JSON client reads both back; the plan is clean; a new
// description, visibility and filter, and a second resource name in the
// scope, apply in place; the plan is clean again; destroy deletes both.
// The data sources google_logging_sink (of a google_logging_project_sink
// in the module) and google_logging_project_cmek_settings are compared with
// the official ConfigClient's GetSink and GetCmekSettings.
// covers: google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/GetSink, google.logging.v2.ConfigServiceV2/UpdateSink, google.logging.v2.ConfigServiceV2/DeleteSink, google.logging.v2.ConfigServiceV2/GetCmekSettings
func TestTerraformLoggingSavedQueryAndLogScope(t *testing.T) {
	h := New(t)
	opts := loggingClientOptions(h) // skips without CLOUDBURROW_TEST_LOGGING
	svc := loggingJSONService(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	config, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()
	p := h.Project()
	parent := "projects/" + p + "/locations/global"
	queryName, scopeName := parent+"/savedQueries/tf-errors", parent+"/logScopes/tf-scope"
	sinkName := "projects/" + p + "/sinks/tf-sq-sink"
	module := func(description, visibility, filter string, second bool) string {
		resources := fmt.Sprintf("%q", "projects/"+p)
		if second {
			resources += fmt.Sprintf(", %q", "projects/"+p+"/locations/global/buckets/_Default/views/_AllLogs")
		}
		return fmt.Sprintf(`
resource "google_logging_saved_query" "q" {
  parent       = %[1]q
  location     = "global"
  name         = "tf-errors"
  display_name = "Errors"
  description  = %[2]q
  visibility   = %[3]q
  logging_query {
    filter = %[4]q
    summary_fields {
      field = "jsonPayload.msg"
    }
  }
}
resource "google_logging_log_scope" "s" {
  parent         = %[1]q
  location       = "global"
  name           = "tf-scope"
  description    = %[2]q
  resource_names = [%[5]s]
}
resource "google_logging_project_sink" "k" {
  project                = %[6]q
  name                   = "tf-sq-sink"
  destination            = "logging.googleapis.com/projects/%[6]s/locations/global/buckets/_Default"
  filter                 = %[4]q
  unique_writer_identity = true
}
data "google_logging_sink" "k" {
  id = google_logging_project_sink.k.id
}
data "google_logging_project_cmek_settings" "c" {
  project = %[6]q
}
output "outputs" {
  value = {
    sink_filter      = data.google_logging_sink.k.filter
    sink_destination = data.google_logging_sink.k.destination
    sink_writer      = data.google_logging_sink.k.writer_identity
    cmek_name        = data.google_logging_project_cmek_settings.c.name
    cmek_sa          = data.google_logging_project_cmek_settings.c.service_account_id
  }
}
`, "projects/"+p, description, visibility, filter, resources, p)
	}
	check := func(when, description, visibility, filter string, scopeResources int) {
		t.Helper()
		q, err := svc.Projects.Locations.SavedQueries.Get(queryName).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: SavedQueries.Get: %v", when, err)
		}
		if q.DisplayName != "Errors" || q.Description != description || q.Visibility != visibility ||
			q.LoggingQuery == nil || q.LoggingQuery.Filter != filter || len(q.LoggingQuery.SummaryFields) != 1 ||
			q.LoggingQuery.SummaryFields[0].Field != "jsonPayload.msg" {
			t.Errorf("%s: the saved query = %+v (query %+v)", when, q, q.LoggingQuery)
		}
		s, err := svc.Projects.Locations.LogScopes.Get(scopeName).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: LogScopes.Get: %v", when, err)
		}
		if s.Description != description || len(s.ResourceNames) != scopeResources || s.ResourceNames[0] != "projects/"+p {
			t.Errorf("%s: the log scope = %+v; want %d resource names", when, s, scopeResources)
		}
		sink, err := config.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: sinkName})
		if err != nil {
			t.Fatalf("%s: GetSink: %v", when, err)
		}
		cmek, err := config.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: "projects/" + p + "/cmekSettings"})
		if err != nil {
			t.Fatalf("%s: GetCmekSettings: %v", when, err)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		want := map[string]string{
			"sink_filter": sink.GetFilter(), "sink_destination": sink.GetDestination(), "sink_writer": sink.GetWriterIdentity(),
			"cmek_name": cmek.GetName(), "cmek_sa": cmek.GetServiceAccountId(),
		}
		if fmt.Sprint(got) != fmt.Sprint(want) || got["sink_filter"] != filter {
			t.Errorf("%s: the data sources = %v; the official client reads %v", when, got, want)
		}
	}

	m.write(module("first", "PRIVATE", "severity>=ERROR", false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first", "PRIVATE", "severity>=ERROR", 1)
	m.planClean()

	m.write(module("second", "SHARED", "severity>=WARNING", true))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 3 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "second", "SHARED", "severity>=WARNING", 2)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svc.Projects.Locations.SavedQueries.Get(queryName).Context(ctx).Do(); httpCode(err) != 404 {
		t.Errorf("the saved query after destroy: %v; want 404", err)
	}
	if _, err := svc.Projects.Locations.LogScopes.Get(scopeName).Context(ctx).Do(); httpCode(err) != 404 {
		t.Errorf("the log scope after destroy: %v; want 404", err)
	}
}

// TestTerraformBigtableSchemaBundle (#1304): a google_bigtable_schema_bundle
// on a google_bigtable_table, its proto_descriptors a FileDescriptorSet of
// google/protobuf/timestamp.proto, applies; the official admin client's
// GetSchemaBundle reads the same bytes back; the plan is clean; a set of
// timestamp.proto and duration.proto applies in place; the plan is clean
// again; destroy deletes the bundle, the table and the instance. The data
// sources google_bigtable_instance_iam_policy and
// google_bigtable_table_iam_policy read the policies a
// google_bigtable_table_iam_member and the instance's
// google_bigtable_instance_iam_member store (ADR-0006).
// covers: google.bigtable.admin.v2.BigtableTableAdmin/CreateSchemaBundle, google.bigtable.admin.v2.BigtableTableAdmin/GetSchemaBundle, google.bigtable.admin.v2.BigtableTableAdmin/UpdateSchemaBundle, google.bigtable.admin.v2.BigtableTableAdmin/DeleteSchemaBundle, google.bigtable.admin.v2.BigtableTableAdmin/GetIamPolicy, google.bigtable.admin.v2.BigtableInstanceAdmin/GetIamPolicy
func TestTerraformBigtableSchemaBundle(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h) // skips without CLOUDBURROW_TEST_BIGTABLE
	m := newTFModule(t)
	ctx := h.Context()
	const id, cluster, table = "tf-bt-bundle", "tf-bt-bundle-c1", "tfbundle"
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	set := func(files ...*descriptorpb.FileDescriptorProto) []byte {
		b, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: files})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ts := protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto)
	dur := protodesc.ToFileDescriptorProto(durationpb.File_google_protobuf_duration_proto)
	one, two := set(ts), set(ts, dur)
	module := func(descriptors []byte) string {
		return fmt.Sprintf(`
resource "google_bigtable_instance" "i" {
  project             = %[1]q
  name                = %[2]q
  deletion_protection = false
  cluster {
    cluster_id   = %[3]q
    zone         = "us-central1-b"
    num_nodes    = 1
    storage_type = "SSD"
  }
}
resource "google_bigtable_table" "t" {
  project             = google_bigtable_instance.i.project
  instance_name       = google_bigtable_instance.i.name
  name                = %[4]q
  deletion_protection = "UNPROTECTED"
  column_family { family = "cf" }
}
resource "google_bigtable_schema_bundle" "b" {
  project          = google_bigtable_table.t.project
  instance         = google_bigtable_table.t.instance_name
  table            = google_bigtable_table.t.name
  schema_bundle_id = "tfbundle"
  ignore_warnings  = true
  proto_schema {
    proto_descriptors = %[5]q
  }
}
# Stored, never enforced (ADR-0006).
resource "google_bigtable_instance_iam_member" "i" {
  project  = google_bigtable_instance.i.project
  instance = google_bigtable_instance.i.name
  role     = "roles/bigtable.reader"
  member   = %[6]q
}
resource "google_bigtable_table_iam_member" "t" {
  project       = google_bigtable_table.t.project
  instance_name = google_bigtable_table.t.instance_name
  table         = google_bigtable_table.t.name
  role          = "roles/bigtable.user"
  member        = %[6]q
}
data "google_bigtable_instance_iam_policy" "i" {
  project    = google_bigtable_instance.i.project
  instance   = google_bigtable_instance.i.name
  depends_on = [google_bigtable_instance_iam_member.i]
}
data "google_bigtable_table_iam_policy" "t" {
  project       = google_bigtable_table.t.project
  instance_name = google_bigtable_table.t.instance_name
  table         = google_bigtable_table.t.name
  depends_on    = [google_bigtable_table_iam_member.t]
}
output "outputs" {
  value = {
    instance = data.google_bigtable_instance_iam_policy.i.policy_data
    table    = data.google_bigtable_table_iam_policy.t.policy_data
  }
}
`, h.Project(), id, cluster, table, base64.StdEncoding.EncodeToString(descriptors), tfIAMMember)
	}
	check := func(when string, want []byte) {
		t.Helper()
		info, err := ac.GetSchemaBundle(ctx, table, "tfbundle")
		if err != nil {
			t.Fatalf("%s: GetSchemaBundle: %v", when, err)
		}
		if string(info.SchemaBundle) != string(want) || info.Etag == "" {
			t.Errorf("%s: the schema bundle holds %d bytes (etag %q); want the module's %d", when, len(info.SchemaBundle), info.Etag, len(want))
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		for k, role := range map[string]string{"instance": "roles/bigtable.reader", "table": "roles/bigtable.user"} {
			var pol struct {
				Bindings []struct {
					Role    string
					Members []string
				}
			}
			if err := json.Unmarshal([]byte(got[k]), &pol); err != nil || len(pol.Bindings) != 1 ||
				pol.Bindings[0].Role != role || strings.Join(pol.Bindings[0].Members, ",") != tfIAMMember {
				t.Errorf("%s: the %s IAM policy data source = %s (%v); want %s for %s", when, k, got[k], err, role, tfIAMMember)
			}
		}
		ip, err := iac.InstanceIAM(id).Policy(ctx)
		if err != nil || strings.Join(ip.Members("roles/bigtable.reader"), ",") != tfIAMMember {
			t.Errorf("%s: the instance's policy through the official client = %v, %v", when, ip, err)
		}
		tp, err := ac.TableIAM(table).Policy(ctx)
		if err != nil || strings.Join(tp.Members("roles/bigtable.user"), ",") != tfIAMMember {
			t.Errorf("%s: the table's policy through the official client = %v, %v", when, tp, err)
		}
	}
	m.write(module(one))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", one)
	m.planClean()

	m.write(module(two))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", two)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := iac.InstanceInfo(ctx, id); status.Code(err) != codes.NotFound {
		t.Errorf("InstanceInfo after destroy = %v, want NotFound", err)
	}
}

// TestTerraformSpannerInstancePartition (#1304): a
// google_spanner_instance_partition of one node on the emulator's
// configuration, in a google_spanner_instance, applies; the official
// InstanceAdminClient reads it back; the plan is clean; a new display name
// and two nodes apply in place (UpdateInstancePartition); the plan is clean
// again; destroy deletes the partition and the instance. The data sources
// google_spanner_instance_iam_policy and google_spanner_database_iam_policy
// read the policies the module's *_iam_member resources store (ADR-0006),
// compared with the official clients' GetIamPolicy.
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstancePartition, google.spanner.admin.instance.v1.InstanceAdmin/GetInstancePartition, google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstancePartition, google.spanner.admin.instance.v1.InstanceAdmin/DeleteInstancePartition, google.spanner.admin.instance.v1.InstanceAdmin/GetIamPolicy, google.spanner.admin.database.v1.DatabaseAdmin/GetIamPolicy
func TestTerraformSpannerInstancePartition(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvSpannerREST)                          // skips without the Spanner REST front
	f := newSpannerAdmin(t, "tf-part-fixture", "fixdb") // the clients
	m := newTFModule(t)
	const id, db = "tf-part", "tfpartdb"
	project := "projects/" + h.Project()
	name := project + "/instances/" + id + "/instancePartitions/tf-p1"
	module := func(display string, nodes int) string {
		return fmt.Sprintf(`
resource "google_spanner_instance" "i" {
  project      = %[1]q
  name         = %[2]q
  config       = "emulator-config"
  display_name = "Partitions"
  num_nodes    = 1
}
resource "google_spanner_database" "d" {
  project             = google_spanner_instance.i.project
  instance            = google_spanner_instance.i.name
  name                = %[3]q
  deletion_protection = false
}
resource "google_spanner_instance_partition" "p" {
  project      = google_spanner_instance.i.project
  instance     = google_spanner_instance.i.name
  name         = "tf-p1"
  config       = "projects/%[1]s/instanceConfigs/emulator-config"
  display_name = %[4]q
  node_count   = %[5]d
}
# Stored, never enforced (ADR-0006).
resource "google_spanner_instance_iam_member" "i" {
  project  = google_spanner_instance.i.project
  instance = google_spanner_instance.i.name
  role     = "roles/spanner.viewer"
  member   = %[6]q
}
resource "google_spanner_database_iam_member" "d" {
  project  = google_spanner_database.d.project
  instance = google_spanner_database.d.instance
  database = google_spanner_database.d.name
  role     = "roles/spanner.databaseReader"
  member   = %[6]q
}
data "google_spanner_instance_iam_policy" "i" {
  project    = google_spanner_instance.i.project
  instance   = google_spanner_instance.i.name
  depends_on = [google_spanner_instance_iam_member.i]
}
data "google_spanner_database_iam_policy" "d" {
  project    = google_spanner_database.d.project
  instance   = google_spanner_database.d.instance
  database   = google_spanner_database.d.name
  depends_on = [google_spanner_database_iam_member.d]
}
output "outputs" {
  value = {
    instance = data.google_spanner_instance_iam_policy.i.policy_data
    database = data.google_spanner_database_iam_policy.d.policy_data
  }
}
`, h.Project(), id, db, display, nodes, tfIAMMember)
	}
	check := func(when, display string, nodes int32) {
		t.Helper()
		p, err := f.inst.GetInstancePartition(f.ctx, &instancepb.GetInstancePartitionRequest{Name: name})
		if err != nil {
			t.Fatalf("%s: GetInstancePartition: %v", when, err)
		}
		if p.GetDisplayName() != display || p.GetNodeCount() != nodes || !strings.HasSuffix(p.GetConfig(), "/instanceConfigs/emulator-config") {
			t.Errorf("%s: the partition = %v; want %q with %d nodes", when, p, display, nodes)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		ip, err := f.inst.GetIamPolicy(f.ctx, &iampb.GetIamPolicyRequest{Resource: project + "/instances/" + id})
		if err != nil {
			t.Fatalf("%s: the instance's GetIamPolicy: %v", when, err)
		}
		dp, err := f.db.GetIamPolicy(f.ctx, &iampb.GetIamPolicyRequest{Resource: project + "/instances/" + id + "/databases/" + db})
		if err != nil {
			t.Fatalf("%s: the database's GetIamPolicy: %v", when, err)
		}
		for k, sdk := range map[string]*iampb.Policy{"instance": ip, "database": dp} {
			var pol struct {
				Bindings []struct {
					Role    string
					Members []string
				}
			}
			if err := json.Unmarshal([]byte(got[k]), &pol); err != nil {
				t.Fatalf("%s: the %s policy_data %q: %v", when, k, got[k], err)
			}
			fromTF := map[string][]string{}
			for _, b := range pol.Bindings {
				fromTF[b.Role] = b.Members
			}
			if want := policyMap(sdk); fmt.Sprint(fromTF) != fmt.Sprint(want) || len(want) != 1 {
				t.Errorf("%s: the %s IAM policy data source = %v; the official client reads %v", when, k, fromTF, want)
			}
		}
	}
	m.write(module("first", 1))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first", 1)
	m.planClean()

	m.write(module("second", 2))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "second", 2)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := f.inst.GetInstancePartition(f.ctx, &instancepb.GetInstancePartitionRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("the partition after destroy = %v, want NotFound", err)
	}
}

// TestTerraformRegionalSecretIAM (#1304):
// google_secret_manager_regional_secret_iam_member, _binding and _policy on
// a google_secret_manager_regional_secret, through
// TestTerraformBigQueryIAM's phases (member and binding, none, policy,
// none), each read back with the official client's GetIamPolicy and planned
// clean; destroy deletes the secret. Stored, never enforced (ADR-0006).
// covers: google.cloud.secretmanager.v1.SecretManagerService/GetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/SetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret
func TestTerraformRegionalSecretIAM(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h) // asks for the Secret Manager endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	name := regionalParent(h) + "/secrets/tf-regional-iam"
	base := fmt.Sprintf(`
resource "google_secret_manager_regional_secret" "s" {
  project   = %q
  location  = "us-central1"
  secret_id = "tf-regional-iam"
}
`, h.Project())
	targets := []tfIAMTarget{{
		kind: "google_secret_manager_regional_secret", label: "s",
		ref: map[string]string{"project": "google_secret_manager_regional_secret.s.project",
			"location": "google_secret_manager_regional_secret.s.location", "secret_id": "google_secret_manager_regional_secret.s.secret_id"},
		read: func() map[string][]string {
			t.Helper()
			pol, err := sm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			return policyMap(pol)
		},
	}}
	runTFIAMPhases(t, m, base, [2]string{"roles/secretmanager.secretAccessor", "roles/secretmanager.viewer"}, targets)
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
}

// TestTerraformRegionalSecretDataSources (#1304): the data sources
// google_secret_manager_regional_secrets (filtered to the module's
// secrets), google_secret_manager_regional_secret_version_access (the
// latest version, and version 1 by number) and
// google_secret_manager_regional_secret_iam_policy, each compared with what
// the official client reads. The change adds a second version and a
// label, both read on the next apply, and the plan stays clean after each.
// covers: google.cloud.secretmanager.v1.SecretManagerService/ListSecrets, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/GetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/AddSecretVersion
func TestTerraformRegionalSecretDataSources(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h) // asks for the Secret Manager endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	p := h.Project()
	parent := regionalParent(h)
	module := func(env string, second bool) string {
		extra := ""
		if second {
			extra = `
resource "google_secret_manager_regional_secret_version" "v2" {
  secret      = google_secret_manager_regional_secret.a.id
  secret_data = "two"
  depends_on  = [google_secret_manager_regional_secret_version.v1]
}
`
		}
		latestAfter := "google_secret_manager_regional_secret_version.v1"
		if second {
			latestAfter = "google_secret_manager_regional_secret_version.v2"
		}
		return fmt.Sprintf(`
resource "google_secret_manager_regional_secret" "a" {
  project   = %[1]q
  location  = "us-central1"
  secret_id = "tfds11-a"
  labels    = { env = %[2]q }
}
resource "google_secret_manager_regional_secret" "b" {
  project   = %[1]q
  location  = "us-central1"
  secret_id = "tfds11-b"
}
resource "google_secret_manager_regional_secret_version" "v1" {
  secret      = google_secret_manager_regional_secret.a.id
  secret_data = "one"
}
# Stored, never enforced (ADR-0006).
resource "google_secret_manager_regional_secret_iam_member" "m" {
  project   = %[1]q
  location  = "us-central1"
  secret_id = google_secret_manager_regional_secret.a.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = %[3]q
}
%[4]s
data "google_secret_manager_regional_secrets" "all" {
  project    = %[1]q
  location   = "us-central1"
  filter     = "name:tfds11-"
  depends_on = [google_secret_manager_regional_secret.a, google_secret_manager_regional_secret.b]
}
data "google_secret_manager_regional_secret_version_access" "latest" {
  project    = %[1]q
  location   = "us-central1"
  secret     = google_secret_manager_regional_secret.a.secret_id
  depends_on = [%[5]s]
}
data "google_secret_manager_regional_secret_version_access" "first" {
  project  = %[1]q
  location = "us-central1"
  secret   = google_secret_manager_regional_secret.a.secret_id
  version  = google_secret_manager_regional_secret_version.v1.version
}
data "google_secret_manager_regional_secret_iam_policy" "p" {
  project    = %[1]q
  location   = "us-central1"
  secret_id  = google_secret_manager_regional_secret.a.secret_id
  depends_on = [google_secret_manager_regional_secret_iam_member.m]
}
output "outputs" {
  value = {
    secrets        = join(",", sort([for s in data.google_secret_manager_regional_secrets.all.secrets : "${s.secret_id}=${lookup(s.labels, "env", "")}"]))
    latest         = nonsensitive(data.google_secret_manager_regional_secret_version_access.latest.secret_data)
    latest_version = data.google_secret_manager_regional_secret_version_access.latest.version
    first          = nonsensitive(data.google_secret_manager_regional_secret_version_access.first.secret_data)
    policy         = data.google_secret_manager_regional_secret_iam_policy.p.policy_data
  }
}
`, p, env, tfIAMMember, extra, latestAfter)
	}
	check := func(when, env, latest, latestVersion string) {
		t.Helper()
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		var listed []string
		it := sm.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: parent, Filter: "name:tfds11-"})
		for {
			s, err := it.Next()
			if err != nil {
				break
			}
			listed = append(listed, s.GetName()[strings.LastIndex(s.GetName(), "/")+1:]+"="+s.GetLabels()["env"])
		}
		sort.Strings(listed)
		if got["secrets"] != strings.Join(listed, ",") || got["secrets"] != "tfds11-a="+env+",tfds11-b=" {
			t.Errorf("%s: google_secret_manager_regional_secrets = %q; the official client lists %v", when, got["secrets"], listed)
		}
		acc, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: parent + "/secrets/tfds11-a/versions/latest"})
		if err != nil {
			t.Fatalf("%s: AccessSecretVersion latest: %v", when, err)
		}
		if got["latest"] != string(acc.GetPayload().GetData()) || got["latest"] != latest || got["latest_version"] != latestVersion || got["first"] != "one" {
			t.Errorf("%s: the version data sources = latest %q (version %q), first %q; the official client reads %q",
				when, got["latest"], got["latest_version"], got["first"], acc.GetPayload().GetData())
		}
		pol, err := sm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: parent + "/secrets/tfds11-a"})
		if err != nil {
			t.Fatalf("%s: GetIamPolicy: %v", when, err)
		}
		var tfPol struct {
			Bindings []struct {
				Role    string
				Members []string
			}
		}
		if err := json.Unmarshal([]byte(got["policy"]), &tfPol); err != nil {
			t.Fatalf("%s: policy_data %q: %v", when, got["policy"], err)
		}
		fromTF := map[string][]string{}
		for _, b := range tfPol.Bindings {
			fromTF[b.Role] = b.Members
		}
		if want := policyMap(pol); fmt.Sprint(fromTF) != fmt.Sprint(want) || len(want) != 1 {
			t.Errorf("%s: google_secret_manager_regional_secret_iam_policy = %v; the official client reads %v", when, fromTF, want)
		}
	}
	m.write(module("dev", false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "dev", "one", "1")
	m.planClean()

	m.write(module("prod", true))
	m.apply()
	check("after the change", "prod", "two", "2")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, id := range []string{"tfds11-a", "tfds11-b"} {
		if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: parent + "/secrets/" + id}); status.Code(err) != codes.NotFound {
			t.Errorf("GetSecret %s after destroy: %v; want NotFound", id, err)
		}
	}
}

// tfPolicyBindings is the role-to-members map of an IAM data source's
// policy_data.
func tfPolicyBindings(t *testing.T, policyData string) map[string][]string {
	t.Helper()
	var pol struct {
		Bindings []struct {
			Role    string
			Members []string
		}
	}
	if err := json.Unmarshal([]byte(policyData), &pol); err != nil {
		t.Fatalf("policy_data %q: %v", policyData, err)
	}
	out := map[string][]string{}
	for _, b := range pol.Bindings {
		out[b.Role] = b.Members
	}
	return out
}

// TestTerraformKMSDataSourcesBatch11 (#1304): the KMS data sources not yet
// verified, each output compared with what the official clients read:
// google_kms_key_ring_iam_policy, google_kms_crypto_key_iam_policy and
// google_kms_ekm_connection_iam_policy (the policies the module's
// *_iam_member resources store, ADR-0006), google_kms_key_handles (the key
// handle Autokey made in a project of a folder with an AutokeyConfig) and,
// after the change, google_kms_secret_asymmetric, which decrypts with an
// ASYMMETRIC_DECRYPT key a ciphertext the test made with the key's public
// key. The change also relabels the symmetric key in place; the plan is
// clean after each apply.
// covers: google.iam.v1.IAMPolicy/GetIamPolicy, google.cloud.kms.v1.Autokey/ListKeyHandles, google.cloud.kms.v1.KeyManagementService/GetPublicKey, google.cloud.kms.v1.KeyManagementService/AsymmetricDecrypt
func TestTerraformKMSDataSourcesBatch11(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvKMS) // skips without CLOUDBURROW_TEST_KMS
	c := kmsClients(t, h)["grpc"]
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	ringID := fmt.Sprintf("tfds11-%s-%d", strings.TrimPrefix(project, "cb-test-"), time.Now().Unix()%100000)
	ring := "projects/" + project + "/locations/us-central1/keyRings/" + ringID
	folder := folderLabel(h, "tfds11-")
	resourceProject := v1ID(h, "tfds11-")
	ekm := "projects/" + project + "/locations/asia-east1/ekmConnections/tf-ds11"
	der := base64.StdEncoding.EncodeToString(ekmCertificate(t))
	module := func(team, ciphertext string) string {
		extra := ""
		if ciphertext != "" {
			extra = fmt.Sprintf(`
data "google_kms_secret_asymmetric" "s" {
  crypto_key_version = "${google_kms_crypto_key.asym.id}/cryptoKeyVersions/1"
  ciphertext         = %q
}
output "plaintext" {
  value = nonsensitive(data.google_kms_secret_asymmetric.s.plaintext)
}
`, ciphertext)
		}
		return fmt.Sprintf(`
resource "google_kms_key_ring" "kr" {
  project  = %[1]q
  name     = %[2]q
  location = "us-central1"
}
resource "google_kms_crypto_key" "k" {
  name     = "k"
  key_ring = google_kms_key_ring.kr.id
  labels   = { team = %[3]q }
}
resource "google_kms_crypto_key" "asym" {
  name     = "asym"
  key_ring = google_kms_key_ring.kr.id
  purpose  = "ASYMMETRIC_DECRYPT"
  version_template {
    algorithm = "RSA_DECRYPT_OAEP_2048_SHA256"
  }
}
resource "google_kms_ekm_connection" "ekm" {
  project             = %[1]q
  name                = "tf-ds11"
  location            = "asia-east1"
  key_management_mode = "MANUAL"
  service_resolvers {
    service_directory_service = "projects/%[1]s/locations/asia-east1/namespaces/ekm/services/replica"
    hostname                  = "ekm.example.com"
    server_certificates {
      raw_der = %[4]q
    }
  }
}
# Stored, never enforced (ADR-0006).
resource "google_kms_key_ring_iam_member" "kr" {
  key_ring_id = google_kms_key_ring.kr.id
  role        = "roles/cloudkms.viewer"
  member      = %[5]q
}
resource "google_kms_crypto_key_iam_member" "k" {
  crypto_key_id = google_kms_crypto_key.k.id
  role          = "roles/cloudkms.cryptoKeyEncrypter"
  member        = %[5]q
}
resource "google_kms_ekm_connection_iam_member" "ekm" {
  project  = %[1]q
  location = "asia-east1"
  name     = google_kms_ekm_connection.ekm.id
  role     = "roles/cloudkms.viewer"
  member   = %[5]q
}
resource "google_folder" "f" {
  display_name        = %[6]q
  parent              = %[7]q
  deletion_protection = false
}
resource "google_project" "p" {
  project_id      = %[8]q
  name            = "Autokey handles"
  folder_id       = google_folder.f.folder_id
  deletion_policy = "DELETE"
}
resource "google_kms_autokey_config" "ak" {
  folder      = google_folder.f.folder_id
  key_project = "projects/%[1]s"
}
resource "google_kms_key_handle" "kh" {
  project                = google_project.p.project_id
  name                   = "tf-ds11-bucket"
  location               = "us-central1"
  resource_type_selector = "storage.googleapis.com/Bucket"
  depends_on             = [google_kms_autokey_config.ak]
}
data "google_kms_key_ring_iam_policy" "kr" {
  key_ring_id = google_kms_key_ring.kr.id
  depends_on  = [google_kms_key_ring_iam_member.kr]
}
data "google_kms_crypto_key_iam_policy" "k" {
  crypto_key_id = google_kms_crypto_key.k.id
  depends_on    = [google_kms_crypto_key_iam_member.k]
}
data "google_kms_ekm_connection_iam_policy" "ekm" {
  project    = %[1]q
  location   = "asia-east1"
  name       = google_kms_ekm_connection.ekm.id
  depends_on = [google_kms_ekm_connection_iam_member.ekm]
}
data "google_kms_key_handles" "h" {
  project                = google_project.p.project_id
  location               = "us-central1"
  resource_type_selector = "storage.googleapis.com/Bucket"
  depends_on             = [google_kms_key_handle.kh]
}
output "outputs" {
  value = {
    ring    = data.google_kms_key_ring_iam_policy.kr.policy_data
    key     = data.google_kms_crypto_key_iam_policy.k.policy_data
    ekm     = data.google_kms_ekm_connection_iam_policy.ekm.policy_data
    handles = join(",", [for k in data.google_kms_key_handles.h.key_handles : "${k.name}=${k.kms_key}=${k.resource_type_selector}"])
  }
}
%[9]s`, project, ringID, team, der, tfIAMMember, folder, localOrg, resourceProject, extra)
	}
	ec, err := newEkmClient(h)
	if err != nil {
		t.Fatal(err)
	}
	defer ec.Close()
	ak, err := newAutokeyClient(h)
	if err != nil {
		t.Fatal(err)
	}
	defer ak.Close()
	check := func(when, team string) {
		t.Helper()
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		for k, name := range map[string]string{"ring": ring, "key": ring + "/cryptoKeys/k", "ekm": ekm} {
			var pol *iampb.Policy
			var err error
			if k == "ekm" {
				pol, err = ec.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			} else {
				pol, err = c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			}
			if err != nil {
				t.Fatalf("%s: GetIamPolicy on %s: %v", when, name, err)
			}
			if fromTF, want := tfPolicyBindings(t, got[k]), policyMap(pol); fmt.Sprint(fromTF) != fmt.Sprint(want) || len(want) != 1 {
				t.Errorf("%s: the %s IAM policy data source = %v; the official client reads %v", when, k, fromTF, want)
			}
		}
		var handles []string
		it := ak.ListKeyHandles(ctx, &kmspb.ListKeyHandlesRequest{Parent: "projects/" + resourceProject + "/locations/us-central1",
			Filter: "resource_type_selector=storage.googleapis.com/Bucket"})
		for {
			kh, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("%s: ListKeyHandles: %v", when, err)
			}
			handles = append(handles, kh.GetName()+"="+kh.GetKmsKey()+"="+kh.GetResourceTypeSelector())
		}
		if got["handles"] != strings.Join(handles, ",") || len(handles) != 1 ||
			!strings.HasPrefix(handles[0], "projects/"+resourceProject+"/locations/us-central1/keyHandles/tf-ds11-bucket=") {
			t.Errorf("%s: google_kms_key_handles = %q; the official client lists %v", when, got["handles"], handles)
		}
		k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/k"})
		if err != nil || k.GetLabels()["team"] != team {
			t.Errorf("%s: the symmetric key = %v, %v; want label team=%s", when, k, err, team)
		}
	}
	m.write(module("burrow", ""))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "burrow")
	m.planClean()

	// The ciphertext the data source decrypts, made with the key's public key.
	pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: ring + "/cryptoKeys/asym/cryptoKeyVersions/1"})
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	block, _ := pem.Decode([]byte(pk.GetPem()))
	if block == nil {
		t.Fatalf("the public key is not PEM: %q", pk.GetPem())
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("the public key is a %T, not RSA", pub)
	}
	const secret = "burrow secret"
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, rsaPub, []byte(secret), nil)
	if err != nil {
		t.Fatal(err)
	}

	m.write(module("warren", base64.StdEncoding.EncodeToString(ct)))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "warren")
	if got := strings.TrimSpace(m.must("output", "-raw", "plaintext")); got != secret {
		t.Errorf("google_kms_secret_asymmetric's plaintext = %q; want %q", got, secret)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}

// TestTerraformStorageObjectContentsDataSource (#1304): the data source
// google_storage_bucket_object_contents lists the objects under a prefix
// of a bucket the module makes, with each one's content, compared with
// what the official client reads. The change adds an object under the
// prefix and changes one's content, both read on the next apply; the plan
// is clean after each.
// covers: storage.objects.list, storage.objects.get
func TestTerraformStorageObjectContentsDataSource(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h) // asks for the Storage endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tfds11"
	module := func(a string, third bool) string {
		extra := ""
		if third {
			extra = `
resource "google_storage_bucket_object" "c" {
  bucket  = google_storage_bucket.b.name
  name    = "logs/c.txt"
  content = "gamma"
}
`
		}
		deps := "google_storage_bucket_object.a, google_storage_bucket_object.b, google_storage_bucket_object.other"
		if third {
			deps += ", google_storage_bucket_object.c"
		}
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %[1]q
  name                        = %[2]q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
}
resource "google_storage_bucket_object" "a" {
  bucket  = google_storage_bucket.b.name
  name    = "logs/a.txt"
  content = %[3]q
}
resource "google_storage_bucket_object" "b" {
  bucket  = google_storage_bucket.b.name
  name    = "logs/b.txt"
  content = "beta"
}
resource "google_storage_bucket_object" "other" {
  bucket  = google_storage_bucket.b.name
  name    = "other/x.txt"
  content = "x"
}
%[4]s
data "google_storage_bucket_object_contents" "logs" {
  bucket     = google_storage_bucket.b.name
  prefix     = "logs/"
  depends_on = [%[5]s]
}
output "contents" {
  value = join(",", [for o in data.google_storage_bucket_object_contents.logs.bucket_objects : "${o.name}=${o.content}"])
}
`, h.Project(), bucket, a, extra, deps)
	}
	check := func(when string) {
		t.Helper()
		var want []string
		it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "logs/"})
		for {
			o, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("%s: listing the bucket: %v", when, err)
			}
			r, err := sc.Bucket(bucket).Object(o.Name).NewReader(ctx)
			if err != nil {
				t.Fatalf("%s: reading %s: %v", when, o.Name, err)
			}
			b, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, o.Name+"="+string(b))
		}
		got := strings.Split(strings.TrimSpace(m.must("output", "-raw", "contents")), ",")
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") || len(want) < 2 {
			t.Errorf("%s: google_storage_bucket_object_contents = %v; the official client reads %v", when, got, want)
		}
	}
	m.write(module("alpha", false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply")
	m.planClean()

	m.write(module("alpha2", true))
	m.apply()
	check("after the change")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sc.Bucket(bucket).Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket after destroy: %v; want it gone", err)
	}
}
