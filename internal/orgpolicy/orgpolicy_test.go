package orgpolicy

import (
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

func vals(allowed, denied []string) *orgpolicypb.PolicySpec_PolicyRule {
	return &orgpolicypb.PolicySpec_PolicyRule{Kind: &orgpolicypb.PolicySpec_PolicyRule_Values{
		Values: &orgpolicypb.PolicySpec_PolicyRule_StringValues{AllowedValues: allowed, DeniedValues: denied}}}
}

func TestLocationGroups(t *testing.T) {
	for _, c := range []struct {
		value, loc string
		want       bool
	}{
		{"in:us-locations", "us-central1", true},
		{"in:us-locations", "us-central1-b", true},
		{"in:us-locations", "US", true},
		{"in:us-locations", "nam5", true},
		{"in:us-locations", "europe-west1", false},
		{"in:eu-locations", "europe-west3", true},
		{"in:eu-locations", "europe-west2", false}, // London is Europe, not the EU
		{"in:europe-locations", "europe-west2", true},
		{"in:asia-east1-locations", "asia-east1-a", true},
		{"us-east1", "us-east1", true},
		{"us-east1", "us-east1-b", false},
		{"is:us", "US", true},
	} {
		if got := locationMatches(c.value, c.loc); got != c.want {
			t.Errorf("locationMatches(%q, %q) = %v, want %v", c.value, c.loc, got, c.want)
		}
	}
	if !KnownGroup("in:us-west9-locations") || KnownGroup("in:nowhere-locations") {
		t.Error("KnownGroup misjudges a region's group or an unknown group")
	}
}

func TestMergeAndEvaluate(t *testing.T) {
	org := &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{vals([]string{"in:us-locations"}, nil)}}
	child := &orgpolicypb.PolicySpec{InheritFromParent: true, Rules: []*orgpolicypb.PolicySpec_PolicyRule{vals([]string{"europe-west1"}, []string{"us-east1"})}}
	eff := Merge(Merge(nil, org, true), child, true)
	for loc, want := range map[string]bool{"us-central1": true, "europe-west1": true, "us-east1": false, "asia-east1": false} {
		if got := ListAllows(eff, nil, loc, locationMatches); got != want {
			t.Errorf("merged ListAllows(%s) = %v, want %v", loc, got, want)
		}
	}
	if Merge(eff, &orgpolicypb.PolicySpec{Reset_: true}, true) != nil {
		t.Error("a reset did not restore the default")
	}
	if Merge(eff, &orgpolicypb.PolicySpec{}, true) != eff {
		t.Error("a spec without rules did not inherit")
	}
	replace := &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{vals([]string{"asia-east1"}, nil)}}
	if !ListAllows(Merge(eff, replace, true), nil, "asia-east1", locationMatches) || ListAllows(Merge(eff, replace, true), nil, "us-central1", locationMatches) {
		t.Error("a spec without inheritFromParent did not replace")
	}
	// A denyAll above gives way to the child's allowed values.
	deny := &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_DenyAll{DenyAll: true}}}}
	if !ListAllows(Merge(deny, child, true), nil, "europe-west1", locationMatches) {
		t.Error("an inheriting child's value under denyAll was refused")
	}

	tags := []iamcond.Tag{{NamespacedKey: "100000000001/env", ShortValue: "dev"}}
	b := &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
		{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}},
		{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: false}, Condition: &expr.Expr{Expression: "resource.matchTag('100000000001/env', 'dev')"}}}}
	if BoolEnforced(b, tags) || !BoolEnforced(b, nil) || BoolEnforced(nil, nil) {
		t.Error("BoolEnforced does not let a matching conditional rule decide first")
	}
}

func TestMembers(t *testing.T) {
	spec := &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{vals([]string{"is:" + Customer.ID}, nil)}}
	for m, want := range map[string]bool{
		"user:a@" + Customer.Domain:                  true,
		"group:g@" + Customer.Domain:                 true,
		"domain:" + Customer.Domain:                  true,
		"serviceAccount:x@p.iam.gserviceaccount.com": true,
		"user:a@example.com":                         false,
		"allUsers":                                   false,
		"allAuthenticatedUsers":                      false,
		"principalSet://goog/public:all":             false,
	} {
		if got := ListAllows(spec, nil, m, memberMatches); got != want {
			t.Errorf("member %s allowed = %v, want %v", m, got, want)
		}
	}
	org := &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
		vals([]string{"is:principalSet://cloudresourcemanager.googleapis.com/organizations/" + Customer.OrgNumber}, nil)}}
	if !ListAllows(org, nil, "user:a@"+Customer.Domain, memberMatches) || ListAllows(org, nil, "user:a@example.com", memberMatches) {
		t.Error("the organization's principal set does not stand for its customer")
	}
	if ProjectOf("projects/p/locations/l/keyRings/k") != "projects/p" || ProjectOf("projects/_/buckets/b") != "" {
		t.Error("ProjectOf")
	}
}
