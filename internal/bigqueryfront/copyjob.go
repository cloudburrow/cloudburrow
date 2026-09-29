package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Copy jobs (#987).
//
// The emulator runs only query, load and extract jobs: a copy job
// (configuration.copy) was answered 400 "unspecified job configuration
// query", jobInternalError (measured against the pinned image with the
// official Go client, Table.CopierFrom(...).Run, and the generated one),
// and nothing was copied. BigQuery copies the tables
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationTableCopy).
//
// So the front carries a copy job out itself, as the job's own (frontJobs:
// jobs.get, jobs.list, jobs.cancel and jobs.delete of it are the front's),
// with what the emulator does run:
//
//   - The destination table, when it is made, is made through tables.insert
//     with the first source's schema (names, types, modes, descriptions):
//     a DDL CREATE TABLE of it would not keep a REQUIRED mode (measured: NOT
//     NULL read back NULLABLE). It is made through createTable, which
//     makes a FLOAT column FLOAT64 in the emulator's engine (#1000,
//     floattype.go).
//   - The rows are written by one INSERT INTO destination (columns) SELECT
//     columns FROM each source, joined with UNION ALL, which the emulator
//     runs as one statement: every value is copied as the engine holds it,
//     nested and repeated ones too (measured: a RECORD with a REPEATED
//     field, a TIMESTAMP with microseconds, a NUMERIC and a FLOAT64 read
//     back as written).
//   - writeDisposition (default WRITE_EMPTY): WRITE_EMPTY fails when the
//     destination has rows, "Already Exists"; WRITE_APPEND adds the rows;
//     WRITE_TRUNCATE replaces the table, schema and rows: the rows are
//     first written to a scratch table, so a copy that fails leaves the
//     destination as it was, then the destination is made again with the
//     source's schema and the rows moved in.
//   - createDisposition (default CREATE_IF_NEEDED): CREATE_NEVER fails when
//     the destination does not exist, "Not found".
//   - Several sources (sourceTables) are copied into one table.
//
// A failure BigQuery reports on the job (a source or the destination's
// dataset not found, a view as a source, the destination not empty) fails
// the job, as do several sources whose schemas are not identical, which
// BigQuery's documentation requires (#1002): jobs.insert answers it done
// with an errorResult. 501, and nothing is copied: another operationType
// (SNAPSHOT, RESTORE, CLONE: the emulator has no snapshots or clones, and
// the front does not keep a table's snapshotDefinition or
// cloneDefinition), destinationEncryptionConfiguration,
// destinationExpirationTime, a table of another project, and a WRITE_APPEND (or
// a WRITE_EMPTY into an empty table) whose destination's schema differs
// from the source's (BigQuery's rules for which differences it accepts
// are not given in its documentation). A dry run is 501 (startOwnJob).

// copyConfig is a copy job's configuration.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationTableCopy
type copyConfig struct {
	SourceTable                        *tableRef       `json:"sourceTable"`
	SourceTables                       []tableRef      `json:"sourceTables"`
	DestinationTable                   *tableRef       `json:"destinationTable"`
	CreateDisposition                  string          `json:"createDisposition"`
	WriteDisposition                   string          `json:"writeDisposition"`
	OperationType                      string          `json:"operationType"`
	DestinationEncryptionConfiguration json.RawMessage `json:"destinationEncryptionConfiguration"`
	DestinationExpirationTime          json.RawMessage `json:"destinationExpirationTime"`
}

// copyTable is a table a copy job reads or writes, as tables.get gives it.
type copyTable struct {
	ref    tableRef
	exists bool
	kind   string
	schema json.RawMessage
	fields []field
}

// copyJob carries out a copy job (above).
func (f front) copyJob(w http.ResponseWriter, r *http.Request, c *copyConfig) {
	project := projectOf(f.base)
	notImplemented := func(what string) {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a copy job "+what+
			" Nothing was copied. CloudBurrow carries out a copy job of tables of the instance's project, with any "+
			"writeDisposition and createDisposition (docs/compatibility.md).")
	}
	invalid := func(msg string) { writeError(w, http.StatusBadRequest, "invalid", msg) }
	srcs := c.SourceTables
	if len(srcs) == 0 && c.SourceTable != nil {
		srcs = []tableRef{*c.SourceTable}
	}
	switch op := strings.ToUpper(c.OperationType); {
	case op != "" && op != "COPY" && op != "OPERATION_TYPE_UNSPECIFIED":
		notImplemented(fmt.Sprintf("with operationType %s. BigQuery makes a snapshot, restores one or clones a table, "+
			"but the emulator behind CloudBurrow has no snapshots or clones (CREATE SNAPSHOT TABLE and CREATE TABLE CLONE "+
			"are 400 \"not supported\" there, measured).", c.OperationType))
		return
	case len(c.DestinationEncryptionConfiguration) > 0 && string(c.DestinationEncryptionConfiguration) != "null":
		notImplemented("with destinationEncryptionConfiguration. The emulator behind CloudBurrow has no customer-managed keys.")
		return
	case len(c.DestinationExpirationTime) > 0 && string(c.DestinationExpirationTime) != "null":
		notImplemented("with destinationExpirationTime. The emulator behind CloudBurrow does not expire tables.")
		return
	case len(srcs) == 0 || c.DestinationTable == nil:
		invalid("Required parameter is missing: a copy job needs sourceTable or sourceTables, and destinationTable.")
		return
	}
	write := strings.ToUpper(c.WriteDisposition)
	if write == "" {
		write = "WRITE_EMPTY"
	}
	create := strings.ToUpper(c.CreateDisposition)
	if create == "" {
		create = "CREATE_IF_NEEDED"
	}
	switch {
	case write != "WRITE_EMPTY" && write != "WRITE_APPEND" && write != "WRITE_TRUNCATE":
		invalid("Invalid value for writeDisposition: " + c.WriteDisposition)
		return
	case create != "CREATE_IF_NEEDED" && create != "CREATE_NEVER":
		invalid("Invalid value for createDisposition: " + c.CreateDisposition)
		return
	}
	for _, t := range append(append([]tableRef{}, srcs...), *c.DestinationTable) {
		if t.DatasetID == "" || t.TableID == "" {
			invalid("Required parameter is missing: a table reference of the copy job has no datasetId or tableId.")
			return
		}
		if t.ProjectID != "" && t.ProjectID != project {
			notImplemented(fmt.Sprintf("of a table of project %s, not the job's project %s. CloudBurrow copies "+
				"tables within the job's project only.", t.ProjectID, project))
			return
		}
	}

	// The tables, before anything is done.
	sources := make([]copyTable, len(srcs))
	for i, s := range srcs {
		sources[i] = f.copyTableMeta(r, s)
	}
	dest := f.copyTableMeta(r, *c.DestinationTable)
	if dest.exists && isView(dest.kind) {
		notImplemented(fmt.Sprintf("into %s, which is a %s.", tableName(project, dest.ref), dest.kind))
		return
	}
	appending := dest.exists && write != "WRITE_TRUNCATE"
	if appending && sources[0].exists && !sameSchema(dest.fields, sources[0].fields) {
		if write == "WRITE_APPEND" || f.emptyTable(r, dest.ref.DatasetID, dest.ref.TableID, nil) {
			notImplemented(fmt.Sprintf("with %s into %s, whose schema differs from the source's. BigQuery's documentation "+
				"does not give which differences it accepts.", write, tableName(project, dest.ref)))
			return
		}
	}

	j, ok := f.startOwnJob(w, r, "a copy job")
	if !ok {
		return
	}
	fail := func(reason, msg string) {
		f.finishOwnJob(w, r, j, "COPY", "copy", nil, &rowError{Reason: reason, Message: msg})
	}
	for _, s := range sources {
		switch {
		case !s.exists:
			fail("notFound", "Not found: Table "+tableName(project, s.ref))
			return
		case isView(s.kind) || strings.EqualFold(s.kind, "EXTERNAL"):
			fail("invalid", fmt.Sprintf("%s is not allowed for this operation because it is currently a %s.",
				tableName(project, s.ref), strings.ToUpper(s.kind)))
			return
		}
	}
	// "All source tables must have identical schemas" when several are
	// copied into one (#1002; https://cloud.google.com/bigquery/docs/managing-tables#copy_multiple_source_tables).
	// The job fails; BigQuery's own message is not measured.
	for i := 1; i < len(sources); i++ {
		if !sameSchema(sources[i].fields, sources[0].fields) {
			fail("invalid", fmt.Sprintf("Invalid copy job: the source tables must have identical schemas, and the "+
				"schemas of %s and %s differ.", tableName(project, sources[0].ref), tableName(project, sources[i].ref)))
			return
		}
	}
	if !dest.exists {
		if create == "CREATE_NEVER" {
			fail("notFound", "Not found: Table "+tableName(project, dest.ref))
			return
		}
		if status, _ := f.get(r, "/datasets/"+url.PathEscape(dest.ref.DatasetID)); status == http.StatusNotFound {
			fail("notFound", "Not found: Dataset "+project+":"+dest.ref.DatasetID)
			return
		}
	}
	if dest.exists && write == "WRITE_EMPTY" && !f.emptyTable(r, dest.ref.DatasetID, dest.ref.TableID, nil) {
		fail("duplicate", "Already Exists: Table "+tableName(project, dest.ref))
		return
	}

	rows, msg := f.countRows(r, sources)
	if msg != "" {
		fail("backendError", msg)
		return
	}
	var err string
	switch {
	case appending:
		err = f.insertSelect(r, dest.ref, sources)
	case !dest.exists:
		if err = f.makeTable(r, dest.ref, sources[0].schema); err == "" {
			if err = f.insertSelect(r, dest.ref, sources); err != "" {
				f.send(r, http.MethodDelete, tablePath(dest.ref.DatasetID, dest.ref.TableID), nil)
			}
		}
	default: // WRITE_TRUNCATE of a table that exists
		scratch := tableRef{DatasetID: resultsDataset, TableID: scratchTable()} // scratch.go
		if err = f.makeTable(r, scratch, sources[0].schema); err == "" {
			defer f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
			if err = f.insertSelect(r, scratch, sources); err == "" {
				f.send(r, http.MethodDelete, tablePath(dest.ref.DatasetID, dest.ref.TableID), nil)
				if err = f.makeTable(r, dest.ref, sources[0].schema); err == "" {
					err = f.insertSelect(r, dest.ref, []copyTable{{ref: scratch, fields: sources[0].fields}})
				}
			}
		}
	}
	if err != "" {
		fail("backendError", err)
		return
	}
	f.finishOwnJob(w, r, j, "COPY", "copy", map[string]any{"copiedRows": strconv.FormatInt(rows, 10)}, nil)
}

// copyTableMeta reads a table through tables.get.
func (f front) copyTableMeta(r *http.Request, ref tableRef) copyTable {
	t := copyTable{ref: ref}
	status, got := f.get(r, tablePath(ref.DatasetID, ref.TableID))
	var meta struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil {
		return t
	}
	var s tableSchema
	_ = json.Unmarshal(meta.Schema, &s)
	t.exists, t.kind, t.schema, t.fields = true, meta.Type, meta.Schema, s.Fields
	return t
}

func isView(kind string) bool {
	return strings.EqualFold(kind, "VIEW") || strings.EqualFold(kind, "MATERIALIZED_VIEW")
}

// tableName writes a table as BigQuery's messages name it,
// project:dataset.table.
func tableName(project string, ref tableRef) string {
	if ref.ProjectID != "" {
		project = ref.ProjectID
	}
	return project + ":" + ref.DatasetID + "." + ref.TableID
}

// canonicalType maps a type's legacy name to its GoogleSQL one.
func canonicalType(t string) string {
	switch t = strings.ToUpper(t); t {
	case "INTEGER":
		return "INT64"
	case "FLOAT":
		return "FLOAT64"
	case "BOOLEAN":
		return "BOOL"
	case "STRUCT":
		return "RECORD"
	}
	return t
}

// sameSchema reports whether two schemas have the same columns, in the same
// order, with the same types and modes (names compared without case).
func sameSchema(a, b []field) bool {
	if len(a) != len(b) {
		return false
	}
	mode := func(m string) string {
		if m == "" {
			return "NULLABLE"
		}
		return strings.ToUpper(m)
	}
	for i := range a {
		if !strings.EqualFold(a[i].Name, b[i].Name) || canonicalType(a[i].Type) != canonicalType(b[i].Type) ||
			mode(a[i].Mode) != mode(b[i].Mode) || !sameSchema(a[i].Fields, b[i].Fields) {
			return false
		}
	}
	return true
}

// makeTable makes a table through tables.insert with schema, a
// TableSchema as tables.get gave it, through createTable, which sends each
// FLOAT column as FLOAT64 and sets the schema back (#1000). It returns why
// it could not, or "".
func (f front) makeTable(r *http.Request, ref tableRef, schema json.RawMessage) string {
	s, ok := decodeMap(schema)
	if !ok {
		return "could not read the source's schema"
	}
	status, got := f.createTable(r, ref.DatasetID, map[string]any{
		"tableReference": map[string]string{"projectId": projectOf(f.base), "datasetId": ref.DatasetID, "tableId": ref.TableID},
		"schema":         s,
	})
	if status != http.StatusOK {
		return errorMessage(got, status)
	}
	return ""
}

// selectAll writes SELECT of the columns of fields FROM each source,
// joined with UNION ALL.
func selectAll(fields []field, sources []copyTable) string {
	cols := make([]string, len(fields))
	for i, fl := range fields {
		cols[i] = quoteName(fl.Name)
	}
	list := strings.Join(cols, ", ")
	parts := make([]string, len(sources))
	for i, s := range sources {
		parts[i] = "SELECT " + list + " FROM " + quotePath([]string{s.ref.DatasetID, s.ref.TableID})
	}
	return strings.Join(parts, " UNION ALL ")
}

// insertSelect writes the sources' rows into dest with one INSERT ...
// SELECT, and returns the emulator's error, or "".
func (f front) insertSelect(r *http.Request, dest tableRef, sources []copyTable) string {
	fields := sources[0].fields
	if len(fields) == 0 {
		return ""
	}
	cols := make([]string, len(fields))
	for i, fl := range fields {
		cols[i] = quoteName(fl.Name)
	}
	sql := "INSERT INTO " + quotePath([]string{dest.DatasetID, dest.TableID}) + " (" + strings.Join(cols, ", ") + ") " +
		selectAll(fields, sources)
	legacy := false
	body, err := json.Marshal(queryOptions{Query: sql, UseLegacySQL: &legacy})
	if err != nil {
		return err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK {
		return errorMessage(got, status)
	}
	return ""
}

// countRows counts the sources' rows, for statistics.copy.copiedRows.
func (f front) countRows(r *http.Request, sources []copyTable) (int64, string) {
	if len(sources[0].fields) == 0 {
		return 0, ""
	}
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "SELECT COUNT(*) FROM (" + selectAll(sources[0].fields, sources) + ")",
		UseLegacySQL: &legacy})
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
		return 0, "could not count the source's rows"
	}
	n, err := strconv.ParseInt(res.Rows[0].F[0].V, 10, 64)
	if err != nil {
		return 0, "could not count the source's rows"
	}
	return n, ""
}
