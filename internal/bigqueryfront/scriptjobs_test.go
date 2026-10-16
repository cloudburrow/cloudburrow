package bigqueryfront

import (
	"strings"
	"testing"
)

// TestFailedStatementIsAChildJob (#1201): a statement of a script that
// fails, here one an EXCEPTION handler catches, is made a job of its own,
// a query job of a statement that does nothing, so it can be the script's
// failed child job; the handler still runs and answers.
func TestFailedStatementIsAChildJob(t *testing.T) {
	sql := "BEGIN SELECT * FROM nope.nope; EXCEPTION WHEN ERROR THEN SELECT 2 AS n; END"
	emu := &scriptEmulator{run: func(q string) (int, string) {
		if strings.Contains(q, "FROM nope.nope") {
			return 400, `{"error":{"code":400,"message":"Table not found: nope.nope","errors":[{"reason":"notFound"}]}}`
		}
		return 200, `{"jobComplete":true}`
	}}
	code, got := do(t, Wrap(emu), "POST", base+"/jobs", queryBody("/jobs", sql))
	if code != 200 {
		t.Fatalf("jobs.insert: %d %v", code, got)
	}
	made := false
	for i, l := range emu.log {
		if strings.Contains(l, "FROM nope.nope") && i+1 < len(emu.log) &&
			strings.HasPrefix(emu.log[i+1], "QUERY DROP TABLE IF EXISTS `_cloudburrow.") {
			made = true
		}
	}
	if !made {
		t.Errorf("no job was made for the failed statement: %q", emu.log)
	}
	if last := emu.log[len(emu.log)-1]; last != "QUERY SELECT 2 AS n" {
		t.Errorf("the handler's query was not the answer: %q", emu.log)
	}
}
