package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Table snapshots (#1002).
//
// BigQuery's table snapshots are read-only: "You can only update a table
// snapshot's metadata; for example, its description, expiration date, and
// access policy", and "you can't update the data in a table snapshot"
// (https://cloud.google.com/bigquery/docs/table-snapshots-intro). The
// emulator has no snapshots: CREATE SNAPSHOT TABLE is 400 "not supported"
// there, and a table made through tables.insert with a snapshotDefinition
// reads back type TABLE, writable (measured). It keeps the
// snapshotDefinition, and tables.get answers it (measured), so the front
// takes a table with one for a snapshot, with nothing kept of its own:
//
//   - tables.get answers its type SNAPSHOT, as BigQuery's does, and
//     tables.list too (one tables.get per TABLE it lists);
//   - a write to it is refused before anything runs: tabledata.insertAll,
//     a load or copy job into it, a query job with it as destinationTable,
//     a DML statement (INSERT, UPDATE, DELETE, MERGE, TRUNCATE TABLE),
//     ALTER TABLE or CREATE OR REPLACE TABLE of it in a query, and a
//     tables.patch or tables.update that changes its schema. BigQuery's
//     documentation gives neither the status nor the message of these
//     refusals; the front answers 400 invalid (invalidQuery for a query,
//     a failed job for a query job), UNVERIFIED. The Storage Write API
//     refuses a stream of it (storageWrite.writableFields).
//     tables.delete of it is left to the emulator, and DROP TABLE too:
//     BigQuery's DDL reference gives DROP SNAPSHOT TABLE for snapshots
//     (carried out since #1002, cloneddl.go) but does not say DROP TABLE
//     of one fails.
//
// A copy job with operationType SNAPSHOT makes a new table with a
// snapshotDefinition (the source as baseTableReference, the job's start as
// snapshotTime) and the source's rows; RESTORE copies a snapshot's rows
// into a standard table, with any writeDisposition; COPY of a snapshot
// into a new table makes a snapshot, as "the source and destination table
// have the same table type" (OperationType, BigQuery's REST reference).
// copyjob.go carries them out.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/table-snapshots-intro

// snapshotDefinitionOf returns the snapshotDefinition of a table's
// tables.get answer, or nil when it has none.
func snapshotDefinitionOf(meta []byte) map[string]any {
	var t struct {
		SnapshotDefinition map[string]any `json:"snapshotDefinition"`
	}
	if json.Unmarshal(meta, &t) != nil || len(t.SnapshotDefinition) == 0 {
		return nil
	}
	return t.SnapshotDefinition
}

// isSnapshot reports whether dataset.table exists and is a snapshot.
func (f front) isSnapshot(r *http.Request, dataset, table string) bool {
	status, got := f.get(r, tablePath(dataset, table))
	return status == http.StatusOK && snapshotDefinitionOf(got) != nil
}

// snapshotWriteMessage is the message a write to a snapshot is refused
// with (BigQuery's wording UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/table-snapshots-intro
func snapshotWriteMessage(project, dataset, table, what string) string {
	return fmt.Sprintf("%s is a table snapshot, which is read-only: %s cannot write to it. Restore it to a "+
		"standard table (a copy job with operationType RESTORE) to change its data.",
		tableName(project, tableRef{DatasetID: dataset, TableID: table}), what)
}

// refuseSnapshotWrite answers 400 invalid when dataset.table is a snapshot,
// and reports whether it did.
func (f front) refuseSnapshotWrite(w http.ResponseWriter, r *http.Request, dataset, table, what string) bool {
	if f.refuseLinkedWrite(w, r, dataset, what) { // #1423, loglinks.go
		return true
	}
	if dataset == "" || table == "" || !f.isSnapshot(r, dataset, table) {
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid", snapshotWriteMessage(projectOf(f.base), dataset, table, what))
	return true
}

// snapshotTyped sets a tables.get answer's type to SNAPSHOT when it has a
// snapshotDefinition, and returns it.
func snapshotTyped(meta map[string]any) map[string]any {
	if d, _ := meta["snapshotDefinition"].(map[string]any); len(d) > 0 {
		meta["type"] = "SNAPSHOT"
	}
	return meta
}

// listTables answers tables.list with each snapshot's type SNAPSHOT: the
// emulator's list gives every such table type TABLE and no
// snapshotDefinition (measured), so each TABLE it lists is read through
// tables.get.
func (f front) listTables(w http.ResponseWriter, r *http.Request, dataset string) {
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	var list map[string]any
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
		rec.copyTo(w)
		return
	}
	changed := withoutLinkedEntries(list) // #1423, loglinks.go
	tables, _ := list["tables"].([]any)
	for _, t := range tables {
		m, _ := t.(map[string]any)
		ref, _ := m["tableReference"].(map[string]any)
		id, _ := ref["tableId"].(string)
		if m == nil || id == "" || m["type"] != "TABLE" {
			continue
		}
		if status, got := f.get(r, tablePath(dataset, id)); status == http.StatusOK && snapshotDefinitionOf(got) != nil {
			m["type"] = "SNAPSHOT"
			changed = true
		}
	}
	if !changed {
		rec.copyTo(w)
		return
	}
	writeRecorded(w, rec, list)
}

// writtenTable returns the table a statement writes when it is DML, a
// TRUNCATE TABLE, an ALTER TABLE or a CREATE OR REPLACE TABLE: its path
// as the statement gives it.
func writtenTable(t []token) []string {
	at := func(i int, skip ...string) []string {
		for _, s := range skip {
			if i < len(t) && t[i].is(s) {
				i++
			}
		}
		parts, _ := path(t, i)
		return parts
	}
	if len(t) == 0 || t[0].kind != tokWord {
		return nil
	}
	switch w := strings.ToUpper(t[0].text); {
	case w == "INSERT" || w == "MERGE":
		return at(1, "INTO")
	case w == "DELETE":
		return at(1, "FROM")
	case w == "UPDATE":
		return at(1)
	case w == "TRUNCATE":
		return at(1, "TABLE")
	case w == "ALTER" && len(t) > 1 && t[1].is("TABLE"):
		if len(t) > 3 && t[2].is("IF") && t[3].is("EXISTS") {
			return at(4)
		}
		return at(2)
	case w == "CREATE" && len(t) > 3 && t[1].is("OR") && t[2].is("REPLACE") && t[3].is("TABLE"):
		return at(4)
	}
	return nil
}

// queryWritesSnapshot returns the dataset and table of a snapshot a
// statement of q writes (writtenTable), with the statement's text, or ""
// when none does. A name without a dataset is read in q's default
// dataset; one of another project is not read.
func (f front) queryWritesSnapshot(r *http.Request, q queryOptions) (dataset, table, stmt string) {
	toks, ok := lex(q.Query)
	if !ok {
		return "", "", ""
	}
	project := projectOf(f.base)
	seen := map[string]bool{}
	for _, s := range splitStatements(toks) {
		body, _, _ := stripControlFlow(s)
		parts := writtenTable(body)
		switch len(parts) {
		case 1:
			parts = []string{defaultDatasetOf(q), parts[0]}
		case 2:
		case 3:
			if parts[0] != project {
				continue
			}
			parts = parts[1:]
		default:
			continue
		}
		if parts[0] == "" {
			continue
		}
		key := parts[0] + "." + parts[1]
		if seen[key] {
			continue
		}
		seen[key] = true
		if f.isSnapshot(r, parts[0], parts[1]) {
			return parts[0], parts[1], statementText(q.Query, body)
		}
	}
	return "", "", ""
}

// refuseSnapshotQuery refuses a query, before anything of it runs, that
// writes a snapshot, and reports whether it did: 400 invalidQuery through
// jobs.query, a failed job for a query job (insert).
func (f front) refuseSnapshotQuery(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	upper, writes := strings.ToUpper(q.Query), false
	for _, w := range []string{"INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE", "ALTER", "REPLACE"} {
		writes = writes || strings.Contains(upper, w)
	}
	if !writes {
		return false
	}
	dataset, table, stmt := f.queryWritesSnapshot(r, q)
	if table == "" {
		return false
	}
	f.failBeforeRun(w, r, q, insert, dataset, http.StatusBadRequest, rowError{Reason: "invalidQuery",
		Message: snapshotWriteMessage(projectOf(f.base), dataset, table, "the statement "+stmt)})
	return true
}
