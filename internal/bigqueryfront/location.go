package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Locations (#1371). BigQuery makes a dataset created without a location
// in the default location: "If you don't specify a location, the default
// location is used" (https://cloud.google.com/bigquery/docs/datasets),
// which is the project's or organization's default_location configuration
// and, with none set, the US multi-region
// (https://cloud.google.com/bigquery/docs/default-configuration). An
// instance serves no ALTER PROJECT SET OPTIONS, so its default is US.
// datasets.get, datasets.list and tables.get then give that location (a
// table's is its dataset's: "This value is inherited from the dataset").
//
// The emulator stores the location a datasets.insert gives and none
// otherwise: measured against the pinned image through the front, a
// dataset made without one read back from datasets.get and datasets.list
// with no location at all, a table of it from tables.get too, and
// INFORMATION_SCHEMA.SCHEMATA's location was NULL. bq reads the field
// (`datasets().get(...).execute()['location']`, clients/client_dataset.py),
// so `bq cp` failed with KeyError: 'location'.
//
// So the front gives datasets.insert's body the default location when it
// gives none, which the emulator stores; and fills it in on read-back for a
// dataset stored without one (state saved before #1371), never changing a
// location the dataset has.
//
// Jobs. A job's location, when the request gives none, is BigQuery's
// "location of the datasets referenced in the request ... the location of
// a destination table", else the default location
// (https://cloud.google.com/bigquery/docs/locations). The front gives a
// jobs.insert's jobReference, and a jobs.query's location, the location
// of the first dataset the job names (a load's, query's or copy's
// destination, a copy's or extract's source, a query's default dataset,
// then the tables a GoogleSQL query reads), else US; and every job
// resource it answers with no jobReference.location (the emulator's
// jobs.query answer gives none, nor do jobs of state saved before #1371)
// gets the request's location, else US.

// defaultLocation is an instance's default location: BigQuery's when no
// default_location is configured.
const defaultLocation = "US"

// withLocation sets content's location to defaultLocation when it has none,
// and reports whether it did.
func withLocation(content map[string]json.RawMessage) bool {
	var have string
	if v, ok := content["location"]; ok && json.Unmarshal(v, &have) == nil && have != "" {
		return false
	}
	content["location"], _ = json.Marshal(defaultLocation)
	return true
}

// datasetInsertLocation gives r's datasets.insert body the default
// location when it names none.
func datasetInsertLocation(r *http.Request) {
	b, err := readBody(r)
	if err != nil {
		return
	}
	var body map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&body) != nil || body == nil || !withLocation(body) {
		return
	}
	if out, err := json.Marshal(body); err == nil {
		setBody(r, out)
	}
}

// listDatasets answers datasets.list with each dataset's location.
func (f front) listDatasets(w http.ResponseWriter, r *http.Request) {
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	var list map[string]json.RawMessage
	var items []map[string]json.RawMessage
	if rec.status != 0 && rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil ||
		list == nil || json.Unmarshal(list["datasets"], &items) != nil {
		rec.copyTo(w)
		return
	}
	changed := false
	for _, d := range items {
		if d != nil && withLocation(d) {
			changed = true
		}
	}
	if !changed {
		rec.copyTo(w)
		return
	}
	list["datasets"], _ = json.Marshal(items)
	writeRecorded(w, rec, list)
}

// datasetLocation returns the location of the project's dataset, the
// default for one stored without one, or "" when it cannot be read.
func (f front) datasetLocation(r *http.Request, dataset string) string {
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	var ds map[string]json.RawMessage
	if st != http.StatusOK || json.Unmarshal(got, &ds) != nil || ds == nil {
		return ""
	}
	withLocation(ds)
	var loc string
	_ = json.Unmarshal(ds["location"], &loc)
	return loc
}

// jobDatasets returns the datasets of project a job's configuration
// (conf, or for a jobs.query, the QueryRequest itself) names, in the order
// its location is taken from.
func jobDatasets(conf map[string]any, project string, query bool) []string {
	var out []string
	add := func(v any) {
		ref, _ := v.(map[string]any)
		if ref == nil {
			return
		}
		p, _ := ref["projectId"].(string)
		ds, _ := ref["datasetId"].(string)
		if ds != "" && (p == "" || p == project) {
			out = append(out, ds)
		}
	}
	var q map[string]any
	if query {
		q = conf
	} else {
		if load, _ := conf["load"].(map[string]any); load != nil {
			add(load["destinationTable"])
		}
		if c, _ := conf["copy"].(map[string]any); c != nil {
			add(c["destinationTable"])
			add(c["sourceTable"])
			if srcs, _ := c["sourceTables"].([]any); len(srcs) > 0 {
				for _, s := range srcs {
					add(s)
				}
			}
		}
		if e, _ := conf["extract"].(map[string]any); e != nil {
			add(e["sourceTable"])
			add(e["sourceModel"])
		}
		q, _ = conf["query"].(map[string]any)
	}
	if q == nil {
		return out
	}
	add(q["destinationTable"])
	add(q["defaultDataset"])
	legacy, _ := q["useLegacySql"].(bool)
	if _, set := q["useLegacySql"]; !set && query {
		legacy = true // jobs.query's default (QueryRequest.useLegacySql)
	}
	text, _ := q["query"].(string)
	if legacy || text == "" {
		return out
	}
	def := ""
	if d, _ := q["defaultDataset"].(map[string]any); d != nil {
		def, _ = d["datasetId"].(string)
	}
	if reads, _, ok := tableReads(text, project, def); ok {
		for _, rd := range reads {
			if rd.dataset != "" && !strings.EqualFold(rd.table, "INFORMATION_SCHEMA") {
				out = append(out, rd.dataset)
			}
		}
	}
	return out
}

// jobLocation is the location of a job that gives none: the first named
// dataset's that can be read, else the default.
func (f front) jobLocation(r *http.Request, datasets []string) string {
	seen := map[string]bool{}
	for _, ds := range datasets {
		if seen[ds] {
			continue
		}
		seen[ds] = true
		if loc := f.datasetLocation(r, ds); loc != "" {
			return loc
		}
	}
	return defaultLocation
}

// requestJobLocation gives r's jobs.insert jobReference (insert) or
// jobs.query location the job's location when it names none, and returns
// the location the job runs in, or "" for a body the front cannot read.
func (f front) requestJobLocation(r *http.Request, insert bool) string {
	project := projectOf(f.base)
	loc := ""
	if insert {
		editJob(r, func(job map[string]any) bool {
			ref, _ := job["jobReference"].(map[string]any)
			if have, _ := ref["location"].(string); have != "" {
				loc = have
				return false
			}
			if ref == nil {
				ref = map[string]any{}
				job["jobReference"] = ref
			}
			conf, _ := job["configuration"].(map[string]any)
			loc = f.jobLocation(r, jobDatasets(conf, project, false))
			ref["location"] = loc
			return true
		})
		return loc
	}
	b, err := readBody(r)
	if err != nil {
		return ""
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&body) != nil || body == nil {
		return ""
	}
	if have, _ := body["location"].(string); have != "" {
		return have
	}
	loc = f.jobLocation(r, jobDatasets(body, project, true))
	body["location"] = loc
	if out, err := json.Marshal(body); err == nil {
		setBody(r, out)
	}
	return loc
}

// withJobLocations serves r through serve, giving every job resource of
// its answer (the answer itself, its "job", each of its "jobs") with no
// jobReference.location the location loc, else the default.
func withJobLocations(w http.ResponseWriter, loc string, serve func(http.ResponseWriter)) {
	if loc == "" {
		loc = defaultLocation
	}
	rec := newRecorder()
	serve(rec)
	var resp map[string]any
	dec := json.NewDecoder(bytes.NewReader(rec.body.Bytes()))
	dec.UseNumber()
	if rec.status != 0 && rec.status != http.StatusOK || dec.Decode(&resp) != nil || resp == nil {
		rec.copyTo(w)
		return
	}
	changed := false
	fill := func(v any) {
		job, _ := v.(map[string]any)
		ref, _ := job["jobReference"].(map[string]any)
		if ref == nil {
			return
		}
		if have, _ := ref["location"].(string); have == "" {
			ref["location"] = loc
			changed = true
		}
	}
	fill(resp)
	fill(resp["job"])
	if jobs, _ := resp["jobs"].([]any); jobs != nil {
		for _, j := range jobs {
			fill(j)
		}
	}
	if !changed {
		rec.copyTo(w)
		return
	}
	writeRecorded(w, rec, resp)
}

// serveJobLocations wraps the front's handling of a jobs request (jobs.insert,
// jobs.query, jobs.get, jobs.list, jobs.getQueryResults, jobs.cancel) with
// its location (above), and reports whether r was one.
func serveJobLocations(w http.ResponseWriter, r *http.Request, next http.Handler, serve func(http.ResponseWriter)) bool {
	p := r.URL.EscapedPath()
	var base string
	insert, post := false, false
	switch {
	case jobsRoute.MatchString(p):
		j := jobsRoute.FindStringSubmatch(p)
		base, insert = j[1]+"/projects/"+j[2], j[3] == "jobs"
		post = r.Method == http.MethodPost
		if post && strings.HasPrefix(p, "/upload/") && r.URL.Query().Get("uploadType") == "resumable" {
			// A resumable upload's first request is the job, read as
			// jobs.insert's body; its chunks are not.
			post = r.Header.Get("Content-Type") == "" || strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
		}
	case jobRoute.MatchString(p), jobActionRoute.MatchString(p):
	default:
		return false
	}
	loc := r.URL.Query().Get("location")
	if post && r.Header.Get("Content-Encoding") == "" {
		if got := (front{next: next, base: base}).requestJobLocation(r, insert); got != "" {
			loc = got
		}
	}
	withJobLocations(w, loc, serve)
	return true
}

// INFORMATION_SCHEMA.SCHEMATA (#1371). The engine served it with NULL
// location, creation_time and last_modified_time (STRINGs); BigQuery
// documents a row per dataset with its location
// (https://cloud.google.com/bigquery/docs/information-schema-datasets-schemata).
// The front answers it as the other dataset views (infoschema.go): dataset-
// qualified, that dataset's row, as the engine did; region-qualified, the
// rows of the project's datasets in that region, but the front's own
// results dataset (results.go). ddl and sync_status are
// left out: Google documents no form of ddl the front could be held to,
// and the front keeps no replica. default_collation_name is the dataset's
// defaultCollation, or NULL.
const schemataColumns = "STRUCT<catalog_name STRING, schema_name STRING, schema_owner STRING, " +
	"creation_time TIMESTAMP, last_modified_time TIMESTAMP, location STRING, default_collation_name STRING>"

func init() {
	infoSchemaBuilders["SCHEMATA"] = schemataIn("")
	engineInfoSchema["SCHEMATA"] = true // in a script, the engine's, as TABLES is
	regionViews["SCHEMATA"], regionColumns["SCHEMATA"] = true, schemataColumns
}

// schemataIn builds a dataset's SCHEMATA row, none for a dataset outside
// region (a location, any case) unless region is "".
func schemataIn(region string) func(front, *http.Request, string, *datasetMeta) (string, int, string) {
	return func(f front, r *http.Request, dataset string, _ *datasetMeta) (string, int, string) {
		project := projectOf(f.base)
		if region != "" && dataset == resultsDataset {
			return rowsSubquery(schemataColumns, nil), 0, "" // the front's own (results.go)
		}
		st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
		if st == http.StatusNotFound {
			return "", http.StatusNotFound, "Not found: Dataset " + project + ":" + dataset
		}
		var raw map[string]json.RawMessage
		if st != http.StatusOK || json.Unmarshal(got, &raw) != nil || raw == nil {
			return "", http.StatusInternalServerError, "cloudburrow: could not read the dataset: " + errorMessage(got, st)
		}
		withLocation(raw)
		b, _ := json.Marshal(raw)
		var ds map[string]any
		_ = json.Unmarshal(b, &ds)
		loc := str(ds, "location")
		if region != "" && !strings.EqualFold(loc, region) {
			return rowsSubquery(schemataColumns, nil), 0, ""
		}
		collation := "NULL"
		if c := str(ds, "defaultCollation"); c != "" {
			collation = sqlString(c)
		}
		row := []string{sqlString(project), sqlString(dataset), "NULL", millisTimestamp(ds["creationTime"]),
			millisTimestamp(ds["lastModifiedTime"]), sqlString(loc), collation}
		return rowsSubquery(schemataColumns, [][]string{row}), 0, ""
	}
}
