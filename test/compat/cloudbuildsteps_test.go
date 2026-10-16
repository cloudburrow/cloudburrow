//go:build compat

package compat

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stepImage is the image the arbitrary-step tests run: small, with a shell.
// CLOUDBURROW_TEST_STEP_IMAGE overrides it with one already on the host.
func stepImage() string {
	if v := os.Getenv("CLOUDBURROW_TEST_STEP_IMAGE"); v != "" {
		return v
	}
	return "alpine:3.20"
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/ListBuilds, google.devtools.cloudbuild.v1.CloudBuild/RetryBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild
//
// TestCloudBuildArbitraryStepsFilterAndRetry (#1382), with the official
// Cloud Build client over gRPC on the Run port:
//   - a build whose first step is a shell image, not docker, runs it as
//     Cloud Build's worker does: in /workspace (its dir under it), with its
//     env and entrypoint, the Docker socket at /var/run/docker.sock,
//     HOME=/builder/home, and a named volume a later step reads; two steps
//     with waitFor "-" run at once; a docker build step after them builds
//     the file the first step wrote into /workspace;
//   - a step that exits 3 is FAILURE with exitCode 3, and the step after it
//     stays QUEUED;
//   - ListBuilds' filter (tags, status, AND, OR, create_time), page_size and
//     page_token through the client's iterator, in the global and the
//     regional form;
//   - RetryBuild reruns the build as a new build with a new ID.
func TestCloudBuildArbitraryStepsFilterAndRetry(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(h.Context(), 8*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	cb := cloudBuildClient(ctx, t, h)
	bucket := uploadSource(ctx, t, h, "steps.tar.gz", sourceArchive(t, map[string][]byte{
		"Dockerfile":  []byte("FROM scratch\nCOPY gen.txt /gen.txt\n"),
		"sub/in.txt":  []byte("from the source\n"),
		"placeholder": []byte("x"),
	}))
	src := &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: bucket, Object: "steps.tar.gz"}}}
	tag := "t" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	image := "us-central1-docker.pkg.dev/" + h.Project() + "/cb-repo/steps:v1"
	img := stepImage()
	vol := []*cbpb.Volume{{Name: "shared", Path: "/shared"}}
	start := time.Now().UTC().Add(-time.Second)

	op, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{
		Source: src, Tags: []string{tag, "steps"}, Substitutions: map[string]string{"_WHO": "steps"},
		LogsBucket: "gs://" + bucket + "/logs",
		Steps: []*cbpb.BuildStep{
			{Id: "write", Name: img, Entrypoint: "sh", Dir: "sub", WaitFor: []string{"-"}, Env: []string{"WHO=$_WHO"}, Volumes: vol,
				Args: []string{"-c", `set -e; cat in.txt; echo "hello $WHO" > /workspace/gen.txt; echo vol > /shared/v.txt; ` +
					`test -S /var/run/docker.sock && echo "socket ok"; echo "home=$HOME pwd=$(pwd)"; sleep 2`}},
			{Id: "side", Name: img, Entrypoint: "sh", WaitFor: []string{"-"}, Args: []string{"-c", "echo side-ran; sleep 2"}},
			{Id: "read", Name: img, Entrypoint: "sh", WaitFor: []string{"write", "side"}, Volumes: vol,
				Args: []string{"-c", "cat gen.txt /shared/v.txt"}},
			{Id: "image", Name: "gcr.io/cloud-builders/docker", WaitFor: []string{"read"}, Args: []string{"build", "-t", image, "."}},
		},
		Images: []string{image}}})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	b, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateBuild.Wait: %v", err)
	}
	if b.GetStatus() != cbpb.Build_SUCCESS || len(b.GetResults().GetImages()) != 1 {
		t.Fatalf("build = %v", b)
	}
	w, s := b.GetSteps()[0].GetTiming(), b.GetSteps()[1].GetTiming()
	if !w.GetStartTime().AsTime().Before(s.GetEndTime().AsTime()) || !s.GetStartTime().AsTime().Before(w.GetEndTime().AsTime()) {
		t.Errorf("the two waitFor \"-\" steps did not overlap: %v %v", w, s)
	}
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(),
		option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	r, err := sc.Bucket(bucket).Object("logs/log-" + b.GetId() + ".txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("the build's log: %v", err)
	}
	logText, _ := io.ReadAll(r)
	r.Close()
	for _, want := range []string{`Step #0 - "write": from the source`, `Step #0 - "write": socket ok`,
		`Step #0 - "write": home=/builder/home pwd=/workspace/sub`, `Step #1 - "side": side-ran`,
		`Step #2 - "read": hello steps`, `Step #2 - "read": vol`, "DONE"} {
		if !strings.Contains(string(logText), want) {
			t.Errorf("the log lacks %q:\n%s", want, logText)
		}
	}

	// A failing step: FAILURE, its exit code, the next step never started.
	fop, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: h.Project(), Build: &cbpb.Build{Tags: []string{tag},
		Steps: []*cbpb.BuildStep{{Name: img, Entrypoint: "sh", Args: []string{"-c", "exit 3"}}, {Name: img, Args: []string{"true"}}}}})
	if err != nil {
		t.Fatalf("CreateBuild (failing): %v", err)
	}
	if _, err := fop.Wait(ctx); err == nil {
		t.Errorf("a step that exits 3 succeeded")
	}
	fmd, _ := fop.Metadata()
	failed, err := cb.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: h.Project(), Id: fmd.GetBuild().GetId()})
	if err != nil || failed.GetStatus() != cbpb.Build_FAILURE || failed.GetSteps()[0].GetExitCode() != 3 ||
		failed.GetSteps()[1].GetStatus() != cbpb.Build_QUEUED ||
		!strings.Contains(failed.GetStatusDetail(), "step exited with non-zero status: 3") {
		t.Errorf("the failed build = %v, %v", failed, err)
	}
	if _, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: h.Project(), Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: img, SecretEnv: []string{"TOKEN"}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("secretEnv = %v; want UNIMPLEMENTED", err)
	}

	// ListBuilds' filter and paging.
	list := func(req *cbpb.ListBuildsRequest) []string {
		t.Helper()
		var ids []string
		it := cb.ListBuilds(ctx, req)
		for {
			x, err := it.Next()
			if err == iterator.Done {
				return ids
			}
			if err != nil {
				t.Fatalf("ListBuilds(%q): %v", req.GetFilter(), err)
			}
			ids = append(ids, x.GetId())
		}
	}
	both := []string{failed.GetId(), b.GetId()} // newest first
	if got := list(&cbpb.ListBuildsRequest{ProjectId: h.Project(), Filter: `tags="` + tag + `"`, PageSize: 1}); strings.Join(got, ",") != strings.Join(both, ",") {
		t.Errorf("tags filter, one per page = %v, want %v", got, both)
	}
	var page []*cbpb.Build
	next, err := iterator.NewPager(cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{ProjectId: h.Project(), Filter: `tags="` + tag + `"`}), 1, "").NextPage(&page)
	if err != nil || len(page) != 1 || page[0].GetId() != failed.GetId() || next == "" {
		t.Errorf("a page of 1 = %v, next %q, %v", page, next, err)
	}
	for filter, want := range map[string][]string{
		`tags="` + tag + `" AND status="SUCCESS"`:                                 {b.GetId()},
		`tags="` + tag + `" AND (status="FAILURE" OR status="TIMEOUT")`:           {failed.GetId()},
		`tags="` + tag + `" AND create_time>"` + start.Format(time.RFC3339) + `"`: both,
		`tags="` + tag + `" AND images="` + image + `"`:                           {b.GetId()},
		`tags="` + tag + `" AND build_id="` + b.GetId() + `"`:                     {b.GetId()},
	} {
		if got := list(&cbpb.ListBuildsRequest{ProjectId: h.Project(), Filter: filter}); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("filter %s = %v, want %v", filter, got, want)
		}
	}
	if got := list(&cbpb.ListBuildsRequest{Parent: parent, ProjectId: h.Project(), Filter: `tags="` + tag + `"`}); len(got) != 1 || got[0] != b.GetId() {
		t.Errorf("the regional list = %v, want only %s", got, b.GetId())
	}
	it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{ProjectId: h.Project(), Filter: `nosuchfield="x"`})
	if _, err := it.Next(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an unknown filter field = %v; want INVALID_ARGUMENT", err)
	}

	// RetryBuild: a new build, a new ID, the same steps.
	rop, err := cb.RetryBuild(ctx, &cbpb.RetryBuildRequest{Name: b.GetName(), ProjectId: h.Project(), Id: b.GetId()})
	if err != nil {
		t.Fatalf("RetryBuild: %v", err)
	}
	again, err := rop.Wait(ctx)
	if err != nil || again.GetId() == b.GetId() || again.GetStatus() != cbpb.Build_SUCCESS || len(again.GetSteps()) != 4 ||
		again.GetTags()[0] != tag {
		t.Errorf("the retried build = %v, %v", again, err)
	}
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/ListBuilds
//
// TestGcloudBuildsStepsAndList (#1382): `gcloud builds submit --config` of
// a cloudbuild.yaml whose first step is a shell image, not docker, writing
// a file a docker build step then copies in, through the configuration
// `cloudburrow gcloud-setup` writes, egress blocked; then `gcloud builds
// list --filter` by the build's tag and status.
func TestGcloudBuildsStepsAndList(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	g := newGcloudSession(t, h)
	src := t.TempDir()
	tag := "g" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	image := "us-central1-docker.pkg.dev/" + g.project + "/gcloud-repo/steps"
	files := map[string]string{
		"Dockerfile": "FROM scratch\nCOPY gen.txt /gen.txt\n",
		"cloudbuild.yaml": "steps:\n" +
			"- name: " + stepImage() + "\n  id: gen\n  entrypoint: sh\n  args: ['-c', 'echo generated > gen.txt']\n" +
			"- name: gcr.io/cloud-builders/docker\n  args: ['build', '-t', '" + image + ":$_TAG', '.']\n" +
			"- name: gcr.io/cloud-builders/docker\n  args: ['push', '" + image + ":$_TAG']\n" +
			"images: ['" + image + ":$_TAG']\n" +
			"tags: ['" + tag + "']\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := g.run(nil, "builds", "submit", src, "--config="+filepath.Join(src, "cloudbuild.yaml"),
		"--substitutions=_TAG=v1", "--async", "--format=value(id)")
	t.Logf("gcloud builds submit --config:\n%s", out)
	if err != nil {
		t.Fatalf("gcloud builds submit: %v", err)
	}
	id := lastLine(out)
	var desc string
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		desc = g.must("builds", "describe", id, "--format=value(status,steps[0].status,steps[1].status,results.images[0].name)")
		if f := strings.Fields(desc); len(f) > 0 && f[0] != "QUEUED" && f[0] != "WORKING" {
			break
		}
	}
	if f := strings.Fields(desc); len(f) != 4 || f[0] != "SUCCESS" || f[1] != "SUCCESS" || f[2] != "SUCCESS" || f[3] != image+":v1" {
		t.Fatalf("the build = %q", desc)
	}
	listed := g.must("builds", "list", "--filter=tags="+tag+" AND status=SUCCESS", "--format=value(id)")
	if strings.TrimSpace(listed) != id {
		t.Errorf("gcloud builds list --filter = %q, want %s", listed, id)
	}
	if none := g.must("builds", "list", "--filter=tags="+tag+" AND status=FAILURE", "--format=value(id)"); strings.TrimSpace(none) != "" {
		t.Errorf("gcloud builds list --filter status=FAILURE = %q, want none", none)
	}
}
