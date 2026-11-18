package bigqueryfront

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// A job naming a table of a shut-down project (#1774). refuseShutDown
// answers a request whose path names a DELETE_REQUESTED project 403
// (#1767); a job of another project that names such a project's table, in
// its query (`p.dataset.table` after FROM, JOIN, INTO, UPDATE, MERGE,
// USING, TABLE, CLONE or a comma) or in its configuration (a query's or a
// load's destination, a copy's sources and destination, an extract's
// source), is refused the same way, nothing run: the shut-down project's
// "resources ... are shut down"
// (https://cloud.google.com/resource-manager/docs/creating-managing-projects#shutting_down_projects).

// jobsInsertPath is jobs.insert's and jobs.query's (and a media upload's).
var jobsInsertPath = regexp.MustCompile(`^(?:/upload)?(?:/bigquery/v2)?/projects/[^/]+/(?:jobs|queries)/?$`)

// tableKeywords are the words a table's path follows in a statement.
var tableKeywords = []string{"FROM", "JOIN", "INTO", "UPDATE", "MERGE", "USING", "TABLE", "CLONE"}

// sqlProjects are the projects of the three-part table paths sql names.
func sqlProjects(sql string) []string {
	t, ok := lex(sql)
	if !ok {
		return nil
	}
	var out []string
	for i := 1; i < len(t); i++ {
		prev := t[i-1]
		after := prev.punct(",")
		for _, k := range tableKeywords {
			after = after || prev.is(k)
		}
		if !after || (t[i].kind != tokWord && t[i].kind != tokQuoted) {
			continue
		}
		if p, _ := path(t, i); len(p) >= 3 && p[0] != "" {
			out = append(out, p[0])
		}
	}
	return out
}

// jobProjects are the projects other than project whose tables the job in
// r's body names (above), each once, sorted.
func jobProjects(r *http.Request, project string) []string {
	var job struct {
		Configuration struct {
			Query *struct {
				Query            string    `json:"query"`
				DestinationTable *tableRef `json:"destinationTable"`
			} `json:"query"`
			Load *struct {
				DestinationTable *tableRef `json:"destinationTable"`
			} `json:"load"`
			Copy *struct {
				SourceTable      *tableRef  `json:"sourceTable"`
				SourceTables     []tableRef `json:"sourceTables"`
				DestinationTable *tableRef  `json:"destinationTable"`
			} `json:"copy"`
			Extract *struct {
				SourceTable *tableRef `json:"sourceTable"`
			} `json:"extract"`
		} `json:"configuration"`
		Query string `json:"query"` // jobs.query's QueryRequest
	}
	if !decodeJob(r, &job) {
		return nil
	}
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && p != project {
			seen[p] = true
		}
	}
	addRef := func(t *tableRef) {
		if t != nil {
			add(t.ProjectID)
		}
	}
	c := job.Configuration
	sql := job.Query
	if q := c.Query; q != nil {
		sql = q.Query
		addRef(q.DestinationTable)
	}
	if l := c.Load; l != nil {
		addRef(l.DestinationTable)
	}
	if cp := c.Copy; cp != nil {
		addRef(cp.SourceTable)
		for i := range cp.SourceTables {
			addRef(&cp.SourceTables[i])
		}
		addRef(cp.DestinationTable)
	}
	if e := c.Extract; e != nil {
		addRef(e.SourceTable)
	}
	for _, p := range sqlProjects(sql) {
		add(p)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// refuseShutDownJob answers r 403 when it is a job of project that names a
// table of a DELETE_REQUESTED project (above), reporting true.
func (k *knownProjects) refuseShutDownJob(w http.ResponseWriter, r *http.Request, project string) bool {
	if k == nil || k.rm == "" || r.Method != http.MethodPost || !jobsInsertPath.MatchString(r.URL.Path) {
		return false
	}
	for _, p := range jobProjects(r, project) {
		if strings.HasPrefix(strings.ToLower(p), "region-") {
			continue
		}
		k.isActive(r.Context(), p)
		if k.refuseShutDown(w, p) {
			return true
		}
	}
	return false
}
