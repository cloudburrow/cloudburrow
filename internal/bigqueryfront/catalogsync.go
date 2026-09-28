package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// resyncCatalog puts the emulator's SQL catalog and its list of tables
// back in step with its tables after a script it failed (#955), for the
// tables and views the statements before offset before create or drop.
// committed is whether the emulator kept what the statements before the
// failing one did (a failed query job, serveQuery), else it rolled the
// script back.
//
// The emulator runs a script in one transaction, but its SQL engine
// (goccy/googlesqlite v0.3.1) changes its in-memory catalog as each
// statement runs (Catalog.AddNewTableSpec, DeleteTableSpec) and reloads it
// only from rows of its googlesqlite_catalog table updated since it last
// did (Catalog.Sync: `WHERE updatedAt >= lastSyncedAt`); and the emulator
// updates its own list of tables (tables.get, tables.list) from what a
// query changed only after a query that succeeded (syncCatalog). No
// upstream issue or fix was found (goccy/bigquery-emulator,
// goccy/googlesqlite, as of 2026-09-28). Measured against the pinned
// image:
//
//   - Rolled back (jobs.query): the rows are put back, not the catalog.
//     After `DROP TABLE ds.keep; SELECT * FROM nope.nope` failed, tables.get
//     of ds.keep answered 200 and `SELECT a FROM ds.keep` failed "Table not
//     found"; after `CREATE TABLE ds.c1 AS SELECT 1 AS a; SELECT * FROM
//     nope.nope`, `SELECT * FROM ds.c1` failed "sqlite3: SQL logic error:
//     no such table"; after `DROP TABLE ds.keep2; CREATE TABLE ds.keep2 (z
//     STRING); SELECT * FROM nope.nope`, `SELECT a FROM ds.keep2` failed
//     "Unrecognized name: a"; after `CREATE TEMP TABLE ghost AS SELECT 1 AS
//     a; SELECT * FROM nope.nope`, `CREATE TEMP TABLE ghost AS SELECT 2 AS
//     b; SELECT b FROM ghost` failed "Unrecognized name: b".
//   - Committed (a query job): the tables and the catalog are as the
//     statements before the failing one left them, the list of tables as
//     before the script: after `DROP TABLE ds.u; SELECT * FROM nope.nope`,
//     tables.get of ds.u answered 200 and a query of it failed "Table not
//     found"; after `CREATE TABLE ds.e1 AS SELECT 1 AS a; SELECT * FROM
//     nope.nope`, tables.get answered 404 and `SELECT * FROM ds.e1` read
//     the row.
//
// The front puts them back with statements of its own, each measured to
// do what is said here (their answers are not read, and the emulator
// records them as jobs, as it does the front's other checks):
//
//   - Take a table out of the catalog: a script that drops it IF EXISTS
//     and then fails (it reads a table that does not exist), sent to
//     jobs.query, which rolls it back: the table and its rows stay.
//   - Add a table to the catalog: CREATE TABLE (or VIEW) IF NOT EXISTS,
//     from a schema: the engine, not finding it in its catalog, adds it
//     (SQLite's CREATE TABLE IF NOT EXISTS leaves an existing table and its
//     rows as they are), and the emulator adds it to its list of tables,
//     or, when the list has it, fails the statement "table is already
//     created" after the engine's change is committed. The engine's copy
//     of the table is then the one made from the schema (names, types,
//     modes, defaults), without the options of the original.
//   - Remove a table from the catalog and the list: DROP TABLE (or VIEW) IF
//     EXISTS; the emulator answers 500 "nil pointer dereference" when its
//     list has no such table, after the engine's change is committed.
//   - A TEMP table is dropped IF EXISTS (the emulator answers 400
//     "unexpected table name path"), after which a new TEMP table of the
//     name has its own columns.
//
// Rolled back, the list of tables is right and the catalog is made to
// match it: a table the list has is taken out and added again from its
// schema, one it does not have is dropped. Committed, the catalog is
// right and the list is made to match it: a table the engine reads
// (`SELECT * FROM t LIMIT 0`) that the list does not have is taken out of
// the catalog and added again from the columns the engine gave, which
// adds it to the list; one the list has that the engine does not read is
// added and then dropped, which removes it from the list; one both have
// with other columns is patched in the list (tables.patch changes only
// the list's copy). A DROP SCHEMA and a function are not put back, nor a
// table whose schema has a type the front cannot write as DDL.
func (f front) resyncCatalog(r *http.Request, q queryOptions, v ddlVerdict, before int, committed bool) {
	type target struct {
		pos        int
		path       []string
		temp, view bool
		query      string // a view's, from the script's CREATE VIEW
	}
	var targets []target
	for _, c := range v.creates {
		if c.pos < before {
			t := target{pos: c.pos, path: c.path, temp: c.temp, view: c.view}
			if c.view {
				t.query = c.query
			}
			targets = append(targets, t)
		}
	}
	for _, d := range v.drops {
		if d.pos < before && !d.schema {
			targets = append(targets, target{pos: d.pos, path: d.path})
		}
	}
	sort.SliceStable(targets, func(a, b int) bool { return targets[a].pos < targets[b].pos })
	done := map[string]bool{}
	for _, t := range targets {
		if t.temp {
			name, _ := tempName(t.path)
			if name == "" || done["temp\x00"+strings.ToLower(name)] {
				continue
			}
			done["temp\x00"+strings.ToLower(name)] = true
			f.sendDDL(r, "DROP TABLE IF EXISTS "+quotePath([]string{name}))
			continue
		}
		ds, table, ok := tableOf(q, t.path)
		if !ok || done[ds+"\x00"+table] {
			continue
		}
		done[ds+"\x00"+table] = true
		full := []string{ds, table}
		if len(t.path) >= 3 {
			full = t.path[len(t.path)-3:]
		}
		status, meta := f.get(r, tablePath(ds, table))
		if status != http.StatusOK && status != http.StatusNotFound {
			continue
		}
		listed := status == http.StatusOK
		if !committed {
			if !listed {
				what := "TABLE"
				if t.view {
					what = "VIEW"
				}
				f.sendDDL(r, "DROP "+what+" IF EXISTS "+quotePath(full))
				continue
			}
			if ddl, ok := recreateDDL(full, meta); ok {
				f.uncatalog(r, full, ddl)
				f.sendDDL(r, ddl)
			}
			continue
		}
		fields, read := f.engineColumns(r, full)
		switch {
		case read == engineUnknown:
		case read == engineReads && !listed:
			ddl := ""
			if t.view && t.query != "" {
				ddl = "CREATE VIEW IF NOT EXISTS " + quotePath(full) + " AS " + t.query
			} else if b, err := json.Marshal(map[string]any{"type": "TABLE", "schema": map[string]any{"fields": fields}}); err == nil {
				ddl, _ = recreateDDL(full, b)
			}
			if ddl != "" {
				f.uncatalog(r, full, ddl)
				f.sendDDL(r, ddl)
			}
		case read == engineMissing && listed:
			if ddl, ok := recreateDDL(full, meta); ok {
				what := "TABLE"
				if strings.HasPrefix(ddl, "CREATE VIEW") {
					what = "VIEW"
				}
				f.sendDDL(r, ddl)
				f.sendDDL(r, "DROP "+what+" IF EXISTS "+quotePath(full))
			}
		case read == engineReads && listed:
			var m struct {
				Schema struct {
					Fields json.RawMessage `json:"fields"`
				} `json:"schema"`
				Type string `json:"type"`
			}
			if json.Unmarshal(meta, &m) != nil || strings.EqualFold(m.Type, "VIEW") || sameColumns(m.Schema.Fields, fields) {
				continue
			}
			if b, err := json.Marshal(map[string]any{"schema": map[string]any{"fields": fields}}); err == nil {
				f.send(r, http.MethodPatch, tablePath(ds, table), b)
			}
		}
	}
}

// uncatalog takes a table or view out of the engine's catalog, leaving it
// and its rows, by a script that drops it and then fails, which jobs.query
// rolls back (resyncCatalog). ddl is the statement that will add it again.
func (f front) uncatalog(r *http.Request, path []string, ddl string) {
	what := "TABLE"
	if strings.HasPrefix(ddl, "CREATE VIEW") {
		what = "VIEW"
	}
	f.sendDDL(r, "DROP "+what+" IF EXISTS "+quotePath(path)+"; SELECT * FROM "+quotePath([]string{scratchTable(), "t"}))
}

const (
	engineUnknown = iota
	engineReads
	engineMissing
)

// engineColumns reads a table's columns as the engine has it, by a query
// that returns none of its rows, and reports whether it read them, found
// no such table, or could not tell.
func (f front) engineColumns(r *http.Request, path []string) (json.RawMessage, int) {
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "SELECT * FROM " + quotePath(path) + " LIMIT 0", UseLegacySQL: &legacy})
	if err != nil {
		return nil, engineUnknown
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK {
		if strings.Contains(string(got), "Table not found") {
			return nil, engineMissing
		}
		return nil, engineUnknown
	}
	var res struct {
		Schema struct {
			Fields json.RawMessage `json:"fields"`
		} `json:"schema"`
	}
	if json.Unmarshal(got, &res) != nil || len(res.Schema.Fields) == 0 {
		return nil, engineUnknown
	}
	return res.Schema.Fields, engineReads
}

// sameColumns reports whether two schemas' fields have the same names,
// types and modes, in order.
func sameColumns(a, b json.RawMessage) bool {
	var fa, fb []catalogField
	if json.Unmarshal(a, &fa) != nil || json.Unmarshal(b, &fb) != nil {
		return true
	}
	var same func(x, y []catalogField) bool
	same = func(x, y []catalogField) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			tx, ty := ddlTypes[strings.ToUpper(x[i].Type)], ddlTypes[strings.ToUpper(y[i].Type)]
			if tx == "" {
				tx = strings.ToUpper(x[i].Type)
			}
			if ty == "" {
				ty = strings.ToUpper(y[i].Type)
			}
			if tx == "STRUCT" {
				tx = "RECORD"
			}
			if ty == "STRUCT" {
				ty = "RECORD"
			}
			mx, my := strings.ToUpper(x[i].Mode), strings.ToUpper(y[i].Mode)
			if mx == "" {
				mx = "NULLABLE"
			}
			if my == "" {
				my = "NULLABLE"
			}
			if !strings.EqualFold(x[i].Name, y[i].Name) || tx != ty || mx != my || !same(x[i].Fields, y[i].Fields) {
				return false
			}
		}
		return true
	}
	return same(fa, fb)
}

// sendDDL sends a statement of the front's own to the emulator, whose
// paths are all qualified, and ignores the answer.
func (f front) sendDDL(r *http.Request, sql string) {
	legacy := false
	body, err := json.Marshal(queryOptions{Query: sql, UseLegacySQL: &legacy})
	if err != nil {
		return
	}
	f.send(r, http.MethodPost, "/queries", body)
}

// catalogField is the part of a TableFieldSchema recreateDDL writes.
type catalogField struct {
	Name                   string         `json:"name"`
	Type                   string         `json:"type"`
	Mode                   string         `json:"mode"`
	Fields                 []catalogField `json:"fields"`
	MaxLength              json.Number    `json:"maxLength"`
	Precision              json.Number    `json:"precision"`
	Scale                  json.Number    `json:"scale"`
	DefaultValueExpression string         `json:"defaultValueExpression"`
	RangeElementType       *struct {
		Type string `json:"type"`
	} `json:"rangeElementType"`
}

// recreateDDL returns the CREATE TABLE or CREATE VIEW ... IF NOT EXISTS
// that makes a table or view as the emulator's Table resource (meta)
// describes it, and whether it could be written.
func recreateDDL(path []string, meta []byte) (string, bool) {
	var t struct {
		Type string `json:"type"`
		View *struct {
			Query string `json:"query"`
		} `json:"view"`
		Schema struct {
			Fields []catalogField `json:"fields"`
		} `json:"schema"`
	}
	if json.Unmarshal(meta, &t) != nil {
		return "", false
	}
	if strings.EqualFold(t.Type, "VIEW") {
		if t.View == nil || strings.TrimSpace(t.View.Query) == "" {
			return "", false
		}
		return "CREATE VIEW IF NOT EXISTS " + quotePath(path) + " AS " + t.View.Query, true
	}
	if t.Type != "" && !strings.EqualFold(t.Type, "TABLE") || len(t.Schema.Fields) == 0 {
		return "", false
	}
	cols := make([]string, 0, len(t.Schema.Fields))
	for _, fl := range t.Schema.Fields {
		typ, ok := columnDDL(fl, true)
		if !ok {
			return "", false
		}
		cols = append(cols, quoteName(fl.Name)+" "+typ)
	}
	return "CREATE TABLE IF NOT EXISTS " + quotePath(path) + " (" + strings.Join(cols, ", ") + ")", true
}

// ddlTypes are the GoogleSQL names of TableFieldSchema's scalar types.
var ddlTypes = map[string]string{
	"STRING": "STRING", "BYTES": "BYTES", "INTEGER": "INT64", "INT64": "INT64", "FLOAT": "FLOAT64",
	"FLOAT64": "FLOAT64", "NUMERIC": "NUMERIC", "BIGNUMERIC": "BIGNUMERIC", "BOOLEAN": "BOOL", "BOOL": "BOOL",
	"TIMESTAMP": "TIMESTAMP", "DATE": "DATE", "TIME": "TIME", "DATETIME": "DATETIME", "GEOGRAPHY": "GEOGRAPHY",
	"JSON": "JSON", "INTERVAL": "INTERVAL",
}

// columnDDL writes a field's type as DDL: its mode (ARRAY<...>, NOT NULL),
// its parameters and, at the top level, its default.
func columnDDL(fl catalogField, top bool) (string, bool) {
	var typ string
	switch u := strings.ToUpper(fl.Type); {
	case u == "RECORD" || u == "STRUCT":
		parts := make([]string, 0, len(fl.Fields))
		for _, sub := range fl.Fields {
			s, ok := columnDDL(sub, false)
			if !ok {
				return "", false
			}
			parts = append(parts, quoteName(sub.Name)+" "+s)
		}
		if len(parts) == 0 {
			return "", false
		}
		typ = "STRUCT<" + strings.Join(parts, ", ") + ">"
	case u == "RANGE":
		if fl.RangeElementType == nil || ddlTypes[strings.ToUpper(fl.RangeElementType.Type)] == "" {
			return "", false
		}
		typ = "RANGE<" + ddlTypes[strings.ToUpper(fl.RangeElementType.Type)] + ">"
	case ddlTypes[u] != "":
		typ = ddlTypes[u]
		switch {
		case fl.Precision != "" && fl.Scale != "":
			typ += fmt.Sprintf("(%s, %s)", fl.Precision, fl.Scale)
		case fl.Precision != "":
			typ += fmt.Sprintf("(%s)", fl.Precision)
		case fl.MaxLength != "":
			typ += fmt.Sprintf("(%s)", fl.MaxLength)
		}
	default:
		return "", false
	}
	switch strings.ToUpper(fl.Mode) {
	case "REPEATED":
		typ = "ARRAY<" + typ + ">"
	case "REQUIRED":
		typ += " NOT NULL"
	}
	if top && fl.DefaultValueExpression != "" {
		typ += " DEFAULT " + fl.DefaultValueExpression
	}
	return typ, true
}

// quoteName writes a name as a quoted identifier.
func quoteName(name string) string {
	return "`" + strings.ReplaceAll(strings.ReplaceAll(name, `\`, `\\`), "`", "\\`") + "`"
}
