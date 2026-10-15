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
// a query is run once more as a child of its own, which only reads; a
// last statement that changes data is not run twice, so it has no child
// job, and its counts are the parent's. A script with no control flow and
// no earlier query is sent whole to the emulator, which runs it as one job:
// it makes no child jobs (documented, docs/compatibility.md), because
// running it a statement at a time would end a TEMP table or function with
// the statement that made it.

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
	}
	return true
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
