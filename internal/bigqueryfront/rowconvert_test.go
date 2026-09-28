package bigqueryfront

import (
	"strings"
	"testing"
)

// TestConvertColumns (#1036, #1083): each column of the target is read
// from the source's of its name, a RECORD rebuilt by name in the target's
// order at any depth (a REPEATED one element by element, in order), a
// missing field NULL (a REPEATED one empty); an extra field or a missing
// REQUIRED one is invalid, another type or REPEATED mode not mapped.
func TestConvertColumns(t *testing.T) {
	rec := func(name, mode string, fields ...field) field {
		return field{Name: name, Type: "RECORD", Mode: mode, Fields: fields}
	}
	s := func(name string) field { return field{Name: name, Type: "STRING"} }
	for _, c := range []struct {
		name     string
		src, dst []field
		want     string
		same     bool
		invalid  bool
		problem  string
	}{
		{"the same", []field{s("a"), rec("r", "", s("x"))}, []field{s("a"), rec("r", "", s("x"))}, "`a`, `r`", true, false, ""},
		{"in another order", []field{s("b"), s("a")}, []field{s("a"), s("b")}, "`a`, `b`", false, false, ""},
		{"a field added at depth", []field{rec("r", "", s("x"), rec("n", "", s("z")))},
			[]field{rec("r", "", s("x"), rec("n", "", s("z"), field{Name: "w", Type: "BYTES"}), field{Name: "y", Type: "INTEGER", Mode: "REPEATED"})},
			"IF(`r` IS NULL, NULL, STRUCT(`r`.`x` AS `x`, IF(`r`.`n` IS NULL, NULL, STRUCT(`r`.`n`.`z` AS `z`, CAST(NULL AS BYTES) AS `w`)) AS `n`, " +
				"ARRAY<INT64>[] AS `y`))", false, false, ""},
		{"a REPEATED RECORD", []field{rec("rr", "REPEATED", s("x"))}, []field{rec("rr", "REPEATED", s("x"), field{Name: "y", Type: "FLOAT"})},
			"ARRAY(SELECT STRUCT(_cloudburrow_e0.`x` AS `x`, CAST(NULL AS FLOAT64) AS `y`) FROM UNNEST(`rr`) AS _cloudburrow_e0 " +
				"WITH OFFSET AS _cloudburrow_o0 ORDER BY _cloudburrow_o0)", false, false, ""},
		{"a new RECORD column", []field{s("a")}, []field{s("a"), rec("r", "", s("x"))}, "`a`, CAST(NULL AS STRUCT<`x` STRING>)", false, false, ""},
		{"an extra field", []field{rec("r", "", s("x"), s("q"))}, []field{rec("r", "", s("x"))}, "", false, true, "r.q is not in"},
		{"a missing REQUIRED", []field{s("b")}, []field{{Name: "a", Type: "STRING", Mode: "REQUIRED"}, s("b")}, "", false, true, "REQUIRED field a"},
		{"another type", []field{{Name: "a", Type: "INT64"}}, []field{{Name: "a", Type: "FLOAT"}}, "", false, false, "INT64 in the result and FLOAT"},
		{"REPEATED and not", []field{{Name: "a", Type: "STRING", Mode: "REPEATED"}}, []field{s("a")}, "", false, false, "REPEATED in the result"},
	} {
		exprs, same, p := convertColumns(c.src, c.dst, "")
		switch {
		case c.problem != "":
			if p == nil || p.invalid != c.invalid || !strings.Contains(p.msg, c.problem) {
				t.Errorf("%s: %+v, want a problem %q (invalid %v)", c.name, p, c.problem, c.invalid)
			}
		case p != nil:
			t.Errorf("%s: %+v", c.name, p)
		case strings.Join(exprs, ", ") != c.want || same != c.same:
			t.Errorf("%s:\n %s (same %v)\nwant %s (same %v)", c.name, strings.Join(exprs, ", "), same, c.want, c.same)
		}
	}
}

// TestNullViolations (#1083): the condition that finds a NULL where a
// REQUIRED column or field is, at any depth.
func TestNullViolations(t *testing.T) {
	fields := []field{{Name: "a", Type: "STRING", Mode: "REQUIRED"}, {Name: "b", Type: "STRING"},
		{Name: "r", Type: "RECORD", Fields: []field{{Name: "x", Type: "INTEGER", Mode: "REQUIRED"}}},
		{Name: "rr", Type: "RECORD", Mode: "REPEATED", Fields: []field{{Name: "y", Type: "INTEGER", Mode: "REQUIRED"}}}}
	want := "`a` IS NULL OR (`r` IS NOT NULL AND (`r`.`x` IS NULL)) OR EXISTS(SELECT 1 FROM UNNEST(`rr`) AS _cloudburrow_v0 " +
		"WHERE _cloudburrow_v0.`y` IS NULL)"
	if got := nullViolations(fields, "", 0); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := nullViolations(fields[1:2], "", 0); got != "" {
		t.Errorf("no REQUIRED field: %q", got)
	}
}
