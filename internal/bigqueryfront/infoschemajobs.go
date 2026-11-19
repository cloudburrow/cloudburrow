package bigqueryfront

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// those it cannot fill (jobsStatisticsColumns: empty or NULL). Every user
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
// table, its physical byte counts estimates (storageColumns). Measured first: each was 400 "Table not found". The
// JOBS columns of statistics the emulator never makes are empty or NULL
// (jobsStatisticsColumns, #1458);
// bi_engine_statistics, session_info, continuous_query_info and
// ml_statistics are given (#1458); destination_table, referenced_tables and labels
// are read from the job record.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/information-schema-jobs

// The nine JOBS columns of statistics the emulator never makes (#1458):
// the INFORMATION_SCHEMA.JOBS page (read again 2026-10-16) gives
// job_stages ("Query stages", RECORD REPEATED), object_storage_stats
// (RECORD REPEATED: cloud_provider, object_storage_bytes_read,
// cache_bytes_read), timeline, query_info, materialized_view_statistics,
// metadata_cache_statistics, search_statistics, vector_search_statistics
// and external_service_costs, typed here by their REST types
// (ExplainQueryStage, QueryTimelineSample, MaterializedViewStatistics,
// MetadataCacheStatistics, SearchStatistics, VectorSearchStatistics,
// ExternalServiceCost; the documented fields of query_info), each field
// of them the REST field's name in snake case. The page says of none what
// a job without such statistics holds; the emulator makes no query plan,
// timeline, index, materialized view rewrite or external call, so each
// job's REST statistics have none of them, and the columns are what a
// REST field left out reads as: an empty ARRAY for a repeated one, NULL
// for a RECORD (UNVERIFIED that BigQuery gives the same).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/information-schema-jobs
const jobsStatisticsColumns = ", job_stages ARRAY<STRUCT<name STRING, id INT64, start_ms INT64, end_ms INT64, " +
	"status STRING, parallel_inputs INT64, completed_parallel_inputs INT64, records_read INT64, records_written INT64, " +
	"shuffle_output_bytes INT64, shuffle_output_bytes_spilled INT64, slot_ms INT64>>, " +
	"object_storage_stats ARRAY<STRUCT<cloud_provider STRING, object_storage_bytes_read INT64, cache_bytes_read INT64>>, " +
	"timeline ARRAY<STRUCT<elapsed_ms INT64, total_slot_ms INT64, pending_units INT64, completed_units INT64, " +
	"active_units INT64, estimated_runnable_units INT64>>, " +
	"query_info STRUCT<resource_warning STRING, query_hashes STRUCT<normalized_literals STRING>, " +
	"performance_insights STRUCT<avg_previous_execution_ms INT64>>, " +
	"materialized_view_statistics STRUCT<materialized_view ARRAY<STRUCT<table_reference " + jobsTableRef +
	", chosen BOOL, estimated_bytes_saved INT64, rejected_reason STRING>>>, " +
	"metadata_cache_statistics STRUCT<table_metadata_cache_usage ARRAY<STRUCT<table_reference " + jobsTableRef +
	", unused_reason STRING, explanation STRING, table_type STRING>>>, " +
	"search_statistics STRUCT<index_usage_mode STRING, index_unused_reasons ARRAY<STRUCT<code STRING, message STRING, " +
	"base_table " + jobsTableRef + ", index_name STRING>>>, " +
	"vector_search_statistics STRUCT<index_usage_mode STRING, index_unused_reasons ARRAY<STRUCT<code STRING, " +
	"message STRING, base_table " + jobsTableRef + ", index_name STRING>>>, " +
	"external_service_costs ARRAY<STRUCT<external_service STRING, bytes_processed INT64, bytes_billed INT64, " +
	"slot_ms INT64, reserved_slot_count INT64>>"

const jobsTableRef = "STRUCT<project_id STRING, dataset_id STRING, table_id STRING>"

// jobsStatisticsValues are a job's values of jobsStatisticsColumns.
var jobsStatisticsValues = []string{"[]", "[]", "[]", "NULL", "NULL", "NULL", "NULL", "NULL", "[]"}

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
	"principal_subject STRING, transferred_bytes INT64, query_dialect STRING, continuous BOOL, " +
	"bi_engine_statistics STRUCT<bi_engine_mode STRING, acceleration_mode STRING, " +
	"bi_engine_reasons ARRAY<STRUCT<code STRING, message STRING>>>, session_info STRUCT<session_id STRING>, " +
	"continuous_query_info STRUCT<output_watermark TIMESTAMP>, ml_statistics STRUCT<model_type STRING>, " +
	"global_query_info STRUCT<parent_job_id STRING, parent_job_location STRING, remote_regions ARRAY<STRING>>, " +
	"copy_job_statistics STRUCT<copied_rows INT64, copied_logical_bytes INT64>" +
	jobsStatisticsColumns + ">"

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

// jobRow is one JOBS row of a Job resource jobs.list gave. settings are
// the project and organization configurations (projectoptions.go), or nil.
func jobRow(project string, job map[string]any, settings *projectOptions) []string {
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
			// No dialect asked: the default was the region's
			// default_sql_dialect_option when the job was made, legacy
			// SQL unless set (information-schema-jobs, read 2026-10-17;
			// #1595).
			dialect = sqlString("DEFAULT_LEGACY_SQL")
			if id, _ := ref["jobId"].(string); settings.googleDefaulted(project, id) {
				dialect = sqlString("DEFAULT_GOOGLE_SQL")
			}
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
	return append([]string{cache, millisTimestamp(stats["creationTime"]), tableRefSQL(dest), dml, millisTimestamp(stats["endTime"]),
		errResult, "NULL", sqlString(str(ref, "jobId")), sqlOrNull(jobType),
		"ARRAY<STRUCT<key STRING, value STRING>>[" + strings.Join(labels, ", ") + "]", sqlOrNull(str(stats, "parentJobId")),
		priority, sqlString(project), "NULL", sqlOrNull(str(qc, "query")), tableRefsSQL(qs["referencedTables"]),
		tableRefsSQL(nil), "NULL", "ARRAY<STRING>[]", "NULL", millisTimestamp(stats["startTime"]), sqlOrNull(state),
		sqlOrNull(str(qs, "statementType")), int64Or(qs["totalBytesBilled"]), int64Or(bytesProcessed), "NULL",
		int64Or(stats["totalSlotMs"]), "NULL", tx, sqlOrNull(str(job, "user_email")), "NULL", "NULL", dialect, "FALSE",
		// bi_engine_statistics: "If the project is configured to use the BI
		// Engine, then this field contains BiEngineStatistics. Otherwise
		// NULL"; an instance has no BI Engine. continuous_query_info: no
		// job is continuous (continuous FALSE). ml_statistics.model_type:
		// "For all other jobs" than a model's creation "the value is NULL"
		// (#1458).
		"NULL", sessionInfoSQL(stats), "NULL", modelTypeSQL(qs),
		// global_query_info: "Information about a global query"; an
		// instance runs every query in the region it names, so no job is
		// one (NULL, UNVERIFIED that BigQuery gives NULL, docs re-read 2026-11-17).
		"NULL", copyStatisticsSQL(stats)}, statisticsValues(job, qc, qs, cache)...)
}

// statisticsValues are a job's values of jobsStatisticsColumns: those of
// jobsStatisticsValues, but query_info, which holds query_hashes for "a
// successful GoogleSQL query that is not a cache hit" (#1458).
func statisticsValues(job, qc, qs map[string]any, cache string) []string {
	v := append([]string(nil), jobsStatisticsValues...)
	status, _ := job["status"].(map[string]any)
	legacy, _ := qc["useLegacySql"].(bool)
	if qc == nil || legacy || jobStateOf(job) != "DONE" || status["errorResult"] != nil || cache == "TRUE" {
		return v
	}
	if h := normalizedLiteralsHash(str(qc, "query")); h != "" {
		v[3] = "STRUCT(CAST(NULL AS STRING) AS resource_warning, STRUCT(" + sqlString(h) +
			" AS normalized_literals) AS query_hashes, CAST(NULL AS STRUCT<avg_previous_execution_ms INT64>) AS performance_insights)"
	}
	return v
}

// normalizedLiteralsHash is JOBS' query_info.query_hashes.normalized_literals
// of a query's text: "a hexadecimal STRING hash that ignores comments,
// parameter values, UDFs, and literals" (information-schema-jobs, read
// 2026-11-17), so that queries alike but for those hash the same (the
// page's example finds "the job IDs with the same query hash as a
// specific job"). The page names neither the hash function nor how the
// text is normalized, so CloudBurrow's is its own: the SHA-256, in hex, of
// the query's tokens (comments and spacing gone, by the front's lexer),
// with each string, bytes, number, TRUE, FALSE and NULL literal and each
// query parameter written ?, keywords in capitals. UNVERIFIED, docs re-read 2026-11-17:
// BigQuery's value differs; it is "ignores UDFs" and "will differ when
// underlying views change" that CloudBurrow does not follow (a view's or
// a table's schema plays no part). "" when the text does not lex.
func normalizedLiteralsHash(sql string) string {
	t, ok := lex(sql)
	if !ok || len(t) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		x := t[i]
		w := x.text
		switch {
		case x.kind == tokString:
			w = "?"
		case x.kind == tokWord && (w[0] >= '0' && w[0] <= '9' || x.is("TRUE") || x.is("FALSE") || x.is("NULL")):
			w = "?"
			// A number's fraction (3.5) lexes as three tokens.
			if w0 := x.text[0]; w0 >= '0' && w0 <= '9' && i+2 < len(t) && t[i+1].punct(".") && t[i+1].pos == x.end &&
				t[i+2].kind == tokWord && t[i+2].pos == t[i+1].end && t[i+2].text[0] >= '0' && t[i+2].text[0] <= '9' {
				i += 2
			}
		case x.kind == tokPunct && (w == "@" || w == "?"):
			w = "?"
			if x.text == "@" && i+1 < len(t) && t[i+1].kind == tokWord && t[i+1].pos == x.end {
				i++
			}
		case x.kind == tokWord && reservedWords[strings.ToUpper(w)]:
			w = strings.ToUpper(w)
		case x.kind == tokQuoted:
			w = "`" + w + "`"
		}
		b.WriteString(w)
		b.WriteByte(' ')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// copyStatisticsSQL is JOBS' copy_job_statistics of a job's statistics,
// "Statistics for a copy job" typed by REST CopyJobStatistics (copiedRows,
// copiedLogicalBytes, in snake case); NULL for a job that is not a copy.
func copyStatisticsSQL(stats map[string]any) string {
	c, _ := stats["copy"].(map[string]any)
	if c == nil {
		return "NULL"
	}
	return "STRUCT(" + castInt64(c["copiedRows"]) + " AS copied_rows, " + castInt64(c["copiedLogicalBytes"]) + " AS copied_logical_bytes)"
}

// castInt64 is int64Or typed, so a NULL is an INT64 in a STRUCT literal.
func castInt64(v any) string {
	return "CAST(" + int64Or(v) + " AS INT64)"
}

// sessionInfoSQL is JOBS' session_info of a job's statistics: its
// sessionInfo.sessionId (REST SessionInfo), NULL for a job in no session.
func sessionInfoSQL(stats map[string]any) string {
	si, _ := stats["sessionInfo"].(map[string]any)
	if id := str(si, "sessionId"); id != "" {
		return "STRUCT(" + sqlString(id) + " AS session_id)"
	}
	return "NULL"
}

// modelTypeSQL is JOBS' ml_statistics of a query job's statistics: its
// mlStatistics.modelType, NULL for a job that made no model.
func modelTypeSQL(qs map[string]any) string {
	ml, _ := qs["mlStatistics"].(map[string]any)
	if t := str(ml, "modelType"); t != "" {
		return "STRUCT(" + sqlString(t) + " AS model_type)"
	}
	return "NULL"
}

// jobsRows returns the subquery of the project's jobs, or the status and
// message of why it cannot.
func (f front) jobsRows(r *http.Request, filter *jobsFilter) (string, int, string) {
	project := projectOf(f.base)
	// Nothing pushed down, or a table already made, which an incremental
	// sync brings up to date (jobstable.go, #1378, #1559): the table of
	// rows, which the query's WHERE filters as it filters a literal. A
	// filtered query read a list of every job, which the engine did not
	// cut (bigquery-emulator patch 0060 cuts it by creation time alone),
	// so it cost what the project's jobs did (1.0 s at 10,000, #1564);
	// reading the table costs what the jobs changed since the last sync
	// do.
	if filter == nil || f.jobsTable.isMade(project) {
		if sub := f.everyJobRows(r, project); sub != "" {
			return sub, 0, ""
		}
	}
	listed := &engineList{}
	cut := filter.bounded()
	if ids, after := filter.jobIDs(), f.newest.get(project); ids != nil && after != "" {
		// A query that names its jobs (job_id = or IN) lists those and
		// the jobs the engine made since the newest it listed
		// (joblisting.go), not every job (#1633): what the list costs
		// grew with the project's jobs (0.36 s at 3,600, six such
		// queries a run of TestBigQueryInformationSchemaJobs). The filter
		// cuts them as it cuts a list of every job, so the rows are the
		// same.
		listed = &engineList{some: true, after: after, ids: ids}
		cut = true
	} else if l, ok := f.parentListing(project, filter); ok {
		listed, cut = l, true
	}
	by := filter // what the list is cut by
	if !listed.some && filter.onlyOpen() {
		// A query of jobs not DONE (state = 'PENDING' or 'RUNNING') lists
		// those the front keeps and the ones made since its cursor
		// (jobsopen.go, #1633), not every job; with nothing kept, every
		// job, uncut, which the front keeps them from.
		by, cut = nil, false
		if l, ok := f.open.listing(project); ok {
			listed, cut = l, true
		}
	}
	jobs, status, msg := f.listedJobs(r, by, listed)
	if status == 0 && listed.invalid {
		// The engine's job of the cursor's ID is another: it restarted
		// while the front was down (jobsopen.go).
		listed, cut = &engineList{}, false
		jobs, status, msg = f.listedJobs(r, nil, listed)
	}
	if status != 0 {
		return "", status, msg
	}
	kept := filter.apply(project, jobs)
	if filter != nil && len(kept) > jobsLiteralMax {
		// Too many for a literal (#1515): the table, which the query's
		// WHERE filters as it filters the literal. Its rows are every
		// job's, so a list cut by creation_time or job ID is read again
		// whole.
		if cut {
			listed = &engineList{}
			jobs, status, msg = f.listedJobs(r, nil, listed)
			if status != 0 {
				return "", status, msg
			}
		}
		if sub := f.lockedTableRows(r, project, jobs, listed.last); sub != "" {
			return sub, 0, ""
		}
	}
	if f.jobsTable != nil && len(kept) > jobsLiteralMax {
		// The table could not be written, twice: a literal of so many
		// rows is a statement too large for the engine (#1553).
		return "", http.StatusInternalServerError, fmt.Sprintf("cloudburrow: could not write the %d rows of INFORMATION_SCHEMA.JOBS", len(kept))
	}
	jobs = kept
	rows := make([][]string, len(jobs))
	for i, job := range jobs {
		rows[i] = jobRow(project, job, f.settings)
	}
	return rowsSubquery(jobsColumns, rows), 0, ""
}

// parentListing is the list a query that keeps the children of some jobs
// (parent_job_id = or IN) sends (#1633): of the children the front
// recorded of them (jobRecords.childrenOf), which alone have a
// parent_job_id, and of the jobs made since the newest the engine listed
// (newestJobs); ok is false when no newest job is known, or the children
// are too many to name, and every job is listed.
func (f front) parentListing(project string, filter *jobsFilter) (*engineList, bool) {
	parents := filter.parentIDs()
	after := f.newest.get(project)
	if parents == nil || after == "" {
		return nil, false
	}
	ids := f.records.childrenOf(project, parents)
	if len(ids) > jobIDsMax {
		return nil, false
	}
	sort.Strings(ids)
	if ids == nil {
		ids = []string{}
	}
	return &engineList{some: true, after: after, ids: ids}, true
}

// tableRowsOrRetry is tableJobsRows of every job, made first if need be,
// and once more if that fails: a failed write forgets the table (sync),
// so the second makes it anew. A query reads the literal of its jobs
// only when both fail, and then only of jobsLiteralMax jobs at most: a
// literal of every job is compiled whole, and with thousands it did not
// fit the engine's WebAssembly heap (#1553).
func (f front) tableRowsOrRetry(r *http.Request, project string, jobs []map[string]any, last string) string {
	if sub := f.tableJobsRows(r, project, jobs, last, nil, true); sub != "" {
		return sub
	}
	return f.tableJobsRows(r, project, jobs, last, nil, true)
}

// jobsLiteralMax is the most jobs a filtered query of JOBS reads as a
// literal of rows (jobsRows); past it, the query reads the table of rows
// (jobstable.go), as an unfiltered one does (#1515). Measured on one
// instance with 2,100 jobs (--services bigquery,storage, arm64,
// 2026-10-17): a literal costs the engine about 2 ms a row (a COUNT(*)
// WHERE job_type = 'QUERY' of 2,100 took 4.0 to 4.8 s), the table about
// 0.3 s whatever its rows, once written; a literal of 100 rows takes
// about 0.2 s.
var jobsLiteralMax = 100

// allJobs lists the project's jobs, top-level and children, within the
// bounds of creation_time filter gives, or the status and message of why
// it cannot.
func (f front) allJobs(r *http.Request, filter *jobsFilter) ([]map[string]any, int, string) {
	return f.listedJobs(r, filter, &engineList{})
}

// everyJobList lets one list of every job (listedJobs) run at a time
// (#1632). Each decoded the engine's whole list in each layer of the
// jobs.list path (now once, joblisting.go, #1639): measured natively at 10,000 jobs (arm64, 2026-10-18),
// one INFORMATION_SCHEMA.JOBS query took the front's heap from about 25
// to 100 MB and allocated about 800 MB, its live heap otherwise 57 MB, so
// the filtered JOBS queries a client sends at once, each listing every
// job, held several such lists together: the front's memory reached
// 326 MiB, past its 256 MiB GOMEMLIMIT (components.FrontGoMemLimitMiB),
// with what it keeps (job texts, configurations, the JOBS table's
// digests, the mapped job errors) a fraction of that. A list of some jobs
// (a sync's, jobstable.go) is small and is not held back.
var everyJobList = make(chan struct{}, 1)

// listedJobs is allJobs, of the engine's jobs some names (engineListing,
// jobstable.go), and the front's own; some has the engine's last job
// after.
func (f front) listedJobs(r *http.Request, filter *jobsFilter, some *engineList) ([]map[string]any, int, string) {
	if f.listJobs == nil {
		return nil, http.StatusInternalServerError, "cloudburrow: the job list cannot be read here"
	}
	if some == nil || !some.some {
		select {
		case everyJobList <- struct{}{}:
			defer func() { <-everyJobList }()
		case <-r.Context().Done():
			return nil, http.StatusServiceUnavailable, "cloudburrow: the job list was not read: " + r.Context().Err().Error()
		}
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
		// Children too (#1353).
		req := r.Clone(context.WithValue(withAllJobLevels(r.Context()), engineListKey{}, some))
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
		// Decoded once, never written (joblisting.go, #1639).
		l := f.listJobs(req)
		if l.list == nil {
			return nil, http.StatusInternalServerError, "cloudburrow: could not list the jobs: " + errorMessage(l.raw.body.Bytes(), l.raw.status)
		}
		items := l.jobs()
		if jobs == nil {
			jobs = make([]map[string]any, 0, len(items))
		}
		for _, item := range items {
			if job, ok := item.(map[string]any); ok {
				jobs = append(jobs, job)
			}
		}
		next, _ := l.list["nextPageToken"].(string)
		if next == "" || next == token {
			break
		}
		token = next
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
		// The datasets and tables the query can read (#1510,
		// infoschemanames.go), read with their own datasetMeta.
		only := pushedNameFilter(q.Query, ref.pos, ref.end, ref.alias, ref.view)
		m := metaFor(meta, ref.view, only)
		sub, ok := subs[key+only.key()]
		if !ok {
			var status int
			var msg string
			switch {
			case key == "JOBS":
				sub, status, msg = f.jobsRows(r, pushedJobsFilter(q.Query, refs, f.now()))
			case projectViews[key] != nil: // #1375, projectoptions.go
				sub = projectViews[key](f, location)
			case hierarchyJobsViews[key]: // #1392, infoschemahierarchy.go
				sub, status, msg = f.hierarchyJobsRows(r, key, pushedJobsFilter(q.Query, refs, f.now()))
			case key == "TABLE_STORAGE":
				sub, status, msg = f.regionRows(r, m, "TABLE_STORAGE", storageColumns, inRegion(location, front.storageRows))
			default: // the region's datasets alone (#1375, projectoptions.go)
				sub, status, msg = f.regionRows(r, m, key, "", inRegion(location, infoSchemaBuilders[key]))
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
			subs[key+only.key()] = sub
		}
		b.WriteString(q.Query[last:ref.pos])
		if optimizationDetailsViews[ref.view] {
			b.WriteString(withOptimizationDetails(sub))
		} else {
			b.WriteString(sub)
		}
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
	f.runQuery(rec, withMetadataReads(r), q, insert) // #1763, tableoptions.go
	f.clientText(w, rec, client)
}

// optimizationDetailsViews are the JOBS views whose query_info has
// optimization_details: "Only the JOBS_BY_PROJECT view has this column",
// and "INFORMATION_SCHEMA.JOBS and INFORMATION_SCHEMA.JOBS_BY_PROJECT are
// synonymous and can be used interchangeably" (information-schema-jobs,
// read 2026-11-18), so JOBS_BY_USER, JOBS_BY_FOLDER and
// JOBS_BY_ORGANIZATION have none (#1458).
var optimizationDetailsViews = map[string]bool{"JOBS": true, "JOBS_BY_PROJECT": true}

// withOptimizationDetails is a JOBS subquery (sub, of jobsColumns) with
// query_info.optimization_details, the job's history-based optimizations
// (REST QueryInfo.optimizationDetails, "object (Struct format)"). It is
// JSON: the history-based-optimizations page (read 2026-11-18) speaks of
// "The JSON in optimization_details" and reads it with
// JSON_QUERY_ARRAY(query_info.optimization_details.optimizations); its
// schema row says STRUCT and names no field, and a STRUCT field passed to
// JSON_QUERY_ARRAY would have to be JSON or STRING. "If no history-based
// optimizations were applied, NULL is produced for optimization_details",
// and the emulator applies none, so it is NULL; query_info is NULL as it
// was for a job with no query_info (#1458).
func withOptimizationDetails(sub string) string {
	return "(SELECT * REPLACE (IF(query_info IS NULL, NULL, STRUCT(query_info.resource_warning AS resource_warning, " +
		"query_info.query_hashes AS query_hashes, query_info.performance_insights AS performance_insights, " +
		"CAST(NULL AS JSON) AS optimization_details)) AS query_info) FROM " + sub + ")"
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

// datasetIDs lists the project's datasets, but the front's own
// (frontDataset): the region views had the rows of _cloudburrow_dropped's
// copies of deleted tables (dropped.go), which no client made, and read
// every one of them (#1559). Measured on an instance late in the CI
// emulators shard's run (391 copies, 2026-10-17): a query of region
// OBJECT_PRIVILEGES took 7.7 s, reading each copy's tables.get, and the
// test's four took 30 s on this machine, more than its minute on CI.
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
			if id := d.DatasetReference.DatasetID; !frontDataset(id) {
				out = append(out, id) // not the front's own (#1559)
			}
		}
		if list.NextPageToken == "" || list.NextPageToken == token {
			break
		}
		token = list.NextPageToken
	}
	sort.Strings(out)
	return out, 0, ""
}

// regionRows is the subquery of view's rows of every dataset meta reads
// (all of them, or those its filter keeps).
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
		if !meta.only.schema(ds) {
			continue // no row of it can pass the query's WHERE (#1510)
		}
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
// (https://cloud.google.com/bigquery/docs/information-schema-table-storage,
// read again 2026-11-17), in the page's order. The logical ones and
// storage_last_modified_time: storageBytes (#1458). The physical ones are
// estimates (physicalBytes, #1458): BigQuery's are compressed sizes, which
// no value defines and the engine does not keep (UNVERIFIED, docs re-read 2026-11-17). project_number is NULL: an instance has none.
// total_partitions, "the number of partitions present in the table" (0
// for a table not partitioned), counts the partitions PARTITIONS has a
// row for (#1458): those holding rows, with __NULL__ and
// __UNPARTITIONED__ (whether BigQuery counts those two, and a streaming
// buffer's rows, is UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/information-schema-table-storage
const storageColumns = "STRUCT<project_id STRING, project_number INT64, table_catalog STRING, table_schema STRING, " +
	"table_name STRING, creation_time TIMESTAMP, total_rows INT64, total_partitions INT64, total_logical_bytes INT64, " +
	"active_logical_bytes INT64, long_term_logical_bytes INT64, current_physical_bytes INT64, total_physical_bytes INT64, " +
	"active_physical_bytes INT64, long_term_physical_bytes INT64, time_travel_physical_bytes INT64, " +
	"storage_last_modified_time TIMESTAMP, deleted BOOL, table_type STRING, " +
	"managed_table_type STRING, fail_safe_physical_bytes INT64, last_metadata_index_refresh_time TIMESTAMP, " +
	"table_deletion_reason STRING, table_deletion_time TIMESTAMP>"

// totalPartitions is TABLE_STORAGE's total_partitions of table t (above):
// the distinct partition IDs of its rows, as PARTITIONS reads them
// (partitionIDExpr), or 0 for a table not partitioned.
func (f front) totalPartitions(r *http.Request, project, dataset string, t infoTable) (int64, int, string) {
	raw, err := json.Marshal(t)
	var pm partitionMeta
	if err != nil || json.Unmarshal(raw, &pm) != nil {
		return 0, http.StatusInternalServerError, "cloudburrow: could not read a table's partitioning"
	}
	name := str(t, "tableId")
	id, ok := partitionIDExpr(pm)
	if !ok {
		return 0, http.StatusInternalServerError, "cloudburrow: could not read the range partitioning of " + name
	}
	if id == "" {
		return 0, 0, ""
	}
	n, msg := f.scalar(r, "SELECT COUNT(DISTINCT "+id+") FROM `"+project+"."+dataset+"."+name+"`")
	if msg != "" {
		return 0, http.StatusInternalServerError, "cloudburrow: could not count the partitions of " + name + ": " + msg
	}
	return n, 0, ""
}

// storageBytes is TABLE_STORAGE's total_logical_bytes,
// active_logical_bytes, long_term_logical_bytes,
// storage_last_modified_time and the logical bytes held for time travel
// and fail-safe of table t, as SQL (#1458). The logical bytes are the
// rows' documented sizes (logicalbytes.go); a snapshot's and a clone's "as
// if they were complete tables". Data is long-term once unmodified for 90
// days: a table made less than 90 days ago (by the front's clock) has
// none, and an older one's bytes are split by each partition's last write
// (tierBytes, partitionlastwrites.go), NULL where one is unknown.
// storage_last_modified_time is "the most recent time that data was
// written to the table", the latest write record the engine holds
// (allWrites), and NULL "if no data exists", or when it holds none.
func (f front) storageBytes(r *http.Request, project, dataset string, t infoTable, hours int64) (s storageSizes, st int, msg string) {
	raw, err := json.Marshal(t)
	var pm partitionMeta
	if err != nil || json.Unmarshal(raw, &pm) != nil {
		return s, http.StatusInternalServerError, "cloudburrow: could not read a table's schema"
	}
	name := str(t, "tableId")
	path := quotePath([]string{project, dataset, name})
	s = storageSizes{"NULL", "NULL", "NULL", "NULL", "NULL", "NULL"}
	n := int64(0)
	e := f.tableBytesExpr(r, path, pm.Schema.Fields)
	if e != "" {
		var m string
		if n, m = f.scalar(r, "SELECT IFNULL(SUM("+e+"), 0) FROM "+path); m != "" {
			return s, http.StatusInternalServerError, "cloudburrow: could not count the bytes of " + name + ": " + m
		}
		s.logical = strconv.FormatInt(n, 10)
	}
	if s.logical != "NULL" {
		if ms, err := strconv.ParseInt(str(t, "creationTime"), 10, 64); err == nil && f.now().Sub(time.UnixMilli(ms)) < 90*24*time.Hour {
			s.active, s.long = s.logical, "0"
		} else {
			s.active, s.long = f.tierBytes(r, project, dataset, name, pm, e, hours)
		}
	}
	rows, _ := strconv.ParseInt(str(t, "numRows"), 10, 64)
	if c := f.allWrites(r, project, dataset, name, pm, "CAST(NULL AS STRING)"); c != "" && (rows > 0 || n > 0) {
		if us, m := f.scalar(r, "SELECT IFNULL(UNIX_MICROS(MAX(t)), -1) FROM "+c); m == "" && us >= 0 {
			s.modified = "TIMESTAMP_MICROS(" + strconv.FormatInt(us, 10) + ")"
		}
	}
	if e != "" && e == logicalBytesExpr(pm.Schema.Fields) {
		if c := f.changeWrites(r, project, dataset, name, false); c != "" {
			if b, m := f.scalar(r, "SELECT "+timeTravelBytesExpr(pm.Schema.Fields, hours)+" FROM "+c); m == "" {
				s.travel = strconv.FormatInt(b, 10)
			}
			all := c
			if k := f.keptWrites(r, project, dataset, name, false); k != "" {
				all = "(SELECT * FROM " + c + " UNION ALL SELECT * FROM " + k + ")"
			}
			if b, m := f.scalar(r, "SELECT "+failSafeBytesExpr(e, hours)+" FROM "+all); m == "" {
				s.failSafe = strconv.FormatInt(b, 10)
			}
		}
	}
	return s, 0, ""
}

// storageSizes are a table's TABLE_STORAGE logical bytes, as SQL, each
// "NULL" when unknown: total, active and long-term, the
// storage_last_modified_time, and the logical bytes of its rows deleted or
// replaced within the time travel window and in the fail-safe period after
// it.
type storageSizes struct {
	logical, active, long, modified, travel, failSafe string
}

// timeTravelBytesExpr is the SQL, over a table's change table, of the
// logical bytes of its rows deleted or replaced within the last hours of
// time travel (#1458): a change's DELETE row holds the row as it was
// (googlesqlite patch 0036), what BigQuery keeps for time travel
// ("deleted or changed data").
func timeTravelBytesExpr(fields []field, hours int64) string {
	return "IFNULL(SUM(IF(_CHANGE_TYPE = 'DELETE' AND _CHANGE_TIMESTAMP >= TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL " +
		strconv.FormatInt(hours, 10) + " HOUR), " + logicalBytesExpr(fields) + ", 0)), 0)"
}

// physicalBytes are TABLE_STORAGE's current_physical_bytes,
// total_physical_bytes, active_physical_bytes, long_term_physical_bytes,
// time_travel_physical_bytes and fail_safe_physical_bytes, as SQL, of a
// table's storageSizes, each NULL when unknown (#1458). BigQuery's
// physical bytes are compressed sizes, which no value defines and the
// engine does not keep; CloudBurrow estimates them as the logical bytes,
// uncompressed, as tables.get's numBytes is (logicalbytes.go): current the
// table's, time travel its deleted and replaced rows' within the dataset's
// time travel window, total and active the two added ("including
// time-travel (deleted or changed data) bytes"), long-term the long-term
// logical bytes, and fail-safe the deleted and replaced rows' of the seven
// days after the window ("deleted or changed data retained after the
// time-travel window", not in total; the engine keeps them,
// partitionlastwrites.go). UNVERIFIED, docs re-read 2026-11-17: estimates,
// not BigQuery's compressed sizes
// (https://docs.cloud.google.com/bigquery/docs/information-schema-table-storage).
func physicalBytes(s storageSizes) []string {
	sum := func(a, b string) string {
		x, err1 := strconv.ParseInt(a, 10, 64)
		y, err2 := strconv.ParseInt(b, 10, 64)
		if err1 != nil || err2 != nil {
			return "NULL"
		}
		return strconv.FormatInt(x+y, 10)
	}
	failSafe := s.failSafe
	if s.logical == "NULL" {
		failSafe = "NULL"
	}
	return []string{s.logical, sum(s.logical, s.travel), sum(s.active, s.travel), s.long, s.travel, failSafe}
}

// storageAtOnce is totalPartitions and storageBytes of table t read by
// one query of the engine, not one a column (#1510): its partitions, its
// logical bytes, its last write and its time travel and fail-safe bytes.
// ok is false when that query fails (a column the engine keeps under
// another name, a table with no change or kept table) and when t is 90
// days old or more (its bytes are split by partition, tierBytes), and they
// are read one by one, as they say.
func (f front) storageAtOnce(r *http.Request, project, dataset string, t infoTable, hours int64) (parts int64, s storageSizes, ok bool) {
	raw, err := json.Marshal(t)
	var pm partitionMeta
	if err != nil || json.Unmarshal(raw, &pm) != nil {
		return 0, s, false
	}
	if ms, err := strconv.ParseInt(str(t, "creationTime"), 10, 64); err != nil || f.now().Sub(time.UnixMilli(ms)) >= 90*24*time.Hour {
		return 0, s, false
	}
	name := str(t, "tableId")
	id, ok := partitionIDExpr(pm)
	if !ok {
		return 0, s, false
	}
	path := quotePath([]string{project, dataset, name})
	count := "0"
	if id != "" {
		count = "(SELECT COUNT(DISTINCT " + id + ") FROM " + path + ")"
	}
	changes := quotePath([]string{project, dataset, name + changeTableSuffix})
	kept := quotePath([]string{project, dataset, name + keptTableSuffix})
	bytes := logicalBytesExpr(pm.Schema.Fields)
	got, msg := f.scalars(r, "SELECT "+count+", (SELECT IFNULL(SUM("+bytes+"), 0) FROM "+path+"), "+
		"(SELECT IFNULL(UNIX_MICROS(MAX(t)), -1) FROM (SELECT _CHANGE_TIMESTAMP AS t FROM "+changes+" WHERE _CHANGE_TYPE != 'START' "+
		"UNION ALL SELECT _CHANGE_TIMESTAMP FROM "+kept+")), "+
		"(SELECT "+timeTravelBytesExpr(pm.Schema.Fields, hours)+" FROM "+changes+"), "+
		"(SELECT "+failSafeBytesExpr(bytes, hours)+" FROM (SELECT * FROM "+changes+" UNION ALL SELECT * FROM "+kept+"))", 5)
	if msg != "" {
		return 0, s, false
	}
	n := got[1]
	s = storageSizes{strconv.FormatInt(n, 10), strconv.FormatInt(n, 10), "0", "NULL", strconv.FormatInt(got[3], 10), strconv.FormatInt(got[4], 10)}
	rows, _ := strconv.ParseInt(str(t, "numRows"), 10, 64)
	if us := got[2]; us >= 0 && (rows > 0 || n > 0) {
		s.modified = "TIMESTAMP_MICROS(" + strconv.FormatInt(us, 10) + ")"
	}
	return got[0], s, true
}

// storageRows are a dataset's TABLE_STORAGE rows: its tables, snapshots
// and clones (no view), none deleted (the instance keeps none).
func (f front) storageRows(r *http.Request, dataset string, m *datasetMeta) (string, int, string) {
	tables, st, msg := f.tablesOf(r, dataset, m)
	if st != 0 {
		return "", st, msg
	}
	project := projectOf(f.base)
	hours, _, ok := f.timeTravelHours(r, dataset)
	if !ok {
		hours = 168
	}
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
		parts, sz, ok := f.storageAtOnce(r, project, dataset, t, hours)
		if !ok {
			var st int
			var msg string
			if parts, st, msg = f.totalPartitions(r, project, dataset, t); st != 0 {
				return "", st, msg
			}
			if sz, st, msg = f.storageBytes(r, project, dataset, t, hours); st != 0 {
				return "", st, msg
			}
		}
		phys := physicalBytes(sz)
		row := []string{sqlString(project), "NULL", sqlString(project), sqlString(dataset), sqlString(str(t, "tableId")),
			millisTimestamp(t["creationTime"]), int64Or(t["numRows"]), strconv.FormatInt(parts, 10), sz.logical, sz.active, sz.long}
		row = append(row, phys[:5]...)
		rows = append(rows, append(row, sz.modified, "FALSE", sqlString(tt), managed, phys[5], "NULL", "NULL", "NULL"))
	}
	return rowsSubquery(storageColumns, rows), 0, ""
}
