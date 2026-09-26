//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// Terraform's storage resources (#515) against the builtin server, through
// `cloudburrow terraform` with the official hashicorp/google provider. Every
// command but init runs under noGoogleEgress.

type tfModule struct {
	t       *testing.T
	cli     string
	flags   []string
	dir     string
	project string
}

func newTFModule(t *testing.T) *tfModule {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	out, err := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	var st struct{ Project string }
	if _ = json.Unmarshal(out, &st); st.Project == "" {
		t.Fatalf("status --format json gave no project (%v): %s", err, out)
	}
	m := &tfModule{t: t, cli: cli, flags: flags, dir: t.TempDir(), project: st.Project}
	t.Cleanup(func() { _, _ = m.tf("destroy", "-auto-approve", "-input=false", "-no-color") })
	return m
}

func (m *tfModule) write(body string) {
	m.t.Helper()
	src := `terraform {
  required_providers {
    google = { source = "hashicorp/google", version = "~> 8.0" }
  }
}
` + body
	if err := os.WriteFile(filepath.Join(m.dir, "main.tf"), []byte(src), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *tfModule) tf(args ...string) (string, error) {
	cmd := exec.Command(m.cli, append(append(append([]string{"terraform"}, m.flags...), "--"), args...)...)
	cmd.Dir = m.dir
	if args[0] != "init" {
		cmd.Env = noGoogleEgress()
	}
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func (m *tfModule) must(args ...string) string {
	m.t.Helper()
	out, err := m.tf(args...)
	if err != nil {
		m.t.Fatalf("cloudburrow terraform %s: %v\n%s", strings.Join(args, " "), err, lastLines(out, 30))
	}
	return out
}

// planClean fails when a plan after apply would change anything, and names
// what the provider wanted to change.
func (m *tfModule) planClean() {
	m.t.Helper()
	out, err := m.tf("plan", "-detailed-exitcode", "-input=false", "-no-color")
	if err != nil {
		m.t.Errorf("plan after apply is not clean (%v):\n%s", err, lastLines(out, 60))
	}
}

func (m *tfModule) apply() {
	m.t.Helper()
	m.must("apply", "-auto-approve", "-input=false", "-no-color")
}

// TestTerraformStorageResources: a bucket with versioning, labels,
// lifecycle, cors, retention_policy and soft_delete_policy; an object with
// holds and retention; a bucket IAM member; an HMAC key; and a
// notification. Each applies, plans clean, and destroys.
// covers: storage.buckets.insert, storage.buckets.get, storage.buckets.patch, storage.buckets.delete, storage.objects.insert, storage.objects.get, storage.objects.patch, storage.objects.delete, storage.buckets.getIamPolicy, storage.buckets.setIamPolicy, storage.projects.hmacKeys.create, storage.projects.hmacKeys.get, storage.projects.hmacKeys.update, storage.projects.hmacKeys.delete, storage.notifications.insert, storage.notifications.get, storage.notifications.delete
func TestTerraformStorageResources(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	c := storageClient(t, h)
	bucket, held := h.Project()+"-tfb", h.Project()+"-tfo"
	topic := "tf-note-" + h.Project()
	email := "tf-hmac@" + m.project + ".iam.gserviceaccount.com"
	module := func(hold bool) string {
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  name                        = %[1]q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
  labels = { team = "burrow", env = "dev" }
  versioning { enabled = true }
  lifecycle_rule {
    action { type = "Delete" }
    condition {
      age        = 30
      with_state = "ARCHIVED"
    }
  }
  cors {
    origin          = ["https://example.test"]
    method          = ["GET", "HEAD"]
    response_header = ["Content-Type"]
    max_age_seconds = 600
  }
  retention_policy { retention_period = 1 }
  soft_delete_policy { retention_duration_seconds = 604800 }
}
resource "google_storage_bucket" "o" {
  name                    = %[2]q
  location                = "US"
  force_destroy           = true
  enable_object_retention = true
}
resource "google_storage_bucket_object" "held" {
  name           = "held.txt"
  bucket         = google_storage_bucket.o.name
  content        = "held"
  temporary_hold = %[3]t
  event_based_hold = %[3]t
}
resource "google_storage_bucket_iam_member" "m" {
  bucket = google_storage_bucket.b.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:viewer@%[4]s.iam.gserviceaccount.com"
}
resource "google_storage_hmac_key" "k" {
  service_account_email = %[5]q
}
resource "google_pubsub_topic" "t" {
  name = %[6]q
}
resource "google_storage_notification" "n" {
  bucket         = google_storage_bucket.b.name
  payload_format = "JSON_API_V1"
  topic          = google_pubsub_topic.t.id
  event_types    = ["OBJECT_FINALIZE"]
}
`, bucket, held, hold, m.project, email, topic)
	}
	m.write(module(true))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()

	attrs, err := c.Bucket(bucket).Attrs(h.Context())
	if err != nil {
		t.Fatalf("the applied bucket is not in CloudBurrow: %v", err)
	}
	if !attrs.VersioningEnabled || attrs.Labels["team"] != "burrow" || len(attrs.Lifecycle.Rules) != 1 || len(attrs.CORS) != 1 ||
		attrs.RetentionPolicy == nil || attrs.SoftDeletePolicy == nil {
		t.Errorf("the applied bucket reads back as %+v", attrs)
	}
	o, err := c.Bucket(held).Object("held.txt").Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !o.TemporaryHold || !o.EventBasedHold {
		t.Errorf("the object's holds read back as temporary %v, event-based %v", o.TemporaryHold, o.EventBasedHold)
	}
	if err := c.Bucket(held).Object("held.txt").Delete(h.Context()); err == nil {
		t.Error("a held object was deleted")
	}
	pol, err := c.Bucket(bucket).IAM().Policy(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !pol.HasRole("serviceAccount:viewer@"+m.project+".iam.gserviceaccount.com", "roles/storage.objectViewer") {
		t.Errorf("the iam_member is not in the bucket's policy: %v", pol.Roles())
	}
	notes, err := c.Bucket(bucket).Notifications(h.Context())
	if err != nil || len(notes) != 1 {
		t.Errorf("notifications = %v (%v), want one", notes, err)
	}
	keys := 0
	it := c.ListHMACKeys(h.Context(), m.project, storage.ForHMACKeyServiceAccountEmail(email))
	for {
		k, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if k.State == storage.Active {
			keys++
		}
	}
	if keys != 1 {
		t.Errorf("%d active HMAC keys for %s, want 1", keys, email)
	}

	// Released holds let destroy delete the object, as they would on Google.
	m.write(module(false))
	m.apply()
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, b := range []string{bucket, held} {
		if _, err := c.Bucket(b).Attrs(h.Context()); !errors.Is(err, storage.ErrBucketNotExist) {
			t.Errorf("bucket %s survived destroy: %v", b, err)
		}
	}
}

// TestTerraformForceDestroyVersionedBucket: force_destroy on a versioned
// bucket with noncurrent versions deletes every generation, then the
// bucket (resource_storage_bucket.go: delete by generation).
// covers: storage.objects.list, storage.objects.delete, storage.buckets.delete
func TestTerraformForceDestroyVersionedBucket(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	c := storageClient(t, h)
	bucket := h.Project() + "-tfv"
	m.write(fmt.Sprintf(`
resource "google_storage_bucket" "v" {
  name          = %q
  location      = "US"
  force_destroy = true
  versioning { enabled = true }
}
`, bucket))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	for i := 0; i < 3; i++ {
		if err := writeObject(h, c, bucket, "o.txt", fmt.Sprintf("v%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	versions := 0
	it := c.Bucket(bucket).Objects(h.Context(), &storage.Query{Versions: true})
	for _, err := it.Next(); err == nil; _, err = it.Next() {
		versions++
	}
	if versions != 3 {
		t.Fatalf("%d versions before destroy, want 3", versions)
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.Bucket(bucket).Attrs(h.Context()); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the versioned bucket survived force_destroy: %v", err)
	}
}
