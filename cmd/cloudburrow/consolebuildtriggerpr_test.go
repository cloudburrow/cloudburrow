package main

import (
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
)

// TestTriggerFormPullRequest (#2077): Create trigger's Pull request makes a
// pullRequest filter of Base branch, Invert Regex and Comment control, and
// the trigger's Edit form reads them back.
func TestTriggerFormPullRequest(t *testing.T) {
	link := "projects/p/locations/us-central1/connections/c/gitRepositoryLinks/k"
	for choice, want := range map[string]cbpb.PullRequestFilter_CommentControl{
		"":             cbpb.PullRequestFilter_COMMENTS_ENABLED_FOR_EXTERNAL_CONTRIBUTORS_ONLY,
		ccExceptOwners: cbpb.PullRequestFilter_COMMENTS_ENABLED_FOR_EXTERNAL_CONTRIBUTORS_ONLY,
		ccRequired:     cbpb.PullRequestFilter_COMMENTS_ENABLED,
		ccNotRequired:  cbpb.PullRequestFilter_COMMENTS_DISABLED,
	} {
		tr, err := triggerFromValues(map[string]string{"name": "pr", "event": evPullRequest, "repository": link,
			"base": "^main$", "invert": "true", "commentControl": choice, "config": cfgAuto})
		if err != nil {
			t.Fatalf("Comment control %q: %v", choice, err)
		}
		f := tr.GetRepositoryEventConfig().GetPullRequest()
		if tr.GetRepositoryEventConfig().GetRepository() != link || f.GetBranch() != "^main$" || !f.GetInvertRegex() ||
			f.GetCommentControl() != want || !tr.GetAutodetect() {
			t.Errorf("Comment control %q: the trigger = %v", choice, tr)
		}
		tr.ResourceName = "projects/p/locations/us-central1/triggers/x"
		v := triggerValues(tr)
		if v["event"] != evPullRequest || v["base"] != "^main$" || v["invert"] != "true" || v["commentControl"] != commentControlChoice(want) {
			t.Errorf("Comment control %q: the Edit form's values = %v", choice, v)
		}
	}
	if _, err := triggerFromValues(map[string]string{"name": "pr", "event": evPullRequest, "base": "^main$"}); err == nil {
		t.Error("a pull request trigger without a repository was made")
	}
	if _, err := triggerFromValues(map[string]string{"name": "pr", "event": evPullRequest, "repository": link, "commentControl": "Sometimes"}); err == nil {
		t.Error("an unknown comment control was taken")
	}
}
