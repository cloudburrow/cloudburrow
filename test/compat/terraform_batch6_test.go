//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// #1304's sixth batch of google provider resources, each through
// `cloudburrow terraform` with hashicorp/google and egress blocked, read
// back through an official SDK, planned clean, changed in place where the
// resource has an updatable field, planned clean again and destroyed. Each
// test asks for the endpoints of the services it uses first, so it skips in
// a shard without them.

// TestTerraformStorageAnywhereCache (#1304): a google_storage_anywhere_cache
// in a zone of its bucket's region applies (anywhereCaches.insert, whose
// operation the provider waits for), the official JSON API client reads
// it back running with its TTL and the default admission policy, the plan
// is clean, a new TTL applies in place (anywhereCaches.update and its
// operation), the plan is clean again, and destroy disables the cache
// (anywhereCaches.disable) and deletes the bucket. The provider's
// admission_policy is deprecated, saying the backend ignores it and admits
// on the first miss, so the module leaves it out. Its create POSTs to
// b/{bucket}/anywhereCaches/, with a trailing slash, which the storage
// server now routes to anywhereCaches.insert.
// covers: storage.anywhereCaches.insert, storage.anywhereCaches.get, storage.anywhereCaches.update, storage.anywhereCaches.disable, storage.buckets.operations.get
func TestTerraformStorageAnywhereCache(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	s := storageJSON(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tfcache"
	const zone = "us-central1-a"
	module := func(ttl string) string {
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %q
  name                        = %q
  location                    = "US-CENTRAL1"
  force_destroy               = true
  uniform_bucket_level_access = true
}
resource "google_storage_anywhere_cache" "c" {
  bucket = google_storage_bucket.b.name
  zone   = %q
  ttl    = %q
}
`, h.Project(), bucket, zone, ttl)
	}
	check := func(when, ttl string) {
		t.Helper()
		c, err := s.AnywhereCaches.Get(bucket, zone).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, anywhereCaches.get: %v", when, err)
		}
		if c.State != "running" || c.Ttl != ttl || c.AdmissionPolicy != "admit-on-first-miss" || c.PendingUpdate {
			t.Errorf("%s, the cache = state %q, ttl %q, admission %q, pending update %t; want running, %s, admit-on-first-miss and none",
				when, c.State, c.Ttl, c.AdmissionPolicy, c.PendingUpdate, ttl)
		}
	}

	m.write(module("172800s"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "172800s")
	m.planClean()

	m.write(module("86400s"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the new TTL was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "86400s")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err := s.Buckets.Get(bucket).Context(ctx).Do()
	if e, ok := err.(*googleapi.Error); !ok || e.Code != 404 {
		t.Errorf("bucket %s after destroy: %v; want 404", bucket, err)
	}
}

// TestTerraformStorageFolder (#1304): two google_storage_folder, one inside
// the other, in a bucket with hierarchical_namespace enabled, apply
// (folders.insert), are read back by the official JSON API client, plan
// clean, take a force_destroy change in place (it is the provider's own
// flag: no request is sent, every other argument replaces a folder), plan
// clean again, and are destroyed (folders.delete), the bucket with them.
// covers: storage.folders.insert, storage.folders.get, storage.folders.delete
func TestTerraformStorageFolder(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	s := storageJSON(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tffolder"
	module := func(force bool) string {
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %q
  name                        = %q
  location                    = "US-CENTRAL1"
  force_destroy               = true
  uniform_bucket_level_access = true
  hierarchical_namespace {
    enabled = true
  }
}
resource "google_storage_folder" "parent" {
  bucket        = google_storage_bucket.b.name
  name          = "docs/"
  force_destroy = %t
}
resource "google_storage_folder" "child" {
  bucket        = google_storage_bucket.b.name
  name          = "${google_storage_folder.parent.name}old/"
  force_destroy = %t
}
`, h.Project(), bucket, force, force)
	}
	check := func(when string) {
		t.Helper()
		for _, name := range []string{"docs/", "docs/old/"} {
			f, err := s.Folders.Get(bucket, name).Context(ctx).Do()
			if err != nil || f.Name != name || f.Bucket != bucket {
				t.Errorf("%s, folders.get %s = %+v, %v", when, name, f, err)
			}
		}
	}

	m.write(module(false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply")
	m.planClean()

	m.write(module(true))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("force_destroy was not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err := s.Buckets.Get(bucket).Context(ctx).Do()
	if e, ok := err.(*googleapi.Error); !ok || e.Code != 404 {
		t.Errorf("bucket %s after destroy: %v; want 404", bucket, err)
	}
}

// TestTerraformStorageObjectRetention (#1304): a google_storage_bucket with
// enable_object_retention and a google_storage_bucket_object with an
// Unlocked retention apply; the official client reads the object's mode
// and retain-until time back; the plan is clean; a later retain-until time
// applies in place (objects.patch); the plan is clean again; removing the
// retention applies in place too (an Unlocked retention may be removed, by
// objects.patch with overrideUnlockedRetention), and then destroy deletes
// the object and the bucket.
// covers: storage.objects.insert, storage.objects.get, storage.objects.patch, storage.objects.delete
func TestTerraformStorageObjectRetention(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	sc := storageClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tfretain"
	const object = "kept.txt"
	first := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	later := first.Add(24 * time.Hour)
	module := func(until *time.Time) string {
		retention := ""
		if until != nil {
			retention = fmt.Sprintf(`
  retention {
    mode              = "Unlocked"
    retain_until_time = %q
  }`, until.Format("2006-01-02T15:04:05.000Z"))
		}
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %q
  name                        = %q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
  enable_object_retention     = true
}
resource "google_storage_bucket_object" "o" {
  bucket  = google_storage_bucket.b.name
  name    = %q
  content = "retained"%s
}
`, h.Project(), bucket, object, retention)
	}
	check := func(when string, until *time.Time) {
		t.Helper()
		a, err := sc.Bucket(bucket).Object(object).Attrs(ctx)
		if err != nil {
			t.Fatalf("%s, Attrs: %v", when, err)
		}
		switch {
		case until == nil && a.Retention != nil:
			t.Errorf("%s, the object's retention = %+v; want none", when, a.Retention)
		case until != nil && (a.Retention == nil || a.Retention.Mode != "Unlocked" || !a.Retention.RetainUntil.Equal(*until)):
			t.Errorf("%s, the object's retention = %+v; want Unlocked until %s", when, a.Retention, until)
		}
	}

	m.write(module(&first))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", &first)
	m.planClean()

	m.write(module(&later))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the later retain-until time was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", &later)
	m.planClean()

	m.write(module(nil))
	out = m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("removing the retention was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after removing it", nil)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sc.Bucket(bucket).Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket after destroy: %v; want ErrBucketNotExist", err)
	}
}

// TestTerraformRunWorkerPoolIAM (#1304): google_cloud_run_v2_worker_pool_iam_member,
// _binding and _policy on a google_cloud_run_v2_worker_pool with no
// instances, through TestTerraformBigQueryIAM's phases (member and binding,
// none, policy, none), each read back by the official WorkerPoolsClient
// and planned clean. Stored, never enforced (ADR-0006). Destroy removes
// the pool with its policy.
// covers: google.cloud.run.v2.WorkerPools/GetIamPolicy, google.cloud.run.v2.WorkerPools/SetIamPolicy
func TestTerraformRunWorkerPoolIAM(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	m := newTFModule(t)
	ctx := h.Context()
	pools, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	id := "tfpooliam-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/workerPools/" + id
	base := fmt.Sprintf(`
resource "google_cloud_run_v2_worker_pool" "p" {
  project             = %q
  name                = %q
  location            = "us-central1"
  deletion_protection = false
  scaling {
    manual_instance_count = 0
  }
  template {
    containers {
      image   = %q
      command = ["sh", "-c", "exec sleep 3600"]
    }
  }
}
`, h.Project(), id, jobImage)
	targets := []tfIAMTarget{{
		kind: "google_cloud_run_v2_worker_pool", label: "p",
		ref: map[string]string{"project": "google_cloud_run_v2_worker_pool.p.project", "location": "google_cloud_run_v2_worker_pool.p.location",
			"name": "google_cloud_run_v2_worker_pool.p.name"},
		read: func() map[string][]string {
			t.Helper()
			pol, err := pools.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			return policyMap(pol)
		},
	}}
	runTFIAMPhases(t, m, base, [2]string{"roles/run.invoker", "roles/run.developer"}, targets)
	if _, err := pools.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWorkerPool after destroy: %v; want NotFound", err)
	}
}

// TestTerraformBigQueryDatasetCMEK (#1304): a google_bigquery_dataset with
// default_encryption_configuration and a google_bigquery_table with
// encryption_configuration apply; the official client reads both key names
// back; the plan is clean; a new default key on the dataset applies in
// place (datasets.patch), the table keeping its own; the plan is clean
// again; destroy deletes both. The keys are recorded, not used: the
// emulator encrypts nothing, so they need not exist in Cloud KMS.
// covers: bigquery.datasets.insert, bigquery.datasets.get, bigquery.datasets.patch, bigquery.datasets.delete, bigquery.tables.insert, bigquery.tables.get, bigquery.tables.delete
func TestTerraformBigQueryDatasetCMEK(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	m := newTFModule(t)
	ctx := h.Context()
	ds := "tfcmek_" + strings.ReplaceAll(h.Project(), "-", "_")
	keyOf := func(k string) string {
		return "projects/" + project + "/locations/us/keyRings/tf/cryptoKeys/" + k
	}
	module := func(datasetKey string) string {
		return fmt.Sprintf(`
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  location                   = "US"
  delete_contents_on_destroy = true
  default_encryption_configuration {
    kms_key_name = %[3]q
  }
}
resource "google_bigquery_table" "t" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.d.dataset_id
  table_id            = "orders"
  deletion_protection = false
  schema = jsonencode([{ name = "id", type = "INTEGER", mode = "NULLABLE" }])
  encryption_configuration {
    kms_key_name = %[4]q
  }
}
`, project, ds, keyOf(datasetKey), keyOf("table"))
	}
	check := func(when, datasetKey string) {
		t.Helper()
		md, err := c.Dataset(ds).Metadata(ctx)
		if err != nil {
			t.Fatalf("%s, the dataset: %v", when, err)
		}
		if e := md.DefaultEncryptionConfig; e == nil || e.KMSKeyName != keyOf(datasetKey) {
			t.Errorf("%s, the dataset's default key = %+v; want %s", when, e, keyOf(datasetKey))
		}
		tm, err := c.Dataset(ds).Table("orders").Metadata(ctx)
		if err != nil {
			t.Fatalf("%s, the table: %v", when, err)
		}
		if e := tm.EncryptionConfig; e == nil || e.KMSKeyName != keyOf("table") {
			t.Errorf("%s, the table's key = %+v; want %s", when, e, keyOf("table"))
		}
	}

	m.write(module("first"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first")
	m.planClean()

	m.write(module("second"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the dataset's new key was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "second")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	var gerr *googleapi.Error
	if _, err := c.Dataset(ds).Metadata(ctx); !errors.As(err, &gerr) || gerr.Code != 404 {
		t.Errorf("the dataset after destroy: %v; want 404", err)
	}
}

// TestTerraformPubSubSubscriptionDeadLetterAndRetry (#1304): a
// google_pubsub_subscription with a dead_letter_policy (its topic made by
// the module too) and a retry_policy applies; the official client reads
// both back; the plan is clean; a new max_delivery_attempts and backoff
// apply in place (UpdateSubscription); the plan is clean again; destroy
// deletes the subscription and both topics.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/DeleteSubscription
func TestTerraformPubSubSubscriptionDeadLetterAndRetry(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvPubSub)
	ps := pubsubClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	p := h.Project()
	sub := "projects/" + p + "/subscriptions/tf-dlq-sub"
	module := func(attempts int, minBackoff string) string {
		return fmt.Sprintf(`
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-dlq-main"
}
resource "google_pubsub_topic" "dead" {
  project = %[1]q
  name    = "tf-dlq-dead"
}
resource "google_pubsub_subscription" "s" {
  project = %[1]q
  name    = "tf-dlq-sub"
  topic   = google_pubsub_topic.t.id
  dead_letter_policy {
    dead_letter_topic     = google_pubsub_topic.dead.id
    max_delivery_attempts = %[2]d
  }
  retry_policy {
    minimum_backoff = %[3]q
    maximum_backoff = "600s"
  }
}
`, p, attempts, minBackoff)
	}
	check := func(when string, attempts int32, minBackoff time.Duration) {
		t.Helper()
		s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
		if err != nil {
			t.Fatalf("%s, GetSubscription: %v", when, err)
		}
		dl, rp := s.GetDeadLetterPolicy(), s.GetRetryPolicy()
		if dl.GetDeadLetterTopic() != "projects/"+p+"/topics/tf-dlq-dead" || dl.GetMaxDeliveryAttempts() != attempts ||
			rp.GetMinimumBackoff().AsDuration() != minBackoff || rp.GetMaximumBackoff().AsDuration() != 600*time.Second {
			t.Errorf("%s, the subscription's policies = %v, %v; want dead letters to tf-dlq-dead after %d attempts, backoff %s to 600s",
				when, dl, rp, attempts, minBackoff)
		}
	}

	m.write(module(5, "10s"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 5, 10*time.Second)
	m.planClean()

	m.write(module(7, "20s"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the new policies were not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", 7, 20*time.Second)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after destroy: %v; want NotFound", err)
	}
	if _, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/" + p + "/topics/tf-dlq-dead"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic of the dead-letter topic after destroy: %v; want NotFound", err)
	}
}

// TestTerraformDataSources (#1304): the data sources that read what a
// module made: google_kms_secret decrypts a google_kms_secret_ciphertext
// (with its additional authenticated data, which the data source takes
// base64-encoded and the resource does not), google_kms_crypto_key names
// the key, google_secret_manager_secret_version reads a version's payload
// and google_secret_manager_secret its secret's labels, and
// google_storage_bucket_object_content an object's content, each output
// compared with what the official clients read. A data source has no
// update or destroy of its own: a changed version is read on the next
// apply, and the plan stays clean after each.
// covers: google.cloud.kms.v1.KeyManagementService/Decrypt, google.cloud.kms.v1.KeyManagementService/GetCryptoKey, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, storage.objects.get
func TestTerraformDataSources(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_KMS", EnvKMS)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	h.Endpoint(EnvKMS)
	h.Endpoint(EnvSecrets)
	h.Endpoint(EnvStorage)
	sm := secretsClient(t, h)
	sc := storageClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	p := h.Project()
	ring := "tfdata-" + p
	bucket := p + "-tfdata"
	module := func(payload string) string {
		return fmt.Sprintf(`
resource "google_kms_key_ring" "kr" {
  project  = %[1]q
  name     = %[2]q
  location = "us-central1"
}
resource "google_kms_crypto_key" "k" {
  name     = "k"
  key_ring = google_kms_key_ring.kr.id
}
resource "google_kms_secret_ciphertext" "c" {
  crypto_key                    = google_kms_crypto_key.k.id
  plaintext                     = %[4]q
  additional_authenticated_data = "burrow"
}
data "google_kms_secret" "d" {
  crypto_key                    = google_kms_crypto_key.k.id
  ciphertext                    = google_kms_secret_ciphertext.c.ciphertext
  # The data source sends its additional authenticated data as given, in
  # the JSON request's bytes field, so it is base64 here; the ciphertext
  # resource encodes its own.
  additional_authenticated_data = base64encode("burrow")
}
data "google_kms_crypto_key" "d" {
  name     = google_kms_crypto_key.k.name
  key_ring = google_kms_key_ring.kr.id
}
resource "google_secret_manager_secret" "s" {
  project   = %[1]q
  secret_id = "tf-data"
  labels    = { team = "burrow" }
  replication {
    auto {}
  }
}
resource "google_secret_manager_secret_version" "v" {
  secret      = google_secret_manager_secret.s.id
  secret_data = %[4]q
}
data "google_secret_manager_secret_version" "d" {
  project = %[1]q
  secret  = google_secret_manager_secret.s.secret_id
  version = google_secret_manager_secret_version.v.version
}
data "google_secret_manager_secret" "d" {
  project   = %[1]q
  secret_id = google_secret_manager_secret.s.secret_id
}
resource "google_storage_bucket" "b" {
  project                     = %[1]q
  name                        = %[3]q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
}
resource "google_storage_bucket_object" "o" {
  bucket  = google_storage_bucket.b.name
  name    = "data.txt"
  content = %[4]q
}
data "google_storage_bucket_object_content" "d" {
  bucket = google_storage_bucket.b.name
  name   = google_storage_bucket_object.o.name
}
output "outputs" {
  sensitive = true
  value = {
    kms_plaintext  = data.google_kms_secret.d.plaintext
    kms_key        = data.google_kms_crypto_key.d.id
    secret_data    = data.google_secret_manager_secret_version.d.secret_data
    secret_version = data.google_secret_manager_secret_version.d.name
    secret_labels  = data.google_secret_manager_secret.d.labels
    object_content = data.google_storage_bucket_object_content.d.content
  }
}
`, p, ring, bucket, payload)
	}
	check := func(when, payload string) {
		t.Helper()
		var got struct {
			KMSPlaintext  string            `json:"kms_plaintext"`
			KMSKey        string            `json:"kms_key"`
			SecretData    string            `json:"secret_data"`
			SecretVersion string            `json:"secret_version"`
			SecretLabels  map[string]string `json:"secret_labels"`
			ObjectContent string            `json:"object_content"`
		}
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		key := "projects/" + p + "/locations/us-central1/keyRings/" + ring + "/cryptoKeys/k"
		if got.KMSPlaintext != payload || got.KMSKey != key {
			t.Errorf("%s, google_kms_secret = %q, google_kms_crypto_key = %q; want %q and %q", when, got.KMSPlaintext, got.KMSKey, payload, key)
		}
		r, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: got.SecretVersion})
		if err != nil || string(r.GetPayload().GetData()) != payload || got.SecretData != payload {
			t.Errorf("%s, google_secret_manager_secret_version %s = %q; the official client reads %q, %v; want %q",
				when, got.SecretVersion, got.SecretData, r.GetPayload().GetData(), err, payload)
		}
		if got.SecretLabels["team"] != "burrow" {
			t.Errorf("%s, google_secret_manager_secret's labels = %v; want team burrow", when, got.SecretLabels)
		}
		if got.ObjectContent != payload {
			t.Errorf("%s, google_storage_bucket_object_content = %q; want %q", when, got.ObjectContent, payload)
		}
	}

	m.write(module("first payload"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first payload")
	m.planClean()

	m.write(module("second payload"))
	m.apply()
	check("after the change", "second payload")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sc.Bucket(bucket).Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket after destroy: %v; want ErrBucketNotExist", err)
	}
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/" + p + "/secrets/tf-data"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
}
