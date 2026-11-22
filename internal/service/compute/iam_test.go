package compute

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestSubnetworkIAMPolicy: a subnetwork's allow policy is set (as a
// RegionSetPolicyRequest's policy, then its bindings and etag), read back
// with its etag, kept across a restart, refused on a stale etag and on a
// resource that does not exist, and listed by PolicyResourceNames.
func TestSubnetworkIAMPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compute.json")
	s, err := New(Options{State: path})
	if err != nil {
		t.Fatal(err)
	}
	const sub = "regions/us-central1/subnetworks/default"
	if _, err := s.Call("GET", "p", sub, nil, nil); err != nil {
		t.Fatalf("the default subnetwork: %v", err)
	}
	empty, err := s.Call("GET", "p", sub+"/getIamPolicy", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty["etag"] != "ACAB" || empty["bindings"] != nil {
		t.Fatalf("empty policy = %v", empty)
	}
	set, err := s.Call("POST", "p", sub+"/setIamPolicy", nil, obj{"policy": obj{"etag": "ACAB",
		"bindings": []any{obj{"role": "roles/compute.networkUser", "members": []any{"user:dev@example.com"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if set["etag"] == "ACAB" {
		t.Fatalf("set kept the empty etag: %v", set)
	}
	if _, err := s.Call("POST", "p", sub+"/setIamPolicy", nil, obj{"policy": obj{"etag": "ACAB"}}); ErrorStatus(err) != 409 {
		t.Fatalf("stale etag: %v", err)
	}
	// The request's own bindings and etag, without a policy.
	if _, err := s.Call("POST", "p", sub+"/setIamPolicy", nil, obj{"etag": set["etag"],
		"bindings": []any{obj{"role": "roles/compute.networkUser", "members": []any{"user:ops@example.com"}}}}); err != nil {
		t.Fatal(err)
	}
	s2, err := New(Options{State: path})
	if err != nil {
		t.Fatal(err)
	}
	p := s2.Policy("projects/p/" + sub)
	if p == nil || len(p.Bindings) != 1 || p.Bindings[0].Members[0] != "user:ops@example.com" {
		t.Fatalf("restored policy = %+v", p)
	}
	got, err := s2.Call("POST", "p", sub+"/testIamPermissions", nil, obj{"permissions": []any{"compute.subnetworks.use"}})
	if err != nil || len(strs(got, "permissions")) != 1 {
		t.Fatalf("testIamPermissions = %v, %v", got, err)
	}
	if _, err := s2.Call("GET", "p", "regions/us-central1/subnetworks/absent/getIamPolicy", nil, nil); ErrorStatus(err) != 404 {
		t.Fatalf("absent subnetwork: %v", err)
	}
	if _, err := s2.Call("GET", "p", sub+"/getIamPolicy", map[string][]string{"optionsRequestedPolicyVersion": {"2"}}, nil); ErrorStatus(err) != 400 {
		t.Fatalf("version 2: %v", err)
	}
	if names := s2.PolicyResourceNames("p"); !slices.Contains(names, "projects/p/"+sub) {
		t.Fatalf("PolicyResourceNames = %v", names)
	}
	// A conditional binding is accepted, Compute Engine allowing them.
	if _, err := s2.Call("POST", "p", sub+"/setIamPolicy", nil, obj{"policy": obj{"version": 3.0,
		"bindings": []any{obj{"role": "roles/compute.networkUser", "members": []any{"user:dev@example.com"},
			"condition": obj{"title": "t", "expression": `request.time < timestamp("2030-01-01T00:00:00Z")`}}}}}); err != nil {
		t.Fatalf("conditional binding: %v", err)
	}
}
