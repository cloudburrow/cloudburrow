package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ALTER TABLE ADD PRIMARY KEY, ADD [CONSTRAINT [IF NOT EXISTS] name]
// FOREIGN KEY, DROP PRIMARY KEY [IF EXISTS] and DROP CONSTRAINT [IF EXISTS]
// name (#1334), as
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_table_add_primary_key_statement
// and the statements after it document them. BigQuery's keys are NOT
// ENFORCED, kept in the table's tableConstraints; so the front keeps them
// there, through the tables.patch path (updateTable), alone or with the
// other actions alterTable carries out in one statement.
//
// Measured against the pinned image (2026-10-15): tables.insert keeps a
// tableConstraints, tables.patch replaces it as a whole (a patch with a
// primary key alone lost the table's foreign key), and a tables.update
// without one clears it; updateTable sends a patch that gives one as that
// update (patchTable), so a statement that drops the last key clears it.
//
// Refused 400 before anything is changed (wording CloudBurrow's,
// UNVERIFIED): a key without NOT ENFORCED (BigQuery has no enforced keys),
// a key's column the table lacks, a second primary key, a foreign key of a
// name the table has (unless IF NOT EXISTS), column lists of two lengths,
// a referenced column its table lacks, DROP PRIMARY KEY of a table with
// none and DROP CONSTRAINT of a name it lacks (unless IF EXISTS); a
// referenced table that does not exist is 404 notFound.

// tableConstraints is a Table's tableConstraints, as the REST reference
// gives it.
type tableConstraints struct {
	PrimaryKey  *primaryKey  `json:"primaryKey,omitempty"`
	ForeignKeys []foreignKey `json:"foreignKeys,omitempty"`
}

type primaryKey struct {
	Columns []string `json:"columns"`
}

type foreignKey struct {
	Name            string `json:"name,omitempty"`
	ReferencedTable struct {
		ProjectID string `json:"projectId"`
		DatasetID string `json:"datasetId"`
		TableID   string `json:"tableId"`
	} `json:"referencedTable"`
	ColumnReferences []columnReference `json:"columnReferences"`
}

type columnReference struct {
	ReferencingColumn string `json:"referencingColumn"`
	ReferencedColumn  string `json:"referencedColumn"`
}

func (c *tableConstraints) empty() bool { return c.PrimaryKey == nil && len(c.ForeignKeys) == 0 }

// columnList reads (a, b, ...) at t[i], and returns the index after it.
func columnList(t []token, i int) ([]string, int, bool) {
	if i >= len(t) || !t[i].punct("(") {
		return nil, i, false
	}
	end := closing(t, i)
	if end < 0 {
		return nil, i, false
	}
	var out []string
	for j := i + 1; j < end; j++ {
		switch {
		case isName(t[j]) && (j+1 == end || t[j+1].punct(",")):
			out = append(out, t[j].text)
		case t[j].punct(",") && len(out) > 0:
		default:
			return nil, i, false
		}
	}
	return out, end + 1, len(out) > 0
}

// notEnforced reports whether t, the rest of a key's action, is NOT
// ENFORCED alone.
func notEnforced(t []token) bool {
	return len(t) == 2 && t[0].is("NOT") && t[1].is("ENFORCED")
}

// parseKeyAction reads an ADD or DROP of a key; msg is the 400 of one
// written as BigQuery does not take it.
func parseKeyAction(a []token) (act alterAction, msg string) {
	const enforced = "Syntax error: BigQuery's primary and foreign keys are NOT ENFORCED; write NOT ENFORCED after the key"
	switch {
	case a[0].is("DROP") && a[1].is("PRIMARY"):
		if len(a) < 3 || !a[2].is("KEY") {
			return act, "Syntax error: DROP PRIMARY without KEY"
		}
		rest := a[3:]
		if len(rest) == 2 && rest[0].is("IF") && rest[1].is("EXISTS") {
			act.ifExists, rest = true, nil
		}
		if len(rest) > 0 {
			return act, fmt.Sprintf("Syntax error: unexpected %q after DROP PRIMARY KEY", rest[0].text)
		}
		act.kind = "drop primary key"
	case a[0].is("DROP"): // CONSTRAINT
		j := 2
		if j+1 < len(a) && a[j].is("IF") && a[j+1].is("EXISTS") {
			act.ifExists = true
			j += 2
		}
		if j+1 != len(a) || !isName(a[j]) {
			return act, "Syntax error: DROP CONSTRAINT takes a constraint name"
		}
		act.kind, act.newName = "drop constraint", a[j].text
	case a[1].is("PRIMARY"):
		if len(a) < 3 || !a[2].is("KEY") {
			return act, "Syntax error: ADD PRIMARY without KEY"
		}
		cols, next, ok := columnList(a, 3)
		if !ok {
			return act, "Syntax error: ADD PRIMARY KEY takes a list of columns"
		}
		if !notEnforced(a[next:]) {
			return act, enforced
		}
		act.kind, act.columns = "add primary key", cols
	default: // [CONSTRAINT [IF NOT EXISTS] name] FOREIGN KEY
		j := 1
		if a[j].is("CONSTRAINT") {
			j++
			if j+2 < len(a) && a[j].is("IF") && a[j+1].is("NOT") && a[j+2].is("EXISTS") {
				act.ifExists = true
				j += 3
			}
			if j >= len(a) || !isName(a[j]) {
				return act, "Syntax error: ADD CONSTRAINT without a constraint name"
			}
			act.newName = a[j].text
			j++
			if j < len(a) && a[j].is("PRIMARY") {
				return act, "Syntax error: ADD CONSTRAINT names a FOREIGN KEY; a primary key is added with ADD PRIMARY KEY"
			}
		}
		if j+1 >= len(a) || !a[j].is("FOREIGN") || !a[j+1].is("KEY") {
			return act, "Syntax error: ADD CONSTRAINT without FOREIGN KEY"
		}
		cols, next, ok := columnList(a, j+2)
		if !ok {
			return act, "Syntax error: FOREIGN KEY takes a list of columns"
		}
		if next >= len(a) || !a[next].is("REFERENCES") {
			return act, "Syntax error: FOREIGN KEY without REFERENCES"
		}
		ref, after := path(a, next+1)
		refCols, after, ok := columnList(a, after)
		if len(ref) == 0 || !ok {
			return act, "Syntax error: REFERENCES takes a table and a list of its columns"
		}
		if !notEnforced(a[after:]) {
			return act, enforced
		}
		act.kind, act.columns, act.refPath, act.refColumns = "add foreign key", cols, ref, refCols
	}
	return act, ""
}

// applyKey applies a, a key's action, to cons, the constraints of the
// table name; has reports whether the table has a column. It reports
// whether cons changed, or the failed statement's flow.
func (s *scriptRun) applyKey(cons *tableConstraints, a alterAction, has func(string) bool, name, text string) (bool, *scriptFlow) {
	bad := func(msg string) (bool, *scriptFlow) {
		return false, failWith(http.StatusBadRequest, "invalidQuery", msg, text)
	}
	missing := func(cols []string, has func(string) bool) string {
		for _, c := range cols {
			if !has(c) {
				return c
			}
		}
		return ""
	}
	switch a.kind {
	case "drop primary key":
		if cons.PrimaryKey == nil {
			if a.ifExists {
				return false, nil
			}
			return bad("Table " + name + " has no primary key to drop.")
		}
		cons.PrimaryKey = nil
		return true, nil
	case "drop constraint":
		for i, fk := range cons.ForeignKeys {
			if fk.Name != "" && strings.EqualFold(fk.Name, a.newName) {
				cons.ForeignKeys = append(cons.ForeignKeys[:i:i], cons.ForeignKeys[i+1:]...)
				return true, nil
			}
		}
		if a.ifExists {
			return false, nil
		}
		return bad("Constraint " + a.newName + " not found in table " + name + ".")
	case "add primary key":
		if cons.PrimaryKey != nil {
			return bad("Table " + name + " already has a primary key.")
		}
		if c := missing(a.columns, has); c != "" {
			return bad("Column not found: " + c + " in table " + name)
		}
		cons.PrimaryKey = &primaryKey{Columns: a.columns}
		return true, nil
	}
	// add foreign key
	if a.newName != "" {
		for _, fk := range cons.ForeignKeys {
			if strings.EqualFold(fk.Name, a.newName) {
				if a.ifExists {
					return false, nil
				}
				return bad("Constraint " + a.newName + " already exists in table " + name + ".")
			}
		}
	}
	if c := missing(a.columns, has); c != "" {
		return bad("Column not found: " + c + " in table " + name)
	}
	if len(a.columns) != len(a.refColumns) {
		return bad("The foreign key's columns and the referenced columns are not of the same number.")
	}
	ds, table, ok := tableOf(s.q, a.refPath)
	if !ok {
		return bad("Table \"" + strings.Join(a.refPath, ".") + "\" must be qualified with a dataset (e.g. dataset.table).")
	}
	project := s.project()
	if len(a.refPath) > 2 && a.refPath[0] != project {
		s.notImpl = alterNotImplemented("ADD FOREIGN KEY referencing a table of another project")
		return false, &scriptFlow{}
	}
	refName := project + ":" + ds + "." + table
	status, got := s.f.get(s.r, tablePath(ds, table))
	if status == http.StatusNotFound {
		return false, failWith(http.StatusNotFound, "notFound", "Not found: Table "+refName, text)
	}
	var ref struct {
		Schema struct {
			Fields []field `json:"fields"`
		} `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &ref) != nil {
		return false, failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not read "+refName, text)
	}
	refHas := func(c string) bool {
		for _, f := range ref.Schema.Fields {
			if strings.EqualFold(f.Name, c) {
				return true
			}
		}
		return false
	}
	if c := missing(a.refColumns, refHas); c != "" {
		return bad("Column not found: " + c + " in table " + refName)
	}
	fk := foreignKey{Name: a.newName}
	fk.ReferencedTable.ProjectID, fk.ReferencedTable.DatasetID, fk.ReferencedTable.TableID = project, ds, table
	for i, c := range a.columns {
		fk.ColumnReferences = append(fk.ColumnReferences, columnReference{ReferencingColumn: c, ReferencedColumn: a.refColumns[i]})
	}
	cons.ForeignKeys = append(cons.ForeignKeys, fk)
	return true, nil
}
