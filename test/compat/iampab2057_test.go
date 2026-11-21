//go:build compat

package compat

import (
	"context"
	"testing"

	iam "cloud.google.com/go/iam/apiv3"
	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.iam.v3.PrincipalAccessBoundaryPolicies/CreatePrincipalAccessBoundaryPolicy, google.iam.v3.PrincipalAccessBoundaryPolicies/GetPrincipalAccessBoundaryPolicy, google.iam.v3.PrincipalAccessBoundaryPolicies/ListPrincipalAccessBoundaryPolicies, google.iam.v3.PrincipalAccessBoundaryPolicies/UpdatePrincipalAccessBoundaryPolicy, google.iam.v3.PrincipalAccessBoundaryPolicies/DeletePrincipalAccessBoundaryPolicy, google.iam.v3.PrincipalAccessBoundaryPolicies/SearchPrincipalAccessBoundaryPolicyBindings
// covers: google.iam.v3.PolicyBindings/CreatePolicyBinding, google.iam.v3.PolicyBindings/GetPolicyBinding, google.iam.v3.PolicyBindings/ListPolicyBindings, google.iam.v3.PolicyBindings/UpdatePolicyBinding, google.iam.v3.PolicyBindings/DeletePolicyBinding, google.iam.v3.PolicyBindings/SearchTargetPolicyBindings
//
// TestIAMEnforcementPrincipalAccessBoundary (#2057), with the official
// cloud.google.com/go/iam/apiv3 clients over gRPC and Cloud Storage's
// official client: a service account holding Storage Object Viewer on a
// bucket in its own project and on one in another project reads both; a
// principal access boundary policy listing only its own project, bound to
// its project's principal set with a condition naming the account, leaves
// the first readable and refuses the second with Storage's 403; at
// enforcement version 1, which blocks storage.objects.get but not
// storage.objects.list, the account can list the second bucket's objects
// and not read them, and at version 2 (and latest) it can do neither;
// a condition the account does not satisfy lifts the boundary; removing
// the binding restores access; the policy cannot be deleted while bound.
func TestIAMEnforcementPrincipalAccessBoundary(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	home := iamProject(t, h, "pabh-")
	other := iamProject(t, h, "pabo-")
	owner := storageClient(t, h)
	p := newIAMPrincipal(t, h, home, "pab")
	buckets := map[string]string{home: home + "-pab", other: other + "-pab"}
	for project, bucket := range buckets {
		bh := owner.Bucket(bucket)
		if err := bh.Create(ctx, project, &storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
			t.Fatalf("Create bucket %s: %v", bucket, err)
		}
		t.Cleanup(func() {
			_ = bh.Object("a.txt").Delete(context.Background())
			_ = bh.Delete(context.Background())
		})
		w := bh.Object("a.txt").NewWriter(ctx)
		_, _ = w.Write([]byte("hello"))
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", bucket, err)
		}
		pol, err := bh.IAM().Policy(ctx)
		if err != nil {
			t.Fatalf("getIamPolicy: %v", err)
		}
		pol.Add("serviceAccount:"+p.email, "roles/storage.objectViewer")
		if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
			t.Fatalf("setIamPolicy: %v", err)
		}
	}
	as := newStorageAs(t, h, p.token)
	read := func(project string) error {
		_, err := storageRead(t, h, as.json, buckets[project], "a.txt")
		return err
	}
	list := func(project string) error {
		_, err := as.json.Bucket(buckets[project]).Objects(ctx, nil).Next()
		if err == iterator.Done {
			err = nil
		}
		return err
	}
	getMsg := p.email + " does not have storage.objects.get access to the Google Cloud Storage object. Permission 'storage.objects.get' denied on resource (or it may not exist)."
	listMsg := p.email + " does not have storage.objects.list access to the Google Cloud Storage bucket. Permission 'storage.objects.list' denied on resource (or it may not exist)."
	waitGranted(t, "read the home bucket", func() error { return read(home) })
	waitGranted(t, "read the other bucket", func() error { return read(other) })

	pc, err := iam.NewPrincipalAccessBoundaryPoliciesClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	bc, err := iam.NewPolicyBindingsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	parent := localOrg + "/locations/global"
	op, err := pc.CreatePrincipalAccessBoundaryPolicy(ctx, &iamv3.CreatePrincipalAccessBoundaryPolicyRequest{Parent: parent, PrincipalAccessBoundaryPolicyId: home,
		PrincipalAccessBoundaryPolicy: &iamv3.PrincipalAccessBoundaryPolicy{DisplayName: "home only", Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{
			EnforcementVersion: "1",
			Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{Description: "the home project",
				Resources: []string{"//cloudresourcemanager.googleapis.com/projects/" + home}, Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW}}}}})
	if err != nil {
		t.Fatalf("CreatePrincipalAccessBoundaryPolicy: %v", err)
	}
	policy, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreatePrincipalAccessBoundaryPolicy: %v", err)
	}
	t.Cleanup(func() {
		if op, err := pc.DeletePrincipalAccessBoundaryPolicy(context.Background(), &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: policy.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if policy.GetName() != parent+"/principalAccessBoundaryPolicies/"+home || policy.GetDetails().GetEnforcementVersion() != "1" {
		t.Fatalf("created %v", policy)
	}
	if got, err := pc.GetPrincipalAccessBoundaryPolicy(ctx, &iamv3.GetPrincipalAccessBoundaryPolicyRequest{Name: policy.GetName()}); err != nil || got.GetEtag() != policy.GetEtag() {
		t.Fatalf("GetPrincipalAccessBoundaryPolicy = %v, %v", got, err)
	}
	found := false
	it := pc.ListPrincipalAccessBoundaryPolicies(ctx, &iamv3.ListPrincipalAccessBoundaryPoliciesRequest{Parent: parent})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListPrincipalAccessBoundaryPolicies: %v", err)
		}
		found = found || x.GetName() == policy.GetName()
	}
	if !found {
		t.Error("ListPrincipalAccessBoundaryPolicies misses the policy")
	}

	// Bound to the home project's principal set, for this account only.
	bop, err := bc.CreatePolicyBinding(ctx, &iamv3.CreatePolicyBindingRequest{Parent: "projects/" + home + "/locations/global", PolicyBindingId: "pab-" + home[len(home)-8:],
		PolicyBinding: &iamv3.PolicyBinding{DisplayName: "the test's account",
			Target:     &iamv3.PolicyBinding_Target{Target: &iamv3.PolicyBinding_Target_PrincipalSet{PrincipalSet: "//cloudresourcemanager.googleapis.com/projects/" + home}},
			PolicyKind: iamv3.PolicyBinding_PRINCIPAL_ACCESS_BOUNDARY, Policy: policy.GetName(),
			Condition: &expr.Expr{Expression: "principal.subject == '" + p.email + "'"}}})
	if err != nil {
		t.Fatalf("CreatePolicyBinding: %v", err)
	}
	binding, err := bop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreatePolicyBinding: %v", err)
	}
	t.Cleanup(func() {
		if op, err := bc.DeletePolicyBinding(context.Background(), &iamv3.DeletePolicyBindingRequest{Name: binding.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if binding.GetPolicyUid() != policy.GetUid() || binding.GetTarget().GetPrincipalSet() != "//cloudresourcemanager.googleapis.com/projects/"+home {
		t.Errorf("binding = %v", binding)
	}
	if got, err := bc.GetPolicyBinding(ctx, &iamv3.GetPolicyBindingRequest{Name: binding.GetName()}); err != nil || got.GetPolicy() != policy.GetName() {
		t.Errorf("GetPolicyBinding = %v, %v", got, err)
	}
	if got, err := bc.ListPolicyBindings(ctx, &iamv3.ListPolicyBindingsRequest{Parent: "projects/" + home + "/locations/global", Filter: "policy:" + home}).Next(); err != nil || got.GetName() != binding.GetName() {
		t.Errorf("ListPolicyBindings = %v, %v", got, err)
	}
	if got, err := bc.SearchTargetPolicyBindings(ctx, &iamv3.SearchTargetPolicyBindingsRequest{Parent: "projects/" + home + "/locations/global",
		Target: "//cloudresourcemanager.googleapis.com/projects/" + home, Filter: "policy_kind=PRINCIPAL_ACCESS_BOUNDARY"}).Next(); err != nil || got.GetName() != binding.GetName() {
		t.Errorf("SearchTargetPolicyBindings = %v, %v", got, err)
	}
	if got, err := pc.SearchPrincipalAccessBoundaryPolicyBindings(ctx, &iamv3.SearchPrincipalAccessBoundaryPolicyBindingsRequest{Name: policy.GetName()}).Next(); err != nil || got.GetName() != binding.GetName() {
		t.Errorf("SearchPrincipalAccessBoundaryPolicyBindings = %v, %v", got, err)
	}

	// Version 1 blocks objects.get outside the boundary, not objects.list.
	if err := read(home); err != nil {
		t.Errorf("read the home bucket inside the boundary: %v", err)
	}
	wantStorageDenied(t, "read the other bucket outside the boundary", read(other), getMsg)
	if err := list(other); err != nil {
		t.Errorf("list the other bucket at version 1, which cannot block objects.list: %v", err)
	}
	setVersion := func(v string) {
		t.Helper()
		op, err := pc.UpdatePrincipalAccessBoundaryPolicy(ctx, &iamv3.UpdatePrincipalAccessBoundaryPolicyRequest{
			PrincipalAccessBoundaryPolicy: &iamv3.PrincipalAccessBoundaryPolicy{Name: policy.GetName(), Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{EnforcementVersion: v}},
			UpdateMask:                    &fieldmaskpb.FieldMask{Paths: []string{"details.enforcement_version"}}})
		if err == nil {
			var got *iamv3.PrincipalAccessBoundaryPolicy
			got, err = op.Wait(ctx)
			if err == nil && (got.GetDetails().GetEnforcementVersion() != v || len(got.GetDetails().GetRules()) != 1) {
				t.Fatalf("updated %v", got)
			}
		}
		if err != nil {
			t.Fatalf("UpdatePrincipalAccessBoundaryPolicy to %s: %v", v, err)
		}
	}
	for _, v := range []string{"2", "latest"} {
		setVersion(v)
		wantStorageDenied(t, "list the other bucket at version "+v, list(other), listMsg)
		wantStorageDenied(t, "read the other bucket at version "+v, read(other), getMsg)
		if err := list(home); err != nil {
			t.Errorf("list the home bucket at version %s: %v", v, err)
		}
	}
	if _, err := pc.DeletePrincipalAccessBoundaryPolicy(ctx, &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: policy.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeletePrincipalAccessBoundaryPolicy while bound = %v; want FAILED_PRECONDITION", err)
	}

	// A condition the account does not satisfy is not enforced.
	uop, err := bc.UpdatePolicyBinding(ctx, &iamv3.UpdatePolicyBindingRequest{PolicyBinding: &iamv3.PolicyBinding{Name: binding.GetName(),
		Condition: &expr.Expr{Expression: "principal.type != 'iam.googleapis.com/ServiceAccount'"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"condition"}}})
	if err == nil {
		binding, err = uop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("UpdatePolicyBinding: %v", err)
	}
	waitGranted(t, "read the other bucket with the binding's condition false", func() error { return read(other) })
	uop, err = bc.UpdatePolicyBinding(ctx, &iamv3.UpdatePolicyBindingRequest{PolicyBinding: &iamv3.PolicyBinding{Name: binding.GetName(),
		Condition: &expr.Expr{Expression: "principal.type == 'iam.googleapis.com/ServiceAccount'"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"condition"}}})
	if err == nil {
		binding, err = uop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("UpdatePolicyBinding: %v", err)
	}
	wantStorageDenied(t, "read the other bucket with the condition true again", read(other), getMsg)

	// Removing the binding restores access.
	dop, err := bc.DeletePolicyBinding(ctx, &iamv3.DeletePolicyBindingRequest{Name: binding.GetName(), Etag: binding.GetEtag()})
	if err == nil {
		err = dop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("DeletePolicyBinding: %v", err)
	}
	waitGranted(t, "read the other bucket once the binding is gone", func() error { return read(other) })
	waitGranted(t, "list the other bucket once the binding is gone", func() error { return list(other) })
	pop, err := pc.DeletePrincipalAccessBoundaryPolicy(ctx, &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: policy.GetName()})
	if err == nil {
		err = pop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("DeletePrincipalAccessBoundaryPolicy: %v", err)
	}
	if _, err := pc.GetPrincipalAccessBoundaryPolicy(ctx, &iamv3.GetPrincipalAccessBoundaryPolicyRequest{Name: policy.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetPrincipalAccessBoundaryPolicy after delete = %v; want NOT_FOUND", err)
	}
}
