//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	exprpb "google.golang.org/genproto/googleapis/type/expr"
)

// covers: google.iam.admin.v1.IAM/LintPolicy
//
// TestIAMLintPolicy (#1751): iamPolicies.lintPolicy through the official
// admin client over gRPC and google.golang.org/api/iam/v1 over REST, with
// every example on Google's "Validate allow policies" page
// (docs.cloud.google.com/iam/docs/linting-policies): each of the eleven
// validation units answers the page's unit name (LintValidationUnits/...),
// level CONDITION, field condition.expression, severity, location offset
// and message. A condition that does not compile is reported by
// ConditionCompileCheck with the parser's message, and every other unit
// NOTICE "The validation unit is skipped due to absence of a required
// object: CheckedExpr"; `1 < 2` has no results.
//
// HasOnlyListConstCheck's offset is CEL's position of the non-constant
// element (62, the "." of resource.name): Google's example gives 59, which
// is no node of its expression (docs/gaps.md; docs re-read 2026-11-17:
// https://docs.cloud.google.com/iam/docs/linting-policies).
func TestIAMLintPolicy(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	adm := iamAdmin(t, h)
	ctx := h.Context()
	lint := func(expr string) []*adminpb.LintResult {
		t.Helper()
		r, err := adm.LintPolicy(ctx, &adminpb.LintPolicyRequest{
			FullResourceName: "//cloudresourcemanager.googleapis.com/projects/lint-project",
			LintObject:       &adminpb.LintPolicyRequest_Condition{Condition: &exprpb.Expr{Title: "t", Expression: expr}},
		})
		if err != nil {
			t.Fatalf("LintPolicy %q: %v", expr, err)
		}
		return r.GetLintResults()
	}
	if got := lint("1 < 2"); len(got) != 0 {
		t.Errorf("1 < 2: %v, want no results", got)
	}
	for _, c := range []struct {
		expr, unit string
		sev        adminpb.LintResult_Severity
		offset     int32
		msg        string
	}{
		{"true=false", "ConditionCompileCheck", adminpb.LintResult_ERROR, 4, "ERROR: Parse expression:1:4: token recognition error at: '=f'\n  | true=false\n  | ....^"},
		{"1<", "ConditionCompileCheck", adminpb.LintResult_ERROR, 2, "ERROR: Parse expression:1:2: mismatched input '<EOF>' expecting {'[', '{', '(', '.', '-', '!', 'true', 'false', 'null', NUM_FLOAT, NUM_INT, NUM_UINT, STRING, BYTES, IDENTIFIER}\n  | 1<\n  | ..^"},
		{"1<2 || 2<3 || 3<4 || 4<5 || 5<6 || 6<7 || 7<8 || 8<9 || 9<10 || 10<11 || 11<12 || 12<13 || 13<14 || 14<15", "ConditionComplexityCheck", adminpb.LintResult_ERROR, 0, "Logical operators count must not be more than 12"},
		{"request.time < timestamp('2000-01-01T00:00:00Z')", "DateTimeCheck", adminpb.LintResult_WARNING, 25, `Ineffective date time value 2000-01-01T00:00:00+00:00 parsed from "2000-01-01T00:00:00Z"; condition is effectively False. Time expired already.`},
		{"request.time.getMonth() > 13", "DateTimeRangeCheck", adminpb.LintResult_WARNING, 26, "The value being compared to the specified timestamp function (getMonth) must be in range [0, 11]."},
		{"api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', ['roles/viewer']).hasOnly(['roles/editor'])", "DrgGetAttributeDefaultValueCheck", adminpb.LintResult_ERROR, 60, "api.getAttribute call on 'iam.googleapis.com/modifiedGrantsByRole' can only accept empty list ('[]') as default value."},
		{"request.time.getMonth() > 5 && request.time.getMonth() < 4", "EffectiveTimeRangeCheck", adminpb.LintResult_WARNING, 0, "The aggregate of time functions [getMonth] results in empty ranges."},
		{"api.getAttribute('somekey', []).hasOnly(['somevalue', resource.name])", "HasOnlyListConstCheck", adminpb.LintResult_ERROR, 62, "hasOnly() expects an argument of type list containing only const values, but a non-const expression was found in the list."},
		{"api.getAttribute('somekey', []).hasOnly([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11])", "HasOnlyListLengthCheck", adminpb.LintResult_ERROR, 39, "The list argument to hasOnly() cannot have more than 10 elements"},
		{"resource.service == 'resourcemanager'", "ResourceServiceLiteralCheck", adminpb.LintResult_WARNING, 20, "resource.service : resourcemanager is not supported. Using this value in condition may lead to unintended consequences. Check user guide at https://cloud.google.com/iam/docs/conditions-resource-attributes#resource_service_values for supported values for resource.service."},
		{"resource.type == 'resourcemanager.projects'", "ResourceTypeLiteralCheck", adminpb.LintResult_WARNING, 17, "resource.type : resourcemanager.projects is not supported. Using this value in condition may lead to unintended consequences. Check user guide at https://cloud.google.com/iam/docs/conditions-resource-attributes#resource_type_values for supported values for resource.type."},
		{"'accessPolicies/123/accesslevels/TRUSTED' in request.auth.access_levels", "RestrictedAttributesCheck", adminpb.LintResult_WARNING, 57, "Condition attribute `request.auth.access_levels` is restricted or unsupported. Please check https://cloud.google.com/iam/docs/conditions-overview for the full list of supported attributes"},
	} {
		got := lint(c.expr)
		if len(got) == 0 {
			t.Errorf("%s: no results", c.expr)
			continue
		}
		g := got[0]
		if g.GetValidationUnitName() != "LintValidationUnits/"+c.unit || g.GetLevel() != adminpb.LintResult_CONDITION || g.GetSeverity() != c.sev ||
			g.GetFieldName() != "condition.expression" || g.GetLocationOffset() != c.offset || g.GetDebugMessage() != c.msg {
			t.Errorf("%s:\n got %v\nwant %s %v %d %q", c.expr, g, c.unit, c.sev, c.offset, c.msg)
		}
		if c.unit == "ConditionCompileCheck" {
			if len(got) != 11 {
				t.Errorf("%s: %d results, want 11", c.expr, len(got))
			}
			for _, r := range got[1:] {
				if r.GetSeverity() != adminpb.LintResult_NOTICE || r.GetDebugMessage() != "The validation unit is skipped due to absence of a required object: CheckedExpr" {
					t.Errorf("%s: skipped unit %v", c.expr, r)
				}
			}
		} else if len(got) != 1 {
			t.Errorf("%s: %d results, want 1: %v", c.expr, len(got), got)
		}
	}

	// Over REST, as Google's page calls it.
	svc, err := iamv1.NewService(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(h.Endpoint(EnvResourceManager), "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.IamPolicies.LintPolicy(&iamv1.LintPolicyRequest{Condition: &iamv1.Expr{Title: "1_less_than_2", Expression: "1 <"}}).Do()
	if err != nil || len(r.LintResults) != 11 {
		t.Fatalf("LintPolicy over REST = %+v, %v", r, err)
	}
	if g := r.LintResults[0]; g.ValidationUnitName != "LintValidationUnits/ConditionCompileCheck" || g.Severity != "ERROR" || g.Level != "CONDITION" || g.LocationOffset != 3 ||
		!strings.HasPrefix(g.DebugMessage, "ERROR: Parse expression:1:3: mismatched input '<EOF>' expecting {") {
		t.Errorf("REST compile result %+v", g)
	}
	if g := r.LintResults[1]; g.ValidationUnitName != "LintValidationUnits/ConditionComplexityCheck" || g.Severity != "NOTICE" {
		t.Errorf("REST skipped result %+v", g)
	}
}
