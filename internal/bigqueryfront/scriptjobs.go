package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// A script's child jobs (#1353).
//
// BigQuery runs a multi-statement query as a parent job of statementType
// SCRIPT, "and each statement in the script creates a child job"
// (https://cloud.google.com/bigquery/docs/multi-statement-queries): the
// parent's statistics.numChildJobs counts them, each child's
// statistics.parentJobId names the parent and its scriptStatistics give
// its evaluationKind (STATEMENT, or EXPRESSION for one that evaluates an
// expression: a DECLARE's DEFAULT, a SET's value, a condition) and its
// stackFrames; jobs.list with parentJobId lists them, jobs.list without it
// leaves them out, and INFORMATION_SCHEMA.JOBS has them with
// parent_job_id (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#ScriptStatistics).
//
// The front carries out a script statement by statement (scriptflow.go),
// each a job of its own on the emulator (scriptRun.send), which were hidden
// as the front's own. They are its child jobs now: once the parent is
// answered (runScript), each is recorded as its child (adoptScript) and
// shown so in jobs.get, jobs.list and INFORMATION_SCHEMA.JOBS (patch). Each
// child is the emulator's job, with its own statementType, DDL or DML
// statistics and, for a query, destination table.
//
// The last statement of a script is the client's request (scriptflow.go):
// a query is run once more as a child of its own, which only reads. Since
// #1459 every script of
// several statements is carried out so (dml.go), one with a TEMP table or
// function as its session's text (tempsession.go); a last statement that
// changes data is a child job of its own, run once, its counts the
// parent's too; a query child is run as a query job, so that it names its
// destination table (sendStatement); and each child's statementType is
// its statement's (statementTypeOf), where the emulator answers SELECT
// for every statement but DML.

// scriptChild is one child job a script ran.
type scriptChild struct {
	project, id string
	kind        string // STATEMENT or EXPRESSION
	text        string
	line, col   int // where text starts in the script, 0 when unknown
	endLine     int
	endCol      int
}

// childInfo is what jobRecords keeps of a child job.
type childInfo struct {
	parent string
	scriptChild
}

// adoptScript records kids as the children of the job rec answers (a
// jobs.insert's Job or a jobs.query's QueryResponse), and writes rec to w,
// a Job with its numChildJobs and statementType SCRIPT.
func (j *jobRecords) adoptScript(w http.ResponseWriter, rec *recorder, kids []scriptChild, insert bool, project string) {
	var resp map[string]any
	if j == nil || len(kids) == 0 || rec.status != 0 && rec.status != http.StatusOK ||
		json.Unmarshal(rec.body.Bytes(), &resp) != nil {
		rec.copyTo(w)
		return
	}
	p, parent := jobRef(resp)
	if parent == "" {
		rec.copyTo(w)
		return
	}
	if p == "" {
		p = project
	}
	j.mu.Lock()
	if j.children == nil {
		j.children, j.parents = map[string]childInfo{}, map[string]int{}
	}
	n := 0
	for _, k := range kids {
		if k.id == "" || k.id == parent {
			continue // a jobs.query answered with the statement's own job
		}
		if k.project == "" {
			k.project = p
		}
		key := k.project + "/" + k.id
		delete(j.internal, key)
		if _, ok := j.children[key]; !ok {
			j.childOrder = append(j.childOrder, key)
		}
		j.children[key] = childInfo{parent: parent, scriptChild: k}
		n++
	}
	if n > 0 {
		j.parents[p+"/"+parent] = n
	}
	for len(j.childOrder) > maxJobRecords {
		delete(j.children, j.childOrder[0])
		j.childOrder = j.childOrder[1:]
	}
	j.mu.Unlock()
	if j.made != nil && n > 0 {
		j.made(p, parent)
		for _, k := range kids {
			if k.id != "" && k.id != parent {
				if k.project == "" {
					k.project = p
				}
				j.made(k.project, k.id)
			}
		}
	}
	if !insert || n == 0 || !j.patchScript(resp, p) {
		rec.copyTo(w)
		return
	}
	writeRecorded(w, rec, resp)
}

// patchScript gives job its place in a script, and reports whether it
// changed it.
func (j *jobRecords) patchScript(job map[string]any, project string) bool {
	if j == nil {
		return false
	}
	p, id := jobRef(job)
	if p == "" {
		p = project
	}
	j.mu.Lock()
	child, isChild := j.children[p+"/"+id]
	n, isParent := j.parents[p+"/"+id]
	j.mu.Unlock()
	if !isChild && !isParent {
		return false
	}
	stats, _ := job["statistics"].(map[string]any)
	if stats == nil {
		stats = map[string]any{}
		job["statistics"] = stats
	}
	if isParent {
		stats["numChildJobs"] = strconv.Itoa(n)
		qs, _ := stats["query"].(map[string]any)
		if qs == nil {
			qs = map[string]any{}
			stats["query"] = qs
		}
		qs["statementType"] = "SCRIPT"
	}
	if isChild {
		stats["parentJobId"] = child.parent
		frame := map[string]any{"text": child.text}
		if child.line > 0 {
			frame["startLine"], frame["startColumn"] = child.line, child.col
			frame["endLine"], frame["endColumn"] = child.endLine, child.endCol
		}
		stats["scriptStatistics"] = map[string]any{"evaluationKind": child.kind, "stackFrames": []any{frame}}
		if typ := statementTypeOf(child.text); typ != "" && child.kind == "STATEMENT" {
			// The emulator answers SELECT for every statement but DML
			// (#1459); BigQuery's child job has its statement's own.
			qs, _ := stats["query"].(map[string]any)
			if qs == nil {
				qs = map[string]any{}
				stats["query"] = qs
			}
			qs["statementType"] = typ
		}
	}
	return true
}

// statementTypeOf is the statementType BigQuery gives a job of the
// statement text (JobStatistics2.statementType,
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatistics2),
// or "" when it is not one read here (#1459).
func statementTypeOf(text string) string {
	toks, ok := lex(text)
	if !ok || len(toks) == 0 {
		return ""
	}
	at := func(i int, words ...string) bool {
		for k, w := range words {
			if i+k >= len(toks) || !toks[i+k].is(w) {
				return false
			}
		}
		return true
	}
	switch {
	case toks[0].is("SELECT") || toks[0].is("WITH") || toks[0].punct("("):
		return "SELECT"
	case toks[0].is("INSERT"), toks[0].is("UPDATE"), toks[0].is("DELETE"), toks[0].is("MERGE"), toks[0].is("ASSERT"):
		return strings.ToUpper(toks[0].text)
	case at(0, "TRUNCATE", "TABLE"):
		return "TRUNCATE_TABLE"
	case toks[0].is("CALL"):
		return "CALL"
	case toks[0].is("EXPORT") && at(1, "DATA"):
		return "EXPORT_DATA"
	case at(0, "LOAD", "DATA"):
		return "LOAD_DATA"
	}
	verb := strings.ToUpper(toks[0].text)
	if verb != "CREATE" && verb != "DROP" && verb != "ALTER" {
		return ""
	}
	i := 1
	if verb == "CREATE" && at(i, "OR", "REPLACE") {
		i += 2
	}
	if verb == "CREATE" && (at(i, "TEMP") || at(i, "TEMPORARY")) {
		i++
	}
	kinds := []struct {
		words []string
		typ   string
	}{
		{[]string{"MATERIALIZED", "VIEW"}, "MATERIALIZED_VIEW"},
		{[]string{"SNAPSHOT", "TABLE"}, "SNAPSHOT_TABLE"},
		{[]string{"EXTERNAL", "TABLE"}, "EXTERNAL_TABLE"},
		{[]string{"TABLE", "FUNCTION"}, "TABLE_FUNCTION"},
		{[]string{"AGGREGATE", "FUNCTION"}, "FUNCTION"},
		{[]string{"ROW", "ACCESS", "POLICY"}, "ROW_ACCESS_POLICY"},
		{[]string{"SEARCH", "INDEX"}, "SEARCH_INDEX"},
		{[]string{"TABLE"}, "TABLE"},
		{[]string{"VIEW"}, "VIEW"},
		{[]string{"FUNCTION"}, "FUNCTION"},
		{[]string{"PROCEDURE"}, "PROCEDURE"},
		{[]string{"SCHEMA"}, "SCHEMA"},
		{[]string{"MODEL"}, "MODEL"},
	}
	for _, k := range kinds {
		if !at(i, k.words...) {
			continue
		}
		typ := verb + "_" + k.typ
		if typ == "CREATE_TABLE" {
			// AS SELECT at the statement's top level makes it
			// CREATE_TABLE_AS_SELECT.
			depth := 0
			for _, t := range toks[i:] {
				switch {
				case t.punct("("):
					depth++
				case t.punct(")"):
					depth--
				case depth == 0 && (t.is("LIKE") || t.is("COPY") || t.is("CLONE")):
					return typ
				case depth == 0 && t.is("AS"):
					return "CREATE_TABLE_AS_SELECT"
				}
			}
		}
		return typ
	}
	return ""
}

// childAt places text in the script sql, when it is there as written.
func childAt(sql, text string) (line, col, endLine, endCol int) {
	i := strings.Index(sql, text)
	if i < 0 || text == "" {
		return 0, 0, 0, 0
	}
	line, col = lineColumn(sql, i)
	endLine, endCol = lineColumn(sql, i+len(text)-1)
	return line, col, endLine, endCol
}

// allJobLevels marks a jobs.list of the front's own that lists child jobs
// with the top-level ones (INFORMATION_SCHEMA.JOBS).
type allJobLevelsKey struct{}

func withAllJobLevels(ctx context.Context) context.Context {
	return context.WithValue(ctx, allJobLevelsKey{}, true)
}

func allJobLevels(r *http.Request) bool {
	v, _ := r.Context().Value(allJobLevelsKey{}).(bool)
	return v
}

// failedChild records a statement that failed as a child job of its own,
// failed with its error (#1201). BigQuery's failed statement is a child
// job like the others, with its errorResult and its scriptStatistics
// stackFrames, also when an EXCEPTION handler catches it; the emulator
// records no job for a jobs.query that fails, so the front makes one: a
// query job of a statement that does nothing, shown with the statement's
// text (jobText) and failed with its error (jobFailures, as failBeforeRun).
func (s *scriptRun) failedChild(text string, rec *recorder, msg string) {
	if s.children == nil || s.f.records == nil {
		return
	}
	reason := "invalidQuery"
	var e struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(rec.body.Bytes(), &e) == nil && len(e.Error.Errors) > 0 && e.Error.Errors[0].Reason != "" {
		reason = e.Error.Errors[0].Reason
	}
	legacy := false
	body, err := json.Marshal(map[string]any{"jobReference": map[string]any{"projectId": projectOf(s.f.base), "jobId": newJobID()},
		"configuration": map[string]any{"query": map[string]any{
			"query": "DROP TABLE IF EXISTS " + quotePath([]string{"_cloudburrow", scratchTable()}), "useLegacySql": &legacy}}})
	if err != nil {
		return
	}
	req, err := s.f.queryRequest(s.r, body)
	if err != nil {
		return
	}
	req.URL.Path, req.URL.RawPath = strings.TrimSuffix(req.URL.Path, "/queries")+"/jobs", ""
	out := newRecorder()
	s.f.forward(out, req, jobText{query: text})
	var job map[string]any
	if out.status != 0 && out.status != http.StatusOK || json.Unmarshal(out.body.Bytes(), &job) != nil {
		return
	}
	project, id := jobRef(job)
	if id == "" {
		return
	}
	if project == "" {
		project = projectOf(s.f.base)
	}
	s.f.failed.add(project, id, rowError{Reason: reason, Message: msg})
	s.f.records.markInternal(project, id)
	*s.children = append(*s.children, s.child(project, id, text))
}
