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
//     unless a column is also renamed or dropped (then 501, below). An
//     ARRAY or a STRUCT is converted element by element and field by field
//     (convertExpr), and a partitioning or clustering column is held to
//     what the table needs of it (keyedRetype, #1416).
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
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language
func (s *scriptRun) remakeTable(st alterStmt, ds, table, name string, got []byte, text string) *scriptFlow {
	var meta map[string]any
	if json.Unmarshal(got, &meta) != nil {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not read "+name, text)
	}
	project := s.project()
	policies := len(s.f.livePolicies(s.r, project, ds, table)) > 0
	schema, _ := meta["schema"].(map[string]any)
	raw, _ := schema["fields"].([]any)
	src := tableRef{DatasetID: ds, TableID: table}
	if a := st.actions[0]; a.kind == "rename table" {
		return s.renameTable(src, a.newName, raw, meta, text)
	}
	// "You cannot use this statement to rename [or drop] ... Columns in a
	// table that has row access policies"; SET DATA TYPE keeps them
	// (#1389, moved to the table made again below).
	for _, a := range st.actions {
		if policies && (a.kind == "rename column" || a.kind == "drop column") {
			return failWith(http.StatusBadRequest, "invalidQuery", "Cannot "+strings.Fields(a.kind)[0]+" column "+a.column+
				" of table "+name+": the table has row access policies.", text)
		}
	}
	// keys: the columns of the table's primary and foreign keys, which
	// "cannot" be renamed or dropped.
	keys := map[string]bool{}
	if tc, ok := meta["tableConstraints"].(map[string]any); ok {
		if pk, ok := tc["primaryKey"].(map[string]any); ok {
			cs, _ := pk["columns"].([]any)
			for _, c := range cs {
				if n, _ := c.(string); n != "" {
					keys[strings.ToLower(n)] = true
				}
			}
		}
		fks, _ := tc["foreignKeys"].([]any)
		for _, fk := range fks {
			m, _ := fk.(map[string]any)
			refs, _ := m["columnReferences"].([]any)
			for _, r := range refs {
				rm, _ := r.(map[string]any)
				if n, _ := rm["referencingColumn"].(string); n != "" {
					keys[strings.ToLower(n)] = true
				}
			}
		}
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
		old   map[string]any // SET DATA TYPE's field as it was, its values converted from (convertExpr)
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
	changed := false
	for _, a := range st.actions {
		i := find(a.column)
		if i < 0 {
			if a.ifExists {
				continue
			}
			return failWith(http.StatusBadRequest, "invalidQuery", "Column not found: "+a.column+" in table "+name, text)
		}
		if a.kind == "set type" {
			if msg := collationRetypeRefused(cols[i].field, a.field, a.column); msg != "" { // #1516, collationddl.go
				return failWith(http.StatusBadRequest, "invalidQuery", msg, text)
			}
			code, msg := coercion(cols[i].field, a.field)
			switch {
			case code == http.StatusBadRequest:
				return failWith(code, "invalidQuery", msg, text)
			case code != 0:
				s.notImpl = alterNotImplemented("ALTER COLUMN SET DATA TYPE " + msg)
				return &scriptFlow{}
			}
			if msg := keyedRetype(meta, cols[i].from, a.field); msg != "" {
				return failWith(http.StatusBadRequest, "invalidQuery", msg, text)
			}
			f := cols[i].field
			orig := map[string]any{}
			for k, v := range f {
				orig[k] = v
			}
			old := canonicalType(str(f, "type"))
			oldFields := f["fields"]
			for _, k := range []string{"type", "maxLength", "precision", "scale", "fields"} {
				delete(f, k)
				if v, ok := a.field[k]; ok {
					f[k] = v
				}
			}
			if f["fields"] != nil {
				// A STRUCT's fields keep their modes and descriptions,
				// and take the new types (coercion checked them).
				f["fields"] = retypeFields(oldFields, f["fields"])
			}
			if canonicalType(str(f, "type")) != old || f["fields"] != nil {
				cols[i].old = orig
			}
			changed = true
			continue
		}
		if keys[strings.ToLower(cols[i].from)] {
			return failWith(http.StatusBadRequest, "invalidQuery", "Cannot "+strings.Fields(a.kind)[0]+" column "+a.column+
				" of table "+name+": it is a column of the table's primary or foreign key.", text)
		}
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
			// "You cannot use this statement to rename ... Partitioning
			// columns, Clustering columns" (#1389).
			return failWith(http.StatusBadRequest, "invalidQuery", "Cannot rename column "+a.column+
				" of table "+name+": it partitions or clusters the table.", text)
		}
		cols[i].field["name"] = a.newName
		changed = true
	}
	if len(cols) == 0 {
		return failWith(http.StatusBadRequest, "invalidQuery", "Cannot drop every column of table "+name+".", text)
	}
	if !changed || s.dry { // a dry run changes nothing (#1416, altertabledry.go)
		return nil
	}
	fields := make([]any, len(cols))
	from, to := make([]string, len(cols)), make([]string, len(cols))
	for i, c := range cols {
		fields[i] = c.field
		from[i] = quoteName(c.from)
		if c.old != nil {
			from[i] = convertExpr(from[i], c.old, c.field, 0)
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
	// The table's change history goes on after it is made again (#1466,
	// changehistorycarry.go).
	saved, hasHistory := f.saveHistory(r, ds, table)
	if hasHistory {
		defer f.dropSavedHistory(r, saved)
	}
	if status, got := f.send(r, http.MethodDelete, tablePath(ds, table), nil); status != http.StatusOK && status != http.StatusNoContent {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not make "+name+" again: "+
			errorMessage(got, status), text)
	}
	held := "; its rows are in " + scratch.DatasetID + "." + scratch.TableID
	if msg := f.makeParquetTable(r, src, newSchema, carriedTableProperties(meta, true)); msg != "" {
		keep = true
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: "+msg+held, text)
	}
	// The table made again keeps its creationTime, as addColumns' does,
	// and so its row access policies (SET DATA TYPE alone, above).
	f.keepCreationTime(r, src, meta)
	// A column's data governance tags follow its RENAME COLUMN and go with
	// its DROP COLUMN (#1456, governancetags.go).
	renamed := map[string]string{}
	for _, c := range cols {
		renamed[strings.ToLower(c.from)] = str(c.field, "name")
	}
	f.tables.renameGovernanceTags(streamKey(projectOf(f.base), ds, table), str(meta, "creationTime"), renamed)
	if msg := f.runDML(r, "INSERT INTO "+quotePath([]string{ds, table})+" ("+list+") SELECT "+list+" FROM "+
		quotePath([]string{scratch.DatasetID, scratch.TableID})); msg != "" {
		keep = true
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: "+msg+held, text)
	}
	if hasHistory {
		carried := make([]carriedColumn, len(cols))
		for i := range cols {
			n, _ := cols[i].field["name"].(string)
			carried[i] = carriedColumn{name: n, expr: from[i]}
		}
		if msg := f.carryHistory(r, quotePath([]string{saved.DatasetID, saved.TableID}), ds, table, carried, false); msg != "" {
			return failWith(http.StatusInternalServerError, "internalError", historyKept(name, msg), text)
		}
	}
	return nil
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
	if s.dry { // #1416, altertabledry.go
		return nil
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
	// The table's change history goes with it (#1466, changehistorycarry.go).
	var carried []carriedColumn
	for _, fl := range raw {
		if m, ok := fl.(map[string]any); ok {
			n, _ := m["name"].(string)
			carried = append(carried, carriedColumn{name: n, expr: quoteName(n)})
		}
	}
	if f.hasHistory(r, src.DatasetID, src.TableID) {
		if msg := f.carryHistory(r, f.historySource(src.DatasetID, src.TableID), dest.DatasetID, dest.TableID, carried, false); msg != "" {
			f.send(r, http.MethodDelete, tablePath(dest.DatasetID, dest.TableID), nil)
			return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not carry the change history of "+
				src.TableID+" to "+to+": "+msg, text)
		}
	}
	// "recreates the table in the destination dataset with the creation
	// timestamp of the original table"; "Existing table ACLs and row
	// access policies are preserved" (#1389).
	f.keepCreationTime(r, dest, meta)
	if pols := f.livePolicies(r, s.project(), src.DatasetID, src.TableID); len(pols) > 0 {
		created, _ := meta["creationTime"].(string)
		f.policies.move(policyKey(s.project(), src.DatasetID, src.TableID), policyKey(s.project(), dest.DatasetID, dest.TableID), created)
	}
	if status, got := f.send(r, http.MethodDelete, tablePath(src.DatasetID, src.TableID), nil); status != http.StatusOK &&
		status != http.StatusNoContent {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: "+dest.TableID+" is made, but "+
			src.TableID+" could not be deleted: "+errorMessage(got, status), text)
	}
	return nil
}

// coercion reports whether BigQuery's ALTER COLUMN SET DATA TYPE takes
// next, a TableFieldSchema's type and parameters, for the column old
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_column_set_data_type_statement):
// "INT64 to NUMERIC, BIGNUMERIC, FLOAT64; NUMERIC to BIGNUMERIC, FLOAT64",
// and, of one type, from a more restrictive parameterized type to a less
// restrictive one ("increase the maximum length of a string type or
// increase the precision or scale of a numeric type"). It returns 400 and
// the refusal (wording UNVERIFIED), 501 and what is not documented, or 0.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language
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
	if (str(old, "mode") == "REPEATED") != (str(next, "mode") == "REPEATED") {
		// An ARRAY to an ARRAY of a type its elements take, by these
		// rules; a column made an ARRAY, or no longer one, is a change
		// of mode SET DATA TYPE does not make (#1416).
		return http.StatusBadRequest, refused
	}
	if ot == "RECORD" || nt == "RECORD" {
		// "Changing the data type for a field": a STRUCT to a STRUCT of
		// the same fields, each by these rules (#1389).
		of, _ := old["fields"].([]any)
		nf, _ := next["fields"].([]any)
		if ot != nt || len(of) != len(nf) {
			return http.StatusBadRequest, refused
		}
		for i := range of {
			om, _ := of[i].(map[string]any)
			nm, _ := nf[i].(map[string]any)
			if !strings.EqualFold(str(om, "name"), str(nm, "name")) {
				return http.StatusBadRequest, refused
			}
			if code, msg := coercion(om, nm); code != 0 {
				return code, msg
			}
		}
		return 0, ""
	}
	if ot != nt {
		widens := ot == "INT64" && (nt == "NUMERIC" || nt == "BIGNUMERIC" || nt == "FLOAT64") ||
			ot == "NUMERIC" && (nt == "BIGNUMERIC" || nt == "FLOAT64")
		switch {
		case !widens:
			return http.StatusBadRequest, refused
		case ot == "NUMERIC" && nt == "BIGNUMERIC":
			// "NUMERIC to BIGNUMERIC(40, 20)" is the documented example:
			// a BIGNUMERIC of no less precision and scale than the
			// NUMERIC's (38 and 9 when it has no parameters) (#1389).
			op, ok := num(old, "precision")
			if !ok {
				op = 38
			}
			os, ok := num(old, "scale")
			if !ok && str(old, "precision") == "" {
				os = 9
			}
			np, npok := num(next, "precision")
			ns, _ := num(next, "scale")
			if npok && (np < op || ns < os) {
				return http.StatusBadRequest, refused
			}
		case ot == "INT64" && str(next, "precision") != "":
			// INT64 to a parameterized NUMERIC or BIGNUMERIC (#1416): "less
			// restrictive" is one that holds every INT64, of 19 digits,
			// so one of at least 19 digits before its point (UNVERIFIED:
			// the reference documents the unparameterized types only).
			// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language
			np, _ := num(next, "precision")
			ns, _ := num(next, "scale")
			if np-ns < 19 {
				return http.StatusBadRequest, refused
			}
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
	if str(f, "mode") == "REPEATED" {
		e := map[string]any{}
		for k, v := range f {
			e[k] = v
		}
		delete(e, "mode")
		return "ARRAY<" + describeType(e) + ">"
	}
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

// retypeFields is a STRUCT's new fields, next, with the modes and
// descriptions of its old ones, old.
func retypeFields(old, next any) []any {
	of, _ := old.([]any)
	nf, _ := next.([]any)
	out := make([]any, len(nf))
	for i, n := range nf {
		nm, _ := n.(map[string]any)
		m := map[string]any{}
		if i < len(of) {
			if om, ok := of[i].(map[string]any); ok {
				for k, v := range om {
					m[k] = v
				}
			}
		}
		for _, k := range []string{"type", "maxLength", "precision", "scale"} {
			delete(m, k)
			if v, ok := nm[k]; ok {
				m[k] = v
			}
		}
		if nm["fields"] != nil {
			m["fields"] = retypeFields(m["fields"], nm["fields"])
		}
		out[i] = m
	}
	return out
}

// convertExpr is expr, a value of the field old, converted to the field
// next by SET DATA TYPE (coercion took it): a scalar CAST to its new type,
// an ARRAY element by element in its order, a STRUCT field by field (a
// NULL STRUCT stays NULL). The engine refuses a CAST of an ARRAY to an
// ARRAY of another element type ("Casting between arrays with incompatible
// element types is not supported", measured on the pinned image), so an
// ARRAY is made again with ARRAY(SELECT ... FROM UNNEST ... WITH OFFSET
// ORDER BY), which it reads (measured, #1416).
func convertExpr(expr string, old, next map[string]any, depth int) string {
	if str(next, "mode") == "REPEATED" {
		elem := func(f map[string]any) map[string]any {
			e := map[string]any{}
			for k, v := range f {
				e[k] = v
			}
			delete(e, "mode")
			return e
		}
		v, o := fmt.Sprintf("`_cloudburrow_e%d`", depth), fmt.Sprintf("`_cloudburrow_o%d`", depth)
		return "ARRAY(SELECT " + convertExpr(v, elem(old), elem(next), depth+1) + " FROM UNNEST(" + expr + ") AS " + v +
			" WITH OFFSET AS " + o + " ORDER BY " + o + ")"
	}
	if canonicalType(str(next, "type")) == "RECORD" {
		of, _ := old["fields"].([]any)
		nf, _ := next["fields"].([]any)
		parts := make([]string, len(nf))
		for i, n := range nf {
			nm, _ := n.(map[string]any)
			om := nm
			if i < len(of) {
				om, _ = of[i].(map[string]any)
			}
			parts[i] = convertExpr(expr+"."+quoteName(str(om, "name")), om, nm, depth+1)
		}
		return "CASE WHEN " + expr + " IS NULL THEN NULL ELSE " + recordType(next) + "(" + strings.Join(parts, ", ") + ") END"
	}
	if nt := canonicalType(str(next, "type")); nt != canonicalType(str(old, "type")) {
		return "CAST(" + expr + " AS " + nt + ")"
	}
	return expr
}

// recordType is a RECORD TableFieldSchema's GoogleSQL STRUCT type
// (sqlType), whatever its mode.
func recordType(f map[string]any) string {
	b, _ := json.Marshal(f)
	var fl field
	_ = json.Unmarshal(b, &fl)
	fl.Mode = ""
	return sqlType(fl)
}

// keyedRetype is the 400 of ALTER COLUMN column SET DATA TYPE next, a
// coercion BigQuery documents, of a table, meta, that column partitions or
// clusters, or "" (#1416). The DDL reference sets no rule of its own for
// such a column, so it is held to what the table needs of it: an
// integer-range partitioning column is an INT64 ("The partitioning column
// must be a top-level INTEGER field",
// https://cloud.google.com/bigquery/docs/partitioned-tables#integer_range),
// a clustering column of a type that clusters (clusterTypes, partitionddl.go); a time
// partitioning column's DATE, TIMESTAMP or DATETIME has no coercion to
// another type (coercion refuses it). The wording is CloudBurrow's,
// UNVERIFIED.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language
func keyedRetype(meta map[string]any, column string, next map[string]any) string {
	nt := canonicalType(str(next, "type"))
	if rp, ok := meta["rangePartitioning"].(map[string]any); ok {
		if f, _ := rp["field"].(string); strings.EqualFold(f, column) && nt != "INT64" {
			return "Cannot change the type of column " + column + " to " + describeType(next) +
				": it partitions the table by integer range, and the partitioning column must be an INT64."
		}
	}
	if cl, ok := meta["clustering"].(map[string]any); ok {
		fs, _ := cl["fields"].([]any)
		for _, f := range fs {
			if n, _ := f.(string); strings.EqualFold(n, column) && !clusterTypes[nt] {
				return "Cannot change the type of column " + column + " to " + describeType(next) +
					": it clusters the table, and a clustering column cannot be of type " + nt + "."
			}
		}
	}
	return ""
}

// keepCreationTime gives ref, a table made again, the creationTime of
// meta, the table it was, as addColumns does (schemaupdate.go).
func (f front) keepCreationTime(r *http.Request, ref tableRef, meta map[string]any) {
	if created, ok := meta["creationTime"]; ok {
		if patch, err := json.Marshal(map[string]any{"creationTime": created}); err == nil {
			f.send(r, http.MethodPatch, tablePath(ref.DatasetID, ref.TableID), patch)
		}
	}
}
