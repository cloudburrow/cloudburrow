package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Locations (#1371). BigQuery makes a dataset created without a location
// in the default location: "If you don't specify a location, the default
// location is used" (https://cloud.google.com/bigquery/docs/datasets),
// which is the project's or organization's default_location configuration
// and, with none set, the US multi-region
// (https://cloud.google.com/bigquery/docs/default-configuration): an
// instance's is the project's default_location, else US
// (projectoptions.go, #1375).
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

// defaultLocation is BigQuery's default location, when no
// default_location is configured; also that of a dataset stored without
// one, before #1371.
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

// datasetInsertLocation gives r's datasets.insert body the location loc,
// the project's default, when it names none.
func datasetInsertLocation(r *http.Request, loc string) {
	b, err := readBody(r)
	if err != nil {
		return
	}
	var body map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&body) != nil || body == nil {
		return
	}
	var have string
	if v, ok := body["location"]; ok && json.Unmarshal(v, &have) == nil && have != "" {
		return
	}
	body["location"], _ = json.Marshal(loc)
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
// dataset's that can be read, else the project's default.
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
	return f.settings.defaultLocation(projectOf(f.base))
}

// regionLocation is the location of the first region-qualified
// INFORMATION_SCHEMA view a GoogleSQL query reads, or "": BigQuery runs
// such a query in the region it names when the request gives no location
// (#1375).
func regionLocation(text string, legacy bool) string {
	if legacy || text == "" {
		return ""
	}
	if refs := informationSchemaJobs(text); len(refs) > 0 {
		return canonLocation(strings.TrimPrefix(strings.ToLower(refs[0].region), "region-"))
	}
	return ""
}

// queryConf returns a query configuration's (or QueryRequest's) text and
// whether it is legacy SQL (query: a jobs.query, whose default it is).
func queryConf(q map[string]any, query bool) (string, bool) {
	text, _ := q["query"].(string)
	legacy, _ := q["useLegacySql"].(bool)
	if _, set := q["useLegacySql"]; !set && query {
		legacy = true
	}
	return text, legacy
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
			qc, _ := conf["query"].(map[string]any)
			if loc = regionLocation(queryConf(qc, false)); loc == "" {
				loc = f.jobLocation(r, jobDatasets(conf, project, false))
			}
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
	if loc = regionLocation(queryConf(body, true)); loc == "" {
		loc = f.jobLocation(r, jobDatasets(body, project, true))
	}
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
		loc = defaultLocation // a request the front could not read
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
func serveJobLocations(w http.ResponseWriter, r *http.Request, next http.Handler, settings *projectOptions, serve func(http.ResponseWriter)) bool {
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
		if got := (front{next: next, base: base, settings: settings}).requestJobLocation(r, insert); got != "" {
			loc = got
		}
	}
	withJobLocations(w, loc, serve)
	return true
}

// INFORMATION_SCHEMA.SCHEMATA (#1371, #1375). The engine served it with
// NULL location, creation_time and last_modified_time (STRINGs); BigQuery
// documents a row per dataset with its location
// (https://cloud.google.com/bigquery/docs/information-schema-datasets-schemata).
// The front answers it as the other dataset views (infoschema.go): dataset-
// qualified, that dataset's row, as the engine did; region-qualified, the
// rows of the project's datasets in that region (inRegion,
// projectoptions.go), but the front's own results dataset (results.go).
// In a script too (#1375): one that makes, alters or drops a dataset
// before it reads SCHEMATA is run a statement at a time (runScript,
// #1417), so SCHEMATA is read in its turn and shows a dataset the script
// made; any other script reads it as the script starts.
//
// ddl is the CREATE SCHEMA statement in the documented example's form:
//
//	CREATE SCHEMA `myproject.mydataset2`
//	OPTIONS(
//	  default_partition_expiration_days=3.0,
//	  location="us"
//	);
//
// its options sorted by name, those the dataset resource holds:
// default_partition_expiration_days, default_table_expiration_days,
// description, friendly_name, labels and location (lower case), each as
// that example writes them (labels as CREATE SCHEMA's
// [("key", "value")], UNVERIFIED); with a defaultCollation, CREATE
// SCHEMA's DEFAULT COLLATE 'und:ci' clause before OPTIONS (#1417; where
// BigQuery breaks the line is UNVERIFIED).
// sync_status is NULL, as BigQuery gives it for a dataset with no replica,
// and an instance keeps none. default_collation_name is the dataset's
// defaultCollation, or NULL.
const schemataColumns = "STRUCT<catalog_name STRING, schema_name STRING, schema_owner STRING, " +
	"creation_time TIMESTAMP, last_modified_time TIMESTAMP, location STRING, ddl STRING, " +
	"default_collation_name STRING, sync_status JSON>"

func init() {
	infoSchemaBuilders["SCHEMATA"] = front.schemataRows
	regionViews["SCHEMATA"], regionColumns["SCHEMATA"] = true, schemataColumns
}

// schemataRows builds a dataset's SCHEMATA row.
func (f front) schemataRows(r *http.Request, dataset string, _ *datasetMeta) (string, int, string) {
	project := projectOf(f.base)
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
	collation := "NULL"
	if c := str(ds, "defaultCollation"); c != "" {
		collation = sqlString(c)
	}
	row := []string{sqlString(project), sqlString(dataset), "NULL", millisTimestamp(ds["creationTime"]),
		millisTimestamp(ds["lastModifiedTime"]), sqlString(loc), sqlString(schemaDDL(project, dataset, ds)), collation, "NULL"}
	return rowsSubquery(schemataColumns, [][]string{row}), 0, ""
}

// schemaDDL is the CREATE SCHEMA statement of dataset ds (above).
func schemaDDL(project, dataset string, ds map[string]any) string {
	quote := func(s string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
	}
	days := func(v any) (string, bool) {
		var ms float64
		switch x := v.(type) {
		case string:
			n, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return "", false
			}
			ms = n
		case float64:
			ms = x
		default:
			return "", false
		}
		s := strconv.FormatFloat(ms/86400000, 'f', -1, 64)
		if !strings.ContainsAny(s, ".e") {
			s += ".0"
		}
		return s, true
	}
	var opts []string
	if d, ok := days(ds["defaultPartitionExpirationMs"]); ok {
		opts = append(opts, "default_partition_expiration_days="+d)
	}
	if d, ok := days(ds["defaultTableExpirationMs"]); ok {
		opts = append(opts, "default_table_expiration_days="+d)
	}
	if s := str(ds, "description"); s != "" {
		opts = append(opts, "description="+quote(s))
	}
	if s := str(ds, "friendlyName"); s != "" {
		opts = append(opts, "friendly_name="+quote(s))
	}
	if labels, _ := ds["labels"].(map[string]any); len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, len(keys))
		for i, k := range keys {
			v, _ := labels[k].(string)
			pairs[i] = "(" + quote(k) + ", " + quote(v) + ")"
		}
		opts = append(opts, "labels=["+strings.Join(pairs, ", ")+"]")
	}
	if loc := str(ds, "location"); loc != "" {
		opts = append(opts, "location="+quote(strings.ToLower(loc)))
	}
	collate := ""
	if c := str(ds, "defaultCollation"); c != "" { // #1417, CREATE SCHEMA's clause, before OPTIONS
		collate = "\nDEFAULT COLLATE '" + strings.ReplaceAll(c, "'", `\'`) + "'"
	}
	return "CREATE SCHEMA `" + project + "." + dataset + "`" + collate + "\nOPTIONS(\n  " + strings.Join(opts, ",\n  ") + "\n);"
}

// schemataInTurn reports whether sql is a script that makes, alters or
// drops a dataset and then reads INFORMATION_SCHEMA.SCHEMATA or
// SCHEMATA_OPTIONS, with no TEMP table or function, which runScript cannot
// keep between its requests (#1417).
func schemataInTurn(sql string) bool {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "SCHEMATA") || !strings.Contains(upper, "SCHEMA") {
		return false
	}
	toks, ok := lex(sql)
	if !ok || tokensHaveTemp(toks) || len(splitStatements(trimSemicolons(toks))) < 2 {
		return false
	}
	changed := false
	for i, t := range toks {
		switch {
		case (t.is("CREATE") || t.is("ALTER") || t.is("DROP")) && i+1 < len(toks) && toks[i+1].is("SCHEMA"):
			changed = true
		case t.is("CREATE") && i+3 < len(toks) && toks[i+1].is("OR") && toks[i+2].is("REPLACE") && toks[i+3].is("SCHEMA"):
			changed = true
		case changed && (t.is("SCHEMATA") || t.is("SCHEMATA_OPTIONS")):
			return true
		case changed && t.kind == tokQuoted && (strings.HasSuffix(strings.ToUpper(t.text), "INFORMATION_SCHEMA.SCHEMATA") ||
			strings.HasSuffix(strings.ToUpper(t.text), "INFORMATION_SCHEMA.SCHEMATA_OPTIONS")):
			return true
		}
	}
	return false
}
