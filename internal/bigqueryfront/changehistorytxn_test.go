package bigqueryfront

import (
	"path/filepath"
	"testing"
)

// TestCommittedTransactionsAreKept (#1503): a table's committed
// transactions are kept by their change time, across a restart, for the
// table made at that time only, those older than seven days let go; and
// CHANGES's range [start, stop) holds one or not.
func TestCommittedTransactionsAreKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tablesettings.json")
	var s tableSettings
	s.keep(path, nil)
	s.addTransaction("p.d.t", "100", 1)
	s.addTransaction("p.d.t", "100", 8*day)
	s.addTransaction("p.d.t", "100", 8*day+5)
	var again tableSettings
	again.keep(path, nil)
	got := again.get("p.d.t", "100")
	if len(got.Transactions) != 2 || got.Transactions[0] != 8*day || got.Transactions[1] != 8*day+5 {
		t.Fatalf("kept %v", got.Transactions)
	}
	if again.get("p.d.t", "200").zero() != true {
		t.Errorf("a table made again keeps the old one's transactions")
	}
	for _, c := range []struct {
		start, stop int64
		want        bool
	}{{0, 8 * day, false}, {0, 8*day + 1, true}, {8*day + 1, 8*day + 5, false}, {8*day + 5, 9 * day, true}} {
		if _, ok := committedIn(got, c.start, c.stop); ok != c.want {
			t.Errorf("[%d, %d): %v, want %v", c.start, c.stop, ok, c.want)
		}
	}
}
