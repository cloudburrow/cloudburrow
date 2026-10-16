package bigqueryfront

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A project's default_location (#1375).
//
// BigQuery sets it with
//
//	ALTER PROJECT [PROJECT_ID] SET OPTIONS (`default_location` = 'LOCATION');
//
// a global setting, written without a `region-REGION.` prefix; NULL clears
// it, and the project's default is then the organization's, else US
// (https://cloud.google.com/bigquery/docs/default-configuration). It is
// "the location used to run jobs when the job's location isn't set or
// can't be determined" (an explicit location, a referenced dataset's or a
// destination table's come first), and a dataset made without a location
// is in the default location (https://cloud.google.com/bigquery/docs/datasets).
// The front keeps each project's in a file on its state directory, as it
// keeps the IAM policies (iam.go), dropped when the emulator restarts
// empty; and uses it for datasets.insert, CREATE SCHEMA and every job that
// names no location (location.go). It takes effect at once (BigQuery's
// "can take up to 10 minutes").
//
// Refused 501, nothing changed: ALTER ORGANIZATION (an instance has no
// organization); any other configuration (default_time_zone, the
// `region-REGION.` ones, ...), as the front would only record them; and
// ALTER PROJECT inside a script of several statements.
//
// `region-REGION`.INFORMATION_SCHEMA.PROJECT_OPTIONS has a row per option
// set on the project ("that differs from the default value"), and
// EFFECTIVE_PROJECT_OPTIONS the same rows with option_set_level PROJECTS
// and option_set_on_id the project; whether BigQuery also gives a DEFAULT
// row for each option left unset is not documented, so neither view does.
// option_description is NULL (Google documents none of its texts) and
// project_number NULL (an instance has none).

// projectOptions are the options ALTER PROJECT set, by project and name.
type projectOptions struct {
	mu   sync.Mutex
	opts map[string]map[string]string
	path string
	logf func(string, ...any)
}

// projectOptionsStateFile is the file the options are kept in, in dir.
func projectOptionsStateFile(dir string) string { return filepath.Join(dir, "projectoptions.json") }

// keep restores the options from path, and from then on keeps them there.
func (p *projectOptions) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved map[string]map[string]string
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the project options it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the project options it kept", path)
		saved = nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts, p.path, p.logf = saved, path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	p.saveLocked()
}

func (p *projectOptions) saveLocked() {
	if p.path == "" {
		return
	}
	b, err := json.Marshal(p.opts)
	if err == nil {
		err = os.WriteFile(p.path, b, 0o600)
	}
	if err != nil && p.logf != nil {
		p.logf("bigquery front: keep the project options: %v", err)
	}
}

// reset drops every option (the emulator restarted, empty).
func (p *projectOptions) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts = nil
	p.saveLocked()
}

// set sets project's option name to value, or clears it when value is nil.
func (p *projectOptions) set(project, name string, value *string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if value == nil {
		delete(p.opts[project], name)
	} else {
		if p.opts == nil {
			p.opts = map[string]map[string]string{}
		}
		if p.opts[project] == nil {
			p.opts[project] = map[string]string{}
		}
		p.opts[project][name] = *value
	}
	p.saveLocked()
}

// list returns project's options, sorted by name.
func (p *projectOptions) list(project string) [][2]string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out [][2]string
	for k, v := range p.opts[project] {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(a, b int) bool { return out[a][0] < out[b][0] })
	return out
}

// defaultLocation is project's default location: its default_location,
// else BigQuery's, US.
func (p *projectOptions) defaultLocation(project string) string {
	if p == nil {
		return defaultLocation
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.opts[project]["default_location"]; v != "" {
		return v
	}
	return defaultLocation
}

// alterProject is a parsed ALTER PROJECT ... SET OPTIONS statement.
type alterProject struct {
	project string
	// sets are the options it sets, a nil value clearing one.
	sets map[string]*string
}

// parseAlterProject reads sql when its first statement is ALTER PROJECT or
// ALTER ORGANIZATION. ok is false for any other query; msg is why it is
// refused 501, and bad (with msg) a statement BigQuery fails 400.
func parseAlterProject(sql, project string) (a alterProject, msg string, bad, ok bool) {
	if !strings.Contains(strings.ToUpper(sql), "ALTER") {
		return a, "", false, false
	}
	toks, lexed := lex(sql)
	if !lexed {
		return a, "", false, false
	}
	stmts := splitStatements(trimSemicolons(toks))
	if len(stmts) == 0 || len(stmts[0]) < 2 || !stmts[0][0].is("ALTER") ||
		!stmts[0][1].is("PROJECT") && !stmts[0][1].is("ORGANIZATION") {
		return a, "", false, false
	}
	t := stmts[0]
	if t[1].is("ORGANIZATION") {
		return a, "ALTER ORGANIZATION: an instance has no organization", false, true
	}
	if len(stmts) > 1 {
		return a, "ALTER PROJECT in a script of several statements; run it as a query of its own", false, true
	}
	i := 2
	a.project, a.sets = project, map[string]*string{}
	if i < len(t) && !t[i].is("SET") {
		parts, next := path(t, i)
		if len(parts) != 1 {
			return a, "Syntax error: expected a project ID after ALTER PROJECT", true, true
		}
		a.project, i = parts[0], next
	}
	if i+2 >= len(t) || !t[i].is("SET") || !t[i+1].is("OPTIONS") || !t[i+2].punct("(") {
		return a, "Syntax error: expected SET OPTIONS (...) in ALTER PROJECT", true, true
	}
	i += 3
	for i < len(t) && !t[i].punct(")") {
		if t[i].kind != tokWord && t[i].kind != tokQuoted {
			return a, "Syntax error: expected an option name in ALTER PROJECT SET OPTIONS", true, true
		}
		name, next := path(t, i)
		i = next
		if i+1 >= len(t) || !t[i].punct("=") {
			return a, "Syntax error: expected = after an option name in ALTER PROJECT SET OPTIONS", true, true
		}
		i++
		full := strings.Join(name, ".")
		if !strings.EqualFold(full, "default_location") {
			return a, "ALTER PROJECT SET OPTIONS " + full + ": CloudBurrow serves default_location only", false, true
		}
		switch {
		case t[i].is("NULL"):
			a.sets["default_location"] = nil
		default:
			v, isString := stringLiteral(t[i])
			if !isString || v == "" {
				return a, "Invalid value for default_location: expected a location as a string literal, or NULL", true, true
			}
			a.sets["default_location"] = &v
		}
		i++
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	if i >= len(t) || i != len(t)-1 {
		return a, "Syntax error: unexpected text after ALTER PROJECT SET OPTIONS (...)", true, true
	}
	return a, "", false, true
}

// alterProjectOptions answers a query whose statement is ALTER PROJECT or
// ALTER ORGANIZATION (above), and reports whether it was one. The option
// is set, and the job is run as a statement that does nothing, keeping
// the client's text.
func (f front) alterProjectOptions(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	a, msg, bad, ok := parseAlterProject(q.Query, projectOf(f.base))
	if !ok {
		return false
	}
	if bad {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery", Message: msg})
		return true
	}
	if msg != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+msg+". Nothing was run.")
		return true
	}
	if f.settings == nil {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: ALTER PROJECT. Nothing was run.")
		return true
	}
	if !dryRun(r, insert) {
		for name, v := range a.sets {
			if v != nil {
				loc := canonLocation(*v)
				v = &loc
			}
			f.settings.set(a.project, name, v)
		}
	}
	if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{"_cloudburrow", scratchTable()})) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	f.forward(w, r, jobText{query: q.Query})
	return true
}

// Project option views (#1375): region-qualified, like the job views.
const projectOptionsColumns = "STRUCT<option_name STRING, project_id STRING, project_number INT64, " +
	"option_description STRING, option_type STRING, option_value STRING>"

const effectiveProjectOptionsColumns = "STRUCT<project_id STRING, project_number INT64, option_name STRING, " +
	"option_description STRING, option_type STRING, option_set_level STRING, option_set_on_id STRING, option_value STRING>"

// projectViews are the region-qualified views of the project itself, not
// of its datasets (infoschemajobs.go reads them).
var projectViews = map[string]func(front) string{
	"PROJECT_OPTIONS": func(f front) string {
		project := projectOf(f.base)
		var rows [][]string
		for _, o := range f.settings.list(project) {
			rows = append(rows, []string{sqlString(o[0]), sqlString(project), "NULL", "NULL", sqlString("STRING"), sqlString(o[1])})
		}
		return rowsSubquery(projectOptionsColumns, rows)
	},
	"EFFECTIVE_PROJECT_OPTIONS": func(f front) string {
		project := projectOf(f.base)
		var rows [][]string
		for _, o := range f.settings.list(project) {
			rows = append(rows, []string{sqlString(project), "NULL", sqlString(o[0]), "NULL", sqlString("STRING"),
				sqlString("PROJECTS"), sqlString(project), sqlString(o[1])})
		}
		return rowsSubquery(effectiveProjectOptionsColumns, rows)
	},
}

func init() {
	for view := range projectViews {
		regionViews[view] = true // read region-qualified (informationSchemaJobs)
	}
}

// inRegion is build for the datasets in region (a location, any case):
// any other dataset is left out of a region-qualified view (#1375), as
// BigQuery's lists "only datasets in that region".
func inRegion(region string, build func(front, *http.Request, string, *datasetMeta) (string, int, string)) func(front, *http.Request, string, *datasetMeta) (string, int, string) {
	return func(f front, r *http.Request, dataset string, m *datasetMeta) (string, int, string) {
		if dataset == resultsDataset {
			return "", http.StatusNotFound, "" // the front's own (results.go); regionRows leaves it out
		}
		if loc := f.datasetLocation(r, dataset); loc != "" && !strings.EqualFold(loc, region) {
			return "", http.StatusNotFound, "" // left out, as regionRows leaves out a dataset deleted meanwhile
		}
		return build(f, r, dataset, m)
	}
}

// canonLocation writes a location as BigQuery names it: the multi-regions
// upper case (US, EU), a region lower case (us-central1).
func canonLocation(s string) string {
	if strings.EqualFold(s, "us") || strings.EqualFold(s, "eu") {
		return strings.ToUpper(s)
	}
	return strings.ToLower(s)
}
