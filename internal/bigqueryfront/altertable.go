package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ALTER TABLE (#1227).
//
// The emulator answers every ALTER TABLE done and leaves the table as it
// was (measured, checkDDL). The front carries out, through the path
// tables.patch takes (updateTable: the schema checked as BigQuery checks
// it, new columns added to the table's rows, labels merged), these
// actions of https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_table_set_options_statement
// and the statements after it, one or several in one statement, with
// ALTER TABLE IF EXISTS:
//
//   - ADD COLUMN [IF NOT EXISTS] name type [OPTIONS(description = ...)],
//     of any type the DDL writes: a scalar (STRING(10) and NUMERIC(10, 2)
//     with their parameters), ARRAY<T> (a REPEATED column) and STRUCT<...>
//     (a RECORD, its fields NULLABLE, REPEATED or NOT NULL). NOT NULL on the
//     column itself is refused, as BigQuery refuses a REQUIRED column added
//     to a table that has one;
//   - SET OPTIONS (description, friendly_name, labels, expiration_timestamp):
//     "Setting the value replaces the existing value of that option for the
//     table, if there was one. Setting the value to NULL clears the table's
//     value for that option"; labels are replaced as a whole;
//   - ALTER COLUMN [IF EXISTS] name SET OPTIONS (description = ...) and
//     ALTER COLUMN [IF EXISTS] name DROP NOT NULL.
//
// An option's value is a literal, or (description, friendly_name,
// expiration_timestamp) an expression the emulator evaluates, so
// TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 7 DAY) is read.
//
// RENAME TO, RENAME COLUMN and DROP COLUMN (#1322) and ALTER COLUMN SET
// DATA TYPE (#1334) make the table again (alterremake.go); ADD PRIMARY KEY,
// ADD [CONSTRAINT] FOREIGN KEY, DROP PRIMARY KEY and DROP CONSTRAINT
// (#1334) change its tableConstraints through the tables.patch path
// (alterkeys.go), with the actions above in one statement. ALTER COLUMN
// SET DEFAULT and DROP DEFAULT (#1340) change the column's
// defaultValueExpression through the tables.patch path, which makes the
// engine's table again with the new DEFAULT (columndefaults.go); "Setting
// the default value for a column only affects future inserts to the
// table. It does not change any existing table data", and SET DEFAULT
// NULL is DROP DEFAULT. ADD COLUMN ... DEFAULT is refused, 400, as
// BigQuery refuses it: "You cannot add a new column with a default value
// to an existing table" (https://cloud.google.com/bigquery/docs/default-values).
// An action on a nested field is refused, 400, as BigQuery refuses it
// (#1389), and so is RENAME COLUMN with an action that does not remake the
// table and RENAME TO a path; actions of different ways in one statement
// are carried out a run at a time (alterGroups). The table options that
// change how the table behaves stay 501 before anything of the statement
// is changed (alterUnsupported).
//
// A script with an ALTER TABLE is carried out by the front's script
// interpreter (hasControlFlow, scriptflow.go), each ALTER TABLE in its
// turn (scriptRun.alterTable); a dry run of one stays 501 (checkDDL).

// alterStmt is an ALTER TABLE statement as alterTable reads it.
type alterStmt struct {
	ifExists bool
	path     []string
	actions  []alterAction
}

// alterAction is one action of an ALTER TABLE.
type alterAction struct {
	kind        string // "add", "options", "column options", "drop not null", "set default", "drop default", "rename table", "rename column", "drop column", "set type", "add primary key", "add foreign key", "drop primary key", "drop constraint"
	ifExists    bool   // ADD COLUMN IF NOT EXISTS, ALTER COLUMN IF EXISTS, ADD CONSTRAINT IF NOT EXISTS, DROP ... IF EXISTS
	column      string
	newName     string         // RENAME TO's table, RENAME COLUMN's new name, a constraint's name
	field       map[string]any // ADD COLUMN's TableFieldSchema, SET DATA TYPE's (no name)
	columns     []string       // a key's columns
	refPath     []string       // FOREIGN KEY's REFERENCES table
	refColumns  []string       // and its columns
	options     []alterOption
	description *alterOption // ADD COLUMN's OPTIONS(description)
}

// alterOption is one name = value of an OPTIONS list.
type alterOption struct {
	name string
	val  []token
}

// isAlterTable reports whether body is an ALTER TABLE statement.
func isAlterTable(body []token) bool {
	return len(body) > 1 && body[0].is("ALTER") && body[1].is("TABLE")
}

// alterUnsupported returns what of an ALTER TABLE the front does not carry
// out, named for the 501, or "".
func alterUnsupported(t []token) string {
	_, unsupported, _ := parseAlter(t)
	return unsupported
}

// alterNotImplemented is the 501 message for what, an action of ALTER
// TABLE the front does not carry out.
func alterNotImplemented(what string) string {
	return "Not implemented here: ALTER TABLE " + what + ". BigQuery runs it, but the emulator behind CloudBurrow " +
		"reports ALTER TABLE done and leaves the table unchanged (measured), and CloudBurrow carries out only ADD COLUMN, " +
		"SET OPTIONS (description, friendly_name, labels, expiration_timestamp, partition_expiration_days, kms_key_name), " +
		"ALTER COLUMN SET OPTIONS (description), " +
		"ALTER COLUMN DROP NOT NULL, ALTER COLUMN SET DATA TYPE, ALTER COLUMN SET DEFAULT and DROP DEFAULT, RENAME TO, RENAME COLUMN, " +
		"DROP COLUMN and primary and foreign keys, " +
		"so nothing of the statement was run."
}

// parseAlter reads an ALTER TABLE statement. unsupported names what the
// front does not carry out (501); msg is a statement BigQuery refuses
// (400).
func parseAlter(t []token) (st alterStmt, unsupported, msg string) {
	i := 2
	if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
		st.ifExists = true
		i += 2
	}
	st.path, i = path(t, i)
	if len(st.path) == 0 || i >= len(t) {
		return st, "", "Syntax error: ALTER TABLE without an action"
	}
	for i < len(t) {
		end := skipTo(t, i, ",")
		a := t[i:end]
		var act alterAction
		switch {
		case len(a) > 1 && a[0].is("ADD") && a[1].is("COLUMN"):
			j := 2
			if j+2 < len(a) && a[j].is("IF") && a[j+1].is("NOT") && a[j+2].is("EXISTS") {
				act.ifExists = true
				j += 3
			}
			if j >= len(a) || a[j].kind != tokWord && a[j].kind != tokQuoted {
				return st, "", "Syntax error: ADD COLUMN without a column name"
			}
			act.kind, act.column = "add", a[j].text
			f, next, what := fieldType(a, j+1)
			if what != "" {
				return st, "ADD COLUMN of " + what, ""
			}
			f["name"] = act.column
			rest := a[next:]
			if len(rest) > 1 && rest[0].is("NOT") && rest[1].is("NULL") {
				f["mode"] = "REQUIRED"
				rest = rest[2:]
			}
			if len(rest) > 0 && rest[0].is("DEFAULT") {
				return st, "", addDefaultRefused(act.column)
			}
			if len(rest) > 0 {
				opts, ok := readOptions(rest)
				if !ok {
					return st, fmt.Sprintf("ADD COLUMN with %q after the column's type", rest[0].text), ""
				}
				for _, o := range opts {
					if o.name != "description" {
						return st, "ADD COLUMN with the column option " + o.name, ""
					}
					o := o
					act.description = &o
				}
			}
			act.field = f
		case len(a) > 1 && a[0].is("SET") && a[1].is("OPTIONS"):
			opts, ok := readOptions(a[1:])
			if !ok {
				return st, "", "Syntax error: SET OPTIONS without its list"
			}
			for _, o := range opts {
				switch o.name {
				case "description", "friendly_name", "labels", "expiration_timestamp", "partition_expiration_days", "kms_key_name":
				default:
					return st, "SET OPTIONS (" + o.name + "), which changes how the table behaves", ""
				}
			}
			act.kind, act.options = "options", opts
		case len(a) > 1 && a[0].is("ALTER") && a[1].is("COLUMN"):
			j := 2
			if j+1 < len(a) && a[j].is("IF") && a[j+1].is("EXISTS") {
				act.ifExists = true
				j += 2
			}
			if j >= len(a) || a[j].kind != tokWord && a[j].kind != tokQuoted {
				return st, "", "Syntax error: ALTER COLUMN without a column name"
			}
			act.column = a[j].text
			rest := a[j+1:]
			switch {
			case len(rest) == 2 && rest[0].is("DROP") && rest[1].is("NOT"):
				return st, "", "Syntax error: DROP NOT without NULL"
			case len(rest) == 3 && rest[0].is("DROP") && rest[1].is("NOT") && rest[2].is("NULL"):
				act.kind = "drop not null"
			case len(rest) > 1 && rest[0].is("SET") && rest[1].is("OPTIONS"):
				opts, ok := readOptions(rest[1:])
				if !ok {
					return st, "", "Syntax error: SET OPTIONS without its list"
				}
				for _, o := range opts {
					if o.name != "description" {
						return st, "ALTER COLUMN SET OPTIONS (" + o.name + ")", ""
					}
				}
				act.kind, act.options = "column options", opts
			case len(rest) > 0 && rest[0].punct("."):
				return st, "", nestedRefused("ALTER COLUMN", act.column)
			case len(rest) > 2 && rest[0].is("SET") && rest[1].is("DATA") && rest[2].is("TYPE"):
				f, next, what := fieldType(rest, 3)
				switch {
				case what != "":
					return st, "ALTER COLUMN SET DATA TYPE " + what, ""
				case next != len(rest):
					return st, "", fmt.Sprintf("Syntax error: unexpected %q after SET DATA TYPE", rest[next].text)
				case f["mode"] == "REPEATED":
					return st, "ALTER COLUMN SET DATA TYPE to an ARRAY", ""
				}
				act.kind, act.field = "set type", f
			case len(rest) == 2 && rest[0].is("DROP") && rest[1].is("DEFAULT"):
				act.kind = "drop default"
			case len(rest) > 1 && rest[0].is("DROP") && rest[1].is("DEFAULT"):
				return st, "", fmt.Sprintf("Syntax error: unexpected %q after DROP DEFAULT", rest[2].text)
			case len(rest) == 2 && rest[0].is("SET") && rest[1].is("DEFAULT"):
				return st, "", "Syntax error: SET DEFAULT without an expression"
			case len(rest) > 2 && rest[0].is("SET") && rest[1].is("DEFAULT"):
				act.kind = "set default"
				if len(rest) == 3 && rest[2].is("NULL") {
					act.kind = "drop default"
				}
				act.options = []alterOption{{name: "default", val: rest[2:]}}
			default:
				return st, "ALTER COLUMN as written", ""
			}
		case len(a) > 1 && a[0].is("RENAME") && a[1].is("TO"):
			to, next := path(a, 2)
			switch {
			case len(to) == 0 || next != len(a):
				return st, "", "Syntax error: RENAME TO without a table name"
			case len(to) > 1:
				// "The value of new_table_name must only include the name
				// of the table, not the full table path syntax."
				return st, "", "ALTER TABLE RENAME TO takes the new name of the table alone, not a path: " +
					strings.Join(to, ".") + " names a dataset."
			}
			act.kind, act.newName = "rename table", to[0]
		case len(a) > 1 && a[0].is("RENAME") && a[1].is("COLUMN"):
			j := 2
			if j+1 < len(a) && a[j].is("IF") && a[j+1].is("EXISTS") {
				act.ifExists = true
				j += 2
			}
			if j+2 >= len(a) || !isName(a[j]) || !a[j+1].is("TO") || !isName(a[j+2]) {
				if j+1 < len(a) && a[j+1].punct(".") {
					return st, "", nestedRefused("RENAME COLUMN", a[j].text)
				}
				return st, "", "Syntax error: RENAME COLUMN takes a column name, TO and a new name"
			}
			if j+3 != len(a) {
				return st, "", fmt.Sprintf("Syntax error: unexpected %q after RENAME COLUMN", a[j+3].text)
			}
			act.kind, act.column, act.newName = "rename column", a[j].text, a[j+2].text
		case len(a) > 1 && a[0].is("DROP") && a[1].is("COLUMN"):
			j := 2
			if j+1 < len(a) && a[j].is("IF") && a[j+1].is("EXISTS") {
				act.ifExists = true
				j += 2
			}
			if j >= len(a) || !isName(a[j]) {
				return st, "", "Syntax error: DROP COLUMN without a column name"
			}
			if j+1 != len(a) {
				if a[j+1].punct(".") {
					return st, "", nestedRefused("DROP COLUMN", a[j].text)
				}
				return st, "", fmt.Sprintf("Syntax error: unexpected %q after DROP COLUMN", a[j+1].text)
			}
			act.kind, act.column = "drop column", a[j].text
		case len(a) > 1 && (a[0].is("ADD") && (a[1].is("PRIMARY") || a[1].is("CONSTRAINT") || a[1].is("FOREIGN")) ||
			a[0].is("DROP") && (a[1].is("PRIMARY") || a[1].is("CONSTRAINT"))):
			var msg string
			if act, msg = parseKeyAction(a); msg != "" {
				return st, "", msg
			}
		case len(a) > 0:
			return st, fmt.Sprintf("%s", strings.ToUpper(a[0].text)), ""
		}
		st.actions = append(st.actions, act)
		i = end
		if i < len(t) {
			i++
		}
	}
	// "RENAME COLUMN cannot be used with other ALTER TABLE actions in one
	// statement" (#1389); DROP COLUMN and SET DATA TYPE with it are kept
	// as #1322 and #1334 carried them out.
	rename, patches := false, false
	for _, a := range st.actions {
		switch actionGroup(a.kind) {
		case "rename column":
			rename = true
		case "patch":
			patches = true
		}
	}
	if rename && patches {
		return st, "", "RENAME COLUMN cannot be used with other ALTER TABLE actions in one statement."
	}
	return st, "", ""
}

// nestedRefused is the 400 of an action on a nested field: BigQuery's
// RENAME COLUMN, DROP COLUMN and ALTER COLUMN take a top-level column
// ("Modifying subfields, such as nested columns in a STRUCT, is not
// supported").
func nestedRefused(action, column string) string {
	return action + " takes a top-level column: modifying subfields, such as the nested columns of the STRUCT " +
		column + ", is not supported."
}

// actionGroup is how alterTable carries out an action of kind: "rename
// table" (renameTable), "rename column" and "remake" (remakeTable), or
// "patch" (the tables.patch path).
func actionGroup(kind string) string {
	switch kind {
	case "rename table", "rename column":
		return kind
	case "drop column", "set type":
		return "remake"
	}
	return "patch"
}

// alterGroups splits actions into runs alterTable carries out in turn:
// consecutive actions of one way (actionGroup), RENAME COLUMN with DROP
// COLUMN and SET DATA TYPE.
func alterGroups(actions []alterAction) [][]alterAction {
	var out [][]alterAction
	way := func(k string) string {
		if g := actionGroup(k); g != "rename column" {
			return g
		}
		return "remake"
	}
	for _, a := range actions {
		if n := len(out); n > 0 && way(out[n-1][0].kind) == way(a.kind) && a.kind != "rename table" {
			out[n-1] = append(out[n-1], a)
			continue
		}
		out = append(out, []alterAction{a})
	}
	return out
}

// isName reports whether t can be a column or table name.
func isName(t token) bool { return t.kind == tokWord || t.kind == tokQuoted }

// readOptions reads OPTIONS(name = value, ...) at t[0]; t is the list and
// nothing after it.
func readOptions(t []token) ([]alterOption, bool) {
	if len(t) < 2 || !t[0].is("OPTIONS") || !t[1].punct("(") || closing(t, 1) != len(t)-1 {
		return nil, false
	}
	var out []alterOption
	i := 2
	for i < len(t)-1 {
		if t[i].kind != tokWord || i+1 >= len(t) || !t[i+1].punct("=") {
			return nil, false
		}
		name := strings.ToLower(t[i].text)
		i += 2
		end := skipTo(t, i, ",", ")")
		if end > len(t)-1 {
			end = len(t) - 1
		}
		out = append(out, alterOption{name: name, val: t[i:end]})
		i = end
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	return out, true
}

// ddlFieldTypes are the TableFieldSchema types of the DDL's type names.
var ddlFieldTypes = map[string]string{
	"INT64": "INTEGER", "FLOAT64": "FLOAT", "BOOL": "BOOLEAN", "STRING": "STRING", "BYTES": "BYTES",
	"NUMERIC": "NUMERIC", "BIGNUMERIC": "BIGNUMERIC", "DATE": "DATE", "TIME": "TIME", "DATETIME": "DATETIME",
	"TIMESTAMP": "TIMESTAMP", "GEOGRAPHY": "GEOGRAPHY", "JSON": "JSON", "INTERVAL": "INTERVAL",
}

// fieldType reads the column type at t[i] as a TableFieldSchema (no name),
// and returns the index after it; what names a type it does not read.
func fieldType(t []token, i int) (map[string]any, int, string) {
	if i >= len(t) || t[i].kind != tokWord {
		return nil, i, "a column with no type"
	}
	kind := strings.ToUpper(t[i].text)
	if alias, ok := typeAliases[kind]; ok {
		kind = alias
	}
	i++
	switch kind {
	case "ARRAY":
		if i >= len(t) || !t[i].punct("<") {
			return nil, i, "ARRAY without its element type"
		}
		elem, next, what := fieldType(t, i+1)
		if what != "" {
			return nil, i, what
		}
		if elem["mode"] == "REPEATED" {
			return nil, i, "an ARRAY of ARRAY"
		}
		if next >= len(t) || !t[next].punct(">") {
			return nil, i, "ARRAY as written"
		}
		elem["mode"] = "REPEATED"
		return elem, next + 1, ""
	case "STRUCT":
		if i >= len(t) || !t[i].punct("<") {
			return nil, i, "STRUCT without its fields"
		}
		i++
		var fields []any
		for i < len(t) && !t[i].punct(">") {
			if t[i].kind != tokWord && t[i].kind != tokQuoted || i+1 >= len(t) {
				return nil, i, "a STRUCT field with no name"
			}
			name := t[i].text
			f, next, what := fieldType(t, i+1)
			if what != "" {
				return nil, i, what
			}
			if next+1 < len(t) && t[next].is("NOT") && t[next+1].is("NULL") {
				f["mode"] = "REQUIRED"
				next += 2
			}
			if next < len(t) && t[next].is("OPTIONS") && next+1 < len(t) && t[next+1].punct("(") {
				end := closing(t, next+1)
				if end < 0 {
					return nil, i, "a STRUCT field's OPTIONS as written"
				}
				opts, ok := readOptions(t[next : end+1])
				if !ok {
					return nil, i, "a STRUCT field's OPTIONS as written"
				}
				for _, o := range opts {
					s, ok := stringLiteral(singleToken(o.val))
					if o.name != "description" || !ok {
						return nil, i, "a STRUCT field's option " + o.name + " other than a description literal"
					}
					f["description"] = s
				}
				next = end + 1
			}
			f["name"] = name
			fields = append(fields, f)
			i = next
			if i < len(t) && t[i].punct(",") {
				i++
			}
		}
		if i >= len(t) || len(fields) == 0 {
			return nil, i, "STRUCT as written"
		}
		return map[string]any{"type": "RECORD", "fields": fields}, i + 1, ""
	case "RANGE":
		if i+2 < len(t) && t[i].punct("<") && t[i+2].punct(">") {
			elem := strings.ToUpper(t[i+1].text)
			return map[string]any{"type": "RANGE", "rangeElementType": map[string]any{"type": elem}}, i + 3, ""
		}
		return nil, i, "RANGE as written"
	}
	typ, ok := ddlFieldTypes[kind]
	if !ok {
		return nil, i, "the type " + kind
	}
	f := map[string]any{"type": typ}
	if i < len(t) && t[i].punct("(") { // STRING(10), NUMERIC(10, 2)
		end := closing(t, i)
		if end < 0 {
			return nil, i, kind + " as written"
		}
		var nums []string
		for _, p := range t[i+1 : end] {
			if !p.punct(",") {
				nums = append(nums, p.text)
			}
		}
		switch {
		case (typ == "STRING" || typ == "BYTES") && len(nums) == 1:
			f["maxLength"] = nums[0]
		case (typ == "NUMERIC" || typ == "BIGNUMERIC") && len(nums) >= 1 && len(nums) <= 2:
			f["precision"] = nums[0]
			if len(nums) == 2 {
				f["scale"] = nums[1]
			}
		default:
			return nil, i, kind + " with those parameters"
		}
		i = end + 1
	}
	return f, i, ""
}

// singleToken is t's one token, or a token no literal reader takes.
func singleToken(t []token) token {
	if len(t) == 1 {
		return t[0]
	}
	return token{}
}

// alterTable carries out an ALTER TABLE statement in its turn of a script
// (above).
func (s *scriptRun) alterTable(toks []token) *scriptFlow {
	if flow := s.flush(); flow != nil {
		return flow
	}
	text := s.text(toks)
	if s.notImpl != "" {
		return &scriptFlow{}
	}
	t, ok := lex(text)
	if !ok {
		return failWith(http.StatusBadRequest, "invalidQuery", "Syntax error in the ALTER TABLE statement", text)
	}
	st, unsupported, msg := parseAlter(t)
	switch {
	case msg != "":
		return failWith(http.StatusBadRequest, "invalidQuery", msg, text)
	case unsupported != "":
		s.notImpl = alterNotImplemented(unsupported)
		return &scriptFlow{}
	}
	if msg := checkAlter(t); msg != "" {
		return failWith(http.StatusBadRequest, "invalidQuery", msg, text)
	}
	ds, table, ok := tableOf(s.q, st.path)
	if !ok {
		return failWith(http.StatusBadRequest, "invalidQuery", "Table \""+strings.Join(st.path, ".")+
			"\" must be qualified with a dataset (e.g. dataset.table).", text)
	}
	// A statement whose actions go different ways (actionGroup: RENAME
	// TO, the remaking ones, the tables.patch ones) is carried out a run
	// of them at a time, in its order, each run on the table as the one
	// before left it (#1389).
	groups := alterGroups(st.actions)
	for i, g := range groups {
		sub := st
		sub.actions, sub.ifExists = g, st.ifExists && i == 0
		if flow := s.alterRun(sub, ds, table, text); flow != nil {
			return flow
		}
		if g[0].kind == "rename table" {
			table = g[0].newName
		}
	}
	return s.assigned()
}

// alterRun carries out st's actions, all of one way (alterGroups), on
// ds.table; nil when done.
func (s *scriptRun) alterRun(st alterStmt, ds, table, text string) *scriptFlow {
	project := s.project()
	name := project + ":" + ds + "." + table
	status, got := s.f.get(s.r, tablePath(ds, table))
	if status == http.StatusNotFound {
		if st.ifExists {
			return nil
		}
		return failWith(http.StatusNotFound, "notFound", "Not found: Table "+name, text)
	}
	var current struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
		Schema struct {
			Fields []map[string]any `json:"fields"`
		} `json:"schema"`
		TableConstraints *tableConstraints `json:"tableConstraints"`
		TimePartitioning map[string]any    `json:"timePartitioning"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &current) != nil {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: could not read "+name, text)
	}
	if current.Type != "" && !strings.EqualFold(current.Type, "TABLE") {
		return failWith(http.StatusBadRequest, "invalidQuery", "ALTER TABLE is not supported for "+name+
			", a "+current.Type+".", text)
	}
	if actionGroup(st.actions[0].kind) != "patch" { // all of one way, alterGroups
		return s.remakeTable(st, ds, table, name, got, text)
	}
	fields := current.Schema.Fields
	schemaChanged := false
	patch := map[string]any{}
	find := func(column string) map[string]any {
		for _, f := range fields {
			if n, _ := f["name"].(string); strings.EqualFold(n, column) {
				return f
			}
		}
		return nil
	}
	cons := current.TableConstraints
	if cons == nil {
		cons = &tableConstraints{}
	}
	consChanged := false
	for _, a := range st.actions {
		switch a.kind {
		case "add primary key", "add foreign key", "drop primary key", "drop constraint":
			changed, flow := s.applyKey(cons, a, func(c string) bool { return find(c) != nil }, name, text)
			if flow != nil {
				return flow
			}
			consChanged = consChanged || changed
		case "add":
			if find(a.column) != nil {
				if a.ifExists {
					continue
				}
				return failWith(http.StatusBadRequest, "invalidQuery", "Column already exists: "+a.column, text)
			}
			if a.field["mode"] == "REQUIRED" {
				return failWith(http.StatusBadRequest, "invalidQuery", "Cannot add a NOT NULL column "+a.column+
					" to an existing table "+name+"; a column added to a table must be NULLABLE or REPEATED.", text)
			}
			if a.description != nil {
				v, flow := s.stringOption(*a.description, text)
				if flow != nil {
					return flow
				}
				if v != nil {
					a.field["description"] = *v
				}
			}
			fields = append(fields, a.field)
			schemaChanged = true
		case "set default", "drop default": // #1340, columndefaults.go
			f := find(a.column)
			if f == nil {
				if a.ifExists {
					continue
				}
				return failWith(http.StatusBadRequest, "invalidQuery", "Column not found: "+a.column+" in table "+name, text)
			}
			if a.kind == "drop default" {
				if _, ok := f["defaultValueExpression"]; ok {
					delete(f, "defaultValueExpression")
					schemaChanged = true
				}
				continue
			}
			if cons.PrimaryKey != nil {
				for _, c := range cons.PrimaryKey.Columns {
					if strings.EqualFold(c, a.column) {
						// "You can't set default values on columns that
						// are primary keys" (#1389).
						return failWith(http.StatusBadRequest, "invalidQuery", "Cannot set a default value on column "+
							a.column+" of table "+name+": it is a column of the table's primary key.", text)
					}
				}
			}
			f["defaultValueExpression"] = exprText(text, a.options[0].val)
			schemaChanged = true
		case "drop not null", "column options":
			f := find(a.column)
			if f == nil {
				if a.ifExists {
					continue
				}
				return failWith(http.StatusBadRequest, "invalidQuery", "Column not found: "+a.column+" in table "+name, text)
			}
			if a.kind == "drop not null" {
				if f["mode"] == "REQUIRED" {
					f["mode"] = "NULLABLE"
					schemaChanged = true
				}
				continue
			}
			for _, o := range a.options {
				v, flow := s.stringOption(o, text)
				if flow != nil {
					return flow
				}
				if v == nil || *v == "" {
					delete(f, "description")
				} else {
					f["description"] = *v
				}
				schemaChanged = true
			}
		case "options":
			for _, o := range a.options {
				switch o.name {
				case "description", "friendly_name":
					v, flow := s.stringOption(o, text)
					if flow != nil {
						return flow
					}
					key := "description"
					if o.name == "friendly_name" {
						key = "friendlyName"
					}
					if v == nil {
						patch[key] = nil
					} else {
						patch[key] = *v
					}
				case "labels":
					labels := map[string]any{}
					for k := range current.Labels {
						labels[k] = nil
					}
					if !(len(o.val) == 1 && o.val[0].is("NULL")) {
						given, ok := labelsLiteral(o.val)
						if !ok {
							s.notImpl = alterNotImplemented("SET OPTIONS (labels) set to other than an array of (key, value) string literals")
							return &scriptFlow{}
						}
						for k, v := range given {
							labels[k] = v
						}
					}
					patch["labels"] = labels
				case "partition_expiration_days": // #1390
					ms, ok := partitionExpirationMs(o.val)
					switch {
					case !ok:
						s.notImpl = alterNotImplemented("SET OPTIONS (partition_expiration_days) set to other than a number literal")
						return &scriptFlow{}
					case current.TimePartitioning == nil:
						return failWith(http.StatusBadRequest, "invalidQuery", partitionExpirationRefused, text)
					}
					tp := map[string]any{}
					for k, v := range current.TimePartitioning {
						tp[k] = v
					}
					delete(tp, "expirationMs")
					if ms != "NULL" {
						tp["expirationMs"] = ms
					}
					patch["timePartitioning"] = tp
				case "kms_key_name": // #1390, stored
					v, flow := s.stringOption(o, text)
					if flow != nil {
						return flow
					}
					if v == nil {
						patch["encryptionConfiguration"] = nil
					} else {
						patch["encryptionConfiguration"] = map[string]any{"kmsKeyName": *v}
					}
				case "expiration_timestamp":
					if len(o.val) == 1 && o.val[0].is("NULL") {
						patch["expirationTime"] = nil
						continue
					}
					v, flow := s.optionValue("UNIX_MILLIS(CAST(("+exprText(text, o.val)+") AS TIMESTAMP))", text)
					if flow != nil {
						return flow
					}
					if v == nil {
						patch["expirationTime"] = nil
					} else {
						patch["expirationTime"] = *v
					}
				}
			}
		}
	}
	if schemaChanged {
		patch["schema"] = map[string]any{"fields": fields}
	}
	if consChanged {
		if cons.empty() {
			patch["tableConstraints"] = nil
		} else {
			patch["tableConstraints"] = cons
		}
	}
	if len(patch) == 0 {
		return nil
	}
	return s.patchTable(ds, table, patch, text)
}

// exprText is the text of the tokens val, a part of text.
func exprText(text string, val []token) string {
	if len(val) == 0 {
		return "NULL"
	}
	return text[val[0].pos:val[len(val)-1].end]
}

// stringOption is an option's STRING value: a literal read here, or an
// expression the emulator evaluates; nil for NULL.
func (s *scriptRun) stringOption(o alterOption, text string) (*string, *scriptFlow) {
	if len(o.val) == 1 && o.val[0].is("NULL") {
		return nil, nil
	}
	if v, ok := stringLiteral(singleToken(o.val)); ok {
		return &v, nil
	}
	return s.optionValue("CAST(("+exprText(text, o.val)+") AS STRING)", text)
}

// optionValue runs SELECT expr on the emulator and returns its one value
// as the REST form gives it, nil for NULL.
func (s *scriptRun) optionValue(expr, text string) (*string, *scriptFlow) {
	rec, flow := s.send("SELECT " + expr)
	if flow != nil {
		return nil, flow
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if json.Unmarshal(rec.body.Bytes(), &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 {
		return nil, failWith(http.StatusBadRequest, "invalidQuery", "Could not evaluate the option "+expr, text)
	}
	return res.Rows[0].F[0].V, nil
}

// patchTable sends patch as a tables.patch of dataset.table through the
// front (updateTable), and returns the failed statement's flow, or nil.
func (s *scriptRun) patchTable(dataset, table string, patch map[string]any, text string) *scriptFlow {
	body, err := json.Marshal(patch)
	if err != nil {
		return failWith(http.StatusInternalServerError, "internalError", err.Error(), text)
	}
	u := *s.r.URL
	p := s.f.base + tablePath(dataset, table)
	u.Path, u.RawPath, u.RawQuery = p, "", ""
	if unescaped, err := url.PathUnescape(p); err == nil {
		u.Path, u.RawPath = unescaped, p
	}
	req, err := http.NewRequestWithContext(s.r.Context(), http.MethodPatch, u.String(), io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return failWith(http.StatusInternalServerError, "internalError", err.Error(), text)
	}
	req.Header.Set("Content-Type", "application/json")
	if a := s.r.Header.Get("Authorization"); a != "" {
		req.Header.Set("Authorization", a)
	}
	req.Host, req.RemoteAddr = s.r.Host, s.r.RemoteAddr
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/1.1", 1, 1
	req.ContentLength = int64(len(body))
	rec := newRecorder()
	s.f.insertTable(rec, req, dataset, table, true)
	if rec.status == 0 || rec.status == http.StatusOK {
		return nil
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.body.Bytes(), &e)
	msg := e.Error.Message
	if msg == "" {
		msg = "ALTER TABLE failed: " + strconv.Itoa(rec.status)
	}
	if rec.status == http.StatusNotImplemented {
		s.notImpl = msg
		return &scriptFlow{}
	}
	reason := "invalidQuery"
	if rec.status == http.StatusNotFound {
		reason = "notFound"
	}
	return failWith(rec.status, reason, msg, text)
}
