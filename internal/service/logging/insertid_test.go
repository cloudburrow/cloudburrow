package logging

import (
	"sort"
	"testing"
)

// TestGeneratedInsertIDsSortInWriteOrder: the entries of one write share a
// timestamp and list in insertId order, so the generated IDs sort as
// strings in the order they were made, across a change in the number of
// base-36 digits (the "cbzz" before "cb100" a page walk once listed last).
func TestGeneratedInsertIDsSortInWriteOrder(t *testing.T) {
	st := &Store{seq: 36*36 - 3}
	var ids []string
	for range 6 {
		ids = append(ids, st.nextInsertID())
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("generated insertIds %v do not sort in write order", ids)
	}
	if st := (&Store{seq: ^uint64(0) - 1}); len(st.nextInsertID()) != len(ids[0]) {
		t.Fatalf("the largest sequence's insertId is not the fixed width")
	}
}
