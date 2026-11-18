package bigqueryfront

import (
	"reflect"
	"testing"
)

// TestNormalizedLiteralsHash (#1458): queries alike but for literals,
// parameters, comments, spacing and keywords' case hash the same; another
// column or table does not.
func TestNormalizedLiteralsHash(t *testing.T) {
	a := normalizedLiteralsHash("SELECT n FROM d.t WHERE n > 1 AND s = 'x' -- one")
	for _, q := range []string{
		"select n  FROM d.t\nWHERE n > 2 AND s = @s",
		"SELECT n FROM d.t /* two */ WHERE n > 3.5 AND s = ?",
		"SELECT n FROM d.t WHERE n > NULL AND s = \"y\"",
	} {
		if got := normalizedLiteralsHash(q); got != a {
			t.Errorf("%q: %s, want %s", q, got, a)
		}
	}
	for _, q := range []string{"SELECT m FROM d.t WHERE n > 1 AND s = 'x'", "SELECT n FROM d.u WHERE n > 1 AND s = 'x'"} {
		if normalizedLiteralsHash(q) == a {
			t.Errorf("%q hashes as the first query", q)
		}
	}
	if len(a) != 64 {
		t.Errorf("hash %q: want 64 hex digits", a)
	}
}

// TestPhysicalBytes (#1458): the estimates of the physical byte columns.
func TestPhysicalBytes(t *testing.T) {
	if got, want := physicalBytes(storageSizes{"27", "27", "0", "NULL", "26", "0"}), []string{"27", "53", "53", "0", "26", "0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("new table: %v, want %v", got, want)
	}
	if got, want := physicalBytes(storageSizes{"27", "NULL", "NULL", "NULL", "NULL", "13"}), []string{"27", "NULL", "NULL", "NULL", "NULL", "13"}; !reflect.DeepEqual(got, want) {
		t.Errorf("old table without a change table: %v, want %v", got, want)
	}
	if got, want := physicalBytes(storageSizes{"NULL", "NULL", "NULL", "NULL", "NULL", "5"}), []string{"NULL", "NULL", "NULL", "NULL", "NULL", "NULL"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unknown: %v, want %v", got, want)
	}
}
