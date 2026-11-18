//go:build compat

package compat

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformMemorystoreACLPolicy (#1578): a google_memorystore_acl_policy
// with two rules applies through `cloudburrow terraform`
// (memorystore_custom_endpoint) with egress blocked, and the official
// Memorystore client's GetAclPolicy reads the rules back; the plan is
// clean; a changed rule applies in place (UpdateAclPolicy, revision 2);
// the plan is clean again; destroy deletes the policy (NOT_FOUND).
func TestTerraformMemorystoreACLPolicy(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h) // skips without the Memorystore admin endpoint
	m := newTFModule(t)
	ctx := h.Context()
	id := "tf-vacl-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := "projects/" + project + "/locations/us-central1/aclPolicies/" + id
	module := func(readerRule string) string {
		return fmt.Sprintf(`
resource "google_memorystore_acl_policy" "p" {
  project       = %[1]q
  location      = "us-central1"
  acl_policy_id = %[2]q
  rules {
    username = "reader"
    rule     = %[3]q
  }
  rules {
    username = "writer"
    rule     = "on >writerpass ~app:* +@all"
  }
}
`, project, id, readerRule)
	}
	check := func(when, readerRule string, revision int64) {
		t.Helper()
		pol, err := c.GetAclPolicy(ctx, &memorystorepb.GetAclPolicyRequest{Name: name})
		if err != nil {
			t.Fatalf("%s: GetAclPolicy: %v", when, err)
		}
		var rules []string
		for _, r := range pol.GetRules() {
			rules = append(rules, r.GetUsername()+"="+r.GetRule())
		}
		sort.Strings(rules)
		if want := "[reader=" + readerRule + " writer=on >writerpass ~app:* +@all]"; fmt.Sprint(rules) != want {
			t.Errorf("%s: the policy's rules are %v; want %s", when, rules, want)
		}
		if _, err := c.GetAclPolicyRevision(ctx, &memorystorepb.GetAclPolicyRevisionRequest{Name: fmt.Sprintf("%s/revisions/%d", name, revision)}); err != nil {
			t.Errorf("%s: GetAclPolicyRevision(%d): %v", when, revision, err)
		}
	}

	m.write(module("on >readerpass ~app:* +@read"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "on >readerpass ~app:* +@read", 1)
	m.planClean()

	m.write(module("on >readerpass ~cache:* +@read +ping"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the rule was not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "on >readerpass ~cache:* +@read +ping", 2)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetAclPolicy(ctx, &memorystorepb.GetAclPolicyRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetAclPolicy after destroy: %v; want NotFound", err)
	}
}
