package run

import (
	"context"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestJobIamPolicyIsStoredNotEnforced(t *testing.T) {
	t.Parallel()
	js, _, _, f := jobsServer(fakeSecrets{})
	ctx := context.Background()
	name := jobParent + "/jobs/iam"
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "iam", Job: sampleJob()}); err != nil {
		t.Fatal(err)
	}
	before, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := js.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
		t.Fatalf("GetIamPolicy on a new job = %v, %v; want no bindings and an etag", empty, err)
	}
	set, err := js.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
		Version: 3, Etag: empty.GetEtag(),
		Bindings: []*iampb.Binding{{Role: "roles/run.invoker", Members: []string{"user:ada@example.com"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := js.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != "user:ada@example.com" ||
		string(got.GetEtag()) != string(set.GetEtag()) {
		t.Fatalf("read back = %v, %v", got, err)
	}
	if _, err := js.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name,
		Policy: &iampb.Policy{Etag: empty.GetEtag()}}); status.Code(err) != codes.Aborted {
		t.Errorf("stale etag = %v, want Aborted", err)
	}
	if _, err := js.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Version: 3,
		Bindings: []*iampb.Binding{{Role: "roles/run.invoker", Members: []string{"allUsers"},
			Condition: &expr.Expr{Expression: "true"}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("condition = %v, want Unimplemented", err)
	}
	// The policy is not on the job: its etag is unchanged.
	after, err := js.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil || after.GetEtag() != before.GetEtag() {
		t.Errorf("job etag %q -> %q (%v); a policy change must not change it", before.GetEtag(), after.GetEtag(), err)
	}
	// Listing jobs does not list the policy's ConfigMap.
	list, err := js.ListJobs(ctx, &runpb.ListJobsRequest{Parent: jobParent})
	if err != nil || len(list.GetJobs()) != 1 {
		t.Errorf("ListJobs = %v, %v; want the one job", list, err)
	}
	perms, err := js.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: []string{"run.jobs.run", "run.jobs.delete"}})
	if err != nil || len(perms.GetPermissions()) != 2 {
		t.Errorf("TestIamPermissions = %v, %v; want both", perms, err)
	}
	missing, err := js.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: jobParent + "/jobs/nope", Permissions: []string{"run.jobs.run"}})
	if err != nil || len(missing.GetPermissions()) != 0 {
		t.Errorf("TestIamPermissions on a missing job = %v, %v; want an empty set", missing, err)
	}
	if _, err := js.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: jobParent + "/jobs/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy on a missing job = %v, want NotFound", err)
	}

	// A policy left by an earlier job of the same name (the garbage
	// collector not yet run) is not the new job's.
	f.mu.Lock()
	cm := f.cms[iamCMPrefix+"job-"+nameScope(jobParent)+"-iam"]
	cm["metadata"].(map[string]any)["annotations"].(map[string]any)[annOwnerUID] = "uid-of-an-old-job"
	f.mu.Unlock()
	stale, err := js.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(stale.GetBindings()) != 0 {
		t.Errorf("policy of an earlier job = %v, %v; want none", stale, err)
	}
	if _, err := js.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/run.developer", Members: []string{"group:g@example.com"}}}}}); err != nil {
		t.Errorf("SetIamPolicy over an earlier job's policy: %v", err)
	}
}
