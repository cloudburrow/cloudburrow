//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformStoredIAM (#1304): the *_iam_member, *_iam_binding and
// *_iam_policy resources of every other stored-IAM surface ADR-0006 names,
// through `cloudburrow terraform` with egress blocked: a Cloud Storage
// bucket, a Secret Manager secret, a Cloud KMS key ring and crypto key, and
// a Cloud Tasks queue, each made by the provider too. The phases are
// TestTerraformBigQueryIAM's (member and binding, none, policy, none), each
// read back through the official Go client and planned clean. Nothing is
// enforced (ADR-0006).
// It runs where Storage, Secret Manager and Tasks are served (the storage
// shard); the KMS resources are TestTerraformKMSStoredIAM, in the shard KMS
// is served in, as no shard serves all four.
// covers: storage.buckets.getIamPolicy, storage.buckets.setIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/GetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/SetIamPolicy, google.cloud.tasks.v2.CloudTasks/GetIamPolicy, google.cloud.tasks.v2.CloudTasks/SetIamPolicy
func TestTerraformStoredIAM(t *testing.T) {
	h := New(t)
	m := newTFModule(t) // skips without terraform or the CLI
	sc := storageClient(t, h)
	smc := secretsClient(t, h)
	tc := tasksClient(t, h)
	ctx := h.Context()
	p := h.Project()
	bucket, secret, queue := p+"-tfiam", "tfiam", "tfiam"

	base := fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %[1]q
  name                        = %[2]q
  location                    = "US"
  uniform_bucket_level_access = true
  force_destroy               = true
}
resource "google_secret_manager_secret" "s" {
  project   = %[1]q
  secret_id = %[3]q
  replication {
    auto {}
  }
}
resource "google_cloud_tasks_queue" "q" {
  project  = %[1]q
  name     = %[4]q
  location = "us-central1"
}
`, p, bucket, secret, queue)

	fromProto := func(get func() (*iampb.Policy, error), what string) func() map[string][]string {
		return func() map[string][]string {
			t.Helper()
			pol, err := get()
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", what, err)
			}
			out := map[string][]string{}
			for _, b := range pol.GetBindings() {
				out[b.GetRole()] = b.GetMembers()
			}
			return out
		}
	}
	req := func(name string) *iampb.GetIamPolicyRequest { return &iampb.GetIamPolicyRequest{Resource: name} }
	secretName := "projects/" + p + "/secrets/" + secret
	queueName := "projects/" + p + "/locations/us-central1/queues/" + queue
	targets := []tfIAMTarget{
		{kind: "google_storage_bucket", label: "b", ref: map[string]string{"bucket": "google_storage_bucket.b.name"},
			read: func() map[string][]string {
				t.Helper()
				pol, err := sc.Bucket(bucket).IAM().Policy(ctx)
				if err != nil {
					t.Fatalf("Bucket.IAM().Policy: %v", err)
				}
				out := map[string][]string{}
				for _, r := range pol.Roles() {
					out[string(r)] = pol.Members(r)
				}
				return out
			}},
		{kind: "google_secret_manager_secret", label: "s",
			ref:  map[string]string{"project": "google_secret_manager_secret.s.project", "secret_id": "google_secret_manager_secret.s.secret_id"},
			read: fromProto(func() (*iampb.Policy, error) { return smc.GetIamPolicy(ctx, req(secretName)) }, secretName)},
		{kind: "google_cloud_tasks_queue", label: "q",
			ref:  map[string]string{"project": "google_cloud_tasks_queue.q.project", "location": "google_cloud_tasks_queue.q.location", "name": "google_cloud_tasks_queue.q.name"},
			read: fromProto(func() (*iampb.Policy, error) { return tc.GetIamPolicy(ctx, req(queueName)) }, queueName)},
	}
	runTFIAMPhases(t, m, base, [2]string{"roles/viewer", "roles/editor"}, targets)

	// Destroy removed the bucket, the secret and the queue.
	if _, err := sc.Bucket(bucket).Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket after destroy: %v; want ErrBucketNotExist", err)
	}
	if _, err := smc.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secretName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
}

// TestTerraformKMSStoredIAM (#1304): google_kms_key_ring_iam_* and
// google_kms_crypto_key_iam_* through the same four phases as
// TestTerraformStoredIAM, read back through the official KMS client; the
// policies are stored, not enforced (ADR-0006). Destroy leaves the key
// ring, as on Google, and the key keeps no enabled version.
// covers: google.iam.v1.IAMPolicy/GetIamPolicy, google.iam.v1.IAMPolicy/SetIamPolicy
func TestTerraformKMSStoredIAM(t *testing.T) {
	h := New(t)
	m := newTFModule(t) // skips without terraform or the CLI
	kc := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	p := h.Project()
	ringID := "tfiam-" + p
	ring := "projects/" + p + "/locations/us-central1/keyRings/" + ringID
	base := fmt.Sprintf(`
resource "google_kms_key_ring" "kr" {
  project  = %[1]q
  name     = %[2]q
  location = "us-central1"
}
resource "google_kms_crypto_key" "k" {
  name     = "k"
  key_ring = google_kms_key_ring.kr.id
}
`, p, ringID)
	read := func(name string) func() map[string][]string {
		return func() map[string][]string {
			t.Helper()
			pol, err := kc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			out := map[string][]string{}
			for _, b := range pol.GetBindings() {
				out[b.GetRole()] = b.GetMembers()
			}
			return out
		}
	}
	targets := []tfIAMTarget{
		{kind: "google_kms_key_ring", label: "kr", ref: map[string]string{"key_ring_id": "google_kms_key_ring.kr.id"}, read: read(ring)},
		{kind: "google_kms_crypto_key", label: "k", ref: map[string]string{"crypto_key_id": "google_kms_crypto_key.k.id"}, read: read(ring + "/cryptoKeys/k")},
	}
	runTFIAMPhases(t, m, base, [2]string{"roles/viewer", "roles/editor"}, targets)
	if _, err := kc.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring}); err != nil {
		t.Errorf("GetKeyRing after destroy: %v; want the ring kept, as on Google", err)
	}
}

// TestTerraformStorageACLLists (#1304): the provider's list-valued ACL
// resources on a fine-grained bucket: google_storage_bucket_acl,
// google_storage_default_object_acl and google_storage_object_acl, each
// with a role_entity list. Google's generated JSON API client reads every
// entry back; the plan is clean; a role change applies in place, and the
// removed entry is gone; destroy removes the bucket.
// covers: storage.bucketAccessControls.list, storage.bucketAccessControls.insert, storage.bucketAccessControls.patch, storage.bucketAccessControls.delete, storage.defaultObjectAccessControls.list, storage.defaultObjectAccessControls.insert, storage.defaultObjectAccessControls.patch, storage.defaultObjectAccessControls.delete, storage.objectAccessControls.list, storage.objectAccessControls.insert, storage.objectAccessControls.patch, storage.objectAccessControls.delete, storage.objects.patch
func TestTerraformStorageACLLists(t *testing.T) {
	h := New(t)
	_ = storageClient(t, h) // skips without CLOUDBURROW_TEST_STORAGE
	s := storageJSON(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	bucket := project + "-tfacls"
	const ada, grace = "user-ada@example.com", "user-grace@example.com"
	module := func(entries ...string) string {
		list := ""
		for _, e := range entries {
			list += fmt.Sprintf("%q, ", e)
		}
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %[1]q
  name                        = %[2]q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = false
}
resource "google_storage_bucket_acl" "b" {
  bucket      = google_storage_bucket.b.name
  role_entity = [%[3]s]
}
resource "google_storage_default_object_acl" "d" {
  bucket      = google_storage_bucket.b.name
  role_entity = [%[3]s]
}
resource "google_storage_bucket_object" "o" {
  bucket  = google_storage_bucket.b.name
  name    = "acl.txt"
  content = "hello"
}
resource "google_storage_object_acl" "o" {
  bucket      = google_storage_bucket.b.name
  object      = google_storage_bucket_object.o.output_name
  role_entity = [%[3]s]
}
`, project, bucket, list)
	}
	// check compares each list's entries for ada and grace with want, an
	// entity's role or "" for none.
	check := func(when string, want map[string]string) {
		t.Helper()
		got := map[string]map[string]string{"bucket": {}, "default object": {}, "object": {}}
		ba, err := s.BucketAccessControls.List(bucket).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, bucketAccessControls.list: %v", when, err)
		}
		for _, a := range ba.Items {
			got["bucket"][a.Entity] = a.Role
		}
		da, err := s.DefaultObjectAccessControls.List(bucket).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, defaultObjectAccessControls.list: %v", when, err)
		}
		for _, a := range da.Items {
			got["default object"][a.Entity] = a.Role
		}
		oa, err := s.ObjectAccessControls.List(bucket, "acl.txt").Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, objectAccessControls.list: %v", when, err)
		}
		for _, a := range oa.Items {
			got["object"][a.Entity] = a.Role
		}
		for list, entries := range got {
			for _, e := range []string{ada, grace} {
				if entries[e] != want[e] {
					t.Errorf("%s, the %s ACL gives %s %q; want %q (all: %v)", when, list, e, entries[e], want[e], entries)
				}
			}
		}
	}

	m.write(module("READER:"+ada, "OWNER:"+grace))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", map[string]string{ada: "READER", grace: "OWNER"})
	m.planClean()

	m.write(module("OWNER:" + ada))
	m.apply()
	check("after the change", map[string]string{ada: "OWNER"})
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := s.Buckets.Get(bucket).Context(ctx).Do(); !isCode(err, http.StatusNotFound) {
		t.Errorf("the bucket after destroy: %v; want 404", err)
	}
}

// TestTerraformBigQueryDatasetIAM (#1304): google_bigquery_dataset_iam_member,
// _iam_binding and _iam_policy, which the provider applies as the dataset's
// access list (datasets.get, then datasets.patch with every entry), so the
// generated client reads them back from datasets.get. The dataset's own
// entries (its creator's OWNER and the project's special groups) are not
// the module's and are left out of the comparison; _iam_policy, being
// authoritative, removes them, as on Google.
// covers: bigquery.datasets.insert, bigquery.datasets.get, bigquery.datasets.patch, bigquery.datasets.delete
func TestTerraformBigQueryDatasetIAM(t *testing.T) {
	h := New(t)
	_, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	svc := bigqueryService(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	ds := "tfdsiam_" + strings.ReplaceAll(h.Project(), "-", "_")
	base := fmt.Sprintf(`
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  location                   = "US"
  delete_contents_on_destroy = true
}
`, project, ds)
	targets := []tfIAMTarget{{kind: "google_bigquery_dataset", label: "d",
		ref: map[string]string{"project": "google_bigquery_dataset.d.project", "dataset_id": "google_bigquery_dataset.d.dataset_id"},
		read: func() map[string][]string {
			t.Helper()
			d, err := svc.Datasets.Get(project, ds).Context(ctx).Do()
			if err != nil {
				t.Fatalf("datasets.get: %v", err)
			}
			out := map[string][]string{}
			for _, a := range d.Access {
				switch {
				case strings.HasSuffix(a.UserByEmail, "@example.com"):
					out[a.Role] = append(out[a.Role], "user:"+a.UserByEmail)
				case strings.HasSuffix(a.GroupByEmail, "@example.com"):
					out[a.Role] = append(out[a.Role], "group:"+a.GroupByEmail)
				}
			}
			return out
		}}}
	runTFIAMPhases(t, m, base, [2]string{"roles/bigquery.dataViewer", "roles/bigquery.dataEditor"}, targets)

	if _, err := svc.Datasets.Get(project, ds).Context(ctx).Do(); !isCode(err, http.StatusNotFound) {
		t.Errorf("the dataset after destroy: %v; want 404", err)
	}
}
