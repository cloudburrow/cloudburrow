package storage

import "testing"

// TestBucketPolicyRewriteMember (#1751): a deleted account's member is
// renamed in place, a binding's other members and condition are kept, the
// set number advances, and dropping the last member drops the binding.
func TestBucketPolicyRewriteMember(t *testing.T) {
	cond := &policyCondition{Title: "t", Expression: "true"}
	p := &bucketPolicy{Version: 3, Seq: 4, Bindings: []policyBinding{
		{Role: "roles/storage.objectViewer", Members: []string{"serviceAccount:a@p.iam.gserviceaccount.com", "user:keep@example.com"}, Condition: cond},
		{Role: "roles/storage.admin", Members: []string{"serviceAccount:a@p.iam.gserviceaccount.com"}},
	}}
	const del = "deleted:serviceAccount:a@p.iam.gserviceaccount.com?uid=1"
	if !p.rewriteMember("serviceAccount:a@p.iam.gserviceaccount.com", del) || p.Seq != 5 {
		t.Fatalf("rewrite: %+v", p)
	}
	if b := p.Bindings[0]; b.Members[0] != del || b.Members[1] != "user:keep@example.com" || b.Condition != cond {
		t.Errorf("binding 0 = %+v", b)
	}
	if p.rewriteMember("user:nobody@example.com", "") {
		t.Error("an absent member changed the policy")
	}
	p.rewriteMember(del, "")
	if len(p.Bindings) != 1 || len(p.Bindings[0].Members) != 1 {
		t.Errorf("after the purge: %+v", p.Bindings)
	}
}
