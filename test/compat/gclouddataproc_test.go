//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGcloudDataproc (#2162): gcloud dataproc, configured by `cloudburrow
// gcloud-setup` alone (its dataproc override), drives the Dataproc API's
// JSON surface: clusters create (single node, image 2.2) waits on its
// operation, list and describe; jobs submit pyspark runs a PySpark file
// from Cloud Storage on the cluster and streams its driver output from
// Cloud Storage; jobs list; workflow-templates create, list and delete;
// clusters delete.
func TestGcloudDataproc(t *testing.T) {
	h, _, project := dataprocEnv(t)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	gdir := t.TempDir()
	conf := gcloudSetup(t, cli, gdir)
	if b, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+conf)); !strings.Contains(string(b), "dataproc = http://") {
		t.Fatalf("the configuration has no dataproc override:\n%s", b)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command(gcloud, append(args, "--quiet", "--project", project)...)
		// The configuration refuses every API this instance does not serve
		// (gcloud-setup), and the proxy refuses anything else bound off the
		// loopback.
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+conf)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	const region = "us-central1"
	name := "gc-" + dataprocSuffix(h, 30)

	gc("dataproc", "clusters", "create", name, "--region", region, "--single-node", "--image-version", "2.2", "--labels", "suite=compat")
	t.Cleanup(func() { _, _ = run("dataproc", "clusters", "delete", name, "--region", region) })
	var desc struct {
		ClusterName string
		Status      struct{ State string }
		Config      struct {
			SoftwareConfig struct{ ImageVersion string }
		}
	}
	if err := json.Unmarshal([]byte(gc("dataproc", "clusters", "describe", name, "--region", region, "--format", "json")), &desc); err != nil ||
		desc.ClusterName != name || desc.Status.State != "RUNNING" || !strings.HasPrefix(desc.Config.SoftwareConfig.ImageVersion, "2.2") {
		t.Errorf("clusters describe: %+v %v", desc, err)
	}
	if out := gc("dataproc", "clusters", "list", "--region", region, "--filter", "labels.suite=compat", "--format", "value(clusterName)"); !strings.Contains(out, name) {
		t.Errorf("clusters list: %q", out)
	}

	pi := dataprocPut(t, h.Context(), bh, "pi.py", dataprocPi)
	out := gc("dataproc", "jobs", "submit", "pyspark", pi, "--cluster", name, "--region", region, "--", "5", "gcloud")
	if !strings.Contains(out, "Pi is roughly 3.") || !strings.Contains(out, "args 5 gcloud") {
		t.Errorf("jobs submit pyspark streamed: %s", out)
	}
	if out := gc("dataproc", "jobs", "list", "--region", region, "--cluster", name, "--format", "value(status.state)"); !strings.Contains(out, "DONE") {
		t.Errorf("jobs list: %q", out)
	}

	tmpl := "gwf-" + dataprocSuffix(h, 30)
	gc("dataproc", "workflow-templates", "create", tmpl, "--region", region)
	if out := gc("dataproc", "workflow-templates", "list", "--region", region, "--format", "value(id)"); !strings.Contains(out, tmpl) {
		t.Errorf("workflow-templates list: %q", out)
	}
	gc("dataproc", "workflow-templates", "delete", tmpl, "--region", region)

	gc("dataproc", "clusters", "delete", name, "--region", region)
	if out, err := run("dataproc", "clusters", "describe", name, "--region", region); err == nil || !strings.Contains(out, "NOT_FOUND") && !strings.Contains(out, "Not found") {
		t.Errorf("describe after delete: %v %s", err, out)
	}
}
