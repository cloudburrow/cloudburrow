package logging

import (
	"fmt"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
)

// A full store drops its oldest entries without allocating the whole slice
// again at each write (#1594), and still keeps the newest limit entries.
func TestFullStoreWritesInPlace(t *testing.T) {
	const limit = 1000
	s := NewStore(limit)
	for i := 0; i < limit+10; i++ {
		s.append([]*loggingpb.LogEntry{{InsertId: fmt.Sprint(i)}}, nil)
	}
	one := []*loggingpb.LogEntry{{InsertId: "next"}}
	if allocs := testing.AllocsPerRun(100, func() { s.append(one, nil) }); allocs > 0 {
		t.Errorf("a write to a full store made %.0f allocations, want none", allocs)
	}
	got := s.snapshot()
	if len(got) != limit {
		t.Fatalf("store holds %d entries, want %d", len(got), limit)
	}
	// AllocsPerRun writes once more than it measures: 1,010 + 101 writes.
	if got[0].GetInsertId() != "111" || got[limit-1].GetInsertId() != "next" {
		t.Errorf("store runs %s..%s, want 111..next", got[0].GetInsertId(), got[limit-1].GetInsertId())
	}
}
