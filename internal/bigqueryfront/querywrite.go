package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// A query job's writeDisposition into a table that exists (#1080).
//
// The emulator writes a query job's result into its destination table
// whatever writeDisposition says (its source, jobsInsertHandler.Handle,
// adds the rows to a table that exists; measured through the front with
// the official Go client: WRITE_TRUNCATE_DATA, WRITE_TRUNCATE and
// WRITE_EMPTY each appended the result to a table with rows, #1067).
// BigQuery (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationQuery):
//
//   - WRITE_TRUNCATE: "If the table already exists, BigQuery overwrites the
//     data, removes the constraints, and uses the schema from the query
//     result."
//   - WRITE_TRUNCATE_DATA: "If the table already exists, BigQuery
//     overwrites the data, but keeps the constraints and schema of the
//     existing table."
//   - WRITE_EMPTY (the default): "If the table already exists and contains
//     data, a 'duplicate' error is returned in the job result."
//
// So the front carries them out (queryWrite). WRITE_EMPTY into a table
// with rows fails the job, reason duplicate, "Already Exists: Table
// project:dataset.table", and runs nothing (into an empty table the job is
// sent on, as the emulator appends to it). WRITE_TRUNCATE and
// WRITE_TRUNCATE_DATA run the job's query into a scratch table of the
// same dataset, a table that does not exist (which the emulator makes
// with the result's schema), and then change the table from it: so the
// query reads the table as it was, even when it reads the table itself,
// and a query that fails leaves it as it was. Then:
//
//   - The result's columns are the table's (the same names in the same
//     order, types and modes; for WRITE_TRUNCATE_DATA the same names and
//     types in any order): the rows are replaced in one script, DELETE and
//     INSERT ... SELECT, which the emulator runs in one transaction
//     (measured: a failed script given to jobs.query left the table's
//     rows), so the table has either its old rows or the result's.
//   - WRITE_TRUNCATE with other columns: the table is made again with the
//     result's schema, keeping its description, labels, expiration,
//     partitioning and clustering but not its constraints (as a Parquet
//     load's WRITE_TRUNCATE does, parquetload.go), and the result copied
//     in. That is two steps: should the second fail, the job fails naming
//     the scratch table, which is kept with the rows.
//   - WRITE_TRUNCATE_DATA whose result has other columns, or into a table
//     with a REQUIRED column (a query's result has none, and what BigQuery
//     then does is not documented): 501, before anything runs.
//
// The result's columns are read before the job runs, by the query run
// alone with no rows (lone). The job the emulator records names the
// scratch table; jobs.insert's answer, jobs.get and jobs.list show the
// client's destination and writeDisposition (jobTexts), and the rows are
// read from the job (jobs.getQueryResults), which the emulator answers
// from the job, not the table (measured: read back after the table was
// deleted). The scratch table is deleted after the job. A dry run, a
// destination in another project and WRITE_APPEND are sent on as they
// are; so is a destination that does not exist, which the emulator makes.

// queryWrite carries out a query job whose writeDisposition the emulator
// would not (above), and reports whether it answered w.
func (f front) queryWrite(w http.ResponseWriter, r *http.Request, job jobBody) bool {
	c := job.Configuration.Query
	dest := c.DestinationTable
	write := strings.ToUpper(c.WriteDisposition)
	if dest == nil || dest.DatasetID == "" || dest.TableID == "" || write == "WRITE_APPEND" ||
		dest.ProjectID != "" && dest.ProjectID != projectOf(f.base) || dryRun(r, true) {
		return false
	}
	status, got := f.get(r, tablePath(dest.DatasetID, dest.TableID))
	table, ok := decodeMap(got)
	if status != http.StatusOK || !ok {
		// Not there (the emulator makes it), or unreadable: the
		// emulator's answer stands.
		return false
	}
	var meta struct {
		Type    string       `json:"type"`
		NumRows any          `json:"numRows"`
		Schema  *tableSchema `json:"schema"`
	}
	_ = json.Unmarshal(got, &meta)
	name := tableName(projectOf(f.base), *dest)
	client := jobText{dest: dest, queryWrite: c.WriteDisposition}
	notImplemented := func(why string) bool {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a query job with "+
			"writeDisposition "+orDefault(write)+" into the table "+dest.DatasetID+"."+dest.TableID+", "+why+
			". Nothing was run.")
		return true
	}
	if meta.Type != "" && !strings.EqualFold(meta.Type, "TABLE") {
		return notImplemented("which is a " + meta.Type + ", not a table")
	}
	switch write {
	case "", "WRITE_EMPTY":
		if f.emptyTable(r, dest.DatasetID, dest.TableID, meta.NumRows) {
			return false
		}
		if !editJob(r, func(job map[string]any) bool {
			conf, _ := job["configuration"].(map[string]any)
			q, _ := conf["query"].(map[string]any)
			if q == nil {
				return false
			}
			delete(q, "destinationTable")
			delete(q, "writeDisposition")
			return true
		}) {
			return notImplemented("which has rows (CloudBurrow could not record the job)")
		}
		rec := newRecorder()
		f.failBeforeRun(rec, r, c.queryOptions, true, dest.DatasetID, http.StatusBadRequest,
			rowError{Reason: "duplicate", Message: "Already Exists: Table " + name})
		f.clientJob(w, rec, client)
		return true
	case "WRITE_TRUNCATE", "WRITE_TRUNCATE_DATA":
	default:
		return false
	}
	if !isLoneQuery(c.Query) {
		return notImplemented("whose query is not one SELECT statement")
	}
	var old []field
	if meta.Schema != nil {
		old = meta.Schema.Fields
	}
	// The result's columns, before anything runs.
	lq, _, _, _ := bytesParameters(c.queryOptions) // as runQuery sends them (#1078)
	result, lstatus, _ := f.lone(r, lq, c.Query)
	if lstatus == http.StatusOK {
		if why := truncateRefused(write, result, old); why != "" {
			return notImplemented(why)
		}
	}

	scratch := tableRef{ProjectID: projectOf(f.base), DatasetID: dest.DatasetID, TableID: scratchTable()}
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
		return notImplemented("which exists (CloudBurrow could not send the job)")
	}
	keep := false
	defer func() {
		if !keep {
			f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
		}
	}()
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
	var sm struct {
		Schema json.RawMessage `json:"schema"`
	}
	sstatus, sgot := f.get(r, tablePath(scratch.DatasetID, scratch.TableID))
	var fields tableSchema
	msg := ""
	if sstatus != http.StatusOK || json.Unmarshal(sgot, &sm) != nil || json.Unmarshal(sm.Schema, &fields) != nil {
		msg = "CloudBurrow could not read the query's result: " + errorMessage(sgot, sstatus)
	}
	e := rowError{Reason: "backendError"}
	if msg == "" {
		if why := truncateRefused(write, fields.Fields, old); why != "" {
			e.Reason, msg = "notImplemented", "Not implemented here: a query job with writeDisposition "+write+
				" into the table "+dest.DatasetID+"."+dest.TableID+", "+why+". The table was not changed."
		}
	}
	if msg == "" {
		var kept bool
		if msg, kept = f.writeResult(r, *dest, scratch, write, table, old, fields.Fields, sm.Schema); msg != "" {
			keep = kept
			msg = "CloudBurrow could not write the query's result into " + name + ": " + msg
		}
	}
	if msg != "" {
		e.Message = msg
		out := newRecorder()
		f.fail(out, rec, resp, e)
		f.clientJob(w, out, client)
		return true
	}
	f.clientJob(w, rec, client)
	return true
}

func orDefault(write string) string {
	if write == "" {
		return "WRITE_EMPTY (the default)"
	}
	return write
}

// truncateRefused returns why the front does not carry out write (above)
// with a result of the columns result into a table of the columns old, or
// "".
func truncateRefused(write string, result, old []field) string {
	if write != "WRITE_TRUNCATE_DATA" {
		return ""
	}
	if required(old) {
		return "which has a REQUIRED column: a query's result has none, and what BigQuery then does is not documented"
	}
	if !sameColumnsByName(result, old) {
		return "whose columns are not the query result's (the same names and types): BigQuery keeps the table's " +
			"schema, and CloudBurrow does not map a result onto other columns"
	}
	return ""
}

// required reports whether fields have a REQUIRED column, at any depth.
func required(fields []field) bool {
	for _, f := range fields {
		if strings.EqualFold(f.Mode, "REQUIRED") || required(f.Fields) {
			return true
		}
	}
	return false
}

// sameColumns reports whether a and b have the same columns by name
// (without case), in any order, with the same types and modes, at every
// depth.
func sameColumnsByName(a, b []field) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		y, ok := fieldNamed(b, x.Name)
		if !ok || canonicalType(x.Type) != canonicalType(y.Type) || modeOf(x) != modeOf(y) || !sameColumnsByName(x.Fields, y.Fields) {
			return false
		}
	}
	return true
}

// writeResult changes dest to hold the rows of scratch, the query's
// result, of the columns result, as write says (above); table is dest's
// tables.get resource and old its columns, and schema the result's
// TableSchema. It returns why it could not, or "", and whether the
// scratch table must be kept, as it holds rows the table lost.
func (f front) writeResult(r *http.Request, dest, scratch tableRef, write string, table map[string]any, old, result []field,
	schema json.RawMessage) (string, bool) {
	cols := make([]string, len(result))
	for i, c := range result {
		cols[i] = quoteName(c.Name)
	}
	list := strings.Join(cols, ", ")
	destPath := quotePath([]string{dest.DatasetID, dest.TableID})
	copyIn := "INSERT INTO " + destPath + " (" + list + ") SELECT " + list + " FROM " +
		quotePath([]string{scratch.DatasetID, scratch.TableID})
	if write == "WRITE_TRUNCATE_DATA" || sameSchema(result, old) {
		return f.runDML(r, "DELETE FROM "+destPath+" WHERE TRUE;\n"+copyIn), false
	}
	// WRITE_TRUNCATE with the result's schema: the table made again.
	s, ok := decodeMap(schema)
	if !ok {
		return "could not read the result's schema", false
	}
	if status, got := f.send(r, http.MethodDelete, tablePath(dest.DatasetID, dest.TableID), nil); status != http.StatusOK &&
		status != http.StatusNoContent {
		return "could not delete the table: " + errorMessage(got, status), false
	}
	where := "; the query's result is in " + scratch.DatasetID + "." + scratch.TableID
	if msg := f.makeParquetTable(r, dest, s, carriedTableProperties(table, false)); msg != "" {
		return msg + where, true
	}
	if msg := f.runDML(r, copyIn); msg != "" {
		return msg + where, true
	}
	return "", false
}
