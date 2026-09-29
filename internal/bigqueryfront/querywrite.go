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
//     order, types and modes), or the job is WRITE_TRUNCATE_DATA: the rows
//     are replaced in one script, DELETE and INSERT ... SELECT, which the
//     emulator runs in one transaction (measured: a failed script given to
//     jobs.query left the table's rows), so the table has either its old
//     rows or the result's.
//   - WRITE_TRUNCATE with other columns: the table is made again with the
//     result's schema, keeping its description, labels, expiration,
//     partitioning and clustering but not its constraints (as a Parquet
//     load's WRITE_TRUNCATE does, parquetload.go), and the result copied
//     in. That is two steps: should the second fail, the job fails naming
//     the scratch table, which is kept with the rows.
//   - WRITE_TRUNCATE_DATA keeps the table's schema (#1083), and the
//     result is written into it as BigQuery writes a query's result into
//     a table it appends to, whose schema it keeps unless
//     schemaUpdateOptions say otherwise (JobConfigurationQuery
//     .schemaUpdateOptions: "supported ... when writeDisposition is
//     WRITE_APPEND; when writeDisposition is WRITE_TRUNCATE_DATA").
//     BigQuery's samples of such an append
//     (https://cloud.google.com/bigquery/docs/managing-table-schemas, Add
//     columns in a query append job; Change a column's mode, in a query
//     append job) write `SELECT "Timmy" as full_name, 85 as age, "Blue"
//     as favorite_color` into a table of full_name and age, both
//     REQUIRED, with ALLOW_FIELD_ADDITION, and `SELECT "Beyonce" as
//     full_name` into that table with ALLOW_FIELD_RELAXATION, which makes
//     age NULLABLE for the row that has none: a result's columns are the
//     table's by name, a result column (always NULLABLE) is written into
//     a REQUIRED one, a table column the result does not have is NULL
//     only when it is NULLABLE, and one the table does not have needs
//     ALLOW_FIELD_ADDITION. So the front writes each of the table's
//     columns from the result's of its name, at any depth, a RECORD
//     rebuilt by name in the table's order (rowconvert.go); a column the
//     result does not have is NULL (a REPEATED one empty). The job fails,
//     reason invalid, the table unchanged: a result column the table does
//     not have, a REQUIRED column the result does not have, and a NULL
//     where the table's column or field is REQUIRED (the engine has no NOT
//     NULL, schemaupdate.go, so the front looks for one first); BigQuery's
//     wording of these errors is UNVERIFIED. 501, before anything runs
//     when the result's columns can be read first: a column whose type or
//     REPEATED mode is not the table's (whether BigQuery coerces, INT64
//     into FLOAT64 for example, is not documented), a missing column with
//     a default value (whether BigQuery writes the default is not
//     documented), and schemaUpdateOptions ALLOW_FIELD_RELAXATION into a
//     REQUIRED column or ALLOW_FIELD_ADDITION of a field inside a RECORD.
//     ALLOW_FIELD_ADDITION of a top-level column is carried out (#1110):
//     the table is made again with the column at its end, NULLABLE
//     (remakeWithColumns), and its rows replaced.
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
	result, lstatus, _ := f.lone(r, lq, lq.Query)
	defaults := defaultedFields(table)
	add := write == "WRITE_TRUNCATE_DATA" && hasOption(c.SchemaUpdateOptions, "ALLOW_FIELD_ADDITION")
	if lstatus == http.StatusOK {
		target := old
		if add {
			target = withAddedColumns(old, result)
		}
		if p := truncateRefused(write, result, target, c.SchemaUpdateOptions, defaults); p != nil && !p.invalid {
			return notImplemented(p.msg)
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
	// ALLOW_FIELD_ADDITION with WRITE_TRUNCATE_DATA (#1110): the result's
	// top-level columns the table lacks are added at its end, NULLABLE.
	target := old
	if msg == "" && add {
		target = withAddedColumns(old, fields.Fields)
	}
	if msg == "" {
		if p := truncateRefused(write, fields.Fields, target, c.SchemaUpdateOptions, defaults); p != nil {
			if p.invalid {
				e.Reason, msg = "invalid", "Invalid schema update of "+name+" with writeDisposition "+write+
					", which keeps the table's schema: "+p.msg+". The table was not changed."
			} else {
				e.Reason, msg = "notImplemented", "Not implemented here: a query job with writeDisposition "+write+
					" into the table "+dest.DatasetID+"."+dest.TableID+", "+p.msg+". The table was not changed."
			}
		}
	}
	if msg == "" && write == "WRITE_TRUNCATE_DATA" {
		if why, bad := f.requiredNulls(r, scratch, fields.Fields, target); why != "" {
			msg = "CloudBurrow could not read the query's result: " + why
		} else if bad {
			e.Reason, msg = "invalid", "The query's result has NULL where "+name+", whose schema WRITE_TRUNCATE_DATA "+
				"keeps, has a REQUIRED column or field. The table was not changed."
		}
	}
	if msg == "" && len(target) > len(old) {
		if why := f.remakeWithColumns(r, *dest, table, target[len(old):], sm.Schema); why != "" {
			msg = "CloudBurrow could not add the result's new columns to " + name + ": " + why
		}
	}
	if msg == "" {
		var kept bool
		if msg, kept = f.writeResult(r, *dest, scratch, write, table, target, fields.Fields, sm.Schema); msg != "" {
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
// with a result of the columns result into a table of the columns old,
// whose fields of a default value are defaults (lower-case paths), with
// a query job's schemaUpdateOptions opts, or nil.
func truncateRefused(write string, result, old []field, opts []string, defaults map[string]bool) *convProblem {
	if write != "WRITE_TRUNCATE_DATA" {
		return nil
	}
	option := func(name string) bool { return hasOption(opts, name) }
	if _, _, p := convertColumns(result, old, ""); p != nil {
		switch {
		case p.invalid && strings.Contains(p.msg, "is not in the table's schema") && option("ALLOW_FIELD_ADDITION"),
			p.invalid && strings.Contains(p.msg, "REQUIRED") && option("ALLOW_FIELD_RELAXATION"):
			return &convProblem{msg: p.msg + ", and schemaUpdateOptions would change the table's schema so: CloudBurrow " +
				"adds only top-level columns from a query job, and relaxes none"}
		case !p.invalid:
			return &convProblem{msg: p.msg + ": BigQuery keeps the table's schema, and whether it converts the result's " +
				"value is not documented"}
		}
		return p
	}
	if option("ALLOW_FIELD_RELAXATION") && required(old) {
		return &convProblem{msg: "which has a REQUIRED column or field, with schemaUpdateOptions ALLOW_FIELD_RELAXATION: " +
			"whether BigQuery then relaxes it for a result that has no NULL there is not documented, and CloudBurrow does " +
			"not update a table's schema from a query job"}
	}
	if missing := missingDefaulted(result, old, "", defaults); missing != "" {
		return &convProblem{msg: "whose column " + missing + ", which has a default value, the result does not have: " +
			"whether BigQuery writes its default is not documented"}
	}
	return nil
}

// missingDefaulted returns the first field of old, at any depth, with a
// default value (defaults) that result does not have, or "".
func missingDefaulted(result, old []field, prefix string, defaults map[string]bool) string {
	for _, o := range old {
		path := prefix + strings.ToLower(o.Name)
		r, ok := fieldNamed(result, o.Name)
		if !ok {
			for d := range defaults {
				if d == path || strings.HasPrefix(d, path+".") {
					return prefix + o.Name
				}
			}
			continue
		}
		if m := missingDefaulted(r.Fields, o.Fields, path+".", defaults); m != "" {
			return m
		}
	}
	return ""
}

// defaultedFields returns the lower-case paths of the fields of a
// tables.get resource that have a defaultValueExpression.
func defaultedFields(table map[string]any) map[string]bool {
	out := map[string]bool{}
	var walk func(fields []any, prefix string)
	walk = func(fields []any, prefix string) {
		for _, f := range fields {
			m, _ := f.(map[string]any)
			name, _ := m["name"].(string)
			path := prefix + strings.ToLower(name)
			if d, _ := m["defaultValueExpression"].(string); d != "" {
				out[path] = true
			}
			sub, _ := m["fields"].([]any)
			walk(sub, path+".")
		}
	}
	schema, _ := table["schema"].(map[string]any)
	fields, _ := schema["fields"].([]any)
	walk(fields, "")
	return out
}

// requiredNulls reports whether the rows of scratch, of the columns
// result, read as the columns old (convertColumns), have NULL where old
// has a REQUIRED column or field; or why it could not tell.
func (f front) requiredNulls(r *http.Request, scratch tableRef, result, old []field) (string, bool) {
	cond := nullViolations(old, "", 0)
	if cond == "" {
		return "", false
	}
	exprs, _, p := convertColumns(result, old, "")
	if p != nil {
		return p.msg, false
	}
	cols := make([]string, len(old))
	for i, o := range old {
		cols[i] = exprs[i] + " AS " + quoteName(o.Name)
	}
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "SELECT COUNT(*) FROM (SELECT " + strings.Join(cols, ", ") + " FROM " +
		quotePath([]string{scratch.DatasetID, scratch.TableID}) + ") WHERE " + cond, UseLegacySQL: &legacy})
	if err != nil {
		return err.Error(), false
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
		return errorMessage(got, status), false
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 {
		return "could not count its rows", false
	}
	return "", res.Rows[0].F[0].V != "0"
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
	from := " FROM " + quotePath([]string{scratch.DatasetID, scratch.TableID})
	copyIn := "INSERT INTO " + destPath + " (" + list + ") SELECT " + list + from
	if write == "WRITE_TRUNCATE_DATA" {
		// The table's columns, each read from the result's (#1083).
		exprs, _, p := convertColumns(result, old, "")
		if p != nil {
			return p.msg, false
		}
		names := make([]string, len(old))
		for i, o := range old {
			names[i] = quoteName(o.Name)
		}
		copyIn = "INSERT INTO " + destPath + " (" + strings.Join(names, ", ") + ") SELECT " + strings.Join(exprs, ", ") + from
	}
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

// hasOption reports whether opts, schemaUpdateOptions, holds name.
func hasOption(opts []string, name string) bool {
	for _, o := range opts {
		if strings.EqualFold(o, name) {
			return true
		}
	}
	return false
}

// withAddedColumns is old with the top-level columns of result it does not
// have (by name, in any case) at its end, NULLABLE (a REPEATED one kept
// REPEATED): ALLOW_FIELD_ADDITION ("New columns and nested fields are
// always added at the end of the table", Modifying table schemas; #1110).
// A field result adds inside a RECORD is left for convertColumns to
// refuse.
func withAddedColumns(old, result []field) []field {
	have := map[string]bool{}
	for _, o := range old {
		have[strings.ToLower(o.Name)] = true
	}
	out := old
	for _, c := range result {
		if have[strings.ToLower(c.Name)] {
			continue
		}
		if len(out) == len(old) {
			out = append([]field{}, old...)
		}
		if modeOf(c) != "REPEATED" {
			c.Mode = "NULLABLE"
		}
		out = append(out, c)
	}
	return out
}

// remakeWithColumns makes the table dest again with added at the end of
// its schema, each as result (the scratch table's schema) gives it, for
// WRITE_TRUNCATE_DATA, whose rows are then replaced: the emulator adds no
// engine column through tables.patch (#1010). The table keeps its other
// properties and its constraints ("keeps the constraints").
func (f front) remakeWithColumns(r *http.Request, dest tableRef, table map[string]any, added []field,
	result json.RawMessage) string {
	var rs struct {
		Fields []map[string]any `json:"fields"`
	}
	if json.Unmarshal(result, &rs) != nil {
		return "could not read the result's schema"
	}
	byName := map[string]map[string]any{}
	for _, m := range rs.Fields {
		n, _ := m["name"].(string)
		byName[strings.ToLower(n)] = m
	}
	schema, _ := table["schema"].(map[string]any)
	fields, _ := schema["fields"].([]any)
	fields = append([]any{}, fields...)
	for _, a := range added {
		m, ok := byName[strings.ToLower(a.Name)]
		if !ok {
			return "the result has no column " + a.Name
		}
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		c["mode"] = modeOf(a)
		fields = append(fields, c)
	}
	if status, got := f.send(r, http.MethodDelete, tablePath(dest.DatasetID, dest.TableID), nil); status != http.StatusOK &&
		status != http.StatusNoContent {
		return "could not delete the table: " + errorMessage(got, status)
	}
	return f.makeParquetTable(r, dest, map[string]any{"fields": fields}, carriedTableProperties(table, true))
}
