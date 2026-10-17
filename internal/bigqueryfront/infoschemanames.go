package bigqueryfront

import (
	"sort"
	"strings"
)

// The dataset and table names a query of an INFORMATION_SCHEMA view can
// read, pushed down into which datasets and tables the view's rows are
// built from (#1510).
//
// Measured on an instance of 300 tables in 30 datasets (2026-10-17):
// `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE WHERE table_schema = 'ds_7'
// took 24 s, as long as with no WHERE, since the front read and costed
// every table of the region (tables.get, then the partitions, logical
// bytes and last change of each, each a query of the engine) before the
// engine applied the WHERE; late in a whole compat run, with hundreds of
// tables more, it passed the test's minute. Region TABLES and COLUMNS of
// one dataset took 4 to 5 s, reading every table's tables.get.
//
// As with JOBS (infoschemajobsfilter.go), what is pushed down never changes
// the answer: the query still runs with its WHERE, and a dataset or table
// is left out only when no row of it can pass. That is the case for a
// conjunct of the top-level WHERE of the one SELECT whose only FROM item
// is the view: the view's dataset column (table_schema, or a routine
// view's routine_schema and specific_schema) or its table_name, = or IN
// string literals, unqualified or qualified by the view's alias. Both are
// compared exactly, as = compares strings.

// nameFilter keeps the datasets and tables a query of a view can read; a
// nil set keeps every one, and a nil filter everything.
type nameFilter struct {
	schemas, tables map[string]bool
}

// schema reports whether the filter keeps dataset.
func (n *nameFilter) schema(dataset string) bool {
	return n == nil || n.schemas == nil || n.schemas[dataset]
}

// table reports whether the filter keeps table.
func (n *nameFilter) table(name string) bool {
	return n == nil || n.tables == nil || n.tables[name]
}

// key names the filter, for the subqueries of one query kept by view.
func (n *nameFilter) key() string {
	if n == nil {
		return ""
	}
	set := func(m map[string]bool) string {
		if m == nil {
			return "*"
		}
		var s []string
		for k := range m {
			s = append(s, k)
		}
		sort.Strings(s)
		return strings.Join(s, "\x01")
	}
	return "\x00" + set(n.schemas) + "\x00" + set(n.tables)
}

// nameColumns are, by view, the columns that name a row's dataset and the
// column that names its table ("" for a view of routines, whose rows are
// not read table by table).
var nameColumns = map[string]struct {
	schemas []string
	table   string
}{
	"TABLES": {[]string{"table_schema"}, "table_name"}, "COLUMNS": {[]string{"table_schema"}, "table_name"},
	"COLUMN_FIELD_PATHS": {[]string{"table_schema"}, "table_name"}, "TABLE_OPTIONS": {[]string{"table_schema"}, "table_name"},
	"VIEWS": {[]string{"table_schema"}, "table_name"}, "TABLE_SNAPSHOTS": {[]string{"table_schema"}, "table_name"},
	"TABLE_STORAGE": {[]string{"table_schema"}, "table_name"}, "PARTITIONS": {[]string{"table_schema"}, "table_name"},
	"MATERIALIZED_VIEWS": {[]string{"table_schema"}, "table_name"}, "TABLE_CONSTRAINTS": {[]string{"table_schema"}, "table_name"},
	"KEY_COLUMN_USAGE": {[]string{"table_schema"}, "table_name"}, "CONSTRAINT_COLUMN_USAGE": {[]string{"table_schema"}, "table_name"},
	"SEARCH_INDEXES": {[]string{"table_schema"}, "table_name"}, "VECTOR_INDEXES": {[]string{"table_schema"}, "table_name"},
	"ROUTINES":        {[]string{"routine_schema", "specific_schema"}, ""},
	"PARAMETERS":      {[]string{"specific_schema"}, ""},
	"ROUTINE_OPTIONS": {[]string{"specific_schema"}, ""},
}

// pushedNameFilter is the filter of the reference sql[pos:end] to view,
// or nil when nothing can be pushed down.
func pushedNameFilter(sql string, pos, end int, alias bool, view string) *nameFilter {
	cols, ok := nameColumns[view]
	if !ok {
		return nil
	}
	w, ok := whereOf(sql, pos, end, alias, view)
	if !ok {
		return nil
	}
	var n nameFilter
	narrow := func(set *map[string]bool, vals []string) {
		next := map[string]bool{}
		for _, v := range vals {
			if *set == nil || (*set)[v] {
				next[v] = true
			}
		}
		*set = next
	}
	for _, c := range w.conjuncts {
		col, vals, ok := stringIn(unwrap(c), w.qualifier)
		if !ok {
			continue
		}
		switch {
		case cols.table != "" && col == cols.table:
			narrow(&n.tables, vals)
		default:
			for _, s := range cols.schemas {
				if col == s {
					narrow(&n.schemas, vals)
				}
			}
		}
	}
	if n.schemas == nil && n.tables == nil {
		return nil
	}
	return &n
}
