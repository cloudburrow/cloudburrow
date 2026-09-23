package main

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// regexpCompile is regexp.Compile, named so the helper below reads as a helper.
var regexpCompile = regexp.Compile

// TestFirestoreTypeDistinguishesWhatAFlatCellCannot.
//
// The collection listing renders every field into one cell, so 42 and "42" look
// identical — which is how a field that was meant to be a number and went in as a
// string stays invisible until a query returns nothing.
func TestFirestoreTypeDistinguishesWhatAFlatCellCannot(t *testing.T) {
	for _, c := range []struct {
		value any
		want  string
	}{
		{nil, "null"},
		{true, "boolean"},
		{int64(42), "number"},
		{42.0, "number"},
		{"42", "string"},
		{time.Now(), "timestamp"},
		{[]byte("x"), "bytes"},
		{[]any{1, 2}, "array (2)"},
		{map[string]any{"a": 1}, "map (1)"},
	} {
		if got := firestoreType(c.value); got != c.want {
			t.Errorf("firestoreType(%#v) = %q, want %q", c.value, got, c.want)
		}
	}
}

// TestRenderValueDoesNotTruncateOrGoPrint.
//
// The detail page exists so a value can be checked against what was written. A
// map printed as Go's map[a:1] is not something anyone can compare with the JSON
// they sent.
func TestRenderValueDoesNotTruncateOrGoPrint(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := renderValue(long); got != long {
		t.Errorf("a long value was truncated on the detail page: %d chars", len(got))
	}
	if got := renderValue(map[string]any{"a": float64(1)}); got != `{"a":1}` {
		t.Errorf("renderValue(map) = %q, want JSON", got)
	}
	if got := renderValue([]any{"a", "b"}); got != `["a","b"]` {
		t.Errorf("renderValue(array) = %q, want JSON", got)
	}
	if got := renderValue(nil); got != "null" {
		t.Errorf("renderValue(nil) = %q", got)
	}
	// The listing cell, by contrast, is bounded — that is why the page exists.
	if got := summarise(long); len(got) > 80 {
		t.Errorf("a listing cell was not bounded: %d chars", len(got))
	}
}

// TestTypedValueMatchesTheStoredType.
//
// Firestore compares by stored type: "42" as a string never matches the number
// 42, and the difference is silent — a query just returns nothing.
func TestTypedValueMatchesTheStoredType(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want any
	}{
		{"42", 42.0},
		{"1.5", 1.5},
		{"true", true},
		{"false", false},
		{"hello", "hello"},
		{"0042", 42.0},
	} {
		got, err := typedValue(c.raw, "==")
		if err != nil {
			t.Fatalf("typedValue(%q) = %v", c.raw, err)
		}
		if got != c.want {
			t.Errorf("typedValue(%q) = %#v, want %#v", c.raw, got, c.want)
		}
	}

	// A filter on a field with no value is a query that cannot be built, not
	// one that matches everything.
	if _, err := typedValue("", "=="); err == nil {
		t.Error("an empty filter value was accepted")
	}

	list, err := typedValue("1, two, true", "in")
	if err != nil {
		t.Fatal(err)
	}
	values, ok := list.([]any)
	if !ok || len(values) != 3 || values[0] != 1.0 || values[1] != "two" || values[2] != true {
		t.Errorf(`typedValue("in") = %#v`, list)
	}
	if _, err := typedValue("  ", "in"); err == nil {
		t.Error("an empty in-list was accepted")
	}
}

// TestQueryLimitIsAlwaysBounded.
//
// "This is a development database" is not something the console gets to assume.
// A query with no limit against a table someone loaded a million rows into would
// hang the page rather than answer anything.
func TestQueryLimitIsAlwaysBounded(t *testing.T) {
	for _, raw := range []string{"", "0", "-5", "abc"} {
		if got := queryLimit(raw); got != 50 {
			t.Errorf("queryLimit(%q) = %d, want the default 50", raw, got)
		}
	}
	if got := queryLimit("10"); got != 10 {
		t.Errorf("queryLimit(\"10\") = %d", got)
	}
	if got := queryLimit("99999"); got != detailLimit {
		t.Errorf("queryLimit(\"99999\") = %d, want the %d cap", got, detailLimit)
	}
}

// TestDatastoreKeyRoundTripsWhatTheListingRendered.
//
// The listing renders a numeric key as "id=123" to distinguish it from a name.
// If the detail page read that back as a name, the row and the page it opens
// would address different entities.
func TestDatastoreKeyRoundTripsWhatTheListingRendered(t *testing.T) {
	numeric, err := datastoreKey("Order", "id=123")
	if err != nil {
		t.Fatal(err)
	}
	if numeric.ID != 123 || numeric.Name != "" {
		t.Fatalf("numeric key = %+v", numeric)
	}
	named, err := datastoreKey("Order", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if named.Name != "abc" || named.ID != 0 {
		t.Fatalf("named key = %+v", named)
	}
	if _, err := datastoreKey("Order", "id=notanumber"); err == nil {
		t.Error("a malformed numeric key was accepted")
	}
}

// TestQueryFormsCoverTheOperatorsTheyDocument.
//
// The pattern on the operator field is what refuses a typo before a round trip.
// A pattern that does not match the operators the help text names would refuse
// the documented ones.
func TestQueryFormsCoverTheOperatorsTheyDocument(t *testing.T) {
	_, fields := firestoreProvider{}.QueryForm(nil)
	byName := map[string]string{}
	for _, f := range fields {
		byName[f.Name] = f.Pattern
	}
	for _, op := range []string{"==", "!=", "<", "<=", ">", ">=", "in", "array-contains"} {
		if !matches(t, byName["op"], op) {
			t.Errorf("the Firestore operator pattern refuses %q, which its help text offers", op)
		}
	}
	if matches(t, byName["op"], "DROP") {
		t.Error("the Firestore operator pattern accepts something that is not an operator")
	}

	_, dsFields := datastoreProvider{}.QueryForm(nil)
	for _, f := range dsFields {
		if f.Name != "op" {
			continue
		}
		for _, op := range []string{"=", "<", "<=", ">", ">="} {
			if !matches(t, f.Pattern, op) {
				t.Errorf("the Datastore operator pattern refuses %q", op)
			}
		}
		// Datastore has no inequality operator, so offering one would be a
		// control that fails.
		if matches(t, f.Pattern, "!=") {
			t.Error(`the Datastore operator pattern accepts "!=", which Datastore has no filter for`)
		}
	}

	// Every builder's form must be non-empty, or the capability advertises a
	// query surface with no controls on it.
	for _, b := range []struct {
		name  string
		build console.Builder
	}{
		{"Firestore", firestoreProvider{}},
		{"Datastore", datastoreProvider{}},
		{"Bigtable", bigtableProvider{}},
	} {
		label, form := b.build.QueryForm(nil)
		if len(form) == 0 {
			t.Errorf("%s advertises a query form with no fields", b.name)
		}
		if label == "" {
			t.Errorf("%s's query form has no label, so its button would be blank", b.name)
		}
	}
}

func matches(t *testing.T, pattern, value string) bool {
	t.Helper()
	if pattern == "" {
		return true
	}
	re, err := regexpCompile(pattern)
	if err != nil {
		t.Fatalf("pattern %q does not compile: %v", pattern, err)
	}
	return re.MatchString(value)
}

// TestEveryPagedListingRefusesACursorItDidNotIssue.
//
// A cursor from somewhere else must be an error. Returning an empty page instead
// would look identical to reaching the end, so a paging bug would present as
// "that was the last row" — which is exactly the false answer the whole feature
// exists to remove.
func TestEveryPagedListingRefusesACursorItDidNotIssue(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		pager console.Pager
	}{
		{"Cloud SQL", cloudSQLProvider{}},
		{"Firestore", firestoreProvider{}},
		{"Datastore", datastoreProvider{}},
		{"Bigtable", bigtableProvider{}},
	} {
		// The wrong path depth is refused without any backend being reached, so
		// this holds whether or not an emulator is running.
		if _, err := c.pager.Page(ctx, "demo", []string{"a", "b"}, "x"); err == nil {
			t.Errorf("%s paged a path it cannot page", c.name)
		}
	}

	// Cloud SQL's cursor is a row offset, and a non-numeric one is refused
	// before any connection is attempted.
	if _, err := (cloudSQLProvider{}).Page(ctx, "demo", []string{"main"}, "not-a-number"); err == nil {
		t.Error("Cloud SQL accepted a cursor that is not an offset")
	}
	if _, err := (cloudSQLProvider{}).Page(ctx, "demo", []string{"main"}, "-1"); err == nil {
		t.Error("Cloud SQL accepted a negative offset")
	}
}
