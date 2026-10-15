//go:build compat

package compat

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func cloudBuildClient(ctx context.Context, t *testing.T, h *Harness) *cloudbuild.Client {
	t.Helper()
	cb, err := cloudbuild.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cb.Close() })
	return cb
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/CancelBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild
//
// TestCloudBuildCreateBuild (#1347): the official Cloud Build client's
// CreateBuild, over gRPC on the Run port, of the sourcebuild fixture from
// the instance's Cloud Storage with a docker build and a docker push step
// and a substitution. The operation is polled to completion; the build is
// SUCCESS with each step's status and timing, its image's digest, and its
// log at logsBucket/log-{id}.txt; a service naming the image by digest runs
// it. A push of an image no step built is FAILURE; a running build is
// CANCELLED by CancelBuild; an arbitrary step's container is UNIMPLEMENTED.
func TestCloudBuildCreateBuild(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(h.Context(), 8*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	cb := cloudBuildClient(ctx, t, h)
	bucket := uploadSource(ctx, t, h, "source.tar.gz", sourceBuildArchive(ctx, t))
	src := &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: bucket, Object: "source.tar.gz"}}}
	image := "us-central1-docker.pkg.dev/$PROJECT_ID/cb-repo/created:${_TAG}"
	want := "us-central1-docker.pkg.dev/" + h.Project() + "/cb-repo/created:v1"

	op, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{
		Source: src, Substitutions: map[string]string{"_TAG": "v1"}, LogsBucket: "gs://" + bucket + "/logs",
		Steps: []*cbpb.BuildStep{
			{Id: "build", Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "--network", "cloudbuild", "-t", image, "."}},
			{Id: "push", Name: "gcr.io/cloud-builders/docker", WaitFor: []string{"build"}, Args: []string{"push", image}},
		},
		Images: []string{image}}})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	md, err := op.Metadata()
	if err != nil || md.GetBuild().GetId() == "" {
		t.Fatalf("CreateBuild metadata = %v, %v", md, err)
	}
	if op.Name() != parent+"/operations/"+md.GetBuild().GetId() {
		t.Errorf("operation name = %s", op.Name())
	}
	b, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateBuild.Wait: %v", err)
	}
	if b.GetStatus() != cbpb.Build_SUCCESS || len(b.GetSteps()) != 2 || b.GetSteps()[0].GetArgs()[4] != want ||
		len(b.GetResults().GetImages()) != 1 || b.GetResults().GetImages()[0].GetName() != want ||
		!strings.HasPrefix(b.GetResults().GetImages()[0].GetDigest(), "sha256:") || b.GetLogUrl() == "" {
		t.Fatalf("build = %v", b)
	}
	for i, st := range b.GetSteps() {
		if st.GetStatus() != cbpb.Build_SUCCESS || st.GetTiming().GetStartTime() == nil || st.GetTiming().GetEndTime() == nil {
			t.Errorf("step %d = %v", i, st)
		}
	}
	if got, err := cb.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: h.Project(), Name: b.GetName()}); err != nil || got.GetStatus() != cbpb.Build_SUCCESS {
		t.Errorf("GetBuild = %v, %v", got, err)
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
	for _, w := range []string{"FETCHSOURCE", `Starting Step #0 - "build"`, `Step #1 - "push": The push refers to`, "PUSH", "DONE"} {
		if !strings.Contains(string(logText), w) {
			t.Errorf("the log lacks %q:\n%s", w, logText)
		}
	}

	// The image by digest runs the build.
	c := runClient(t, h)
	id := "cb-created"
	name := parent + "/services/" + id
	t.Cleanup(func() { _, _ = c.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
	sop, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: strings.TrimSuffix(want, ":v1") + "@" + b.GetResults().GetImages()[0].GetDigest()}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := sop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateService.Wait: %v", err)
	}
	if code, body := functionRequest(t, ingress(t), hostOf(t, svc.GetUri()), "", nil); code != http.StatusOK ||
		body != "built from source by SubmitBuild" {
		t.Errorf("the service answered %d %q", code, body)
	}

	// A push of an image no step built fails at that step.
	fop, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: h.Project(), Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"push", "gcr.io/" + h.Project() + "/never-built"}}}}})
	if err != nil {
		t.Fatalf("CreateBuild (global): %v", err)
	}
	if _, err := fop.Wait(ctx); err == nil {
		t.Errorf("a push of an unbuilt image succeeded")
	}
	fmd, _ := fop.Metadata()
	if got, err := cb.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: h.Project(), Id: fmd.GetBuild().GetId()}); err != nil ||
		got.GetStatus() != cbpb.Build_FAILURE || got.GetSteps()[0].GetStatus() != cbpb.Build_FAILURE || got.GetStatusDetail() == "" {
		t.Errorf("the failed build = %v, %v", got, err)
	}

	// CancelBuild stops a running build.
	cop, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{
		Source: src, Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", "gcr.io/" + h.Project() + "/cancelled", "."}}}}})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	cmd, _ := cop.Metadata()
	cancelled, err := cb.CancelBuild(ctx, &cbpb.CancelBuildRequest{Name: cmd.GetBuild().GetName(), ProjectId: h.Project(), Id: cmd.GetBuild().GetId()})
	if err != nil || cancelled.GetStatus() != cbpb.Build_CANCELLED {
		t.Errorf("CancelBuild = %v, %v", cancelled, err)
	}
	if _, err := cop.Wait(ctx); status.Code(err) != codes.Canceled {
		t.Errorf("the cancelled build's operation = %v; want CANCELLED", err)
	}

	// An arbitrary step's container is refused, with the reason.
	_, err = cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{
		Source: src, Steps: []*cbpb.BuildStep{{Name: "ubuntu", Args: []string{"echo", "hi"}}}}})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "only docker build") {
		t.Errorf("an arbitrary step = %v; want UNIMPLEMENTED with the reason", err)
	}
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild
//
// TestGcloudBuildsSubmit (#1347): `gcloud builds submit` through the
// configuration `cloudburrow gcloud-setup` writes, egress blocked: of a
// Dockerfile source with --tag (the global builds.create), and of a
// cloudbuild.yaml of a docker build and a docker push step with --region
// (locations.builds.create). gcloud stages the source in the
// {project}_cloudbuild bucket and creates the build; `gcloud builds
// describe` polls it to SUCCESS. --async: gcloud 587's wait tails the log
// from https://storage.googleapis.com/{logsBucket}/log-{id}.txt itself,
// ignoring the storage endpoint override (the egress guard blocked it),
// even with --suppress-logs; the log is in the instance's Cloud Storage,
// where TestCloudBuildCreateBuild reads it.
func TestGcloudBuildsSubmit(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	g := newGcloudSession(t, h)
	// wait polls `gcloud builds describe` until the build is done.
	wait := func(id string, region ...string) string {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			out := g.must(append([]string{"builds", "describe", id,
				"--format=value(status,results.images[0].name,steps[0].status,steps[-1].status)"}, region...)...)
			if f := strings.Fields(out); len(f) > 0 && f[0] != "QUEUED" && f[0] != "WORKING" {
				return out
			}
		}
		t.Fatalf("build %s never finished", id)
		return ""
	}
	src := sourceBuildDir(t)
	tag := "us-central1-docker.pkg.dev/" + g.project + "/gcloud-repo/submitted"
	out, err := g.run(nil, "builds", "submit", src, "--tag="+tag, "--async", "--format=value(id)")
	t.Logf("gcloud builds submit --tag:\n%s", out)
	if err != nil {
		t.Fatalf("gcloud builds submit --tag: %v", err)
	}
	if desc := wait(lastLine(out)); !strings.HasPrefix(desc, "SUCCESS") || !strings.Contains(desc, tag+":latest") {
		t.Errorf("the --tag build = %q", desc)
	}

	cfg := "steps:\n" +
		"- name: gcr.io/cloud-builders/docker\n  id: build\n  args: ['build', '-t', '" + tag + ":$_TAG', '.']\n" +
		"- name: gcr.io/cloud-builders/docker\n  args: ['push', '" + tag + ":$_TAG']\n" +
		"images: ['" + tag + ":$_TAG']\n"
	if err := os.WriteFile(filepath.Join(src, "cloudbuild.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = g.run(nil, "builds", "submit", src, "--config="+filepath.Join(src, "cloudbuild.yaml"),
		"--region=us-central1", "--substitutions=_TAG=v2", "--async", "--format=value(id)")
	t.Logf("gcloud builds submit --config:\n%s", out)
	if err != nil {
		t.Fatalf("gcloud builds submit --config: %v", err)
	}
	desc := wait(lastLine(out), "--region=us-central1")
	if f := strings.Fields(desc); len(f) != 4 || f[0] != "SUCCESS" || f[1] != tag+":v2" || f[2] != "SUCCESS" || f[3] != "SUCCESS" {
		t.Errorf("the --config build = %q", desc)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
