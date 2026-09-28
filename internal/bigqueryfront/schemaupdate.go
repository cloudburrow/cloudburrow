package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Schema updates through tables.patch and tables.update (#1010).
//
// In BigQuery, a tables.patch or tables.update whose schema adds a column
// adds it to the table, NULL in the rows it has, and relaxing a column
// from REQUIRED to NULLABLE or changing a description changes only the
// schema; a schema that leaves out a column, changes a column's type, or
// makes a column REQUIRED (or REPEATED, or no longer so) is refused
// (https://cloud.google.com/bigquery/docs/managing-table-schemas).
//
// The emulator changes only the table's metadata with either method (its
// tablesPatchHandler and tablesUpdateHandler call metadata.Table.Patch and
// Replace), never its engine's columns. Measured against the pinned image
// through the front: after a tables.patch that added a column g, tables.get
// read g, but `INSERT INTO ds.t (f, g)` failed 400 "Column g is not present
// in table", and tabledata.insertAll of a row with g failed 500
// internalError; `SELECT h` after a tables.update that added h failed
// "Unrecognized name: h". Its engine has no NOT NULL (a REQUIRED column is
// made as a plain one, contentdata's CreateTable), so a mode or a
// description is the metadata's alone, and a patch changes it as BigQuery
// does.
//
// So the front, before it sends the request on:
//
//   - refuses what BigQuery refuses, 400 invalid, naming the column;
//   - carries out a schema that adds top-level columns, at its end: the
//     table is made again with the new schema and its rows copied in (as
//     remakeFloat does for a FLOAT column, floattype.go): the rows are
//     copied to a scratch table with the new schema (in the hidden
//     dataset, deleted later: scratch.go), the table is deleted
//     and made again, through createTable, with the new schema and its
//     other settings (description, labels, expiration, partitioning,
//     clustering), and the rows copied back; its creationTime is then set
//     back. Then the request is sent on, and the emulator changes the
//     metadata as it would;
//   - answers 501, before anything is changed, a schema that adds a field
//     to a RECORD, or that puts the table's columns in another order or a
//     new column before them, and a view's (whose columns are its
//     query's).

// schemaChange compares a table's schema, old, with the one a
// tables.patch or tables.update gives, next (#1009, #1010, #1054). It
// returns the top-level columns next adds, or the status and message to
// refuse the request with: 400 for what BigQuery refuses (a column left
// out, retyped, or given another mode than REQUIRED to NULLABLE, and a
// REQUIRED column added, at any depth), checked first, and then 501 for
// what BigQuery applies and CloudBurrow does not (above). Names are
// compared as BigQuery compares them, without case; name is the table's,
// as project:dataset.table.
func schemaChange(name string, old, next []field) (added []field, code int, msg string) {
	if msg := refusedChange(old, next, ""); msg != "" {
		return nil, http.StatusBadRequest, "Provided Schema does not match Table " + name + ". " + msg
	}
	notImplemented := func(what string) ([]field, int, string) {
		return nil, http.StatusNotImplemented, "Not implemented here: a schema update of " + name + " that " + what + ". " +
			"BigQuery applies it, but the emulator behind CloudBurrow changes only the table's metadata with tables.patch " +
			"and tables.update, never its columns (measured), and CloudBurrow adds only new top-level columns, after the " +
			"table's own, itself. Nothing was changed."
	}
	if what := unappliedChange(old, next, ""); what != "" {
		return notImplemented(what)
	}
	for i, fl := range old {
		if i >= len(next) || next[i].Name != fl.Name {
			return notImplemented("puts its columns in another order, or a new column before them")
		}
	}
	return next[len(old):], 0, ""
}

// byLowerName indexes fields by their names without case.
func byLowerName(fields []field) map[string]field {
	m := map[string]field{}
	for _, fl := range fields {
		m[strings.ToLower(fl.Name)] = fl
	}
	return m
}

// refusedChange returns why BigQuery refuses next as the new schema of
// fields old (prefix names their RECORD), or "".
func refusedChange(old, next []field, prefix string) string {
	byName := byLowerName(next)
	for _, o := range old {
		n, ok := byName[strings.ToLower(o.Name)]
		if !ok {
			return fmt.Sprintf("Field %s%s is missing in new schema", prefix, o.Name)
		}
		if ot, nt := canonicalType(o.Type), canonicalType(n.Type); ot != nt {
			return fmt.Sprintf("Field %s%s has changed type from %s to %s", prefix, o.Name, strings.ToUpper(o.Type), strings.ToUpper(n.Type))
		}
		if om, nm := modeOf(o), modeOf(n); om != nm && !(om == "REQUIRED" && nm == "NULLABLE") {
			return fmt.Sprintf("Field %s%s has changed mode from %s to %s", prefix, o.Name, om, nm)
		}
		if canonicalType(o.Type) == "RECORD" {
			if msg := refusedChange(o.Fields, n.Fields, prefix+o.Name+"."); msg != "" {
				return msg
			}
		}
	}
	had := byLowerName(old)
	for _, n := range next {
		if _, ok := had[strings.ToLower(n.Name)]; !ok && modeOf(n) == "REQUIRED" {
			return fmt.Sprintf("Cannot add required fields to an existing schema. (field: %s%s)", prefix, n.Name)
		}
	}
	return ""
}

// unappliedChange returns what next, a schema BigQuery would take for
// fields old (refusedChange), changes that CloudBurrow does not carry out
// below the top level, or "": a column renamed in another case, or a
// RECORD's fields added to or put in another order.
func unappliedChange(old, next []field, prefix string) string {
	byName := byLowerName(next)
	for _, o := range old {
		n := byName[strings.ToLower(o.Name)]
		if n.Name != o.Name {
			return fmt.Sprintf("renames the column %s%s to %s%s", prefix, o.Name, prefix, n.Name)
		}
		if canonicalType(o.Type) != "RECORD" {
			continue
		}
		if what := unappliedChange(o.Fields, n.Fields, prefix+o.Name+"."); what != "" {
			return what
		}
		if len(n.Fields) != len(o.Fields) {
			return fmt.Sprintf("adds a field to the RECORD %s%s", prefix, o.Name)
		}
		for i := range o.Fields {
			if n.Fields[i].Name != o.Fields[i].Name {
				return fmt.Sprintf("puts the fields of the RECORD %s%s in another order", prefix, o.Name)
			}
		}
	}
	return ""
}

// kept are the settings of a table that its remaking keeps (addColumns);
// the rest of a tables.get answer is the server's own.
var kept = []string{"description", "friendlyName", "labels", "expirationTime", "timePartitioning", "rangePartitioning",
	"clustering", "requirePartitionFilter", "encryptionConfiguration", "defaultCollation"}

// addColumns makes dataset.table again with schema (the request's, raw),
// which adds columns to its fields, old (above); got is the table as
// updateTable read it. It reports whether it did; when it did not, it
// answered w, and the request is not sent on.
func (f front) addColumns(w http.ResponseWriter, r *http.Request, dataset, table string, got []byte, old []field, schemaRaw json.RawMessage) bool {
	name := tableName(projectOf(f.base), tableRef{DatasetID: dataset, TableID: table})
	failed := func(code int, why string) bool {
		reason := "notImplemented"
		prefix := "Not implemented here: "
		if code != http.StatusNotImplemented {
			reason, prefix = "backendError", ""
		}
		writeError(w, code, reason, prefix+"CloudBurrow could not add the columns of the schema update of "+name+
			" to the emulator behind it, which changes only a table's metadata with tables.patch and tables.update "+
			"(#1010): "+why)
		return false
	}
	current, ok := decodeMap(got)
	schema, ok2 := decodeMap(schemaRaw)
	if !ok || !ok2 {
		return failed(http.StatusNotImplemented, "the table or its new schema could not be read. Nothing was changed.")
	}
	ref := tableRef{DatasetID: dataset, TableID: table}
	scratch := tableRef{DatasetID: resultsDataset, TableID: scratchTable()} // scratch.go
	if why := f.makeTable(r, scratch, schemaRaw); why != "" {
		return failed(http.StatusNotImplemented, why+". Nothing was changed.")
	}
	if why := f.insertSelect(r, scratch, []copyTable{{ref: ref, fields: old}}); why != "" {
		f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
		return failed(http.StatusNotImplemented, why+". Nothing was changed.")
	}
	remade := map[string]any{
		"tableReference": map[string]string{"projectId": projectOf(f.base), "datasetId": dataset, "tableId": table},
		"schema":         schema,
	}
	for _, k := range kept {
		if v, ok := current[k]; ok {
			remade[k] = v
		}
	}
	lost := "; the table's rows are in " + scratch.DatasetID + "." + scratch.TableID
	f.send(r, http.MethodDelete, tablePath(dataset, table), nil)
	if st, got := f.createTable(r, dataset, remade); st != http.StatusOK {
		return failed(http.StatusInternalServerError, errorMessage(got, st)+lost)
	}
	if why := f.insertSelect(r, ref, []copyTable{{ref: scratch, fields: old}}); why != "" {
		return failed(http.StatusInternalServerError, why+lost)
	}
	f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
	if created, ok := current["creationTime"]; ok {
		if patch, err := json.Marshal(map[string]any{"creationTime": created}); err == nil {
			f.send(r, http.MethodPatch, tablePath(dataset, table), patch)
		}
	}
	return true
}
