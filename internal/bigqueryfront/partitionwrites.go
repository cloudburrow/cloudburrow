package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// tabledata.list of a partition, and a query job whose destination is a
// partition (#1273; reads in partitionreads.go).
//
// BigQuery browses one partition with a decorator (`bq head
// 'my_dataset.my_table$20180224'`, which is tabledata.list) and writes a
// query's result "to a specific partition" (`bq query
// --destination_table 'mydataset.mytable$20160501'`, appending with
// --append_table, overwriting the partition without it), where for a
// time-unit column- or integer-range-partitioned table "the partition ID
// specified in the decorator must match the data being written. Otherwise,
// an error occurs" (https://cloud.google.com/bigquery/docs/managing-partitioned-table-data,
// https://cloud.google.com/bigquery/docs/load-data-partitioned-tables).
// Measured first: the emulator answered "Table not found" for both.
//
// tabledata.list of `t$P` is answered from the engine: the partition's
// rows (partitionFilter) are read by a query of the table, in the table's
// order, a page at a time (startIndex, maxResults, pageToken), with the
// columns selectedFields names (a nested one too, selectedfields.go).
//
// tables.get of `t$P` answers the table's resource, which `bq head` and
// the client libraries read the schema of before tabledata.list, with the
// decorated tableId and numRows the partition's (UNVERIFIED: BigQuery does
// not document what it answers); numBytes and the other storage counts,
// which the emulator does not keep per partition, are left out.
//
// A query job into `t$P` runs its query into a scratch table first, as
// queryWrite does, so a query that fails changes nothing. Then every row
// of the result must be in P, or the job fails, reason invalid, the table
// unchanged (BigQuery's wording UNVERIFIED); the result's columns must be
// the table's, or those schemaUpdateOptions make them (partitionSchema,
// below; #1273). WRITE_APPEND adds the rows; WRITE_TRUNCATE (and
// WRITE_TRUNCATE_DATA, the same for one partition: the table's schema is
// kept) replaces the partition's rows, and only those, in one
// script (DELETE of P's rows, INSERT of the result), which the emulator
// runs in one transaction (querywrite.go); WRITE_EMPTY (the default) fails
// the job, reason duplicate, when P holds rows. The job jobs.get reports
// names the client's destination (jobText). `$__NULL__` and
// `$__UNPARTITIONED__` of a column- or range-partitioned table take the
// rows of that partition only. 501: `$__UNPARTITIONED__` of an
// ingestion-time partitioned table (its streaming buffer,
// streamingbuffer.go; BigQuery documents no write to it), a destination
// of another project or that does not exist. A query that is not one
// SELECT is refused, as BigQuery refuses a destination for a DML statement
// ("You cannot specify a destination table for the query",
// https://cloud.google.com/bigquery/docs/updating-data) or a script
// (destination_table "cannot be set for a multi-statement query",
// https://cloud.google.com/bigquery/docs/multi-statement-queries); the
// wording of both errors is UNVERIFIED. A dry run is the query's own,
// without its destination (partitionDryRun). Into a partition of an ingestion-time partitioned table, the
// rows are not checked: they are written with the partition's
// _PARTITIONTIME (and _PARTITIONDATE), as a load or a copy into one
// writes them (ingestionColumns, #1277).

// partitionRows answers tabledata.list of partition dec of base, whose
// rows hold filter.
func (f front) partitionRows(w http.ResponseWriter, r *http.Request, dataset, base, filter string, m partitionMeta) {
	q := r.URL.Query()
	// The partition's columns, all of them, kept to those selectedFields
	// names (a.b for a nested field) as tabledata.list of a table keeps
	// them (#1038, selectedfields.go).
	cols := "*"
	sel, err := parseSelectedFields(m.Schema.Fields, q.Get("selectedFields"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	start := int64(0)
	for _, key := range []string{"startIndex", "pageToken"} {
		if v := q.Get(key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "invalid", "Invalid "+key+": "+v) // UNVERIFIED wording
				return
			}
			start = n
		}
	}
	size := int64(100000)
	if v := q.Get("maxResults"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n < size {
			size = n
		}
	}
	from := quotePath([]string{projectOf(f.base), dataset, base}) + " WHERE " + filter
	total, msg := f.scalar(r, "SELECT COUNT(*) FROM "+from)
	if msg != "" {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not read the partition: "+msg)
		return
	}
	req := map[string]any{"query": fmt.Sprintf("SELECT %s FROM %s LIMIT %d OFFSET %d", cols, from, size, start),
		"useLegacySql": false}
	if v := q.Get("formatOptions.useInt64Timestamp"); v != "" {
		req["formatOptions"] = map[string]any{"useInt64Timestamp": v == "true"}
	}
	body, _ := json.Marshal(req)
	status, got := f.send(r, http.MethodPost, "/queries", body)
	var res struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &res) != nil {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not read the partition: "+
			errorMessage(got, status))
		return
	}
	out := map[string]any{"kind": "bigquery#tableDataList", "totalRows": strconv.FormatInt(total, 10)}
	if res.Rows != nil {
		out["rows"] = selectRows(m.Schema.Fields, sel, res.Rows)
	}
	if next := start + int64(len(res.Rows)); next < total && len(res.Rows) > 0 {
		out["pageToken"] = strconv.FormatInt(next, 10)
	}
	writeJSON(w, http.StatusOK, out)
}

// partitionTable answers tables.get of partition table (decorated) of
// base, whose rows hold filter (above).
func (f front) partitionTable(w http.ResponseWriter, r *http.Request, dataset, base, table, filter string) {
	status, got := f.get(r, tablePath(dataset, base))
	res, ok := decodeMap(got)
	if status != http.StatusOK || !ok {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not read the table: "+
			errorMessage(got, status))
		return
	}
	n, msg := f.scalar(r, "SELECT COUNT(*) FROM "+quotePath([]string{projectOf(f.base), dataset, base})+" WHERE "+filter)
	if msg != "" {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not read the partition: "+msg)
		return
	}
	res["numRows"] = strconv.FormatInt(n, 10)
	for _, k := range []string{"numBytes", "numLongTermBytes", "numPhysicalBytes", "numTotalLogicalBytes",
		"numActiveLogicalBytes", "numLongTermLogicalBytes", "numTotalPhysicalBytes", "numActivePhysicalBytes",
		"numLongTermPhysicalBytes", "numTimeTravelPhysicalBytes", "streamingBuffer", "etag"} {
		delete(res, k)
	}
	if ref, ok := res["tableReference"].(map[string]any); ok {
		ref["tableId"] = table
	}
	if id, ok := res["id"].(string); ok && strings.HasSuffix(id, "."+base) {
		res["id"] = id + table[len(base):]
	}
	writeJSON(w, http.StatusOK, res)
}

// scalar runs sql, a query of one INT64 value, through jobs.query.
func (f front) scalar(r *http.Request, sql string) (int64, string) {
	legacy := false
	body, err := json.Marshal(queryOptions{Query: sql, UseLegacySQL: &legacy})
	if err != nil {
		return 0, err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	var res struct {
		Rows []struct {
			F []struct {
				V string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if status != http.StatusOK {
		return 0, errorMessage(got, status)
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 {
		return 0, "could not read the count"
	}
	n, err := strconv.ParseInt(res.Rows[0].F[0].V, 10, 64)
	if err != nil {
		return 0, "could not read the count"
	}
	return n, ""
}

// partitionQueryWrite carries out a query job whose destination is a
// partition (above), and reports whether it answered w; false when the
// destination is not decorated.
func (f front) partitionQueryWrite(w http.ResponseWriter, r *http.Request, job jobBody) bool {
	c := job.Configuration.Query
	dest := c.DestinationTable
	if dest == nil || !strings.Contains(dest.TableID, "$") || strings.Index(dest.TableID, "$") == 0 {
		return false
	}
	i := strings.Index(dest.TableID, "$")
	base, dec := dest.TableID[:i], dest.TableID[i+1:]
	project := projectOf(f.base)
	name := fmt.Sprintf("%s:%s.%s", project, dest.DatasetID, dest.TableID)
	write := strings.ToUpper(c.WriteDisposition)
	notImplemented := func(why string) bool {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a query job whose destination "+
			"is the partition "+dest.DatasetID+"."+dest.TableID+", "+why+" (docs/compatibility.md). Nothing was run.")
		return true
	}
	if dest.ProjectID != "" && dest.ProjectID != project {
		return notImplemented("of another project")
	}
	m, ok := f.partitionMetaOf(r, dest.DatasetID, base)
	if !ok {
		return notImplemented("of a table that does not exist")
	}
	filter, code, msg := partitionFilter(m, dec, name)
	if code != 0 {
		writeError(w, code, reasonOf(code, "invalid"), msg)
		return true
	}
	if write == "WRITE_TRUNCATE_DATA" {
		write = "WRITE_TRUNCATE" // a partition's rows only; the table's schema is kept either way
	}
	switch {
	case m.ingestion() && dec == "__UNPARTITIONED__":
		return notImplemented("$__UNPARTITIONED__ of an ingestion-time partitioned table, which holds its streaming buffer")
	case write != "" && write != "WRITE_EMPTY" && write != "WRITE_APPEND" && write != "WRITE_TRUNCATE":
		return notImplemented("with writeDisposition " + write)
	}
	if why := notOneQuery(c.Query); why != "" {
		writeError(w, http.StatusBadRequest, "invalid", why)
		return true
	}
	if dryRun(r, true) {
		return f.partitionDryRun(w, r, c.queryOptions, c.WriteDisposition, dest)
	}
	lq, _, _, _ := bytesParameters(c.queryOptions) // as runQuery sends them (#1078)
	opts := c.SchemaUpdateOptions
	if write == "" || write == "WRITE_EMPTY" {
		opts = nil // schemaUpdateOptions are for WRITE_APPEND and WRITE_TRUNCATE (above)
	}
	if result, status, _ := f.lone(r, lq, lq.Query); status == http.StatusOK && !sameSchema(result, m.Schema.Fields) {
		if _, _, _, why := partitionSchema(m.Schema.Fields, result, opts, name); why != "" {
			return notImplemented(why)
		}
	}
	scratch := tableRef{ProjectID: project, DatasetID: dest.DatasetID, TableID: scratchTable()}
	if !editJob(r, func(job map[string]any) bool {
		conf, _ := job["configuration"].(map[string]any)
		q, _ := conf["query"].(map[string]any)
		if q == nil {
			return false
		}
		q["destinationTable"] = map[string]any{"projectId": scratch.ProjectID, "datasetId": scratch.DatasetID,
			"tableId": scratch.TableID}
		q["writeDisposition"] = "WRITE_EMPTY"
		q["createDisposition"] = "CREATE_IF_NEEDED"
		return true
	}) {
		return notImplemented("which CloudBurrow could not send")
	}
	defer f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
	client := jobText{dest: dest, queryWrite: c.WriteDisposition}
	rec := newRecorder()
	f.runQuery(rec, r, c.queryOptions, true)
	if b := rec.body.Bytes(); bytes.Contains(b, []byte(scratch.TableID)) {
		b = bytes.ReplaceAll(b, []byte(scratch.TableID), []byte(dest.TableID))
		rec.body.Reset()
		rec.body.Write(b)
	}
	var resp map[string]any
	if json.Unmarshal(rec.body.Bytes(), &resp) != nil {
		f.clientJob(w, rec, client)
		return true
	}
	if _, failed := queryFailure(rec, resp); failed {
		f.clientJob(w, rec, client)
		return true
	}
	e := rowError{Reason: "backendError"}
	sp := quotePath([]string{project, scratch.DatasetID, scratch.TableID})
	bp := quotePath([]string{project, dest.DatasetID, base})
	var fields tableSchema
	var sm struct {
		Schema json.RawMessage `json:"schema"`
	}
	sstatus, sgot := f.get(r, tablePath(scratch.DatasetID, scratch.TableID))
	if sstatus != http.StatusOK || json.Unmarshal(sgot, &sm) != nil || json.Unmarshal(sm.Schema, &fields) != nil {
		e.Message = "CloudBurrow could not read the query's result: " + errorMessage(sgot, sstatus)
	}
	var target []field
	changed := false
	if e.Message == "" && !sameSchema(fields.Fields, m.Schema.Fields) {
		var invalid, why string
		target, changed, invalid, why = partitionSchema(m.Schema.Fields, fields.Fields, opts, name)
		switch {
		case why != "":
			e.Reason, e.Message = "notImplemented", "Not implemented here: a query job whose destination is the partition "+
				dest.DatasetID+"."+dest.TableID+", whose result's columns are not the table's. The table was not changed."
		case invalid != "":
			e.Reason, e.Message = "invalid", invalid+". The table was not changed."
		}
	}
	if e.Message == "" && !m.ingestion() {
		if n, why := f.scalar(r, "SELECT COUNT(*) FROM "+sp+" WHERE NOT COALESCE("+filter+", FALSE)"); why != "" {
			e.Message = "CloudBurrow could not read the query's result: " + why
		} else if n > 0 {
			e.Reason, e.Message = "invalid", "Some rows belong to different partitions rather than destination partition "+
				dec+" of "+name+". The table was not changed." // UNVERIFIED wording
		}
	}
	if e.Message == "" && (write == "" || write == "WRITE_EMPTY") {
		if n, why := f.scalar(r, "SELECT COUNT(*) FROM "+bp+" WHERE "+filter); why != "" {
			e.Message = "CloudBurrow could not read the partition: " + why
		} else if n > 0 {
			e.Reason, e.Message = "duplicate", "Already Exists: Table "+name
		}
	}
	if e.Message == "" && changed {
		if why := f.updatePartitionedSchema(r, dest.DatasetID, base, m.Schema.Fields, target); why != "" {
			e.Message = "CloudBurrow could not update the schema of " + name + ": " + why
		}
	}
	if e.Message == "" {
		cols := make([]string, len(fields.Fields))
		for i, fl := range fields.Fields {
			cols[i] = quoteName(fl.Name)
		}
		list := strings.Join(cols, ", ")
		pcols, pvals := ingestionColumns(m, dec) // the partition's time (#1277)
		sql := "INSERT INTO " + bp + " (" + list + pcols + ") SELECT " + list + pvals + " FROM " + sp
		if write == "WRITE_TRUNCATE" {
			sql = "DELETE FROM " + bp + " WHERE " + filter + ";\n" + sql
		}
		if why := f.runDML(r, sql); why != "" {
			e.Message = "CloudBurrow could not write the query's result into " + name + ": " + why
		}
	}
	if e.Message != "" {
		out := newRecorder()
		f.fail(out, rec, resp, e)
		f.clientJob(w, out, client)
		return true
	}
	f.clientJob(w, rec, client)
	return true
}

// decorated splits a table ID into its table and partition decorator.
func decorated(id string) (table, dec string, ok bool) {
	if i := strings.Index(id, "$"); i > 0 {
		return id[:i], id[i+1:], true
	}
	return id, "", false
}

// partitionCopy carries out a copy job (copyJob) whose destination or a
// source is a partition (above), or whose sources are ingestion-time
// partitioned tables, whose rows' _PARTITIONTIME it keeps (#1273), and
// reports whether it answered w. write and create are the job's
// writeDisposition and createDisposition, defaulted.
func (f front) partitionCopy(w http.ResponseWriter, r *http.Request, c *copyConfig, srcs []tableRef, write, create string) bool {
	some := false
	for _, t := range append(append([]tableRef{}, srcs...), *c.DestinationTable) {
		if _, _, ok := decorated(t.TableID); ok {
			some = true
		}
	}
	op := strings.ToUpper(c.OperationType)
	plain := op == "" || op == "COPY" || op == "OPERATION_TYPE_UNSPECIFIED"
	if !some {
		// A copy of ingestion-time partitioned tables keeps each row's
		// _PARTITIONTIME (#1273), which copyJob's INSERT of the columns
		// would not: it is carried out here too.
		if !plain {
			return false
		}
		for _, s := range srcs {
			if m, ok := f.partitionMetaOf(r, s.DatasetID, s.TableID); !ok || !m.ingestion() {
				return false
			}
		}
		if m, ok := f.partitionMetaOf(r, c.DestinationTable.DatasetID, c.DestinationTable.TableID); ok && !m.ingestion() {
			return false
		}
	}
	project := projectOf(f.base)
	notImplemented := func(why string) bool {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a copy job of a partition, "+why+
			" (docs/compatibility.md). Nothing was copied.")
		return true
	}
	if !plain {
		return notImplemented("with operationType " + op)
	}
	// part reads a table or partition: its table's metadata and the
	// condition of its rows.
	type part struct {
		ref, base tableRef
		dec       string
		meta      partitionMeta
		kind      string
		where     string
	}
	read := func(t tableRef) (part, bool) {
		p := part{ref: t, base: t, where: "TRUE"}
		var isDec bool
		p.base.TableID, p.dec, isDec = decorated(t.TableID)
		ct := f.copyTableMeta(r, p.base)
		m, ok := f.partitionMetaOf(r, t.DatasetID, p.base.TableID)
		if !ct.exists || !ok {
			writeError(w, http.StatusNotFound, "notFound", "Not found: Table "+tableName(project, p.base))
			return p, false
		}
		p.meta, p.kind = m, ct.kind
		if ct.kind != "" && !strings.EqualFold(ct.kind, "TABLE") || ct.snapshot != nil {
			notImplemented("of " + tableName(project, p.base) + ", which is not a table")
			return p, false
		}
		if isDec {
			filter, code, msg := partitionFilter(m, p.dec, tableName(project, t))
			if code != 0 {
				writeError(w, code, reasonOf(code, "invalid"), msg)
				return p, false
			}
			p.where = filter
		}
		return p, true
	}
	var sources []part
	for _, s := range srcs {
		p, ok := read(s)
		if !ok {
			return true
		}
		if p.meta.ingestion() {
			// "Some recently streamed rows might not be available for
			// table copy": the streaming buffer's are not copied (#1273,
			// streamingbuffer.go).
			p.where = "(" + p.where + ") AND _PARTITIONTIME IS NOT NULL"
		}
		sources = append(sources, p)
	}
	// A destination that does not exist is made with the first source's
	// schema and partitioning, as copyJob makes one (copyjob.go).
	made := false
	if _, _, isDec := decorated(c.DestinationTable.TableID); !isDec && !f.copyTableMeta(r, *c.DestinationTable).exists {
		if create == "CREATE_NEVER" {
			if j, ok := f.startOwnJob(w, r, "a copy job"); ok {
				f.finishOwnJob(w, r, j, "COPY", "copy", nil, &rowError{Reason: "notFound",
					Message: "Not found: Table " + tableName(project, *c.DestinationTable)})
			}
			return true
		}
		if status, _ := f.get(r, "/datasets/"+url.PathEscape(c.DestinationTable.DatasetID)); status == http.StatusNotFound {
			if j, ok := f.startOwnJob(w, r, "a copy job"); ok {
				f.finishOwnJob(w, r, j, "COPY", "copy", nil, &rowError{Reason: "notFound",
					Message: "Not found: Dataset " + project + ":" + c.DestinationTable.DatasetID})
			}
			return true
		}
		src := sources[0].meta
		schema, _ := json.Marshal(src.Schema)
		extra := map[string]any{}
		if src.TimePartitioning != nil {
			extra["timePartitioning"] = src.TimePartitioning
		}
		if src.RangePartitioning != nil {
			extra["rangePartitioning"] = src.RangePartitioning
		}
		if why := f.makeTableWith(r, *c.DestinationTable, schema, extra); why != "" {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not make the copy's destination: "+why)
			return true
		}
		made = true
	}
	dest, ok := read(*c.DestinationTable)
	if !ok {
		return true
	}
	if dest.meta.ingestion() && dest.dec == "__UNPARTITIONED__" {
		return notImplemented("into $__UNPARTITIONED__ of an ingestion-time partitioned table, which holds its streaming buffer")
	}
	for _, p := range sources {
		if !sameSchema(p.meta.Schema.Fields, dest.meta.Schema.Fields) {
			return notImplemented("whose source " + tableName(project, p.ref) + " has a schema other than the destination's")
		}
	}
	j, ok := f.startOwnJob(w, r, "a copy job")
	if !ok {
		return true
	}
	fail := func(reason, msg string) bool {
		if made {
			f.send(r, http.MethodDelete, tablePath(c.DestinationTable.DatasetID, c.DestinationTable.TableID), nil)
		}
		f.finishOwnJob(w, r, j, "COPY", "copy", nil, &rowError{Reason: reason, Message: msg})
		return true
	}
	cols := make([]string, len(dest.meta.Schema.Fields))
	for i, fl := range dest.meta.Schema.Fields {
		cols[i] = quoteName(fl.Name)
	}
	list := strings.Join(cols, ", ")
	// Into an ingestion-time partitioned table: the rows of a partition
	// get its time; without a decorator, rows of ingestion-time
	// partitioned tables keep theirs (truncated to the destination's
	// unit), others get the copy's (#1277).
	pcols, pvals := ingestionColumns(dest.meta, dest.dec)
	keep := dest.dec == "" && pcols != ""
	for _, s := range sources {
		keep = keep && s.meta.ingestion()
	}
	srcExtra := ""
	switch {
	case keep:
		srcExtra, pvals = ", _PARTITIONTIME AS _cb_pt", strings.ReplaceAll(pvals, "_PARTITIONTIME", "_cb_pt")
	case dest.dec == "":
		pcols, pvals = "", ""
	}
	selects := make([]string, len(sources))
	for i, s := range sources {
		selects[i] = "SELECT " + list + srcExtra + " FROM " + quotePath([]string{project, s.base.DatasetID, s.base.TableID}) +
			" WHERE " + s.where
	}
	rows := "(" + strings.Join(selects, " UNION ALL ") + ")"
	n, msg := f.scalar(r, "SELECT COUNT(*) FROM "+rows)
	if msg != "" {
		return fail("backendError", "CloudBurrow could not read the source: "+msg)
	}
	if dest.dec != "" && !dest.meta.ingestion() {
		if bad, msg := f.scalar(r, "SELECT COUNT(*) FROM "+rows+" WHERE NOT COALESCE("+dest.where+", FALSE)"); msg != "" {
			return fail("backendError", "CloudBurrow could not read the source: "+msg)
		} else if bad > 0 {
			return fail("invalid", "Some rows belong to different partitions rather than destination partition "+
				dest.dec+" of "+tableName(project, dest.ref)+". The table was not changed.") // UNVERIFIED wording
		}
	}
	bp := quotePath([]string{project, dest.base.DatasetID, dest.base.TableID})
	if write == "WRITE_EMPTY" {
		if had, msg := f.scalar(r, "SELECT COUNT(*) FROM "+bp+" WHERE "+dest.where); msg != "" {
			return fail("backendError", "CloudBurrow could not read the destination: "+msg)
		} else if had > 0 {
			return fail("duplicate", "Already Exists: Table "+tableName(project, dest.ref))
		}
	}
	sql := "INSERT INTO " + bp + " (" + list + pcols + ") SELECT " + list + pvals + " FROM " + rows
	if write == "WRITE_TRUNCATE" {
		sql = "DELETE FROM " + bp + " WHERE " + dest.where + ";\n" + sql
	}
	if msg := f.runDML(r, sql); msg != "" {
		return fail("backendError", "CloudBurrow could not copy the rows: "+msg)
	}
	f.finishOwnJob(w, r, j, "COPY", "copy", map[string]any{"copiedRows": strconv.FormatInt(n, 10)}, nil)
	return true
}

// partitionLoad prepares a load job whose destination is a partition
// (above): the job is sent to load a scratch table of the table's dataset,
// with the table's schema, and job is read again. It returns what to call,
// with the answer the job got, to move the rows into the partition, or
// false when it answered w.
func (f front) partitionLoad(w http.ResponseWriter, r *http.Request, job *jobBody) (func(w http.ResponseWriter, rec *recorder), bool) {
	l := job.Configuration.Load
	dest := *l.DestinationTable
	base, dec, _ := decorated(dest.TableID)
	project := projectOf(f.base)
	name := tableName(project, dest)
	write := strings.ToUpper(l.WriteDisposition)
	truncate := write == "WRITE_TRUNCATE" || write == "WRITE_TRUNCATE_DATA" // a partition's rows; the schema is kept
	notImplemented := func(why string) bool {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a load job whose destination "+
			"is the partition "+dest.DatasetID+"."+dest.TableID+", "+why+" (docs/compatibility.md). Nothing was loaded.")
		return false
	}
	if dest.ProjectID != "" && dest.ProjectID != project {
		return nil, notImplemented("of another project")
	}
	m, ok := f.partitionMetaOf(r, dest.DatasetID, base)
	if !ok {
		return nil, notImplemented("of a table that does not exist")
	}
	filter, code, msg := partitionFilter(m, dec, name)
	switch {
	case code != 0:
		writeError(w, code, reasonOf(code, "invalid"), msg)
		return nil, false
	case m.ingestion() && dec == "__UNPARTITIONED__":
		return nil, notImplemented("$__UNPARTITIONED__ of an ingestion-time partitioned table, which holds its streaming buffer")
	case write != "" && write != "WRITE_EMPTY" && write != "WRITE_APPEND" && write != "WRITE_TRUNCATE" &&
		write != "WRITE_TRUNCATE_DATA":
		return nil, notImplemented("with writeDisposition " + write)
	}
	// A schema update through the partition (partitionSchema, below).
	next := m.Schema.Fields
	var target []field
	changed := false
	invalid := ""
	if l.Schema != nil && !sameSchema(l.Schema.Fields, m.Schema.Fields) {
		opts := l.SchemaUpdateOptions
		if write == "" || write == "WRITE_EMPTY" {
			opts = nil
		}
		var why string
		target, changed, invalid, why = partitionSchema(m.Schema.Fields, l.Schema.Fields, opts, name)
		if why != "" {
			return nil, notImplemented(why)
		}
		next = l.Schema.Fields
	}
	scratch := tableRef{ProjectID: project, DatasetID: dest.DatasetID, TableID: scratchTable()}
	var client json.RawMessage
	if !editJob(r, func(job map[string]any) bool {
		conf, _ := job["configuration"].(map[string]any)
		ld, _ := conf["load"].(map[string]any)
		if ld == nil {
			return false
		}
		client, _ = json.Marshal(ld)
		ld["destinationTable"] = map[string]any{"projectId": scratch.ProjectID, "datasetId": scratch.DatasetID,
			"tableId": scratch.TableID}
		ld["writeDisposition"] = "WRITE_EMPTY"
		ld["createDisposition"] = "CREATE_IF_NEEDED"
		var s any
		if b, err := json.Marshal(m.Schema); err == nil && ld["schema"] == nil && json.Unmarshal(b, &s) == nil {
			ld["schema"] = s
			delete(ld, "autodetect")
		}
		return true
	}) {
		return nil, notImplemented("which CloudBurrow could not send")
	}
	var again jobBody
	if !decodeJob(r, &again) || again.Configuration.Load == nil {
		return nil, notImplemented("which CloudBurrow could not read again")
	}
	*job = again
	return func(w http.ResponseWriter, rec *recorder) {
		defer f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
		text := jobText{load: client}
		if b := rec.body.Bytes(); bytes.Contains(b, []byte(scratch.TableID)) {
			b = bytes.ReplaceAll(b, []byte(scratch.TableID), []byte(dest.TableID))
			rec.body.Reset()
			rec.body.Write(b)
		}
		var resp map[string]any
		if json.Unmarshal(rec.body.Bytes(), &resp) != nil {
			rec.copyTo(w)
			return
		}
		if _, failed := queryFailure(rec, resp); failed {
			f.clientJob(w, rec, text)
			return
		}
		e := rowError{Reason: "backendError"}
		sp := quotePath([]string{project, scratch.DatasetID, scratch.TableID})
		bp := quotePath([]string{project, dest.DatasetID, base})
		if invalid != "" {
			e.Reason, e.Message = "invalid", invalid+". The table was not changed."
		} else if m.ingestion() {
			// The rows are written into the partition (#1277).
		} else if n, why := f.scalar(r, "SELECT COUNT(*) FROM "+sp+" WHERE NOT COALESCE("+filter+", FALSE)"); why != "" {
			e.Message = "CloudBurrow could not read the loaded rows: " + why
		} else if n > 0 {
			e.Reason, e.Message = "invalid", "Some rows belong to different partitions rather than destination partition "+
				dec+" of "+name+". The table was not changed." // UNVERIFIED wording
		}
		if e.Message == "" && (write == "" || write == "WRITE_EMPTY") {
			if n, why := f.scalar(r, "SELECT COUNT(*) FROM "+bp+" WHERE "+filter); why != "" {
				e.Message = "CloudBurrow could not read the partition: " + why
			} else if n > 0 {
				e.Reason, e.Message = "duplicate", "Already Exists: Table "+name
			}
		}
		if e.Message == "" && changed {
			if why := f.updatePartitionedSchema(r, dest.DatasetID, base, m.Schema.Fields, target); why != "" {
				e.Message = "CloudBurrow could not update the schema of " + name + ": " + why
			}
		}
		if e.Message == "" {
			cols := make([]string, len(next))
			for i, fl := range next {
				cols[i] = quoteName(fl.Name)
			}
			list := strings.Join(cols, ", ")
			pcols, pvals := ingestionColumns(m, dec) // the partition's time (#1277)
			sql := "INSERT INTO " + bp + " (" + list + pcols + ") SELECT " + list + pvals + " FROM " + sp
			if truncate {
				sql = "DELETE FROM " + bp + " WHERE " + filter + ";\n" + sql
			}
			if why := f.runDML(r, sql); why != "" {
				e.Message = "CloudBurrow could not write the loaded rows into " + name + ": " + why
			}
		}
		if e.Message != "" {
			out := newRecorder()
			f.fail(out, rec, resp, e)
			f.clientJob(w, out, text)
			return
		}
		f.clientJob(w, rec, text)
	}, true
}

// A schema update through one partition (#1273). BigQuery takes
// schemaUpdateOptions "when writeDisposition is WRITE_APPEND; when
// writeDisposition is WRITE_TRUNCATE_DATA; when writeDisposition is
// WRITE_TRUNCATE and the destination table is a partition of a table,
// specified by partition decorators"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationQuery):
// ALLOW_FIELD_ADDITION adds a column the rows have and the table does not,
// NULLABLE, at the table's end, and ALLOW_FIELD_RELAXATION relaxes a
// REQUIRED column the rows give as NULLABLE. Without the option, the job
// fails, reason invalid, the table unchanged. The table's columns, in its
// order, must come first; anything else (a column left out, retyped, given
// in another order, a RECORD's fields changed) stays 501; a column
// retyped fails, reason invalid, as "changing a column's data type" is not
// a schema update BigQuery makes through a write. An ingestion-time
// partitioned table's rows keep their _PARTITIONTIME through the table's
// remaking (schemaupdate.go's addColumns).

// partitionSchema compares the rows' columns, next, with the table's,
// old. It returns the table's schema after the write, whether it changes,
// and why the job fails (invalid) or is not carried out (501), or "".
func partitionSchema(old, next []field, opts []string, name string) (target []field, changed bool, invalid, notBuilt string) {
	if len(next) < len(old) {
		return nil, false, "", "whose columns are not the table's (one is left out)"
	}
	target = make([]field, 0, len(next))
	for i, o := range old {
		n := next[i]
		if strings.EqualFold(o.Name, n.Name) && canonicalType(o.Type) != canonicalType(n.Type) {
			// "Changing a column's data type" is not a schema update
			// BigQuery makes through a write (managing-table-schemas).
			return nil, false, "Provided Schema does not match Table " + name + ". Field " + o.Name +
				" has changed type from " + canonicalType(o.Type) + " to " + canonicalType(n.Type), "" // UNVERIFIED wording
		}
		if !strings.EqualFold(o.Name, n.Name) || !sameSchema(o.Fields, n.Fields) {
			return nil, false, "", "whose columns are not the table's (" + n.Name + ")"
		}
		t := o
		switch om, nm := modeOf(o), modeOf(n); {
		case om == nm, om == "NULLABLE" && nm == "REQUIRED":
		case om == "REQUIRED" && nm == "NULLABLE":
			if !hasOption(opts, "ALLOW_FIELD_RELAXATION") {
				return nil, false, "Provided Schema does not match Table " + name + ". Field " + o.Name +
					" has changed mode from REQUIRED to NULLABLE", "" // UNVERIFIED wording
			}
			t.Mode, changed = "NULLABLE", true
		default:
			return nil, false, "", "whose column " + n.Name + " has another mode than the table's"
		}
		target = append(target, t)
	}
	for _, n := range next[len(old):] {
		if !hasOption(opts, "ALLOW_FIELD_ADDITION") {
			return nil, false, "Provided Schema does not match Table " + name + ". Cannot add fields (field: " + n.Name + ")", "" // UNVERIFIED wording
		}
		if modeOf(n) == "REQUIRED" {
			return nil, false, "Provided Schema does not match Table " + name + ". Cannot add required fields to an " +
				"existing schema. (field: " + n.Name + ")", "" // UNVERIFIED wording
		}
		t := n
		if t.Mode == "" {
			t.Mode = "NULLABLE"
		}
		target, changed = append(target, t), true
	}
	return target, changed, "", ""
}

// updatePartitionedSchema gives dataset.table the schema target: added
// columns through addColumns (schemaupdate.go), which keeps its rows and
// settings, or a relaxed mode through tables.patch, the metadata's alone
// (schemaupdate.go). It returns why it could not, or "".
func (f front) updatePartitionedSchema(r *http.Request, dataset, table string, old, target []field) string {
	status, got := f.get(r, tablePath(dataset, table))
	current, ok := decodeMap(got)
	if status != http.StatusOK || !ok {
		return errorMessage(got, status)
	}
	// The table's own schema (descriptions and all), relaxed, with the
	// new columns at its end.
	schema, _ := current["schema"].(map[string]any)
	fields, _ := schema["fields"].([]any)
	if len(fields) != len(old) {
		return "could not read the table's schema"
	}
	out := make([]any, 0, len(target))
	for i, fl := range fields {
		m, _ := fl.(map[string]any)
		if m == nil {
			return "could not read the table's schema"
		}
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		c["mode"] = modeOf(target[i])
		out = append(out, c)
	}
	for _, t := range target[len(old):] {
		b, _ := json.Marshal(t)
		var c map[string]any
		if json.Unmarshal(b, &c) != nil {
			return "could not write the new column " + t.Name
		}
		out = append(out, c)
	}
	raw, err := json.Marshal(map[string]any{"fields": out})
	if err != nil {
		return err.Error()
	}
	if len(target) > len(old) {
		rec := newRecorder()
		if !f.addColumns(rec, r, dataset, table, got, old, raw) {
			return errorMessage(rec.body.Bytes(), rec.status)
		}
		return ""
	}
	body, _ := json.Marshal(map[string]json.RawMessage{"schema": raw})
	if status, got := f.send(r, http.MethodPatch, tablePath(dataset, table), body); status != http.StatusOK {
		return errorMessage(got, status)
	}
	return ""
}

// notOneQuery answers why BigQuery refuses a query job whose
// destinationTable is set and whose query is not one query statement: a
// script, a DML or a DDL statement (#1273), or "" when it is one.
func notOneQuery(sql string) string {
	if isLoneQuery(sql) {
		return ""
	}
	toks, ok := lex(sql)
	if !ok {
		return "" // the emulator's syntax error, as without a destination
	}
	var stmts [][]token
	for _, s := range splitStatements(toks) {
		if len(s) > 0 {
			stmts = append(stmts, s)
		}
	}
	switch {
	case len(stmts) > 1:
		return "configuration.query.destinationTable cannot be set for scripts" // UNVERIFIED wording
	case len(stmts) == 1 && (stmts[0][0].is("INSERT") || stmts[0][0].is("UPDATE") || stmts[0][0].is("DELETE") ||
		stmts[0][0].is("MERGE") || stmts[0][0].is("TRUNCATE")):
		return "Cannot set destination table in jobs with DML statements" // UNVERIFIED wording
	case len(stmts) == 1:
		return "Cannot set destination table in jobs with DDL statements" // UNVERIFIED wording
	}
	return ""
}

// partitionDryRun answers a dry run of a query job whose destination is
// the partition dest (#1273): the query is dry run without its
// destination, which the job BigQuery answers names, with its statistics
// (totalBytesProcessed, and the result's schema, which the front reads with
// lone, as the emulator answers none). Nothing is written, so the
// rows are not checked against the partition.
func (f front) partitionDryRun(w http.ResponseWriter, r *http.Request, q queryOptions, write string, dest *tableRef) bool {
	if !editJob(r, func(job map[string]any) bool {
		conf, _ := job["configuration"].(map[string]any)
		q, _ := conf["query"].(map[string]any)
		if q == nil {
			return false
		}
		delete(q, "destinationTable")
		delete(q, "writeDisposition")
		delete(q, "createDisposition")
		delete(q, "schemaUpdateOptions")
		return true
	}) {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a dry run whose destination "+
			"is the partition "+dest.DatasetID+"."+dest.TableID+", which CloudBurrow could not send (docs/compatibility.md).")
		return true
	}
	rec := newRecorder()
	f.runQuery(rec, r, q, true)
	// BigQuery's dry run gives "the schema of the results"
	// (statistics.query.schema), which the emulator leaves out.
	var resp map[string]any
	if rec.status == 0 || rec.status == http.StatusOK {
		if json.Unmarshal(rec.body.Bytes(), &resp) == nil {
			st, _ := resp["statistics"].(map[string]any)
			qs, _ := st["query"].(map[string]any)
			lq, _, _, _ := bytesParameters(q) // as runQuery sends them (#1078)
			if fields, status, _ := f.lone(r, lq, lq.Query); qs != nil && qs["schema"] == nil && status == http.StatusOK {
				qs["schema"] = map[string]any{"fields": fields}
				if b, err := json.Marshal(resp); err == nil {
					rec.body.Reset()
					rec.body.Write(b)
				}
			}
		}
	}
	f.clientJob(w, rec, jobText{dest: dest, queryWrite: write})
	return true
}
