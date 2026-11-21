package run

import (
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// TestPullRequestFilter (#2077): the branch regex matches the base branch,
// invertRegex reverses it, and comment control gates the events that are
// not a writer's /gcbrun as Google's CommentControl reference says.
func TestPullRequestFilter(t *testing.T) {
	main := &cbpb.PullRequestFilter{GitRef: &cbpb.PullRequestFilter_Branch{Branch: "^main$"}}
	notMain := &cbpb.PullRequestFilter{GitRef: &cbpb.PullRequestFilter_Branch{Branch: "^main$"}, InvertRegex: true}
	if !pullFilterAccepts(main, "main") || pullFilterAccepts(main, "develop") || pullFilterAccepts(notMain, "main") || !pullFilterAccepts(notMain, "develop") {
		t.Error("pullFilterAccepts does not match the base branch, inverted by invertRegex")
	}
	for _, c := range []struct {
		cc     cbpb.PullRequestFilter_CommentControl
		writes bool
		body   string
		want   bool
	}{
		{cbpb.PullRequestFilter_COMMENTS_DISABLED, false, "", true},
		{cbpb.PullRequestFilter_COMMENTS_ENABLED, true, "", false},
		{cbpb.PullRequestFilter_COMMENTS_ENABLED, true, "please /gcbrun", true},
		{cbpb.PullRequestFilter_COMMENTS_ENABLED, false, "/gcbrun", false},
		{cbpb.PullRequestFilter_COMMENTS_ENABLED_FOR_EXTERNAL_CONTRIBUTORS_ONLY, true, "", true},
		{cbpb.PullRequestFilter_COMMENTS_ENABLED_FOR_EXTERNAL_CONTRIBUTORS_ONLY, false, "/gcbrun", false},
	} {
		if got := pullEventBuilds(c.cc, c.writes, c.body); got != c.want {
			t.Errorf("pullEventBuilds(%v, writer %v, %q) = %v, want %v", c.cc, c.writes, c.body, got, c.want)
		}
	}
	comment := &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Comment_{Comment: &ssmpb.PullRequestComment_Comment{Body: "/gcbrun"}}}
	review := &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Review_{Review: &ssmpb.PullRequestComment_Review{Body: "lgtm"}}}
	if !commentRunsBuilds(comment) || commentRunsBuilds(review) {
		t.Error("commentRunsBuilds does not find /gcbrun in a comment's body only")
	}
}

// TestSourceManagerRepoWriter (#2077): the instance's own account writes;
// another writes with an unconditional repoWriter, repoAdmin or
// instanceOwner binding on the repository or its instance.
func TestSourceManagerRepoWriter(t *testing.T) {
	repo, inst := "projects/p/locations/l/repositories/r", "projects/p/locations/l/instances/i"
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	oldRepo, hadRepo := ssmState.repos[repo]
	oldRepoPol, hadRepoPol := ssmState.policies[repo]
	oldInstPol, hadInstPol := ssmState.policies[inst]
	t.Cleanup(func() {
		restore := func(had bool, k string, v iampolicy.Stored) {
			if had {
				ssmState.policies[k] = v
			} else {
				delete(ssmState.policies, k)
			}
		}
		restore(hadRepoPol, repo, oldRepoPol)
		restore(hadInstPol, inst, oldInstPol)
		if hadRepo {
			ssmState.repos[repo] = oldRepo
		} else {
			delete(ssmState.repos, repo)
		}
	})
	ssmState.repos[repo] = &ssmpb.Repository{Name: repo, Instance: inst}
	ssmState.policies[repo] = iampolicy.Stored{Bindings: []iampolicy.Binding{
		{Role: "roles/securesourcemanager.repoWriter", Members: []string{"user:Writer@example.com"}},
		{Role: "roles/securesourcemanager.repoReader", Members: []string{"user:reader@example.com"}},
		{Role: "roles/securesourcemanager.repoAdmin", Members: []string{"user:later@example.com"}, Condition: &iampolicy.Condition{Expression: "false"}},
	}}
	ssmState.policies[inst] = iampolicy.Stored{Bindings: []iampolicy.Binding{
		{Role: "roles/securesourcemanager.instanceOwner", Members: []string{"serviceAccount:bot@p.iam.gserviceaccount.com"}},
	}}
	for account, want := range map[string]bool{ssmOwnerAccount: true, "writer@example.com": true, "bot@p.iam.gserviceaccount.com": true,
		"reader@example.com": false, "later@example.com": false, "": false, "stranger@example.com": false} {
		if got := ssmRepoWriterLocked(repo, account); got != want {
			t.Errorf("ssmRepoWriterLocked(%q) = %v, want %v", account, got, want)
		}
	}
}
