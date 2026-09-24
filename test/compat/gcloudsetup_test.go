//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// TestGcloudSetupConfiguration (#305): after `eval "$(cloudburrow
// gcloud-setup)"`, real gcloud reaches CloudBurrow for storage and pubsub;
// the user's default configuration is unchanged; teardown removes the
// configuration and is harmless to repeat. Skipped without gcloud.
func TestGcloudSetupConfiguration(t *testing.T) {
	h := New(t)
	gcloud, err := exec.LookPath("gcloud")
	if err != nil {
		t.Skip("gcloud is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	var st struct{ Project string }
	if err := json.Unmarshal(out, &st); err != nil || st.Project == "" {
		t.Fatalf("status gave no project: %s", out)
	}

	// An isolated gcloud directory with a user's own default configuration.
	gdir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(gdir, "configurations"), 0o755)
	defaultConf := "[core]\nproject = someone-elses-real-project\n"
	_ = os.WriteFile(filepath.Join(gdir, "configurations", "config_default"), []byte(defaultConf), 0o644)
	_ = os.WriteFile(filepath.Join(gdir, "active_config"), []byte("default"), 0o644)

	cb := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(cli, append(args, flags...)...)
		cmd.Env = append(os.Environ(), "CLOUDSDK_CONFIG="+gdir)
		b, err := cmd.Output()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v", args[0], err)
		}
		return strings.TrimSpace(string(b))
	}
	export := cb("gcloud-setup")
	name, ok := strings.CutPrefix(export, "export CLOUDSDK_ACTIVE_CONFIG_NAME=")
	if !ok {
		t.Fatalf("gcloud-setup printed %q", export)
	}

	bucket := h.Project() + "-gcfg"
	if err := storageClient(t, h).Bucket(bucket).Create(h.Context(), st.Project, nil); err != nil {
		t.Fatal(err)
	}
	ps := pubsubClient(t, h)
	topicName := fmt.Sprintf("projects/%s/topics/gcfg-%s", st.Project, h.Project())
	if _, err := ps.TopicAdminClient.CreateTopic(h.Context(), &pubsubpb.Topic{Name: topicName}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(h.Context(), &pubsubpb.DeleteTopicRequest{Topic: topicName})
	})

	gc := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, args...)
		// Only the configuration name: everything else comes from the
		// configuration gcloud-setup wrote.
		cmd.Env = append(os.Environ(), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	if got := gc("storage", "ls"); !strings.Contains(got, "gs://"+bucket+"/") {
		t.Errorf("gcloud storage ls does not show gs://%s/:\n%s", bucket, got)
	}
	if got := gc("pubsub", "topics", "list", "--format=value(name)"); !strings.Contains(got, topicName) {
		t.Errorf("gcloud pubsub topics list does not show %s:\n%s", topicName, got)
	}

	if b, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_default")); string(b) != defaultConf {
		t.Errorf("the default configuration changed: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(gdir, "active_config")); string(b) != "default" {
		t.Errorf("active_config changed: %q", b)
	}
	for i := 0; i < 2; i++ {
		if got := cb("gcloud-teardown"); got != "unset CLOUDSDK_ACTIVE_CONFIG_NAME" {
			t.Errorf("teardown %d printed %q", i+1, got)
		}
	}
	if _, err := os.Stat(filepath.Join(gdir, "configurations", "config_"+name)); !os.IsNotExist(err) {
		t.Errorf("the configuration survived teardown: %v", err)
	}
}
