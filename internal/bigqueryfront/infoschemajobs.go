package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Region-qualified INFORMATION_SCHEMA.JOBS, JOBS_BY_PROJECT and
// JOBS_BY_USER (#1275).
//
// Measured against the pinned image through the front (2026-10-15):
// `region-us`.INFORMATION_SCHEMA.JOBS failed 400 "Table not found". The
// front answers a query that reads one as it answers a dataset's VIEWS
// (infoschemaviews.go): each reference becomes a subquery of rows, one per
// job the front's jobs.list gives (listJobs, the handler front.go serves
// jobs.list with, passed in: its times, the configurations the emulator
// leaves out, the client's texts, the jobs the front carried out and the
// failures it gave), with BigQuery's documented columns
// (https://cloud.google.com/bigquery/docs/information-schema-jobs) but
// those whose STRUCTs Google does not document (jobsLeftOut). Every user
// is the instance's one, so JOBS_BY_USER is JOBS. The jobs are the
// project's top-level ones and the child jobs of scripts (scriptjobs.go,
// #1353), and not the query reading the view, which has no record yet.
//
// The instance's jobs are in one location, US (the emulator gives jobs and
// datasets none; BigQuery's default). A region other than the query's
// location (the region read when the request names none, #1375) is refused 404 "Not found"
// (wording UNVERIFIED), as BigQuery refuses a view of another region than
// the job's. JOBS_BY_FOLDER and JOBS_BY_ORGANIZATION:
// infoschemahierarchy.go (#1392).
//
// The region-qualified forms of the dataset views (regionViews, #1329) are
// the union of every dataset's rows, and region TABLE_STORAGE a row per
// table but the counts the emulator keeps nothing faithful of
// (storageColumns). Measured first: each was 400 "Table not found". The
// JOBS columns jobsLeftOut stay left out: the INFORMATION_SCHEMA.JOBS page
// gives them as RECORD with no field list, and the front knows none of
// their contents (the emulator gives no query plan, timeline, session or
// BI Engine statistics); destination_table, referenced_tables and labels
// are read from the job record.

// jobsLeftOut are JOBS columns the front does not give.
var jobsLeftOut = []string{"bi_engine_statistics", "job_stages", "object_storage_stats", "session_info", "timeline",
	"query_info", "materialized_view_statistics", "metadata_cache_statistics", "search_statistics",
	"continuous_query_info", "vector_search_statistics", "external_service_costs", "ml_statistics"}

// jobsRef is a reference to a region's INFORMATION_SCHEMA.JOBS view.
type jobsRef struct {
	pos, end int
	region   string // "region-us", as written
	view     string
	alias    bool
}

// jobsViews are the views jobsRef reads; false: refused 501.
var jobsViews = map[string]bool{"JOBS": true, "JOBS_BY_PROJECT": true, "JOBS_BY_USER": true,
	"JOBS_BY_FOLDER": true, "JOBS_BY_ORGANIZATION": true} // the last two: infoschemahierarchy.go (#1392)

// informationSchemaJobs returns sql's references to
// [project.]`region-x`.INFORMATION_SCHEMA.JOBS[_BY_...].
func informationSchemaJobs(sql string) []jobsRef {
	t, ok := lex(sql)
	if !ok {
		return nil
	}
	var refs []jobsRef
	for i := 0; i < len(t); i++ {
		if t[i].kind != tokWord && t[i].kind != tokQuoted || i > 0 && t[i-1].punct(".") {
			continue
		}
		parts, next := path(t, i)
		n := len(parts)
		if n < 3 || n > 4 || !strings.EqualFold(parts[n-2], "INFORMATION_SCHEMA") ||
			!strings.HasPrefix(strings.ToLower(parts[n-3]), "region-") {
			i = next - 1
			continue
		}
		view := strings.ToUpper(parts[n-1])
		if _, ok := jobsViews[view]; !ok && !regionViews[view] && view != "TABLE_STORAGE" {
			i = next - 1
			continue
		}
		ref := jobsRef{pos: t[i].pos, end: t[next-1].end, region: parts[n-3], view: view}
		if next < len(t) && (t[next].is("AS") || t[next].kind == tokQuoted ||
			t[next].kind == tokWord && !reservedWords[strings.ToUpper(t[next].text)]) {
			ref.alias = true
		}
		refs = append(refs, ref)
		i = next - 1
	}
	return refs
}

const jobsColumns = "STRUCT<cache_hit BOOL, creation_time TIMESTAMP, " +
	"destination_table STRUCT<project_id STRING, dataset_id STRING, table_id STRING>, " +
	"dml_statistics STRUCT<inserted_row_count INT64, deleted_row_count INT64, updated_row_count INT64>, " +
	"end_time TIMESTAMP, error_result STRUCT<reason STRING, location STRING, debug_info STRING, message STRING>, " +
	"job_creation_reason STRUCT<code STRING>, job_id STRING, job_type STRING, " +
	"labels ARRAY<STRUCT<key STRING, value STRING>>, parent_job_id STRING, priority STRING, project_id STRING, " +
	"project_number INT64, query STRING, " +
	"referenced_tables ARRAY<STRUCT<project_id STRING, dataset_id STRING, table_id STRING>>, " +
	"referenced_logical_views ARRAY<STRUCT<project_id STRING, dataset_id STRING, table_id STRING>>, " +
	"reservation_id STRING, reservation_group_path ARRAY<STRING>, edition STRING, start_time TIMESTAMP, state STRING, " +
	"statement_type STRING, total_bytes_billed INT64, total_bytes_processed INT64, total_modified_partitions INT64, " +
	"total_slot_ms INT64, total_services_sku_slot_ms INT64, transaction_id STRING, user_email STRING, " +
	"principal_subject STRING, transferred_bytes INT64, query_dialect STRING, continuous BOOL>"

// int64Or is a resource's int64 (a string or number) as SQL, or NULL.
func int64Or(v any) string {
	switch x := v.(type) {
	case string:
		if _, err := strconv.ParseInt(x, 10, 64); err == nil {
			return x
		}
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return "NULL"
}

// tableRefSQL is a TableReference as a STRUCT, or NULL.
func tableRefSQL(v any) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return "NULL"
	}
	return "(" + sqlString(str(m, "projectId")) + ", " + sqlString(str(m, "datasetId")) + ", " + sqlString(str(m, "tableId")) + ")"
}

// tableRefsSQL is a list of TableReferences as an ARRAY of STRUCTs.
func tableRefsSQL(v any) string {
	raw, _ := v.([]any)
	var parts []string
	for _, x := range raw {
		parts = append(parts, tableRefSQL(x))
	}
	return "ARRAY<STRUCT<project_id STRING, dataset_id STRING, table_id STRING>>[" + strings.Join(parts, ", ") + "]"
}

// jobTypeOf is a job's job_type, from its configuration.
func jobTypeOf(conf map[string]any) string {
	jobType := strings.ToUpper(str(conf, "jobType"))
	if jobType == "" {
		for _, k := range []string{"query", "load", "copy", "extract"} {
			if conf[k] != nil {
				jobType = strings.ToUpper(k)
			}
		}
	}
	return jobType
}

// jobStateOf is a listed job's state.
func jobStateOf(job map[string]any) string {
	status, _ := job["status"].(map[string]any)
	if state := str(status, "state"); state != "" {
		return state
	}
	return str(job, "state")
}

// jobRow is one JOBS row of a Job resource jobs.list gave.
func jobRow(project string, job map[string]any) []string {
	ref, _ := job["jobReference"].(map[string]any)
	conf, _ := job["configuration"].(map[string]any)
	stats, _ := job["statistics"].(map[string]any)
	status, _ := job["status"].(map[string]any)
	qs, _ := stats["query"].(map[string]any)
	qc, _ := conf["query"].(map[string]any)
	jobType := jobTypeOf(conf)
	var dest any
	for _, k := range []string{"query", "load", "copy"} {
		if c, _ := conf[k].(map[string]any); c != nil && c["destinationTable"] != nil {
			dest = c["destinationTable"]
		}
	}
	dml := "NULL"
	if d, _ := qs["dmlStats"].(map[string]any); d != nil {
		dml = "(" + int64Or(d["insertedRowCount"]) + ", " + int64Or(d["deletedRowCount"]) + ", " + int64Or(d["updatedRowCount"]) + ")"
	}
	errResult := "NULL"
	if e, _ := status["errorResult"].(map[string]any); e != nil {
		errResult = "(" + sqlOrNull(str(e, "reason")) + ", " + sqlOrNull(str(e, "location")) + ", " +
			sqlOrNull(str(e, "debugInfo")) + ", " + sqlOrNull(str(e, "message")) + ")"
	}
	var labels []string
	if l, _ := conf["labels"].(map[string]any); len(l) > 0 {
		keys := make([]string, 0, len(l))
		for k := range l {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, _ := l[k].(string)
			labels = append(labels, "("+sqlString(k)+", "+sqlString(v)+")")
		}
	}
	state := jobStateOf(job)
	cache := "FALSE"
	if b, _ := qs["cacheHit"].(bool); b {
		cache = "TRUE"
	}
	if jobType != "QUERY" {
		cache = "NULL"
	}
	dialect := "NULL"
	if qc != nil {
		switch v := qc["useLegacySql"].(type) {
		case bool:
			dialect = sqlString(map[bool]string{true: "LEGACY_SQL", false: "GOOGLE_SQL"}[v])
		default:
			dialect = sqlString("DEFAULT_LEGACY_SQL")
		}
	}
	tx := "NULL"
	if ti, _ := stats["transactionInfo"].(map[string]any); ti != nil {
		tx = sqlOrNull(str(ti, "transactionId"))
	}
	priority := "NULL"
	if qc != nil {
		p := str(qc, "priority")
		if p == "" {
			p = "INTERACTIVE"
		}
		priority = sqlString(p)
	}
	bytesProcessed := stats["totalBytesProcessed"]
	if qs["totalBytesProcessed"] != nil {
		bytesProcessed = qs["totalBytesProcessed"]
	}
	return []string{cache, millisTimestamp(stats["creationTime"]), tableRefSQL(dest), dml, millisTimestamp(stats["endTime"]),
		errResult, "NULL", sqlString(str(ref, "jobId")), sqlOrNull(jobType),
		"ARRAY<STRUCT<key STRING, value STRING>>[" + strings.Join(labels, ", ") + "]", sqlOrNull(str(stats, "parentJobId")),
		priority, sqlString(project), "NULL", sqlOrNull(str(qc, "query")), tableRefsSQL(qs["referencedTables"]),
		tableRefsSQL(nil), "NULL", "ARRAY<STRING>[]", "NULL", millisTimestamp(stats["startTime"]), sqlOrNull(state),
		sqlOrNull(str(qs, "statementType")), int64Or(qs["totalBytesBilled"]), int64Or(bytesProcessed), "NULL",
		int64Or(stats["totalSlotMs"]), "NULL", tx, sqlOrNull(str(job, "user_email")), "NULL", "NULL", dialect, "FALSE"}
}

// jobsRows returns the subquery of the project's jobs, or the status and
// message of why it cannot.
func (f front) jobsRows(r *http.Request, filter *jobsFilter) (string, int, string) {
	project := projectOf(f.base)
	jobs, status, msg := f.allJobs(r, filter)
	if status != 0 {
		return "", status, msg
	}
	if filter == nil { // nothing pushed down: the table of rows (jobstable.go, #1378)
		if sub := f.tableJobsRows(r, project, jobs, true); sub != "" {
			return sub, 0, ""
		}
	}
	jobs = filter.apply(project, jobs)
	rows := make([][]string, len(jobs))
	for i, job := range jobs {
		rows[i] = jobRow(project, job)
	}
	return rowsSubquery(jobsColumns, rows), 0, ""
}

// allJobs lists the project's jobs, top-level and children, within the
// bounds of creation_time filter gives, or the status and message of why
// it cannot.
func (f front) allJobs(r *http.Request, filter *jobsFilter) ([]map[string]any, int, string) {
	if f.listJobs == nil {
		return nil, http.StatusInternalServerError, "cloudburrow: the job list cannot be read here"
	}
	var jobs []map[string]any
	token := ""
	for {
		// One list, not pages of it: the engine reads every job of the
		// project for each page (#1369). The bounds of creation_time the
		// query gives cut it before the configurations are read.
		q := url.Values{"projection": {"full"}, "allUsers": {"true"}}
		filter.listBounds(q)
		if token != "" {
			q.Set("pageToken", token)
		}
		req := r.Clone(withAllJobLevels(r.Context())) // children too (#1353)
		u := *r.URL
		u.Path, u.RawPath = "", ""
		p := f.base + "/jobs"
		if unescaped, err := url.PathUnescape(p); err == nil {
			u.Path, u.RawPath = unescaped, p
		}
		u.RawQuery = q.Encode()
		req.Method, req.URL, req.Body, req.ContentLength, req.RequestURI = http.MethodGet, &u, http.NoBody, 0, ""
		req.Header = r.Header.Clone()
		req.Header.Del("Content-Type")
		req.Header.Del("Content-Length")
		rec := newRecorder()
		f.listJobs(rec, req)
		var list struct {
			Jobs          []map[string]any `json:"jobs"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if rec.status != 0 && rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
			return nil, http.StatusInternalServerError, "cloudburrow: could not list the jobs: " + errorMessage(rec.body.Bytes(), rec.status)
		}
		jobs = append(jobs, list.Jobs...)
		if list.NextPageToken == "" || list.NextPageToken == token {
			break
		}
		token = list.NextPageToken
	}
	return jobs, 0, ""
}

// withInformationSchemaJobs answers q, whose text reads refs, by writing
// each as the subquery of the project's jobs (above).
func (f front) withInformationSchemaJobs(w http.ResponseWriter, r *http.Request, q queryOptions, refs []jobsRef, insert bool) {
	location := strings.ToLower(queryLocation(r, insert))
	if location == "" {
		location = "us"
	}
	var b strings.Builder
	last := 0
	subs := map[string]string{}
	meta := &datasetMeta{}
	for _, ref := range refs {
		if region := strings.ToLower(strings.TrimPrefix(strings.ToLower(ref.region), "region-")); region != location {
			writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Dataset %s:%s was not found in location %s",
				projectOf(f.base), ref.region, strings.ToUpper(location)))
			return
		}
		if ref.view == "OBJECT_PRIVILEGES" && !namesObjectName(q.Query) { // #1355, infoschemaprivileges.go
			f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery",
				Message: "Queries of INFORMATION_SCHEMA.OBJECT_PRIVILEGES must filter on object_name (and object_schema for a table or view)"})
			return
		}
		key := ref.view
		if jobsViews[key] && !hierarchyJobsViews[key] {
			key = "JOBS"
		}
		sub, ok := subs[key]
		if !ok {
			var status int
			var msg string
			switch {
			case key == "JOBS":
				sub, status, msg = f.jobsRows(r, pushedJobsFilter(q.Query, refs, time.Now()))
			case projectViews[key] != nil: // #1375, projectoptions.go
				sub = projectViews[key](f, location)
			case hierarchyJobsViews[key]: // #1392, infoschemahierarchy.go
				sub, status, msg = f.hierarchyJobsRows(r, key, pushedJobsFilter(q.Query, refs, time.Now()))
			case key == "TABLE_STORAGE":
				sub, status, msg = f.regionRows(r, meta, "TABLE_STORAGE", storageColumns, inRegion(location, front.storageRows))
			default: // the region's datasets alone (#1375, projectoptions.go)
				sub, status, msg = f.regionRows(r, meta, key, "", inRegion(location, infoSchemaBuilders[key]))
			}
			if status != 0 {
				reason := map[int]string{http.StatusNotFound: "notFound", http.StatusNotImplemented: "notImplemented",
					http.StatusBadRequest: "invalidQuery"}[status]
				if reason == "" {
					reason = "internalError"
				}
				writeError(w, status, reason, msg)
				return
			}
			subs[key] = sub
		}
		b.WriteString(q.Query[last:ref.pos])
		b.WriteString(sub)
		if !ref.alias {
			b.WriteString(" AS " + ref.view)
		}
		last = ref.end
	}
	b.WriteString(q.Query[last:])
	sql := b.String()
	if !setQueryText(r, insert, sql) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	client := jobText{query: q.Query}
	q.Query = sql
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
}

// regionViews are the dataset views BigQuery also answers region-qualified
// (`region-us`.INFORMATION_SCHEMA.TABLES, #1329): the rows of every
// dataset of the project, all of which are in the instance's one location.
var regionViews = map[string]bool{"VIEWS": true, "TABLES": true, "COLUMNS": true, "COLUMN_FIELD_PATHS": true,
	"TABLE_OPTIONS": true, "ROUTINES": true, "PARAMETERS": true, "ROUTINE_OPTIONS": true, "TABLE_SNAPSHOTS": true,
	"SEARCH_INDEXES": true, "VECTOR_INDEXES": true}

// regionColumns are the row types of regionViews, for a project with no
// dataset.
var regionColumns = map[string]string{"VIEWS": viewsColumns, "TABLES": tablesColumns, "COLUMNS": columnsColumns,
	"COLUMN_FIELD_PATHS": fieldPathsColumns, "TABLE_OPTIONS": tableOptionsColumns, "ROUTINES": routinesColumns,
	"PARAMETERS": parametersColumns, "ROUTINE_OPTIONS": routineOptionsColumns, "TABLE_SNAPSHOTS": snapshotsColumns,
	"SEARCH_INDEXES": searchIndexesColumns, "VECTOR_INDEXES": vectorIndexesColumns}

// datasetIDs lists the project's datasets.
func (f front) datasetIDs(r *http.Request) ([]string, int, string) {
	var out []string
	token := ""
	for {
		p := "/datasets?all=true&maxResults=1000"
		if token != "" {
			p += "&pageToken=" + url.QueryEscape(token)
		}
		st, got := f.get(r, p)
		var list struct {
			Datasets []struct {
				DatasetReference struct {
					DatasetID string `json:"datasetId"`
				} `json:"datasetReference"`
			} `json:"datasets"`
			NextPageToken string `json:"nextPageToken"`
		}
		if st != http.StatusOK || json.Unmarshal(got, &list) != nil {
			return nil, http.StatusInternalServerError, "cloudburrow: could not list the datasets: " + errorMessage(got, st)
		}
		for _, d := range list.Datasets {
			out = append(out, d.DatasetReference.DatasetID)
		}
		if list.NextPageToken == "" || list.NextPageToken == token {
			break
		}
		token = list.NextPageToken
	}
	sort.Strings(out)
	return out, 0, ""
}

// regionRows is the subquery of view's rows of every dataset.
func (f front) regionRows(r *http.Request, meta *datasetMeta, view, columns string,
	build func(front, *http.Request, string, *datasetMeta) (string, int, string)) (string, int, string) {
	if columns == "" {
		columns = regionColumns[view]
	}
	datasets, st, msg := f.datasetIDs(r)
	if st != 0 {
		return "", st, msg
	}
	var subs []string
	for _, ds := range datasets {
		sub, st, msg := build(f, r, ds, meta)
		if st == http.StatusNotFound {
			continue // deleted meanwhile
		}
		if st != 0 {
			return "", st, msg
		}
		subs = append(subs, sub)
	}
	switch len(subs) {
	case 0:
		return rowsSubquery(columns, nil), 0, ""
	case 1:
		return subs[0], 0, ""
	}
	return "(SELECT * FROM (" + strings.Join(subs, " UNION ALL ") + "))", 0, ""
}

// storageColumns are region-qualified TABLE_STORAGE's columns
// (https://cloud.google.com/bigquery/docs/information-schema-table-storage)
// but those the emulator keeps no faithful count of, left out: every
// *_bytes column, total_partitions and storage_last_modified_time (it
// keeps no time of a table's last write apart from its metadata's).
// project_number is NULL: an instance has none.
const storageColumns = "STRUCT<project_id STRING, project_number INT64, table_catalog STRING, table_schema STRING, " +
	"table_name STRING, creation_time TIMESTAMP, total_rows INT64, deleted BOOL, table_type STRING, " +
	"managed_table_type STRING, last_metadata_index_refresh_time TIMESTAMP, table_deletion_reason STRING, " +
	"table_deletion_time TIMESTAMP>"

// storageRows are a dataset's TABLE_STORAGE rows: its tables, snapshots
// and clones (no view), none deleted (the instance keeps none).
func (f front) storageRows(r *http.Request, dataset string, m *datasetMeta) (string, int, string) {
	tables, st, msg := f.tablesOf(r, dataset, m)
	if st != 0 {
		return "", st, msg
	}
	project := projectOf(f.base)
	var rows [][]string
	for _, t := range tables {
		typ := str(t, "type")
		if typ != "TABLE" && typ != "SNAPSHOT" && typ != "" {
			continue
		}
		tt, managed := "BASE TABLE", sqlString("NATIVE")
		if typ == "SNAPSHOT" || t["snapshotDefinition"] != nil {
			tt, managed = "SNAPSHOT", "NULL"
		} else if t["cloneDefinition"] != nil {
			tt = "CLONE"
		}
		rows = append(rows, []string{sqlString(project), "NULL", sqlString(project), sqlString(dataset), sqlString(str(t, "tableId")),
			millisTimestamp(t["creationTime"]), int64Or(t["numRows"]), "FALSE", sqlString(tt), managed, "NULL", "NULL", "NULL"})
	}
	return rowsSubquery(storageColumns, rows), 0, ""
}
