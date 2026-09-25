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

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// EnvCLI is the cloudburrow binary, and EnvCLIArgs the flags naming the
	// running instance, so the test drives the same instance the rest of
	// the suite does.
	EnvCLI     = "CLOUDBURROW_TEST_CLI"
	EnvCLIArgs = "CLOUDBURROW_TEST_CLI_ARGS"
)

// TestTerraformAppliesAndDestroysThroughTheWrapper.
//
// Terraform was marked Verified on a transcript alone (#283). This applies a
// module with the official hashicorp/google provider through `cloudburrow
// terraform`, checks the bucket and topic exist through the official SDKs,
// destroys them, and checks they are gone.
func TestTerraformAppliesAndDestroysThroughTheWrapper(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))

	// The provider uses the instance's project, which status reports.
	out, err := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	var st struct{ Project string }
	if _ = json.Unmarshal(out, &st); st.Project == "" {
		t.Fatalf("status --format json gave no project (%v): %s", err, out)
	}

	bucket := h.Project() + "-tf"
	topic := "tf-" + h.Project()
	secretID := "tf-" + h.Project()
	queueID := "tf-" + h.Project()
	member := "serviceAccount:tf@" + st.Project + ".iam.gserviceaccount.com"
	dir := t.TempDir()
	module := fmt.Sprintf(`terraform {
  required_providers {
    google = { source = "hashicorp/google", version = "~> 8.0" }
  }
}
resource "google_storage_bucket" "b" {
  name          = %q
  location      = "US"
  force_destroy = true
}
resource "google_pubsub_topic" "t" {
  name = %q
}
resource "google_secret_manager_secret" "s" {
  secret_id = %q
  replication {
    auto {}
  }
}
# Stored, never enforced (#365, ADR-0006).
resource "google_secret_manager_secret_iam_member" "m" {
  secret_id = google_secret_manager_secret.s.id
  role      = "roles/secretmanager.secretAccessor"
  member    = %q
}
resource "google_cloud_tasks_queue" "q" {
  name     = %q
  location = "us-central1"
}
# Stored, never enforced (#366, ADR-0006).
resource "google_cloud_tasks_queue_iam_member" "qm" {
  name     = google_cloud_tasks_queue.q.id
  location = "us-central1"
  role     = "roles/cloudtasks.enqueuer"
  member   = %q
}
`, bucket, topic, secretID, member, queueID, member)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), args...)...)
		cmd.Dir = dir
		if args[0] != "init" {
			cmd.Env = noGoogleEgress()
		}
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow terraform %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		t.Logf("terraform %s:\n%s", args[0], lastLines(string(b), 6))
		if left, _ := filepath.Glob(filepath.Join(dir, "cloudburrow_providers*")); len(left) != 0 {
			t.Fatalf("the wrapper left %v behind after %s", left, args[0])
		}
	}
	run("init", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color")
	t.Cleanup(func() {
		cmd := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), "destroy", "-auto-approve", "-no-color")...)
		cmd.Dir = dir
		_ = cmd.Run()
	})

	sc := storageClient(t, h)
	if _, err := sc.Bucket(bucket).Attrs(h.Context()); err != nil {
		t.Fatalf("the applied bucket is not in CloudBurrow: %v", err)
	}
	pc := pubsubClient(t, h)
	topicName := "projects/" + st.Project + "/topics/" + topic
	if _, err := pc.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: topicName}); err != nil {
		t.Fatalf("the applied topic is not in CloudBurrow: %v", err)
	}

	smc := secretsClient(t, h)
	secretName := "projects/" + st.Project + "/secrets/" + secretID
	pol, err := smc.GetIamPolicy(h.Context(), &iampb.GetIamPolicyRequest{Resource: secretName})
	if err != nil {
		t.Fatalf("GetIamPolicy on the applied secret: %v", err)
	}
	if b := pol.GetBindings(); len(b) != 1 || b[0].GetRole() != "roles/secretmanager.secretAccessor" || strings.Join(b[0].GetMembers(), ",") != member {
		t.Errorf("the applied iam_member reads back as %v", b)
	}
	// A second plan finds nothing to change in the Secret Manager resources:
	// the provider reads back what it set. Scoped to them because the bucket
	// does drift: fake-gcs-server does not return fields such as versioning
	// and soft_delete_policy, so the provider plans a replacement (#374).
	plan := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), "plan", "-detailed-exitcode", "-input=false", "-no-color",
		"-target=google_secret_manager_secret.s", "-target=google_secret_manager_secret_iam_member.m",
		"-target=google_cloud_tasks_queue.q", "-target=google_cloud_tasks_queue_iam_member.qm")...)
	plan.Dir, plan.Env = dir, noGoogleEgress()
	if b, err := plan.CombinedOutput(); err != nil {
		t.Errorf("plan after apply is not clean (%v):\n%s", err, lastLines(string(b), 20))
	}

	queueName := "projects/" + st.Project + "/locations/us-central1/queues/" + queueID
	qpol, err := tasksClient(t, h).GetIamPolicy(h.Context(), &iampb.GetIamPolicyRequest{Resource: queueName})
	if err != nil {
		t.Fatalf("GetIamPolicy on the applied queue: %v", err)
	}
	if b := qpol.GetBindings(); len(b) != 1 || b[0].GetRole() != "roles/cloudtasks.enqueuer" || strings.Join(b[0].GetMembers(), ",") != member {
		t.Errorf("the applied queue iam_member reads back as %v", b)
	}

	run("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := tasksClient(t, h).GetQueue(h.Context(), &taskspb.GetQueueRequest{Name: queueName}); status.Code(err) != codes.NotFound {
		t.Errorf("the queue survived destroy: %v", err)
	}
	if _, err := smc.GetSecret(h.Context(), &secretmanagerpb.GetSecretRequest{Name: secretName}); status.Code(err) != codes.NotFound {
		t.Errorf("the secret survived destroy: %v", err)
	}
	if _, err := sc.Bucket(bucket).Attrs(h.Context()); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket survived destroy: %v", err)
	}
	if _, err := pc.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: topicName}); status.Code(err) != codes.NotFound {
		t.Errorf("the topic survived destroy: %v", err)
	}
}

// noGoogleEgress is the environment for a Terraform apply or destroy: every
// HTTPS request goes to a closed port. CloudBurrow's endpoints are plain HTTP
// on loopback and unaffected, so the only requests this stops are ones to a
// real Google endpoint the wrapper did not override — which then fail the
// test instead of leaving the machine. It exists because one did (#301: a
// misnamed billing endpoint attribute sent the provider to
// cloudbilling.googleapis.com).
func noGoogleEgress() []string {
	return append(os.Environ(), "HTTPS_PROXY=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1",
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
