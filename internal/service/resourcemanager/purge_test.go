package resourcemanager

import (
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const purgeBool = "constraints/compute.requireOsLogin"

// fillProject gives project id everything the registry keeps for a project:
// an IAM policy, an org policy, a lien, a tag key with a value, a binding of
// an organization tag on it and one on a resource in it.
func fillProject(t *testing.T, r *Registry, id string, orgValue string) {
	t.Helper()
	if _, err := r.SetIAMPolicy("projects/"+id, &iampb.SetIamPolicyRequest{Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetOrgPolicy("projects/"+id, &crmv1.OrgPolicy{Constraint: purgeBool,
		BooleanPolicy: &crmv1.BooleanPolicy{Enforced: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateLien(Lien{Parent: "projects/" + id, Restrictions: []string{"resourcemanager.projects.update"},
		Reason: "r", Origin: "o"}); err != nil {
		t.Fatal(err)
	}
	k, err := r.CreateTagKey(&rmpb.TagKey{Parent: "projects/" + id, ShortName: "team"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: "core"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateTagBinding(&rmpb.TagBinding{Parent: CRMPrefix + "projects/" + id, TagValue: orgValue}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateTagBinding(&rmpb.TagBinding{Parent: "//run.googleapis.com/projects/" + id + "/locations/us-central1/services/s",
		TagValue: orgValue}, false); err != nil {
		t.Fatal(err)
	}
}

// projectState counts what the registry holds for project id.
func projectState(t *testing.T, r *Registry, id string) (n int) {
	t.Helper()
	if p, err := r.IAMPolicy("projects/" + id); err != nil {
		t.Fatal(err)
	} else if p != nil && len(p.Bindings) > 0 {
		n++
	}
	if ps, err := r.OrgPolicies("projects/" + id); err != nil {
		t.Fatal(err)
	} else {
		n += len(ps)
	}
	if ls, err := r.Liens("projects/" + id); err != nil {
		t.Fatal(err)
	} else {
		n += len(ls)
	}
	if ks, err := r.TagKeys("projects/" + id); err != nil {
		t.Fatal(err)
	} else {
		n += len(ks)
	}
	if bs, err := r.TagBindings(CRMPrefix + "projects/" + id); err != nil {
		t.Fatal(err)
	} else {
		n += len(bs)
	}
	if bs, err := r.TagBindings("//run.googleapis.com/projects/" + id + "/locations/us-central1/services/s"); err != nil {
		t.Fatal(err)
	} else {
		n += len(bs)
	}
	return n
}

// TestPurgeDropsTheProjectsState (#1537): a project made again under a purged
// one's ID starts empty, whether the purge came RetentionPeriod after the
// delete or at once through Purge; undelete within the window keeps
// everything; another project's state is untouched; the hooks hear of each
// purge once.
func TestPurgeDropsTheProjectsState(t *testing.T) {
	r := newRegistry(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	var purged []string
	r.OnPurge(func(id string) error {
		// Called without the lock: calling back must not deadlock.
		_, _ = r.Get(id)
		purged = append(purged, id)
		return nil
	})
	k, err := r.CreateTagKey(&rmpb.TagKey{Parent: OrganizationName, ShortName: "env"}, false)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: "prod"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"reused-project", "other-project"} {
		if _, err := r.Create(Project{ProjectID: id}); err != nil {
			t.Fatal(err)
		}
		fillProject(t, r, id, v.GetName())
	}
	const full = 6
	if n := projectState(t, r, "reused-project"); n != full {
		t.Fatalf("set up %d pieces of state, want %d", n, full)
	}

	if err := r.Purge("reused-project"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Purge of an active project = %v, want FailedPrecondition", err)
	}
	if _, err := r.Delete("reused-project"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Undelete("reused-project"); err != nil {
		t.Fatal(err)
	}
	if n := projectState(t, r, "reused-project"); n != full {
		t.Errorf("after undelete %d pieces of state, want all %d", n, full)
	}

	// Purged at once.
	if _, err := r.Delete("reused-project"); err != nil {
		t.Fatal(err)
	}
	if err := r.Purge("reused-project"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("reused-project"); status.Code(err) != codes.NotFound {
		t.Errorf("Get after Purge = %v, want NotFound", err)
	}
	if _, err := r.Create(Project{ProjectID: "reused-project"}); err != nil {
		t.Fatal(err)
	}
	if n := projectState(t, r, "reused-project"); n != 0 {
		t.Errorf("a project made again under a purged ID reads %d pieces of the old one's state", n)
	}
	if n := projectState(t, r, "other-project"); n != full {
		t.Errorf("another project kept %d pieces of state, want %d", n, full)
	}

	// Purged RetentionPeriod after the delete, found on reading it.
	fillProject(t, r, "reused-project", v.GetName())
	if _, err := r.Delete("reused-project"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(RetentionPeriod)
	if _, err := r.Create(Project{ProjectID: "reused-project"}); err != nil {
		t.Fatal(err)
	}
	if n := projectState(t, r, "reused-project"); n != 0 {
		t.Errorf("after the window, the new project reads %d pieces of the old one's state", n)
	}
	if len(purged) != 2 || purged[0] != "reused-project" || purged[1] != "reused-project" {
		t.Errorf("OnPurge heard %v, want reused-project twice", purged)
	}
	// The organization's tag value is the organization's, and stays.
	if _, err := r.TagValue(v.GetName()); err != nil {
		t.Errorf("an organization tag value went with a project: %v", err)
	}
}

// TestResetProjectState: /admin/reset with project= clears that project's
// state and keeps the project; without, every resource's.
func TestResetProjectState(t *testing.T) {
	r := newRegistry(t)
	k, err := r.CreateTagKey(&rmpb.TagKey{Parent: OrganizationName, ShortName: "env"}, false)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: "prod"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"reset-one", "reset-two"} {
		if _, err := r.Create(Project{ProjectID: id}); err != nil {
			t.Fatal(err)
		}
		fillProject(t, r, id, v.GetName())
	}
	if err := r.ResetProjectState("reset-one"); err != nil {
		t.Fatal(err)
	}
	if n := projectState(t, r, "reset-one"); n != 0 {
		t.Errorf("reset project kept %d pieces of state", n)
	}
	if n := projectState(t, r, "reset-two"); n != 6 {
		t.Errorf("the other project kept %d pieces of state, want 6", n)
	}
	if _, err := r.Get("reset-one"); err != nil {
		t.Errorf("reset removed the project: %v", err)
	}
	if err := r.ResetProjectState(""); err != nil {
		t.Fatal(err)
	}
	if n := projectState(t, r, "reset-two"); n != 0 {
		t.Errorf("a full reset kept %d pieces of state", n)
	}
	if keys, _ := r.AllTagKeys(); len(keys) != 0 {
		t.Errorf("a full reset kept tag keys: %v", keys)
	}
}
