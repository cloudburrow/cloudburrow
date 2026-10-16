//go:build compat

package compat

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

var defaultLogsBucketRE = regexp.MustCompile(`^gs://[0-9]+\.cloudbuild-logs\.googleusercontent\.com$`)

// defaultLogsClient is the official Cloud Storage client on the instance's
// storage, as buildLogText's.
func defaultLogsClient(ctx context.Context, t *testing.T, h *Harness) *storage.Client {
	t.Helper()
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(),
		option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, storage.objects.get, storage.objects.list
//
// TestCloudBuildDefaultLogsBucket (#1430): a build with no logsBucket
// reports Cloud Build's default, gs://{project number}.cloudbuild-logs.
// googleusercontent.com, and its log is there: the official Cloud Storage
// client reads log-{id}.txt and lists it. The bucket is Google's, so an
// upload to it through the API is 403, and it is not in the project's
// bucket list.
func TestCloudBuildDefaultLogsBucket(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	op, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: h.Project(), Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: stepImage(), Entrypoint: "sh", Args: []string{"-c", "echo default-logs-bucket"}}}}})
	skipWithoutHostSteps(t, err)
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	b, err := op.Wait(ctx)
	if err != nil || b.GetStatus() != cbpb.Build_SUCCESS {
		t.Fatalf("the build = %v, %v", b, err)
	}
	if !defaultLogsBucketRE.MatchString(b.GetLogsBucket()) {
		t.Fatalf("logsBucket = %q; want gs://NUMBER.cloudbuild-logs.googleusercontent.com", b.GetLogsBucket())
	}
	bucket := strings.TrimPrefix(b.GetLogsBucket(), "gs://")
	logText := buildLogText(ctx, t, h, bucket, "", b.GetId())
	for _, w := range []string{"Step #0: default-logs-bucket", "DONE"} {
		if !strings.Contains(logText, w) {
			t.Errorf("the log lacks %q:\n%s", w, logText)
		}
	}
	sc := defaultLogsClient(ctx, t, h)
	found := false
	it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "log-" + b.GetId()})
	for {
		o, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("objects.list: %v", err)
		}
		found = found || o.Name == "log-"+b.GetId()+".txt"
	}
	if !found {
		t.Errorf("objects.list does not show log-%s.txt", b.GetId())
	}
	w := sc.Bucket(bucket).Object("mine.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("x"))
	var ge *googleapi.Error
	if err := w.Close(); !asGoogleAPI(err, &ge) || ge.Code != http.StatusForbidden {
		t.Errorf("an upload to the default logs bucket = %v; want 403", err)
	}
	bi := sc.Buckets(ctx, h.Project())
	for {
		ba, err := bi.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("buckets.list: %v", err)
		}
		if ba.Name == bucket {
			t.Errorf("buckets.list shows Cloud Build's default logs bucket, which is Google's")
		}
	}
}

func asGoogleAPI(err error, out **googleapi.Error) bool {
	ge, ok := err.(*googleapi.Error)
	if !ok {
		return false
	}
	*out = ge
	return true
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild, storage.objects.get
//
// TestGcloudBuildsSubmitDefaultLogsBucket (#1430): `gcloud builds submit
// --async --config` of a cloudbuild.yaml that names no logsBucket, through
// the configuration `cloudburrow gcloud-setup` writes, egress blocked. The
// build reports the default logs bucket, and `gcloud storage cat` of
// log-{id}.txt there, which honours the storage override, prints the log.
// (`gcloud builds submit` without --async, and `gcloud builds log`, read
// the log from https://storage.{universe_domain}/ themselves, which no
// endpoint override changes: functions-and-builds.md.)
func TestGcloudBuildsSubmitDefaultLogsBucket(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	g := newGcloudSession(t, h)
	src := t.TempDir()
	config := "steps:\n- name: " + stepImage() + "\n  script: echo gcloud-default-logs\n"
	if err := os.WriteFile(filepath.Join(src, "cloudbuild.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := g.run(nil, "builds", "submit", src, "--config="+filepath.Join(src, "cloudbuild.yaml"), "--async", "--format=value(id)")
	t.Logf("gcloud builds submit --async:\n%s", out)
	if err != nil && strings.Contains(out, hostStepsFlag) {
		t.Skip("the instance runs arbitrary build steps only with " + hostStepsFlag + " (#1382, ADR-0004); start it with the flag")
	}
	if err != nil {
		t.Fatalf("gcloud builds submit: %v", err)
	}
	id := lastLine(out)
	var desc string
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		desc = g.must("builds", "describe", id, "--format=value(status,logsBucket)")
		if f := strings.Fields(desc); len(f) > 0 && f[0] != "QUEUED" && f[0] != "WORKING" {
			break
		}
	}
	f := strings.Fields(desc)
	if len(f) != 2 || f[0] != "SUCCESS" || !defaultLogsBucketRE.MatchString(f[1]) {
		t.Fatalf("the build = %q; want SUCCESS with the default logs bucket", desc)
	}
	cat := g.must("storage", "cat", f[1]+"/log-"+id+".txt")
	for _, w := range []string{"Step #0: gcloud-default-logs", "DONE"} {
		if !strings.Contains(cat, w) {
			t.Errorf("gcloud storage cat of the log lacks %q:\n%s", w, cat)
		}
	}
	// The official client reads the same object.
	ctx, cancel := context.WithTimeout(h.Context(), time.Minute)
	defer cancel()
	r, err := defaultLogsClient(ctx, t, h).Bucket(strings.TrimPrefix(f[1], "gs://")).Object("log-" + id + ".txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("the log through the client: %v", err)
	}
	body, _ := io.ReadAll(r)
	r.Close()
	if !strings.Contains(string(body), "gcloud-default-logs") {
		t.Errorf("the log through the client = %q", body)
	}
}
