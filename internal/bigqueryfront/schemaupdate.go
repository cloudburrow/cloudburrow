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
//     copied to a scratch table with the new schema, the table is deleted
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
// tables.patch or tables.update gives, next. It returns the top-level
// columns next adds, or the status (400 or 501) and message to refuse the
// request with; name is the table's, as project:dataset.table.
func schemaChange(name string, old, next []field) (added []field, code int, msg string) {
	refuse := func(format string, args ...any) ([]field, int, string) {
		return nil, http.StatusBadRequest, "Provided Schema does not match Table " + name + ". " + fmt.Sprintf(format, args...)
	}
	notImplemented := func(what string) ([]field, int, string) {
		return nil, http.StatusNotImplemented, "Not implemented here: a schema update of " + name + " that " + what + ". " +
			"BigQuery applies it, but the emulator behind CloudBurrow changes only the table's metadata with tables.patch " +
			"and tables.update, never its columns (measured), and CloudBurrow adds only new top-level columns, after the " +
			"table's own, itself. Nothing was changed."
	}
	if msg := compareFields(old, next, ""); msg != "" {
		if strings.HasPrefix(msg, "501:") {
			return notImplemented(strings.TrimPrefix(msg, "501:"))
		}
		return refuse("%s", msg)
	}
	for i, fl := range old {
		if i >= len(next) || next[i].Name != fl.Name {
			return notImplemented("puts its columns in another order, or a new column before them")
		}
	}
	for _, fl := range next[len(old):] {
		if strings.EqualFold(fl.Mode, "REQUIRED") {
			return refuse("Cannot add required fields to an existing schema. (field: %s)", fl.Name)
		}
		added = append(added, fl)
	}
	return added, 0, ""
}

// compareFields returns why BigQuery refuses next as the new schema of
// fields old (prefix names their RECORD), or "501:" and what CloudBurrow
// does not carry out, or "".
func compareFields(old, next []field, prefix string) string {
	byName := map[string]field{}
	for _, fl := range next {
		byName[strings.ToLower(fl.Name)] = fl
	}
	mode := func(m string) string {
		if m == "" {
			return "NULLABLE"
		}
		return strings.ToUpper(m)
	}
	for _, o := range old {
		n, ok := byName[strings.ToLower(o.Name)]
		if !ok {
			return fmt.Sprintf("Field %s%s is missing in new schema", prefix, o.Name)
		}
		if n.Name != o.Name {
			return fmt.Sprintf("501:renames the column %s%s to %s%s", prefix, o.Name, prefix, n.Name)
		}
		if ot, nt := canonicalType(o.Type), canonicalType(n.Type); ot != nt {
			return fmt.Sprintf("Field %s%s has changed type from %s to %s", prefix, o.Name, strings.ToUpper(o.Type), strings.ToUpper(n.Type))
		}
		if om, nm := mode(o.Mode), mode(n.Mode); om != nm && !(om == "REQUIRED" && nm == "NULLABLE") {
			return fmt.Sprintf("Field %s%s has changed mode from %s to %s", prefix, o.Name, om, nm)
		}
		if canonicalType(o.Type) == "RECORD" {
			if msg := compareFields(o.Fields, n.Fields, prefix+o.Name+"."); msg != "" {
				return msg
			}
			if len(n.Fields) != len(o.Fields) {
				return fmt.Sprintf("501:adds a field to the RECORD %s%s", prefix, o.Name)
			}
			for i := range o.Fields {
				if n.Fields[i].Name != o.Fields[i].Name {
					return fmt.Sprintf("501:puts the fields of the RECORD %s%s in another order", prefix, o.Name)
				}
			}
		}
	}
	return ""
}

// kept are the settings of a table that its remaking keeps (updateSchema);
// the rest of a tables.get answer is the server's own.
var kept = []string{"description", "friendlyName", "labels", "expirationTime", "timePartitioning", "rangePartitioning",
	"clustering", "requirePartitionFilter", "encryptionConfiguration", "defaultCollation"}

// updateSchema carries out a tables.patch or tables.update of
// dataset.table whose schema, fields (raw: the request's body), adds
// columns (above), and reports whether it answered w; when it did not, the
// request is sent on as it is.
func (f front) updateSchema(w http.ResponseWriter, r *http.Request, dataset, table string, raw []byte, fields []field) bool {
	if r.Header.Get("Content-Encoding") != "" {
		return false
	}
	status, got := f.get(r, tablePath(dataset, table))
	if status != http.StatusOK {
		return false // the emulator's own answer
	}
	var meta struct {
		Type   string      `json:"type"`
		Schema tableSchema `json:"schema"`
	}
	current, ok := decodeMap(got)
	if !ok || json.Unmarshal(got, &meta) != nil {
		return false
	}
	name := tableName(projectOf(f.base), tableRef{DatasetID: dataset, TableID: table})
	if isView(meta.Type) || meta.Type != "" && !strings.EqualFold(meta.Type, "TABLE") {
		if !sameSchema(meta.Schema.Fields, fields) {
			writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a schema update of "+name+
				", a "+meta.Type+", other than of its descriptions. CloudBurrow does not carry it out. Nothing was changed.")
			return true
		}
		return false
	}
	added, code, msg := schemaChange(name, meta.Schema.Fields, fields)
	switch {
	case code == http.StatusBadRequest:
		writeError(w, code, "invalid", msg)
		return true
	case code != 0:
		writeError(w, code, "notImplemented", msg)
		return true
	case len(added) == 0:
		return false // a mode relaxed, a description: the metadata's alone
	}
	body, ok := decodeMap(raw)
	schema, _ := body["schema"].(map[string]any)
	if !ok || schema == nil {
		return false
	}
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return false
	}
	failed := func(code int, why string) bool {
		reason := "notImplemented"
		prefix := "Not implemented here: "
		if code != http.StatusNotImplemented {
			reason, prefix = "backendError", ""
		}
		writeError(w, code, reason, prefix+"CloudBurrow could not add the columns of the schema update of "+name+
			" to the emulator behind it, which changes only a table's metadata with tables.patch and tables.update "+
			"(#1010): "+why)
		return true
	}
	ref := tableRef{DatasetID: dataset, TableID: table}
	scratch := tableRef{DatasetID: dataset, TableID: scratchTable()}
	if why := f.makeTable(r, scratch, schemaJSON); why != "" {
		return failed(http.StatusNotImplemented, why+". Nothing was changed.")
	}
	if why := f.insertSelect(r, scratch, []copyTable{{ref: ref, fields: meta.Schema.Fields}}); why != "" {
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
	lost := "; the table's rows are in " + dataset + "." + scratch.TableID
	f.send(r, http.MethodDelete, tablePath(dataset, table), nil)
	if st, got := f.createTable(r, dataset, remade); st != http.StatusOK {
		return failed(http.StatusInternalServerError, errorMessage(got, st)+lost)
	}
	if why := f.insertSelect(r, ref, []copyTable{{ref: scratch, fields: meta.Schema.Fields}}); why != "" {
		return failed(http.StatusInternalServerError, why+lost)
	}
	f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
	if created, ok := current["creationTime"]; ok {
		if patch, err := json.Marshal(map[string]any{"creationTime": created}); err == nil {
			f.send(r, http.MethodPatch, tablePath(dataset, table), patch)
		}
	}
	f.next.ServeHTTP(w, r)
	return true
}
