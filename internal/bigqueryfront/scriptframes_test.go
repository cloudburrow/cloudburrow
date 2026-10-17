package bigqueryfront

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestScriptChildFrames (#1569): every child job of a script carries
// scriptStatistics' stackFrames, as BigQuery documents them: the 1-based
// lines and columns of its statement or expression in the script, with its
// text as written, the first, a middle and the last statement alike, a
// statement that reads a variable and one written twice, inside BEGIN, IF,
// an EXCEPTION handler, a loop and a transaction; a statement of a CALLed
// procedure has its frame in the procedure's body, with procedureId, then
// the CALL's. Measured first: a statement that reads a variable, the last
// one too, had no position.
func TestScriptChildFrames(t *testing.T) {
	expr := regexp.MustCompile(`^SELECT CAST\(\((.*)\) AS (\w+)\)$`)
	jobs := &jobsEmulator{}
	jobs.run = func(q string) (int, string) {
		if strings.Contains(q, probeSentinel) {
			// A probe of a statement in a transaction (transactions.go).
			return 400, `{"error":{"code":400,"message":"` + probeSentinel + `","errors":[{"reason":"invalidQuery"}]}}`
		}
		if strings.Contains(q, "nope") {
			return 400, `{"error":{"code":400,"message":"Table not found: nope.nope","errors":[{"reason":"invalidQuery"}]}}`
		}
		if m := expr.FindStringSubmatch(q); m != nil {
			typ, v := "INTEGER", "2"
			if m[2] == "BOOL" {
				typ, v = "BOOLEAN", "true"
			}
			return 200, `{"jobComplete":true,"totalRows":"1","schema":{"fields":[{"name":"f0_","type":"` + typ + `"}]},` +
				`"rows":[{"f":[{"v":"` + v + `"}]}]}`
		}
		return 200, `{"jobComplete":true}`
	}
	h := Wrap(jobs)
	script := "CREATE PROCEDURE ds.p(y INT64)\nBEGIN\n  INSERT INTO ds.a VALUES (y);\nEND;\n" + // 1-4
		"DECLARE x INT64 DEFAULT 2;\n" + // 5
		"CREATE TABLE ds.a (v INT64);\n" + // 6
		"BEGIN\n  INSERT INTO ds.a VALUES (x);\n" + // 7-8
		"  IF x > 1 THEN\n    SELECT * FROM nope.nope;\n  END IF;\n" + // 9-11
		"EXCEPTION WHEN ERROR THEN\n  INSERT INTO ds.a VALUES (x + 1);\nEND;\n" + // 12-14
		"CALL ds.p(x);\n" + // 15
		"INSERT INTO ds.a VALUES (x);\n" + // 16, as line 8
		"INSERT INTO ds.a VALUES (x * 3)" // 17, the last
	if code, got := do(t, h, "POST", base+"/jobs", queryBody("/jobs", script)); code != 200 {
		t.Fatalf("%d %v", code, got)
	}
	want := []string{
		`EXPRESSION 5:25-5:25 "2"`,
		`STATEMENT 6:1-6:27 "CREATE TABLE ds.a (v INT64)"`,
		`STATEMENT 8:3-8:29 "INSERT INTO ds.a VALUES (x)"`,
		`EXPRESSION 9:6-9:10 "x > 1"`,
		`STATEMENT 10:5-10:27 "SELECT * FROM nope.nope"`,
		`STATEMENT 13:3-13:33 "INSERT INTO ds.a VALUES (x + 1)"`,
		`EXPRESSION 15:11-15:11 "x"`,
		`STATEMENT 2:3-2:29 "INSERT INTO ds.a VALUES (y)" in p.ds.p < 15:1-15:12 "CALL ds.p(x)"`,
		`STATEMENT 16:1-16:27 "INSERT INTO ds.a VALUES (x)"`,
		`STATEMENT 17:1-17:31 "INSERT INTO ds.a VALUES (x * 3)"`,
	}
	if got := childFrames(t, h, "j1"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the children's frames:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A loop, a transaction and a last query.
	script = "LOOP\n  INSERT INTO ds.a VALUES (1);\n  BREAK;\nEND LOOP;\n" + // 1-4
		"BEGIN TRANSACTION;\n  INSERT INTO ds.a VALUES (2);\nCOMMIT TRANSACTION;\n" + // 5-7
		"SELECT 1 AS one" // 8
	if code, got := do(t, h, "POST", base+"/jobs", strings.Replace(queryBody("/jobs", script), `"j1"`, `"j2"`, 1)); code != 200 {
		t.Fatalf("%d %v", code, got)
	}
	want = []string{
		`STATEMENT 2:3-2:29 "INSERT INTO ds.a VALUES (1)"`,
		`STATEMENT 6:3-6:29 "INSERT INTO ds.a VALUES (2)"`,
		`STATEMENT 7:1-7:18 "COMMIT TRANSACTION"`,
		`STATEMENT 8:1-8:15 "SELECT 1 AS one"`,
	}
	if got := childFrames(t, h, "j2"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the children's frames:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// childFrames are the evaluation kind and stack frames of job's children,
// in the order they ran.
func childFrames(t *testing.T, h http.Handler, job string) []string {
	t.Helper()
	_, list := do(t, h, "GET", base+"/jobs?parentJobId="+job, "")
	var frames []string
	for _, j := range list["jobs"].([]any) {
		ss, _ := j.(map[string]any)["statistics"].(map[string]any)["scriptStatistics"].(map[string]any)
		var fs []string
		for _, f := range ss["stackFrames"].([]any) {
			f := f.(map[string]any)
			s := fmt.Sprintf("%v:%v-%v:%v %q", f["startLine"], f["startColumn"], f["endLine"], f["endColumn"], f["text"])
			if p, ok := f["procedureId"]; ok {
				s += " in " + p.(string)
			}
			fs = append(fs, s)
		}
		frames = append(frames, ss["evaluationKind"].(string)+" "+strings.Join(fs, " < "))
	}
	// jobs.list lists the newest first.
	for i, k := 0, len(frames)-1; i < k; i, k = i+1, k-1 {
		frames[i], frames[k] = frames[k], frames[i]
	}
	return frames
}
