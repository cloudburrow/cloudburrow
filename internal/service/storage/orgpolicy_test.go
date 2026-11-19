package storage

import (
	"context"
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

type fakeOrgPolicies map[string]*orgpolicypb.PolicySpec

func (f fakeOrgPolicies) Effective(_ context.Context, resource, constraint string) (orgpolicy.Effective, error) {
	if resource != "projects/op-proj" {
		return orgpolicy.Effective{}, nil
	}
	return orgpolicy.Effective{Spec: f[constraint]}, nil
}

func enforceSpec() *orgpolicypb.PolicySpec {
	return &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}
}

// TestBucketOrgPolicies (#1890): gcp.resourceLocations,
// storage.uniformBucketLevelAccess and storage.publicAccessPrevention are
// 412 conditionNotMet on the project they are set on, and nothing changes
// on another.
func TestBucketOrgPolicies(t *testing.T) {
	orgpolicy.Use(fakeOrgPolicies{
		orgpolicy.ResourceLocations: {Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Values{
			Values: &orgpolicypb.PolicySpec_PolicyRule_StringValues{AllowedValues: []string{"in:eu-locations"}}}}}},
		orgpolicy.UniformBucketLevelAccess: enforceSpec(),
		orgpolicy.PublicAccessPrevention:   enforceSpec(),
	})
	t.Cleanup(func() { orgpolicy.Use(nil) })
	h := newTestServer(t)
	base := h.URL + "/storage/v1/b"
	ubla := `"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}`
	if code, out := doJSON(t, "POST", base+"?project=op-proj", `{"name":"op-us","location":"US",`+ubla+`}`); code != 412 ||
		out["error"].(map[string]any)["message"] != "Location MULTI_REGION:US violates constraint constraints/gcp.resourceLocations on the resource projects/op-proj." {
		t.Fatalf("a US bucket = %d %v, want 412", code, out)
	}
	if code, out := doJSON(t, "POST", base+"?project=op-proj", `{"name":"op-eu-acl","location":"EU"}`); code != 412 {
		t.Fatalf("a bucket without uniform access = %d %v, want 412", code, out)
	}
	if code, out := doJSON(t, "POST", base+"?project=op-proj", `{"name":"op-eu","location":"EU",`+ubla+`}`); code != 200 {
		t.Fatalf("an EU uniform bucket = %d %v, want 200", code, out)
	}
	if code, out := doJSON(t, "PATCH", base+"/op-eu", `{"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":false}}}`); code != 412 {
		t.Fatalf("turning uniform access off = %d %v, want 412", code, out)
	}
	pol := `{"bindings":[{"role":"roles/storage.objectViewer","members":["allUsers"]}]}`
	if code, out := doJSON(t, "PUT", base+"/op-eu/iam", pol); code != 412 {
		t.Fatalf("an allUsers grant under public access prevention = %d %v, want 412", code, out)
	}
	if code, out := doJSON(t, "PUT", base+"/op-eu/iam", `{"bindings":[{"role":"roles/storage.objectViewer","members":["user:a@cloudburrow.local"]}]}`); code != 200 {
		t.Fatalf("a private grant = %d %v, want 200", code, out)
	}
	// Another project has no policy: nothing changes.
	if code, out := doJSON(t, "POST", base+"?project=other-proj", `{"name":"op-other","location":"US"}`); code != 200 {
		t.Fatalf("a bucket in a project without policies = %d %v, want 200", code, out)
	}
	if code, out := doJSON(t, "POST", base+"/op-other/acl", `{"entity":"allUsers","role":"READER"}`); code != 200 {
		t.Fatalf("a public ACL without public access prevention = %d %v, want 200", code, out)
	}
	if code, out := doJSON(t, "PATCH", base+"/op-other", `{"iamConfiguration":{"publicAccessPrevention":"enforced"}}`); code != 200 {
		t.Fatalf("enforcing public access prevention = %d %v", code, out)
	}
	if code, out := doJSON(t, "POST", base+"/op-other/defaultObjectAcl", `{"entity":"allAuthenticatedUsers","role":"READER"}`); code != 412 {
		t.Fatalf("a public ACL on a bucket enforcing public access prevention = %d %v, want 412", code, out)
	}
}
