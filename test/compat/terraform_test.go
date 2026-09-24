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

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
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
`, bucket, topic)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), args...)...)
		cmd.Dir = dir
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

	run("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sc.Bucket(bucket).Attrs(h.Context()); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket survived destroy: %v", err)
	}
	if _, err := pc.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: topicName}); status.Code(err) != codes.NotFound {
		t.Errorf("the topic survived destroy: %v", err)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
