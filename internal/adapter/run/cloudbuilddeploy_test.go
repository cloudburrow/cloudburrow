package run

import (
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
)

// TestPlanGcloudRunReadsGooglesDeployStep (#1852): the Deploy step of
// Cloud Run's Connect repo trigger is planned as a local Cloud Run update;
// any other gcloud command stays an arbitrary step, and an unknown flag is
// refused.
func TestPlanGcloudRunReadsGooglesDeployStep(t *testing.T) {
	st := &cbpb.BuildStep{Name: "gcr.io/google.com/cloudsdktool/cloud-sdk:slim", Entrypoint: "gcloud"}
	g, ok, err := planGcloudRun(st.Name, st, []string{"run", "services", "update", "web", "--platform=managed",
		"--image=us-central1-docker.pkg.dev/p/cloud-run-source-deploy/r/web:abc",
		"--labels=managed-by=gcp-cloud-build-deploy-cloud-run,commit-sha=abc", "--region=us-central1", "--quiet"})
	if !ok || err != nil || g.service != "web" || g.region != "us-central1" || g.labels["commit-sha"] != "abc" ||
		g.image != "us-central1-docker.pkg.dev/p/cloud-run-source-deploy/r/web:abc" {
		t.Fatalf("planGcloudRun = %+v, %v, %v", g, ok, err)
	}
	if _, ok, _ := planGcloudRun(st.Name, st, []string{"storage", "ls"}); ok {
		t.Error("gcloud storage ls was planned as a deploy")
	}
	if _, ok, _ := planGcloudRun(st.Name, &cbpb.BuildStep{Entrypoint: "bash"}, []string{"run", "services", "update", "web"}); ok {
		t.Error("a bash step was planned as a deploy")
	}
	if _, ok, err := planGcloudRun(st.Name, st, []string{"run", "services", "update", "web", "--image=x:1", "--no-traffic"}); !ok || err == nil {
		t.Errorf("an unsupported flag = %v, %v; want refused", ok, err)
	}
	if _, ok, err := planGcloudRun(st.Name, st, []string{"run", "services", "update", "web"}); !ok || err == nil {
		t.Errorf("no --image = %v, %v; want refused", ok, err)
	}
}

// TestPushFilterAccepts (#1852): a branch filter matches pushed branches
// by their short name, a tag filter pushed tags, and invertRegex reverses.
func TestPushFilterAccepts(t *testing.T) {
	branch := &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Branch{Branch: "^main$"}}
	inverted := &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Branch{Branch: "^main$"}, InvertRegex: true}
	tag := &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Tag{Tag: `^v\d+`}}
	for _, c := range []struct {
		f    *cbpb.PushFilter
		ref  string
		want bool
	}{
		{branch, "refs/heads/main", true},
		{branch, "refs/heads/dev", false},
		{branch, "refs/tags/main", false},
		{inverted, "refs/heads/dev", true},
		{inverted, "refs/heads/main", false},
		{tag, "refs/tags/v1", true},
		{tag, "refs/heads/v1", false},
	} {
		if got := pushFilterAccepts(c.f, c.ref); got != c.want {
			t.Errorf("pushFilterAccepts(%v, %s) = %v, want %v", c.f, c.ref, got, c.want)
		}
	}
}

// TestDevConnectEventErrorRefusesWhatIsNotALink (#1852).
func TestDevConnectEventErrorRefusesWhatIsNotALink(t *testing.T) {
	push := &cbpb.RepositoryEventConfig_Push{Push: &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Branch{Branch: ".*"}}}
	for _, rc := range []*cbpb.RepositoryEventConfig{
		{Repository: "projects/p/locations/l/connections/c/repositories/r", Filter: push},
		{Repository: "projects/p/locations/l/connections/c/gitRepositoryLinks/k",
			Filter: &cbpb.RepositoryEventConfig_PullRequest{PullRequest: &cbpb.PullRequestFilter{}}}, // no branch regex
		{Repository: "projects/p/locations/l/connections/c/gitRepositoryLinks/k",
			Filter: &cbpb.RepositoryEventConfig_PullRequest{PullRequest: &cbpb.PullRequestFilter{GitRef: &cbpb.PullRequestFilter_Branch{Branch: "[x"}}}},
		{Repository: "projects/p/locations/l/connections/c/gitRepositoryLinks/missing",
			Filter: &cbpb.RepositoryEventConfig_PullRequest{PullRequest: &cbpb.PullRequestFilter{GitRef: &cbpb.PullRequestFilter_Branch{Branch: "^main$"}}}},
		{Repository: "projects/p/locations/l/connections/c/gitRepositoryLinks/k"},
		{Repository: "projects/p/locations/l/connections/c/gitRepositoryLinks/missing", Filter: push},
	} {
		if err := devConnectEventError(rc); err == nil {
			t.Errorf("devConnectEventError(%v) = nil, want a refusal", rc)
		}
	}
}
