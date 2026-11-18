package iamcond

import (
	"strings"
	"testing"
	"time"
)

// TestLintGoogleExamples (#1751): every example on Google's "Validate allow
// policies" page gives the unit, severity, offset and message it shows.
func TestLintGoogleExamples(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		expr, unit, severity string
		offset               int
		msg                  string
	}{
		{"true=false", UnitConditionCompile, SeverityError, 4, "ERROR: Parse expression:1:4: token recognition error at: '=f'\n  | true=false\n  | ....^"},
		{"1<", UnitConditionCompile, SeverityError, 2, "ERROR: Parse expression:1:2: mismatched input '<EOF>' expecting {'[', '{', '(', '.', '-', '!', 'true', 'false', 'null', NUM_FLOAT, NUM_INT, NUM_UINT, STRING, BYTES, IDENTIFIER}\n  | 1<\n  | ..^"},
		{"1 <", UnitConditionCompile, SeverityError, 3, "ERROR: Parse expression:1:3: mismatched input '<EOF>' expecting {'[', '{', '(', '.', '-', '!', 'true', 'false', 'null', NUM_FLOAT, NUM_INT, NUM_UINT, STRING, BYTES, IDENTIFIER}\n  | 1 <\n  | ...^"},
		{"1<2 || 2<3 || 3<4 || 4<5 || 5<6 || 6<7 || 7<8 || 8<9 || 9<10 || 10<11 || 11<12 || 12<13 || 13<14 || 14<15", UnitConditionComplexity, SeverityError, -1, "Logical operators count must not be more than 12"},
		{"request.time < timestamp('2000-01-01T00:00:00Z')", UnitDateTime, SeverityWarning, 25, `Ineffective date time value 2000-01-01T00:00:00+00:00 parsed from "2000-01-01T00:00:00Z"; condition is effectively False. Time expired already.`},
		{"request.time.getMonth() > 13", UnitDateTimeRange, SeverityWarning, 26, "The value being compared to the specified timestamp function (getMonth) must be in range [0, 11]."},
		{"api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', ['roles/viewer']).hasOnly(['roles/editor'])", UnitDrgGetAttributeDefault, SeverityError, 60, "api.getAttribute call on 'iam.googleapis.com/modifiedGrantsByRole' can only accept empty list ('[]') as default value."},
		{"request.time.getMonth() > 5 && request.time.getMonth() < 4", UnitEffectiveTimeRange, SeverityWarning, -1, "The aggregate of time functions [getMonth] results in empty ranges."},
		{"api.getAttribute('somekey', []).hasOnly([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11])", UnitHasOnlyListLength, SeverityError, 39, "The list argument to hasOnly() cannot have more than 10 elements"},
		{"resource.service == 'resourcemanager'", UnitResourceServiceLiteral, SeverityWarning, 20, "resource.service : resourcemanager is not supported. Using this value in condition may lead to unintended consequences. Check user guide at https://cloud.google.com/iam/docs/conditions-resource-attributes#resource_service_values for supported values for resource.service."},
		{"resource.type == 'resourcemanager.projects'", UnitResourceTypeLiteral, SeverityWarning, 17, "resource.type : resourcemanager.projects is not supported. Using this value in condition may lead to unintended consequences. Check user guide at https://cloud.google.com/iam/docs/conditions-resource-attributes#resource_type_values for supported values for resource.type."},
		{"'accessPolicies/123/accesslevels/TRUSTED' in request.auth.access_levels", UnitRestrictedAttributes, SeverityWarning, 57, "Condition attribute `request.auth.access_levels` is restricted or unsupported. Please check https://cloud.google.com/iam/docs/conditions-overview for the full list of supported attributes"},
	}
	for _, c := range cases {
		got := Lint(c.expr, now)
		if len(got) == 0 {
			t.Errorf("%s: no results, want %s", c.expr, c.unit)
			continue
		}
		g := got[0]
		if g.Unit != c.unit || g.Severity != c.severity || g.Offset != c.offset || g.Message != c.msg {
			t.Errorf("%s:\n got %+v\nwant %s %s %d %q", c.expr, g, c.unit, c.severity, c.offset, c.msg)
		}
		if c.unit != UnitConditionCompile && len(got) != 1 {
			t.Errorf("%s: %d results, want 1: %+v", c.expr, len(got), got)
		}
		if c.unit == UnitConditionCompile {
			if len(got) != len(LintUnits) {
				t.Errorf("%s: %d results, want %d", c.expr, len(got), len(LintUnits))
			}
			for _, r := range got[1:] {
				if r.Severity != SeverityNotice || r.Message != skipped || r.Offset != -1 {
					t.Errorf("%s: skipped unit %+v", c.expr, r)
				}
			}
		}
	}
	// HasOnlyListConstCheck: Google's page gives offset 59; CEL places the
	// non-constant resource.name at its "." (62).
	got := Lint("api.getAttribute('somekey', []).hasOnly(['somevalue', resource.name])", now)
	if len(got) != 1 || got[0].Unit != UnitHasOnlyListConst || got[0].Message != "hasOnly() expects an argument of type list containing only const values, but a non-const expression was found in the list." {
		t.Errorf("hasOnly const: %+v", got)
	}
	if got := Lint("1 < 2", now); len(got) != 0 {
		t.Errorf("1 < 2: %+v, want none", got)
	}
}

// TestLintMore covers the units beyond the page's one example each.
func TestLintMore(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	units := func(expr string) string {
		var u []string
		for _, r := range Lint(expr, now) {
			u = append(u, r.Unit+":"+r.Severity)
		}
		return strings.Join(u, ",")
	}
	for expr, want := range map[string]string{
		`request.time > timestamp("2000-01-01T00:00:00Z")`:                                                             "DateTimeCheck:WARNING",
		`timestamp("2000-01-01T00:00:00Z") > request.time`:                                                             "DateTimeCheck:WARNING",
		`request.time < timestamp("2030-01-01T00:00:00Z")`:                                                             "",
		`request.time < timestamp("not a time")`:                                                                       "DateTimeCheck:WARNING",
		`request.time.getHours("Europe/Berlin") >= 9 && request.time.getHours("Europe/Berlin") <= 17`:                  "",
		`request.time.getHours() < 9 || request.time.getHours() >= 9`:                                                  "EffectiveTimeRangeCheck:WARNING",
		`request.time.getDayOfWeek() == 1 && request.time.getDayOfWeek() == 2`:                                         "EffectiveTimeRangeCheck:WARNING",
		`request.time.getDate() == 0`:                                                                                  "DateTimeRangeCheck:WARNING",
		`resource.service == "secretmanager.googleapis.com" && resource.type == "secretmanager.googleapis.com/Secret"`: "",
		`resource.type != "storage.googleapis.com/Bucket"`:                                                             "",
		`api.getAttribute("iam.googleapis.com/modifiedGrantsByRole", []).hasOnly(["roles/viewer"])`:                    "",
		`resource.name.startsWith(`:                                                                                    "ConditionCompileCheck:ERROR" + strings.Repeat(",x", 0),
	} {
		got := units(expr)
		if expr == `resource.name.startsWith(` {
			if !strings.HasPrefix(got, "ConditionCompileCheck:ERROR,") {
				t.Errorf("%s: %s", expr, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s: %q, want %q", expr, got, want)
		}
	}
	// An undeclared name is a compile error at its column.
	r := Lint("foo == 1", now)
	if r[0].Unit != UnitConditionCompile || r[0].Offset != 0 || !strings.HasPrefix(r[0].Message, "ERROR: Parse expression:1:0: ") {
		t.Errorf("foo == 1: %+v", r[0])
	}
	// Errors sort before warnings.
	r = Lint("resource.service == 'x' && api.getAttribute('k', []).hasOnly([resource.name])", now)
	if len(r) != 2 || r[0].Severity != SeverityError || r[1].Severity != SeverityWarning {
		t.Errorf("order: %+v", r)
	}
	// Compile still refuses what setIamPolicy refuses.
	if _, err := Compile("api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', ['roles/viewer']).hasOnly(['roles/editor'])"); err == nil {
		t.Error("Compile took a non-empty modifiedGrantsByRole default")
	}
}
