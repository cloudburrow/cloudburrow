package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ALTER TABLE RENAME TO, RENAME COLUMN and DROP COLUMN (#1322), and ALTER
// COLUMN SET DATA TYPE (#1334).
//
// The emulator reports each done and leaves the table as it was (#901), and
// tables.patch renames and drops nothing, so the front makes the table
// again (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_table_rename_to_statement
// and the RENAME COLUMN and DROP COLUMN statements after it):
//
//   - RENAME TO makes the new table with the old one's schema and
//     properties (description, labels, expiration, partitioning,
//     clustering, constraints: carriedTableProperties), copies the rows
//     into it, and deletes the old one. A name taken is 409 "Already
//     Exists", as CREATE TABLE of one is.
//   - RENAME COLUMN and DROP COLUMN, one or several in one statement,
//     copy the rows into a scratch table (scratch.go) of the new schema,
//     make the table again with that schema and its properties, and copy
//     the rows back. A column renamed keeps its type, mode and
//     description, and its place.
//   - ALTER COLUMN SET DATA TYPE (#1334), with them in one statement,
//     does the same, each row's value CAST to the new type; a coercion
//     BigQuery does not document is refused (coercion). The column keeps
//     its name, mode, description and place, and the table its keys,
//     unless a column is also renamed or dropped (then 501, below). Of a
//     partitioning or clustering column it is 501.
//
// Refused before anything is changed, as BigQuery refuses them (400; the
// wording is CloudBurrow's, UNVERIFIED): a missing column without IF
// EXISTS, a new name a column of the table has, dropping a partitioning
// or clustering column ("You can't use this statement to drop ...
// Partitioned columns, Clustered columns"), and dropping every column.
// 501, nothing changed: a table with row access policies (they are kept by
// the table's creation time, rowpolicies.go, and would not follow it), a
// table with constraints whose columns change, and renaming a
// partitioning or clustering column (BigQuery's rule for it is not
// documented).
func (s *scriptRun) remakeTable(st alterStmt, ds, table, name string, got []byte, text string) *scriptFlow {
	var meta map[string]any
	if json.Unmarshal(got, &meta) != nil {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not read "+name, text)
	}
	project := s.project()
	if p, _ := s.f.policies.list(policyKey(project, ds, table)); len(p) > 0 {
		s.notImpl = alterNotImplemented(strings.ToUpper(strings.Fields(st.actions[0].kind)[0]) +
			" of a table with row access policies, which would not follow the table made again")
		return &scriptFlow{}
	}
	schema, _ := meta["schema"].(map[string]any)
	raw, _ := schema["fields"].([]any)
	src := tableRef{DatasetID: ds, TableID: table}
	if a := st.actions[0]; a.kind == "rename table" {
		return s.renameTable(src, a.newName, raw, meta, text)
	}
	// keyed columns: partitioning and clustering.
	keyed := map[string]bool{}
	if tp, ok := meta["timePartitioning"].(map[string]any); ok {
		if f, _ := tp["field"].(string); f != "" {
			keyed[strings.ToLower(f)] = true
		}
	}
	if rp, ok := meta["rangePartitioning"].(map[string]any); ok {
		if f, _ := rp["field"].(string); f != "" {
			keyed[strings.ToLower(f)] = true
		}
	}
	if cl, ok := meta["clustering"].(map[string]any); ok {
		fs, _ := cl["fields"].([]any)
		for _, f := range fs {
			if n, _ := f.(string); n != "" {
				keyed[strings.ToLower(n)] = true
			}
		}
	}
	type column struct {
		from  string
		field map[string]any
		cast  string // SET DATA TYPE's type to read the old values as
	}
	var cols []column
	for _, f := range raw {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		n, _ := m["name"].(string)
		cols = append(cols, column{from: n, field: c})
	}
	find := func(n string) int {
		for i, c := range cols {
			if cn, _ := c.field["name"].(string); strings.EqualFold(cn, n) {
				return i
			}
		}
		return -1
	}
	changed, renames := false, false
	for _, a := range st.actions {
		i := find(a.column)
		if i < 0 {
			if a.ifExists {
				continue
			}
			return failWith(http.StatusBadRequest, "invalidQuery", "Column not found: "+a.column+" in table "+name, text)
		}
		if a.kind == "set type" {
			code, msg := coercion(cols[i].field, a.field)
			switch {
			case code == http.StatusBadRequest:
				return failWith(code, "invalidQuery", msg, text)
			case code != 0:
				s.notImpl = alterNotImplemented("ALTER COLUMN SET DATA TYPE " + msg)
				return &scriptFlow{}
			case keyed[strings.ToLower(cols[i].from)]:
				s.notImpl = alterNotImplemented("ALTER COLUMN SET DATA TYPE of a partitioning or clustering column")
				return &scriptFlow{}
			}
			f := cols[i].field
			old := canonicalType(str(f, "type"))
			for _, k := range []string{"type", "maxLength", "precision", "scale"} {
				delete(f, k)
				if v, ok := a.field[k]; ok {
					f[k] = v
				}
			}
			if nt := canonicalType(str(f, "type")); nt != old {
				cols[i].cast = nt
			}
			changed = true
			continue
		}
		renames = true
		if a.kind == "drop column" {
			if keyed[strings.ToLower(cols[i].from)] {
				return failWith(http.StatusBadRequest, "invalidQuery", "Cannot drop column "+a.column+
					" of table "+name+": it partitions or clusters the table.", text)
			}
			cols = append(cols[:i:i], cols[i+1:]...)
			changed = true
			continue
		}
		if j := find(a.newName); j >= 0 && j != i {
			return failWith(http.StatusBadRequest, "invalidQuery", "Column already exists: "+a.newName, text)
		}
		if keyed[strings.ToLower(cols[i].from)] {
			s.notImpl = alterNotImplemented("RENAME COLUMN of a partitioning or clustering column")
			return &scriptFlow{}
		}
		cols[i].field["name"] = a.newName
		changed = true
	}
	if len(cols) == 0 {
		return failWith(http.StatusBadRequest, "invalidQuery", "Cannot drop every column of table "+name+".", text)
	}
	if !changed {
		return s.assigned()
	}
	if _, ok := meta["tableConstraints"]; ok && renames {
		s.notImpl = alterNotImplemented("RENAME COLUMN or DROP COLUMN of a table with a primary or foreign key")
		return &scriptFlow{}
	}
	fields := make([]any, len(cols))
	from, to := make([]string, len(cols)), make([]string, len(cols))
	for i, c := range cols {
		fields[i] = c.field
		from[i] = quoteName(c.from)
		if c.cast != "" {
			from[i] = "CAST(" + from[i] + " AS " + c.cast + ")"
		}
		n, _ := c.field["name"].(string)
		to[i] = quoteName(n)
	}
	newSchema := map[string]any{"fields": fields}
	r, f := s.r, s.f
	scratch := tableRef{DatasetID: resultsDataset, TableID: scratchTable()} // scratch.go
	if msg := f.makeParquetTable(r, scratch, newSchema, nil); msg != "" {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: "+msg, text)
	}
	keep := false
	defer func() {
		if !keep {
			f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
		}
	}()
	list := strings.Join(to, ", ")
	if msg := f.runDML(r, "INSERT INTO "+quotePath([]string{scratch.DatasetID, scratch.TableID})+" ("+list+") SELECT "+
		strings.Join(from, ", ")+" FROM "+quotePath([]string{ds, table})); msg != "" {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not copy the rows of "+name+": "+msg, text)
	}
	if status, got := f.send(r, http.MethodDelete, tablePath(ds, table), nil); status != http.StatusOK && status != http.StatusNoContent {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not make "+name+" again: "+
			errorMessage(got, status), text)
	}
	held := "; its rows are in " + scratch.DatasetID + "." + scratch.TableID
	if msg := f.makeParquetTable(r, src, newSchema, carriedTableProperties(meta, !renames)); msg != "" {
		keep = true
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: "+msg+held, text)
	}
	if msg := f.runDML(r, "INSERT INTO "+quotePath([]string{ds, table})+" ("+list+") SELECT "+list+" FROM "+
		quotePath([]string{scratch.DatasetID, scratch.TableID})); msg != "" {
		keep = true
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: "+msg+held, text)
	}
	return s.assigned()
}

// renameTable carries out ALTER TABLE src RENAME TO to (above).
func (s *scriptRun) renameTable(src tableRef, to string, raw []any, meta map[string]any, text string) *scriptFlow {
	r, f := s.r, s.f
	dest := tableRef{DatasetID: src.DatasetID, TableID: to}
	if strings.EqualFold(to, src.TableID) {
		return failWith(http.StatusConflict, "duplicate", "Already Exists: Table "+s.project()+":"+src.DatasetID+"."+to, text)
	}
	if status, _ := f.get(r, tablePath(dest.DatasetID, dest.TableID)); status == http.StatusOK {
		return failWith(http.StatusConflict, "duplicate", "Already Exists: Table "+s.project()+":"+src.DatasetID+"."+to, text)
	}
	if msg := f.makeParquetTable(r, dest, map[string]any{"fields": raw}, carriedTableProperties(meta, true)); msg != "" {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: "+msg, text)
	}
	cols := make([]string, 0, len(raw))
	for _, fl := range raw {
		if m, ok := fl.(map[string]any); ok {
			n, _ := m["name"].(string)
			cols = append(cols, quoteName(n))
		}
	}
	list := strings.Join(cols, ", ")
	if msg := f.runDML(r, "INSERT INTO "+quotePath([]string{dest.DatasetID, dest.TableID})+" ("+list+") SELECT "+list+
		" FROM "+quotePath([]string{src.DatasetID, src.TableID})); msg != "" {
		f.send(r, http.MethodDelete, tablePath(dest.DatasetID, dest.TableID), nil)
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not copy the rows: "+msg, text)
	}
	if status, got := f.send(r, http.MethodDelete, tablePath(src.DatasetID, src.TableID), nil); status != http.StatusOK &&
		status != http.StatusNoContent {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: "+dest.TableID+" is made, but "+
			src.TableID+" could not be deleted: "+errorMessage(got, status), text)
	}
	return s.assigned()
}

// coercion reports whether BigQuery's ALTER COLUMN SET DATA TYPE takes
// next, a TableFieldSchema's type and parameters, for the column old
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_column_set_data_type_statement):
// "INT64 to NUMERIC, BIGNUMERIC, FLOAT64; NUMERIC to BIGNUMERIC, FLOAT64",
// and, of one type, from a more restrictive parameterized type to a less
// restrictive one ("increase the maximum length of a string type or
// increase the precision or scale of a numeric type"). It returns 400 and
// the refusal (wording UNVERIFIED), 501 and what is not documented, or 0.
func coercion(old, next map[string]any) (int, string) {
	ot, nt := canonicalType(str(old, "type")), canonicalType(str(next, "type"))
	refused := fmt.Sprintf("ALTER TABLE ALTER COLUMN SET DATA TYPE requires that the existing column type (%s) "+
		"is assignable to the new type (%s)", describeType(old), describeType(next))
	num := func(m map[string]any, k string) (int, bool) {
		v := str(m, k)
		if v == "" {
			return 0, false
		}
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	if ot != nt {
		widens := ot == "INT64" && (nt == "NUMERIC" || nt == "BIGNUMERIC" || nt == "FLOAT64") ||
			ot == "NUMERIC" && (nt == "BIGNUMERIC" || nt == "FLOAT64")
		switch {
		case !widens:
			return http.StatusBadRequest, refused
		case str(next, "precision") != "" || str(old, "precision") != "":
			return http.StatusNotImplemented, "from or to a parameterized NUMERIC or BIGNUMERIC of another type, whose rule is not documented"
		}
		return 0, ""
	}
	switch ot {
	case "STRING", "BYTES":
		nl, nok := num(next, "maxLength")
		ol, ook := num(old, "maxLength")
		if nok && (!ook || nl < ol) {
			return http.StatusBadRequest, refused
		}
	case "NUMERIC", "BIGNUMERIC":
		np, npok := num(next, "precision")
		op, opok := num(old, "precision")
		ns, _ := num(next, "scale")
		os, _ := num(old, "scale")
		if npok && (!opok || ns < os || np-ns < op-os) {
			return http.StatusBadRequest, refused
		}
	}
	return 0, ""
}

// describeType is a TableFieldSchema's type as the DDL writes it.
func describeType(f map[string]any) string {
	t := canonicalType(str(f, "type"))
	switch {
	case str(f, "maxLength") != "":
		return t + "(" + str(f, "maxLength") + ")"
	case str(f, "precision") != "" && str(f, "scale") != "":
		return t + "(" + str(f, "precision") + ", " + str(f, "scale") + ")"
	case str(f, "precision") != "":
		return t + "(" + str(f, "precision") + ")"
	}
	return t
}
