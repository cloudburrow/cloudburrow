package dataform

import (
	"testing"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// builtin_assertion_name_prefix (#2094) renames the assertions Dataform core
// generated from a table's assertions config, and the targets that depend
// on them, and leaves a hand-written assertion alone.
func TestBuiltinAssertionNamePrefix(t *testing.T) {
	g := &dfGraph{
		Tables: []dfAction{{Target: &dfTarget{Database: "p", Schema: "s", Name: "t"},
			DependencyTargets: []*dfTarget{{Database: "p", Schema: "s", Name: "s_t_assertions_rowConditions"}}}},
		Assertions: []dfAction{
			{Target: &dfTarget{Database: "p", Schema: "s", Name: "s_t_assertions_rowConditions"}, ParentAction: &dfTarget{Database: "p", Schema: "s", Name: "t"}},
			{Target: &dfTarget{Database: "p", Schema: "s", Name: "manual"}},
		},
	}
	g.prefixBuiltinAssertions("pre_")
	if got := g.Assertions[0].Target.Name; got != "pre_s_t_assertions_rowConditions" {
		t.Errorf("built-in assertion = %q", got)
	}
	if got := g.Assertions[1].Target.Name; got != "manual" {
		t.Errorf("hand-written assertion renamed to %q", got)
	}
	if got := g.Tables[0].DependencyTargets[0].Name; got != "pre_s_t_assertions_rowConditions" {
		t.Errorf("dependency = %q", got)
	}
	for _, bad := range []string{"has space", "a-b"} {
		err := checkCompilationConfig(&dfpb.CodeCompilationConfig{BuiltinAssertionNamePrefix: bad})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("prefix %q: %v, want InvalidArgument", bad, err)
		}
	}
	if err := checkCompilationConfig(&dfpb.CodeCompilationConfig{BuiltinAssertionNamePrefix: "ok_1"}); err != nil {
		t.Errorf("prefix ok_1: %v", err)
	}
}
