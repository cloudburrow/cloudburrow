//go:build compat

package compat

import (
	"fmt"
	"testing"

	apiv1 "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// Terraform resources beyond the first verified set (#1304), each applied
// with the official hashicorp/google provider through `cloudburrow
// terraform`, read back through an official SDK, planned clean, changed in
// place where the resource allows it, and destroyed. They run in the storage
// shard, whose instance serves Storage, Pub/Sub, Secret Manager and Cloud
// Scheduler.

// TestTerraformPubSubSchema (#1304): a google_pubsub_schema (Avro), and a
// google_pubsub_topic that validates against it with schema_settings, apply;
// the official SchemaClient and TopicAdminClient read them back; the plan is
// clean; the topic's encoding changes in place; and destroy removes both.
// covers: google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/GetSchema, google.pubsub.v1.SchemaService/DeleteSchema, google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Publisher/UpdateTopic, google.pubsub.v1.Publisher/DeleteTopic
func TestTerraformPubSubSchema(t *testing.T) {
	h := New(t)
	ps := pubsubClient(t, h) // skips without CLOUDBURROW_TEST_PUBSUB
	m := newTFModule(t)
	ctx := h.Context()
	sch, err := apiv1.NewSchemaClient(ctx, option.WithEndpoint(h.Endpoint(EnvPubSub)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer sch.Close()
	project := h.Project()
	schemaName := "projects/" + project + "/schemas/tf-schema"
	topicName := "projects/" + project + "/topics/tf-schema-topic"
	const def = `{"type":"record","name":"Order","fields":[{"name":"id","type":"string"},{"name":"qty","type":"int"}]}`
	module := func(encoding string) string {
		return fmt.Sprintf(`
resource "google_pubsub_schema" "s" {
  project    = %[1]q
  name       = "tf-schema"
  type       = "AVRO"
  definition = %[2]q
}
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-schema-topic"
  schema_settings {
    schema   = google_pubsub_schema.s.id
    encoding = %[3]q
  }
}
`, project, def, encoding)
	}
	check := func(when string, enc pubsubpb.Encoding) {
		t.Helper()
		s, err := sch.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: schemaName, View: pubsubpb.SchemaView_FULL})
		if err != nil {
			t.Fatalf("%s, GetSchema: %v", when, err)
		}
		if s.GetType() != pubsubpb.Schema_AVRO || s.GetDefinition() != def {
			t.Errorf("%s, the schema = %v %q", when, s.GetType(), s.GetDefinition())
		}
		tp, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName})
		if err != nil {
			t.Fatalf("%s, GetTopic: %v", when, err)
		}
		if ss := tp.GetSchemaSettings(); ss.GetSchema() != schemaName || ss.GetEncoding() != enc {
			t.Errorf("%s, the topic's schema settings = %v; want %s, %v", when, ss, schemaName, enc)
		}
	}

	m.write(module("JSON"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", pubsubpb.Encoding_JSON)
	m.planClean()

	m.write(module("BINARY"))
	m.apply()
	check("after the encoding change", pubsubpb.Encoding_BINARY)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic after destroy: %v; want NotFound", err)
	}
	if _, err := sch.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: schemaName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchema after destroy: %v; want NotFound", err)
	}
}

// TestTerraformSecretVersion (#1304): a google_secret_manager_secret_version
// applies with its payload, the official client accesses the payload, the
// plan is clean, `enabled = false` disables the version in place, and
// destroy destroys the version and deletes the secret.
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/AddSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/GetSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DisableSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DestroySecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret
func TestTerraformSecretVersion(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h) // skips without CLOUDBURROW_TEST_SECRETS
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	secret := "projects/" + project + "/secrets/tf-version"
	version := secret + "/versions/1"
	module := func(enabled bool) string {
		return fmt.Sprintf(`
resource "google_secret_manager_secret" "s" {
  project   = %q
  secret_id = "tf-version"
  replication {
    auto {}
  }
}
resource "google_secret_manager_secret_version" "v" {
  secret      = google_secret_manager_secret.s.id
  secret_data = "s3cr3t-from-terraform"
  enabled     = %t
}
`, project, enabled)
	}
	state := func(when string, want secretmanagerpb.SecretVersion_State) {
		t.Helper()
		v, err := sm.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: version})
		if err != nil {
			t.Fatalf("%s, GetSecretVersion: %v", when, err)
		}
		if v.GetState() != want {
			t.Errorf("%s, the version is %v; want %v", when, v.GetState(), want)
		}
	}

	m.write(module(true))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	state("after apply", secretmanagerpb.SecretVersion_ENABLED)
	a, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: secret + "/versions/latest"})
	if err != nil || string(a.GetPayload().GetData()) != "s3cr3t-from-terraform" {
		t.Errorf("AccessSecretVersion latest = %v; want the module's payload", err)
	}
	m.planClean()

	m.write(module(false))
	m.apply()
	state("after enabled = false", secretmanagerpb.SecretVersion_DISABLED)
	if _, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: version}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("AccessSecretVersion of the disabled version: %v; want FailedPrecondition", err)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secret}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
}

// TestTerraformStorageACLsAndManagedFolder (#1304): on a fine-grained bucket,
// a google_storage_bucket_access_control, a
// google_storage_default_object_access_control and a
// google_storage_object_access_control; on a bucket with uniform
// bucket-level access, a google_storage_managed_folder with a
// google_storage_managed_folder_iam_member (stored, never enforced,
// ADR-0006). Google's generated JSON API client reads each back; the plan is
// clean; each access control's role changes in place; destroy removes them.
// covers: storage.bucketAccessControls.insert, storage.bucketAccessControls.get, storage.bucketAccessControls.patch, storage.bucketAccessControls.delete, storage.defaultObjectAccessControls.insert, storage.defaultObjectAccessControls.get, storage.defaultObjectAccessControls.patch, storage.defaultObjectAccessControls.delete, storage.objectAccessControls.insert, storage.objectAccessControls.get, storage.objectAccessControls.patch, storage.objectAccessControls.delete, storage.managedFolders.insert, storage.managedFolders.get, storage.managedFolders.delete, storage.managedFolders.getIamPolicy, storage.managedFolders.setIamPolicy
func TestTerraformStorageACLsAndManagedFolder(t *testing.T) {
	h := New(t)
	_ = storageClient(t, h) // skips without CLOUDBURROW_TEST_STORAGE
	s := storageJSON(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	fine, uniform := project+"-tfacl", project+"-tfmf"
	const entity = "user-ada@example.com"
	const member = "user:ada@example.com"
	module := func(role string) string {
		return fmt.Sprintf(`
resource "google_storage_bucket" "fine" {
  project                     = %[1]q
  name                        = %[2]q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = false
}
resource "google_storage_bucket_access_control" "b" {
  bucket = google_storage_bucket.fine.name
  entity = %[4]q
  role   = %[5]q
}
resource "google_storage_default_object_access_control" "d" {
  bucket = google_storage_bucket.fine.name
  entity = %[4]q
  role   = %[5]q
}
resource "google_storage_bucket_object" "o" {
  bucket  = google_storage_bucket.fine.name
  name    = "acl.txt"
  content = "hello"
}
resource "google_storage_object_access_control" "o" {
  bucket = google_storage_bucket.fine.name
  object = google_storage_bucket_object.o.output_name
  entity = %[4]q
  role   = %[5]q
}
resource "google_storage_bucket" "uniform" {
  project                     = %[1]q
  name                        = %[3]q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
}
resource "google_storage_managed_folder" "f" {
  bucket = google_storage_bucket.uniform.name
  name   = "team/reports/"
}
# Stored, never enforced (ADR-0006).
resource "google_storage_managed_folder_iam_member" "f" {
  bucket         = google_storage_managed_folder.f.bucket
  managed_folder = google_storage_managed_folder.f.name
  role           = "roles/storage.objectViewer"
  member         = %[6]q
}
`, project, fine, uniform, entity, role, member)
	}
	check := func(when, role string) {
		t.Helper()
		if a, err := s.BucketAccessControls.Get(fine, entity).Context(ctx).Do(); err != nil || a.Role != role {
			t.Errorf("%s, the bucket access control = %v, %v; want %s", when, a, err, role)
		}
		if a, err := s.DefaultObjectAccessControls.Get(fine, entity).Context(ctx).Do(); err != nil || a.Role != role {
			t.Errorf("%s, the default object access control = %v, %v; want %s", when, a, err, role)
		}
		if a, err := s.ObjectAccessControls.Get(fine, "acl.txt", entity).Context(ctx).Do(); err != nil || a.Role != role {
			t.Errorf("%s, the object access control = %v, %v; want %s", when, a, err, role)
		}
		if f, err := s.ManagedFolders.Get(uniform, "team/reports/").Context(ctx).Do(); err != nil || f.Name != "team/reports/" {
			t.Errorf("%s, the managed folder = %v, %v", when, f, err)
		}
		p, err := s.ManagedFolders.GetIamPolicy(uniform, "team/reports/").Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, managedFolders.getIamPolicy: %v", when, err)
		}
		found := false
		for _, b := range p.Bindings {
			for _, mb := range b.Members {
				found = found || (b.Role == "roles/storage.objectViewer" && mb == member)
			}
		}
		if !found {
			t.Errorf("%s, the managed folder's policy has no objectViewer for %s: %+v", when, member, p.Bindings)
		}
	}

	m.write(module("READER"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "READER")
	m.planClean()

	m.write(module("OWNER"))
	m.apply()
	check("after the role change", "OWNER")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, b := range []string{fine, uniform} {
		_, err := s.Buckets.Get(b).Context(ctx).Do()
		if e, ok := err.(*googleapi.Error); !ok || e.Code != 404 {
			t.Errorf("bucket %s after destroy: %v; want 404", b, err)
		}
	}
}

// TestTerraformSchedulerHTTPTarget (#1304): a google_cloud_scheduler_job
// with an http_target carrying headers, a body and an oidc_token applies,
// the official client reads the target back, the plan is clean, the URI and
// the token's audience change in place, and destroy deletes the job.
// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/UpdateJob, google.cloud.scheduler.v1.CloudScheduler/DeleteJob
func TestTerraformSchedulerHTTPTarget(t *testing.T) {
	h := New(t)
	sched := schedulerClient(t, h) // skips without CLOUDBURROW_TEST_SCHEDULER
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	job := "projects/" + project + "/locations/us-central1/jobs/tf-http"
	sa := "tf-invoker@" + project + ".iam.gserviceaccount.com"
	module := func(uri, audience string) string {
		return fmt.Sprintf(`
resource "google_cloud_scheduler_job" "j" {
  project   = %[1]q
  region    = "us-central1"
  name      = "tf-http"
  schedule  = "*/10 * * * *"
  time_zone = "Etc/UTC"
  # Never due during the test: paused, so nothing is dispatched.
  paused    = true
  http_target {
    http_method = "POST"
    uri         = %[2]q
    body        = base64encode("{\"run\":true}")
    headers     = { "Content-Type" = "application/json" }
    oidc_token {
      service_account_email = %[4]q
      audience              = %[3]q
    }
  }
}
`, project, uri, audience, sa)
	}
	check := func(when, uri, audience string) {
		t.Helper()
		j, err := sched.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job})
		if err != nil {
			t.Fatalf("%s, GetJob: %v", when, err)
		}
		ht := j.GetHttpTarget()
		if ht.GetUri() != uri || ht.GetHttpMethod() != schedulerpb.HttpMethod_POST || string(ht.GetBody()) != `{"run":true}` ||
			ht.GetHeaders()["Content-Type"] != "application/json" ||
			ht.GetOidcToken().GetServiceAccountEmail() != sa || ht.GetOidcToken().GetAudience() != audience {
			t.Errorf("%s, the job's http target = %v", when, ht)
		}
	}

	m.write(module("https://example.com/run", "https://example.com"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "https://example.com/run", "https://example.com")
	m.planClean()

	m.write(module("https://example.com/v2/run", "https://example.com/v2"))
	m.apply()
	check("after the change", "https://example.com/v2/run", "https://example.com/v2")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sched.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after destroy: %v; want NotFound", err)
	}
}
