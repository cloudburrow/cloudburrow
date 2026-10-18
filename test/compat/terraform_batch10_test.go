//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// #1304's tenth batch of google provider resources and data sources, each
// through `cloudburrow terraform` with hashicorp/google and egress blocked,
// read back through an official SDK, planned clean, changed in place where
// the resource has an updatable field, planned clean again and destroyed.
// Each test asks for the endpoints of the services it uses first, so it
// skips in a shard without them.

// TestTerraformKMSProjectAutokeyConfig (#1304): a
// google_kms_project_autokey_config on a project of the test's own sets
// key_project_resolution_mode RESOURCE_PROJECT; the official
// AutokeyAdminClient reads it back; the plan is clean; DISABLED applies in
// place; the plan is clean again; destroy (deletion_policy DELETE) leaves
// the project's config with no mode.
// covers: google.cloud.kms.v1.AutokeyAdmin/GetAutokeyConfig, google.cloud.kms.v1.AutokeyAdmin/UpdateAutokeyConfig
func TestTerraformKMSProjectAutokeyConfig(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvKMS)
	shardEndpoint(h, envLoggingResourceManager, EnvResourceManager)
	h.Endpoint(EnvKMS) // skips without CLOUDBURROW_TEST_KMS
	ac := kmsAdminClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	id := iamProject(t, h, "tfpak-")
	cfgName := "projects/" + id + "/autokeyConfig"
	module := func(mode string) string {
		return fmt.Sprintf(`
resource "google_kms_project_autokey_config" "c" {
  project                     = %q
  key_project_resolution_mode = %q
}
`, id, mode)
	}
	check := func(when string, want kmspb.AutokeyConfig_KeyProjectResolutionMode) {
		t.Helper()
		c, err := ac.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: cfgName})
		if err != nil {
			t.Fatalf("%s: GetAutokeyConfig(%s): %v", when, cfgName, err)
		}
		if c.GetKeyProjectResolutionMode() != want || c.GetKeyProject() != "" {
			t.Errorf("%s: the project's AutokeyConfig = %v; want mode %v and no key project", when, c, want)
		}
	}
	m.write(module("RESOURCE_PROJECT"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", kmspb.AutokeyConfig_RESOURCE_PROJECT)
	m.planClean()

	m.write(module("DISABLED"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", kmspb.AutokeyConfig_DISABLED)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	check("after destroy", kmspb.AutokeyConfig_KEY_PROJECT_RESOLUTION_MODE_UNSPECIFIED)
}

// TestTerraformOrganizationIAMAuditConfig (#1304): a
// google_organization_iam_audit_config on the local organization, for one
// service no other test configures, applies; the official v3
// OrganizationsClient reads the organization's audit configs back; the
// plan is clean; new exempted members apply in place; the plan is clean
// again; destroy removes that service's audit config and only it. Stored,
// never enforced: nothing writes audit logs from it (ADR-0006).
// covers: google.cloud.resourcemanager.v3.Organizations/GetIamPolicy, google.cloud.resourcemanager.v3.Organizations/SetIamPolicy
func TestTerraformOrganizationIAMAuditConfig(t *testing.T) {
	h := New(t)
	_, oc, _ := foldersClients(t, h) // asks for the Resource Manager endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	const service = "cloudkms.googleapis.com"
	org := strings.TrimPrefix(localOrg, "organizations/")
	module := func(exempt string) string {
		return fmt.Sprintf(`
resource "google_organization_iam_audit_config" "a" {
  org_id  = %q
  service = %q
  audit_log_config {
    log_type         = "DATA_WRITE"
    exempted_members = [%q]
  }
  audit_log_config {
    log_type = "ADMIN_READ"
  }
}
`, org, service, exempt)
	}
	check := func(when string, want []string) {
		t.Helper()
		p, err := oc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: localOrg})
		if err != nil {
			t.Fatalf("%s: GetIamPolicy on %s: %v", when, localOrg, err)
		}
		var got []string
		for _, a := range auditConfigsOf(p) {
			if strings.HasPrefix(a, service+"/") {
				got = append(got, a)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: the organization's %s audit configs = %v; want %v", when, service, got, want)
		}
	}
	m.write(module(tfIAMMember))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", []string{service + "/ADMIN_READ/", service + "/DATA_WRITE/" + tfIAMMember})
	m.planClean()

	m.write(module(tfIAMMember2))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", []string{service + "/ADMIN_READ/", service + "/DATA_WRITE/" + tfIAMMember2})
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	check("after destroy", nil)
}

// TestTerraformDataSourcesBatch10 (#1304): the data sources that look up
// and list what a module made, each output compared with what the official
// clients read: google_storage_project_service_account, google_storage_bucket,
// google_storage_bucket_object and google_storage_bucket_objects; google_kms_key_ring, google_kms_key_rings,
// google_kms_crypto_keys, google_kms_crypto_key_latest_version and
// google_kms_crypto_key_versions; google_secret_manager_secret_version_access
// and google_secret_manager_secrets; google_pubsub_topic and
// google_pubsub_subscription. A data source has no update or destroy of its
// own: the change adds an object and a key version and replaces a secret's
// payload, all read on the next apply, and the plan stays clean after each.
// google_storage_buckets is not here: the provider lists buckets at
// storage.googleapis.com whatever storage_custom_endpoint says, so the
// wrapper refuses it. The key ring's name has a suffix of the run's own,
// since a key ring cannot be deleted.
// covers: storage.projects.serviceAccount.get, storage.buckets.get, storage.objects.get, storage.objects.list, google.cloud.kms.v1.KeyManagementService/GetKeyRing, google.cloud.kms.v1.KeyManagementService/ListKeyRings, google.cloud.kms.v1.KeyManagementService/ListCryptoKeys, google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/ListSecrets, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Subscriber/GetSubscription
func TestTerraformDataSourcesBatch10(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_KMS", EnvKMS)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	h.Endpoint(EnvKMS)
	h.Endpoint(EnvSecrets)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvPubSub)
	sm := secretsClient(t, h)
	sc := storageClient(t, h)
	kc := kmsClients(t, h)["grpc"]
	ps := pubsubClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	p := h.Project()
	ring := fmt.Sprintf("tfds10-%s-%d", strings.TrimPrefix(p, "cb-test-"), time.Now().Unix()%100000)
	bucket := p + "-tfds10"
	module := func(payload string, second bool) string {
		extra := ""
		if second {
			extra = `
resource "google_storage_bucket_object" "o2" {
  bucket  = google_storage_bucket.b.name
  name    = "logs/b.txt"
  content = "b"
}
resource "google_kms_crypto_key_version" "v2" {
  crypto_key = google_kms_crypto_key.k.id
}
`
		}
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %[1]q
  name                        = %[3]q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
  labels                      = { team = "burrow" }
}
resource "google_storage_bucket_object" "o1" {
  bucket       = google_storage_bucket.b.name
  name         = "logs/a.txt"
  content      = "a"
  content_type = "text/plain"
}
resource "google_storage_bucket_object" "other" {
  bucket  = google_storage_bucket.b.name
  name    = "other.txt"
  content = "other"
}
resource "google_kms_key_ring" "kr" {
  project  = %[1]q
  name     = %[2]q
  location = "us-central1"
}
resource "google_kms_crypto_key" "k" {
  name     = "k"
  key_ring = google_kms_key_ring.kr.id
}
resource "google_kms_crypto_key" "k2" {
  name     = "k2"
  key_ring = google_kms_key_ring.kr.id
}
resource "google_secret_manager_secret" "s" {
  project   = %[1]q
  secret_id = "tf-ds10"
  labels    = { batch = "ten" }
  replication {
    auto {}
  }
}
resource "google_secret_manager_secret_version" "v" {
  secret      = google_secret_manager_secret.s.id
  secret_data = %[4]q
}
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-ds10"
  labels  = { team = "burrow" }
}
resource "google_pubsub_subscription" "s" {
  project              = %[1]q
  name                 = "tf-ds10"
  topic                = google_pubsub_topic.t.id
  ack_deadline_seconds = 42
}
%[5]s
data "google_storage_project_service_account" "d" {
  project = %[1]q
}
data "google_storage_bucket" "d" {
  name       = google_storage_bucket.b.name
}
data "google_storage_bucket_object" "d" {
  bucket = google_storage_bucket.b.name
  name   = google_storage_bucket_object.o1.name
}
data "google_storage_bucket_objects" "d" {
  bucket     = google_storage_bucket.b.name
  prefix     = "logs/"
  depends_on = [google_storage_bucket_object.o1, google_storage_bucket_object.other%[6]s]
}
data "google_kms_key_ring" "d" {
  project  = %[1]q
  name     = google_kms_key_ring.kr.name
  location = "us-central1"
}
data "google_kms_key_rings" "d" {
  project  = %[1]q
  location = "us-central1"
  filter   = "name:${google_kms_key_ring.kr.name}"
}
data "google_kms_crypto_keys" "d" {
  key_ring   = google_kms_key_ring.kr.id
  depends_on = [google_kms_crypto_key.k, google_kms_crypto_key.k2]
}
data "google_kms_crypto_key_latest_version" "d" {
  crypto_key = google_kms_crypto_key.k.id
  depends_on = [google_kms_crypto_key.k%[7]s]
}
data "google_kms_crypto_key_versions" "d" {
  crypto_key = google_kms_crypto_key.k.id
  depends_on = [google_kms_crypto_key.k%[7]s]
}
data "google_secret_manager_secret_version_access" "d" {
  project = %[1]q
  secret  = google_secret_manager_secret.s.secret_id
  version = google_secret_manager_secret_version.v.version
}
data "google_secret_manager_secrets" "d" {
  project    = %[1]q
  filter     = "labels.batch=ten"
  depends_on = [google_secret_manager_secret.s]
}
data "google_pubsub_topic" "d" {
  project = %[1]q
  name    = google_pubsub_topic.t.name
}
data "google_pubsub_subscription" "d" {
  project = %[1]q
  name    = google_pubsub_subscription.s.name
}
output "outputs" {
  sensitive = true
  value = {
    gcs_service_account = data.google_storage_project_service_account.d.email_address
    bucket_labels       = data.google_storage_bucket.d.effective_labels
    object_type         = data.google_storage_bucket_object.d.content_type
    object_md5          = data.google_storage_bucket_object.d.md5hash
    objects             = [for o in data.google_storage_bucket_objects.d.bucket_objects : o.name]
    key_ring            = data.google_kms_key_ring.d.id
    key_rings           = [for r in data.google_kms_key_rings.d.key_rings : r.id]
    keys                = [for k in data.google_kms_crypto_keys.d.keys : k.name]
    latest_version      = data.google_kms_crypto_key_latest_version.d.name
    versions            = [for v in data.google_kms_crypto_key_versions.d.versions : v.name]
    secret_data         = data.google_secret_manager_secret_version_access.d.secret_data
    secret_version      = data.google_secret_manager_secret_version_access.d.name
    secrets             = [for s in data.google_secret_manager_secrets.d.secrets : s.secret_id]
    topic_labels        = data.google_pubsub_topic.d.labels
    sub_ack_deadline    = data.google_pubsub_subscription.d.ack_deadline_seconds
  }
}
`, p, ring, bucket, payload, extra, map[bool]string{true: ", google_storage_bucket_object.o2"}[second],
			map[bool]string{true: ", google_kms_crypto_key_version.v2"}[second])
	}
	keyName := "projects/" + p + "/locations/us-central1/keyRings/" + ring + "/cryptoKeys/k"
	check := func(when, payload string, second bool) {
		t.Helper()
		var got struct {
			GCSServiceAccount string            `json:"gcs_service_account"`
			BucketLabels      map[string]string `json:"bucket_labels"`
			ObjectType        string            `json:"object_type"`
			ObjectMD5         string            `json:"object_md5"`
			Objects           []string          `json:"objects"`
			KeyRing           string            `json:"key_ring"`
			KeyRings          []string          `json:"key_rings"`
			Keys              []string          `json:"keys"`
			LatestVersion     string            `json:"latest_version"`
			Versions          []string          `json:"versions"`
			SecretData        string            `json:"secret_data"`
			SecretVersion     string            `json:"secret_version"`
			Secrets           []string          `json:"secrets"`
			TopicLabels       map[string]string `json:"topic_labels"`
			SubAckDeadline    int               `json:"sub_ack_deadline"`
		}
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		sa, err := sc.ServiceAccount(ctx, p)
		if err != nil || got.GCSServiceAccount != sa {
			t.Errorf("%s, google_storage_project_service_account = %q; the official client reads %q, %v", when, got.GCSServiceAccount, sa, err)
		}
		if got.BucketLabels["team"] != "burrow" {
			t.Errorf("%s, google_storage_bucket's labels = %v; want team burrow", when, got.BucketLabels)
		}
		attrs, err := sc.Bucket(bucket).Object("logs/a.txt").Attrs(ctx)
		if err != nil {
			t.Fatalf("%s, Attrs(logs/a.txt): %v", when, err)
		}
		if got.ObjectType != "text/plain" || got.ObjectMD5 == "" {
			t.Errorf("%s, google_storage_bucket_object = %q, %q; the official client reads %q", when, got.ObjectType, got.ObjectMD5, attrs.ContentType)
		}
		wantObjects := []string{"logs/a.txt"}
		if second {
			wantObjects = append(wantObjects, "logs/b.txt")
		}
		if !slices.Equal(got.Objects, wantObjects) {
			t.Errorf("%s, google_storage_bucket_objects = %v; want %v", when, got.Objects, wantObjects)
		}
		ringName := "projects/" + p + "/locations/us-central1/keyRings/" + ring
		if got.KeyRing != ringName || !slices.Equal(got.KeyRings, []string{ringName}) {
			t.Errorf("%s, google_kms_key_ring = %q, google_kms_key_rings = %v; want %s", when, got.KeyRing, got.KeyRings, ringName)
		}
		slices.Sort(got.Keys)
		if !slices.Equal(got.Keys, []string{"k", "k2"}) && !slices.Equal(got.Keys, []string{keyName, keyName + "2"}) {
			t.Errorf("%s, google_kms_crypto_keys = %v; want k and k2", when, got.Keys)
		}
		var versions []string
		it := kc.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: keyName})
		for {
			v, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("%s, ListCryptoKeyVersions: %v", when, err)
			}
			versions = append(versions, v.GetName())
		}
		slices.Sort(versions)
		slices.Sort(got.Versions)
		wantN := 1
		if second {
			wantN = 2
		}
		if len(versions) != wantN || !slices.Equal(got.Versions, versions) {
			t.Errorf("%s, google_kms_crypto_key_versions = %v; the official client lists %v (want %d)", when, got.Versions, versions, wantN)
		}
		if len(versions) > 0 && got.LatestVersion != versions[len(versions)-1] {
			t.Errorf("%s, google_kms_crypto_key_latest_version = %q; want %q", when, got.LatestVersion, versions[len(versions)-1])
		}
		r, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: got.SecretVersion})
		if err != nil || string(r.GetPayload().GetData()) != payload || got.SecretData != payload {
			t.Errorf("%s, google_secret_manager_secret_version_access %s = %q; the official client reads %q, %v; want %q",
				when, got.SecretVersion, got.SecretData, r.GetPayload().GetData(), err, payload)
		}
		if !slices.Equal(got.Secrets, []string{"tf-ds10"}) {
			t.Errorf("%s, google_secret_manager_secrets = %v; want [tf-ds10]", when, got.Secrets)
		}
		topic, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/" + p + "/topics/tf-ds10"})
		if err != nil || got.TopicLabels["team"] != "burrow" || topic.GetLabels()["team"] != "burrow" {
			t.Errorf("%s, google_pubsub_topic's labels = %v; the official client reads %v, %v", when, got.TopicLabels, topic.GetLabels(), err)
		}
		sub, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: "projects/" + p + "/subscriptions/tf-ds10"})
		if err != nil || got.SubAckDeadline != 42 || sub.GetAckDeadlineSeconds() != 42 {
			t.Errorf("%s, google_pubsub_subscription's ack deadline = %d; the official client reads %d, %v", when, got.SubAckDeadline, sub.GetAckDeadlineSeconds(), err)
		}
	}

	m.write(module("first payload", false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first payload", false)
	m.planClean()

	m.write(module("second payload", true))
	m.apply()
	check("after the change", "second payload", true)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sc.Bucket(bucket).Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket after destroy: %v; want ErrBucketNotExist", err)
	}
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/" + p + "/secrets/tf-ds10"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
	if _, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/" + p + "/topics/tf-ds10"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic after destroy: %v; want NotFound", err)
	}
}

// TestTerraformResourceManagerDataSources (#1304): the Resource Manager
// data sources a module reads its hierarchy with, over a google_folder, a
// google_folder in it and a google_project in the first: google_project
// (number, folder, labels), google_folder (with lookup_organization),
// google_project_ancestry and google_organization, each output compared
// with what the official v3 clients read. The change relabels the project
// and renames the child folder in place; the data sources read the change
// on that apply, and the plan is clean after each. google_projects and
// google_folders are not here: the provider lists them at
// cloudresourcemanager.googleapis.com whatever its custom endpoints say,
// so the wrapper refuses them.
// covers: google.cloud.resourcemanager.v3.Projects/GetProject, google.cloud.resourcemanager.v3.Folders/GetFolder, google.cloud.resourcemanager.v3.Folders/ListFolders, google.cloud.resourcemanager.v3.Organizations/GetOrganization, cloudresourcemanager.projects.get, cloudresourcemanager.projects.getAncestry
func TestTerraformResourceManagerDataSources(t *testing.T) {
	h := New(t)
	fc, oc, pc := foldersClients(t, h) // asks for the Resource Manager endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	folder := folderLabel(h, "tfrmds-")
	project := v1ID(h, "tfrmds-")
	tag := strings.TrimPrefix(project, "tfrmds-")
	module := func(label, child string) string {
		return fmt.Sprintf(`
resource "google_folder" "f" {
  display_name        = %[1]q
  parent              = %[2]q
  deletion_protection = false
}
resource "google_folder" "child" {
  display_name        = %[3]q
  parent              = google_folder.f.name
  deletion_protection = false
}
resource "google_project" "p" {
  project_id      = %[4]q
  name            = "RM data sources"
  folder_id       = google_folder.f.folder_id
  deletion_policy = "DELETE"
  labels          = { run = %[5]q, stage = %[6]q }
}
data "google_project" "d" {
  project_id = google_project.p.project_id
}
data "google_folder" "d" {
  folder              = google_folder.f.name
  lookup_organization = true
}
data "google_project_ancestry" "d" {
  project = google_project.p.project_id
}
data "google_organization" "d" {
  organization = %[2]q
}
output "outputs" {
  value = {
    number       = data.google_project.d.number
    folder_id    = data.google_project.d.folder_id
    labels       = data.google_project.d.effective_labels
    folder_name  = data.google_folder.d.display_name
    folder_org   = data.google_folder.d.organization
    ancestors    = [for a in data.google_project_ancestry.d.ancestors : "${a.type}/${a.id}"]
    org_id       = data.google_organization.d.org_id
    org_name     = data.google_organization.d.name
  }
}
`, folder, localOrg, child, project, tag, label)
	}
	check := func(when, label, child string) {
		t.Helper()
		var got struct {
			Number     string            `json:"number"`
			FolderID   string            `json:"folder_id"`
			Labels     map[string]string `json:"labels"`
			FolderName string            `json:"folder_name"`
			FolderOrg  string            `json:"folder_org"`
			Ancestors  []string          `json:"ancestors"`
			OrgID      string            `json:"org_id"`
			OrgName    string            `json:"org_name"`
		}
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		p, err := pc.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project})
		if err != nil {
			t.Fatalf("%s: GetProject(%s): %v", when, project, err)
		}
		folderID := strings.TrimPrefix(p.GetParent(), "folders/")
		if got.Number != strings.TrimPrefix(p.GetName(), "projects/") || got.FolderID != folderID {
			t.Errorf("%s, google_project number %q, folder %q; the official client reads %s in %s", when, got.Number, got.FolderID, p.GetName(), p.GetParent())
		}
		if got.Labels["stage"] != label || p.GetLabels()["stage"] != label {
			t.Errorf("%s, google_project's labels = %v, the official client's %v; want stage %s", when, got.Labels, p.GetLabels(), label)
		}
		f, err := fc.GetFolder(ctx, &rmpb.GetFolderRequest{Name: "folders/" + folderID})
		if err != nil || got.FolderName != folder || f.GetDisplayName() != folder || got.FolderOrg != localOrg {
			t.Errorf("%s, google_folder = %q in %q; the official client reads %v, %v", when, got.FolderName, got.FolderOrg, f, err)
		}
		children, err := fc.ListFolders(ctx, &rmpb.ListFoldersRequest{Parent: "folders/" + folderID}).Next()
		if err != nil || children.GetDisplayName() != child {
			t.Errorf("%s, the folder's child = %v, %v; want %s", when, children, err, child)
		}
		org := strings.TrimPrefix(localOrg, "organizations/")
		want := []string{"project/" + project, "folder/" + folderID, "organization/" + org}
		if !slices.Equal(got.Ancestors, want) {
			t.Errorf("%s, google_project_ancestry = %v; want %v", when, got.Ancestors, want)
		}
		o, err := oc.GetOrganization(ctx, &rmpb.GetOrganizationRequest{Name: localOrg})
		if err != nil || got.OrgID != org || got.OrgName != o.GetName() {
			t.Errorf("%s, google_organization = %q, %q; the official client reads %v, %v", when, got.OrgID, got.OrgName, o, err)
		}
	}
	m.write(module("one", "child-one"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "one", "child-one")
	m.planClean()

	m.write(module("two", "child-two"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "two", "child-two")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if p, err := pc.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project}); err == nil && p.GetState() != rmpb.Project_DELETE_REQUESTED {
		t.Errorf("the project after destroy = %v; want it DELETE_REQUESTED or gone", p)
	}
}
