//go:build compat

package compat

import (
	"bytes"
	"context"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkRunIamPolicy drives GetIamPolicy, SetIamPolicy and TestIamPermissions
// on one Cloud Run resource through the official client, under ADR-0006 as
// amended by #1216: a policy round-trips with a new etag, a stale etag is
// ABORTED, a condition is UNIMPLEMENTED, version 3 without conditions is
// accepted, and TestIamPermissions returns every requested permission, and
// an empty set on a missing resource.
func checkRunIamPolicy(t *testing.T, ctx context.Context, name, missing string,
	get func(*iampb.GetIamPolicyRequest) (*iampb.Policy, error),
	set func(*iampb.SetIamPolicyRequest) (*iampb.Policy, error),
	test func(*iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error)) {
	t.Helper()
	empty, err := get(&iampb.GetIamPolicyRequest{Resource: name, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	if len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
		t.Fatalf("a new resource's policy = %v, want no bindings and an etag", empty)
	}
	set1, err := set(&iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Version: 3, Etag: empty.GetEtag(),
		Bindings: []*iampb.Binding{{Role: "roles/run.invoker", Members: []string{"user:ada@example.com", "allUsers"}}}}})
	if err != nil {
		t.Fatalf("SetIamPolicy (version 3, no conditions): %v", err)
	}
	if bytes.Equal(set1.GetEtag(), empty.GetEtag()) {
		t.Error("SetIamPolicy returned the old etag")
	}
	got, err := get(&iampb.GetIamPolicyRequest{Resource: name})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	if len(got.GetBindings()) != 1 || got.GetBindings()[0].GetRole() != "roles/run.invoker" ||
		len(got.GetBindings()[0].GetMembers()) != 2 || !bytes.Equal(got.GetEtag(), set1.GetEtag()) {
		t.Errorf("read back = %v, want the binding set with etag %x", got, set1.GetEtag())
	}
	if _, err := set(&iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Etag: empty.GetEtag()}}); status.Code(err) != codes.Aborted {
		t.Errorf("SetIamPolicy with a stale etag = %v, want ABORTED", err)
	}
	if _, err := set(&iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Version: 3,
		Bindings: []*iampb.Binding{{Role: "roles/run.invoker", Members: []string{"allUsers"},
			Condition: &expr.Expr{Title: "t", Expression: `request.time < timestamp("2030-01-01T00:00:00Z")`}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("SetIamPolicy with a condition = %v, want UNIMPLEMENTED", err)
	}
	if after, err := get(&iampb.GetIamPolicyRequest{Resource: name}); err != nil || !bytes.Equal(after.GetEtag(), set1.GetEtag()) {
		t.Errorf("a refused SetIamPolicy changed the policy: %v, %v", after, err)
	}
	perms := []string{"run.services.get", "run.jobs.run", "run.routes.invoke"}
	resp, err := test(&iampb.TestIamPermissionsRequest{Resource: name, Permissions: perms})
	if err != nil || len(resp.GetPermissions()) != len(perms) {
		t.Errorf("TestIamPermissions = %v, %v; want every requested permission", resp, err)
	}
	// On a missing resource, an empty set, as iam_policy.proto documents.
	if resp, err := test(&iampb.TestIamPermissionsRequest{Resource: missing, Permissions: perms}); err != nil || len(resp.GetPermissions()) != 0 {
		t.Errorf("TestIamPermissions on a missing resource = %v, %v; want an empty set", resp, err)
	}
	if _, err := get(&iampb.GetIamPolicyRequest{Resource: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy on a missing resource = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.run.v2.Services/GetIamPolicy, google.cloud.run.v2.Services/SetIamPolicy, google.cloud.run.v2.Services/TestIamPermissions
// An empty set on a missing resource is iam_policy.proto's documented answer (read 2026-10-17).
// A stale etag is ABORTED, as https://cloud.google.com/iam/docs/policies documents (read 2026-10-17).
//
// TestRunServiceIamPolicyIsStoredNotEnforced (#1216): through the official
// ServicesClient. The service's own etag is unchanged by a policy change, so
// a caller's next UpdateService is not ABORTED by it, and a service deleted
// and created again under the same name starts with no policy.
func TestRunServiceIamPolicyIsStoredNotEnforced(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	ctx := runWait(t)
	id := "compat-iam"
	name := runParent(h) + "/services/" + id
	create := func() {
		t.Helper()
		op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id,
			Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
				Image: "ghcr.io/knative/helloworld-go:latest"}}}}})
		if err != nil {
			t.Fatalf("CreateService: %v", err)
		}
		if _, err := waitLRO(ctx, op); err != nil {
			t.Fatalf("waiting for the service: %v", err)
		}
	}
	create()
	t.Cleanup(func() { _, _ = c.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
	before, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	checkRunIamPolicy(t, ctx, name, runParent(h)+"/services/compat-iam-missing",
		func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return c.GetIamPolicy(ctx, r) },
		func(r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) { return c.SetIamPolicy(ctx, r) },
		func(r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
			return c.TestIamPermissions(ctx, r)
		})
	after, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil || after.GetEtag() != before.GetEtag() {
		t.Errorf("service etag %q -> %q (%v): a policy change must leave it alone", before.GetEtag(), after.GetEtag(), err)
	}

	dop, err := c.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitLRO(ctx, dop); err != nil {
		t.Fatal(err)
	}
	create()
	if p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name}); err != nil || len(p.GetBindings()) != 0 {
		t.Errorf("a recreated service's policy = %v, %v; want none", p, err)
	}
}

// covers: google.cloud.run.v2.Jobs/GetIamPolicy, google.cloud.run.v2.Jobs/SetIamPolicy, google.cloud.run.v2.Jobs/TestIamPermissions
// An empty set on a missing resource is iam_policy.proto's documented answer (read 2026-10-17).
// A stale etag is ABORTED, as https://cloud.google.com/iam/docs/policies documents (read 2026-10-17).
//
// TestRunJobIamPolicyIsStoredNotEnforced (#1216): the same through the
// official JobsClient. The job's etag is unchanged, and RunJob still runs
// with a policy that grants run.jobs.run to nobody.
func TestRunJobIamPolicyIsStoredNotEnforced(t *testing.T) {
	h := New(t)
	jc, _ := runJobClients(t, h)
	ctx := runWait(t)
	id := "compat-job-iam"
	name := runParent(h) + "/jobs/" + id
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: shellJob("true", 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	before, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: name}) })
	checkRunIamPolicy(t, ctx, name, runParent(h)+"/jobs/compat-job-iam-missing",
		func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return jc.GetIamPolicy(ctx, r) },
		func(r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) { return jc.SetIamPolicy(ctx, r) },
		func(r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
			return jc.TestIamPermissions(ctx, r)
		})
	after, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil || after.GetEtag() != before.GetEtag() {
		t.Errorf("job etag %q -> %q (%v): a policy change must leave it alone", before.GetEtag(), after.GetEtag(), err)
	}
	// Nothing is enforced: the policy names no one with run.jobs.run.
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob under a policy granting no one: %v", err)
	}
	if _, err := waitLRO(ctx, rop); err != nil {
		t.Errorf("execution: %v", err)
	}
}
