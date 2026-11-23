package main

// Parser-based auto-completion in BigQuery Studio's query editor (#2094):
// "To automatically display code suggestions when you type a query, click
// Tools > Parser-based auto-completion. If you don't need autocomplete
// suggestions, deselect Parser-based auto-completion. This also turns off
// the project name autofill suggestions"
// (https://docs.cloud.google.com/bigquery/docs/running-queries, Run an
// interactive query, step 4, read 2026-11-23).
//
// The editor sends the text before and after the cursor (POST
// /api/studio/bigquery?call=complete); the answer is the text before the
// cursor to replace and the suggestions. They come from reading the
// statement as GoogleSQL's grammar has it (its lexical structure:
// identifiers, backtick-quoted paths, strings, comments, keywords;
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/lexical):
// where a table path goes (after FROM, JOIN, INTO, UPDATE, TABLE, MERGE, or
// a FROM list's comma) they are the project, its datasets and a dataset's
// tables from this instance's catalog (datasets.list, tables.list); in an
// expression they are the columns of the statement's tables (tables.get's
// schema), a table's or alias's columns after "alias.", a STRUCT's fields
// after "column.", a dataset's routines after "dataset.", and GoogleSQL's
// keywords and functions. Nothing is suggested inside a string or a
// comment.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	bqv2 "google.golang.org/api/bigquery/v2"
)

type bqCompleteRequest struct {
	Before         string `json:"before"`
	After          string `json:"after"`
	DefaultDataset string `json:"defaultDataset"`
}

type bqCompletion struct {
	Label  string `json:"label"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	Insert string `json:"insert"`
}

type bqCompleteAnswer struct {
	// Replace is the end of the text before the cursor that a chosen
	// suggestion replaces.
	Replace string         `json:"replace"`
	Items   []bqCompletion `json:"items"`
}

// sqlTok is a token of a GoogleSQL statement: an identifier or keyword
// ('i'), a backtick-quoted identifier or path ('q', its text unquoted), a
// literal ('l') or punctuation ('p').
type sqlTok struct {
	kind       byte
	text       string
	start, end int
}

// sqlLex splits s into tokens. open reports what s ends inside of: 0 for
// nothing, '`' for a quoted identifier begun at openAt, 's' for a string
// literal or a comment.
func sqlLex(s string) (toks []sqlTok, open byte, openAt int) {
	isWord := func(c byte) bool {
		return c == '_' || c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'z' || c >= 0x80
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '#' || strings.HasPrefix(s[i:], "--"):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				return toks, 's', i
			}
			i += end + 1
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return toks, 's', i
			}
			i += end + 4
		case c == '`':
			n, err := skipQuoted(s[i:])
			if err != nil {
				return toks, '`', i
			}
			toks = append(toks, sqlTok{'q', strings.ReplaceAll(s[i+1:i+n-1], "\\`", "`"), i, i + n})
			i += n
		case c == '\'' || c == '"':
			n, err := skipQuoted(s[i:])
			if err != nil {
				return toks, 's', i
			}
			toks = append(toks, sqlTok{'l', s[i : i+n], i, i + n})
			i += n
		case isWord(c):
			j := i
			for j < len(s) && isWord(s[j]) {
				j++
			}
			// A string prefix: r'…', b"…", rb'…'.
			if j < len(s) && (s[j] == '\'' || s[j] == '"') && j-i <= 2 && strings.Trim(strings.ToLower(s[i:j]), "rb") == "" {
				n, err := skipQuoted(s[j:])
				if err != nil {
					return toks, 's', i
				}
				toks = append(toks, sqlTok{'l', s[i : j+n], i, j + n})
				i = j + n
				continue
			}
			kind := byte('i')
			if c >= '0' && c <= '9' {
				kind = 'l'
			}
			toks = append(toks, sqlTok{kind, s[i:j], i, j})
			i = j
		default:
			toks = append(toks, sqlTok{'p', s[i : i+1], i, i + 1})
			i++
		}
	}
	return toks, 0, 0
}

// bqKeywords are GoogleSQL's reserved keywords
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/lexical#reserved_keywords)
// and the statements' leading words.
var bqKeywords = strings.Fields(`ALL AND ANY ARRAY AS ASC ASSERT_ROWS_MODIFIED AT BETWEEN BY CASE CAST COLLATE CONTAINS CREATE CROSS CUBE
CURRENT DEFAULT DEFINE DESC DISTINCT ELSE END ENUM ESCAPE EXCEPT EXCLUDE EXISTS EXTRACT FALSE FETCH FOLLOWING FOR FROM FULL GROUP
GROUPING GROUPS HASH HAVING IF IGNORE IN INNER INTERSECT INTERVAL INTO IS JOIN LATERAL LEFT LIKE LIMIT LOOKUP MERGE NATURAL NEW NO NOT
NULL NULLS OF ON OR ORDER OUTER OVER PARTITION PRECEDING PROTO QUALIFY RANGE RECURSIVE RESPECT RIGHT ROLLUP ROWS SELECT SET SOME STRUCT
TABLESAMPLE THEN TO TREAT TRUE UNBOUNDED UNION UNNEST USING WHEN WHERE WINDOW WITH WITHIN
INSERT UPDATE DELETE VALUES TABLE VIEW REPLACE DROP ALTER TRUNCATE DECLARE BEGIN EXPORT DATA OPTIONS`)

// bqFunctions are GoogleSQL's built-in functions the editor suggests
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/functions-all).
var bqFunctions = strings.Fields(`ABS ANY_VALUE APPROX_COUNT_DISTINCT APPROX_QUANTILES APPROX_TOP_COUNT ARRAY_AGG ARRAY_CONCAT ARRAY_LENGTH
ARRAY_TO_STRING AVG BIT_AND BIT_OR BOOL BYTE_LENGTH CEIL CHAR_LENGTH COALESCE CONCAT CORR COUNT COUNTIF COVAR_POP CURRENT_DATE
CURRENT_DATETIME CURRENT_TIME CURRENT_TIMESTAMP DATE DATE_ADD DATE_DIFF DATE_SUB DATE_TRUNC DATETIME DATETIME_ADD DATETIME_DIFF
DATETIME_TRUNC DENSE_RANK DIV ENDS_WITH EXP FARM_FINGERPRINT FIRST_VALUE FLOOR FORMAT FORMAT_DATE FORMAT_TIMESTAMP GENERATE_ARRAY
GENERATE_DATE_ARRAY GENERATE_UUID GREATEST IFNULL INITCAP INSTR JSON_EXTRACT JSON_EXTRACT_SCALAR JSON_QUERY JSON_VALUE LAG LAST_DAY
LAST_VALUE LEAD LEAST LENGTH LN LOG LOG10 LOGICAL_AND LOGICAL_OR LOWER LPAD LTRIM MAX MD5 MIN MOD NORMALIZE NTILE NULLIF
PARSE_DATE PARSE_DATETIME PARSE_JSON PARSE_TIMESTAMP PERCENT_RANK PERCENTILE_CONT POW RAND RANK REGEXP_CONTAINS REGEXP_EXTRACT
REGEXP_REPLACE REPEAT REVERSE ROUND ROW_NUMBER RPAD RTRIM SAFE_CAST SAFE_DIVIDE SHA256 SIGN SPLIT SQRT ST_DISTANCE ST_GEOGPOINT
STARTS_WITH STDDEV STRING STRING_AGG STRPOS SUBSTR SUM TIMESTAMP TIMESTAMP_ADD TIMESTAMP_DIFF TIMESTAMP_SUB TIMESTAMP_TRUNC
TO_JSON TO_JSON_STRING TRIM TRUNC UNIX_SECONDS UPPER VAR_POP VARIANCE`)

var bqKeywordSet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range bqKeywords {
		m[k] = true
	}
	return m
}()

// bqTableClauses are the keywords a table path follows.
var bqTableClauses = map[string]bool{"FROM": true, "JOIN": true, "INTO": true, "UPDATE": true, "TABLE": true, "MERGE": true}

// bqClauseWords change the clause an expression is in.
var bqClauseWords = map[string]bool{"SELECT": true, "FROM": true, "JOIN": true, "WHERE": true, "GROUP": true, "ORDER": true,
	"HAVING": true, "ON": true, "USING": true, "INTO": true, "UPDATE": true, "TABLE": true, "MERGE": true, "SET": true,
	"QUALIFY": true, "WINDOW": true, "LIMIT": true, "WITH": true, "VALUES": true, "UNION": true, "BY": true, "AS": true}

// bqRef is a table the statement reads, with its alias.
type bqRef struct {
	path  []string
	alias string
}

// sqlPathAt reads a table path at toks[i]: names joined by dots, a quoted
// one split at its dots. It returns the path and the index after it.
func sqlPathAt(toks []sqlTok, i int) ([]string, int) {
	var path []string
	for i < len(toks) && (toks[i].kind == 'i' || toks[i].kind == 'q') {
		if toks[i].kind == 'i' && bqKeywordSet[strings.ToUpper(toks[i].text)] {
			break
		}
		if toks[i].kind == 'q' {
			path = append(path, strings.Split(toks[i].text, ".")...)
		} else {
			path = append(path, toks[i].text)
		}
		i++
		if i+1 < len(toks) && toks[i].kind == 'p' && toks[i].text == "." && toks[i].end == toks[i+1].start {
			i++
			continue
		}
		break
	}
	return path, i
}

// sqlRefs are the tables toks reads (after FROM and JOIN, and a FROM
// list's commas), and the names its WITH clause defines.
func sqlRefs(toks []sqlTok) (refs []bqRef, ctes map[string]bool) {
	ctes = map[string]bool{}
	inFrom := false
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		up := strings.ToUpper(t.text)
		// WITH name AS ( … ), name AS ( … )
		if t.kind == 'i' && i+1 < len(toks) && strings.EqualFold(toks[i+1].text, "AS") && i+2 < len(toks) && toks[i+2].text == "(" && i > 0 &&
			(strings.EqualFold(toks[i-1].text, "WITH") || toks[i-1].text == ",") {
			ctes[strings.ToLower(t.text)] = true
		}
		start := false
		switch {
		case t.kind == 'i' && (up == "FROM" || up == "JOIN"):
			inFrom, start = true, true
		case t.kind == 'p' && t.text == "," && inFrom:
			start = true
		case t.kind == 'i' && bqClauseWords[up] && up != "AS":
			inFrom = false
		case t.kind == 'p' && (t.text == "(" || t.text == ")" || t.text == ";"):
			inFrom = false
		}
		if !start {
			continue
		}
		path, j := sqlPathAt(toks, i+1)
		if len(path) == 0 {
			continue
		}
		ref := bqRef{path: path}
		if j < len(toks) && strings.EqualFold(toks[j].text, "AS") {
			j++
		}
		if j < len(toks) && (toks[j].kind == 'q' || toks[j].kind == 'i' && !bqKeywordSet[strings.ToUpper(toks[j].text)]) {
			ref.alias = toks[j].text
			j++
		}
		refs = append(refs, ref)
		i = j - 1
		if up == "JOIN" || t.text == "," || up == "FROM" {
			inFrom = true
		}
	}
	return refs, ctes
}

// bqCatalog reads this instance's catalog once per request.
type bqCatalog struct {
	ctx     context.Context
	svc     *bqv2.Service
	project string
	sets    []string
	tables  map[string][]*bqv2.TableListTables
	schemas map[string][]*bqv2.TableFieldSchema
}

func (c *bqCatalog) datasets() []string {
	if c.sets == nil {
		c.sets = []string{}
		_ = c.svc.Datasets.List(c.project).Pages(c.ctx, func(l *bqv2.DatasetList) error {
			for _, d := range l.Datasets {
				if d.DatasetReference != nil {
					c.sets = append(c.sets, d.DatasetReference.DatasetId)
				}
			}
			return nil
		})
		sort.Strings(c.sets)
	}
	return c.sets
}

func (c *bqCatalog) hasDataset(ds string) bool {
	for _, d := range c.datasets() {
		if strings.EqualFold(d, ds) {
			return true
		}
	}
	return false
}

func (c *bqCatalog) tablesOf(project, ds string) []*bqv2.TableListTables {
	key := project + "." + ds
	if t, ok := c.tables[key]; ok {
		return t
	}
	var out []*bqv2.TableListTables
	_ = c.svc.Tables.List(project, ds).Pages(c.ctx, func(l *bqv2.TableList) error {
		out = append(out, l.Tables...)
		return nil
	})
	c.tables[key] = out
	return out
}

func (c *bqCatalog) schema(path []string, defaultDataset string) []*bqv2.TableFieldSchema {
	project, ds, table := c.project, defaultDataset, ""
	switch len(path) {
	case 1:
		table = path[0]
	case 2:
		ds, table = path[0], path[1]
	case 3:
		project, ds, table = path[0], path[1], path[2]
	default:
		return nil
	}
	if ds == "" || table == "" {
		return nil
	}
	key := project + "." + ds + "." + table
	if s, ok := c.schemas[key]; ok {
		return s
	}
	var fields []*bqv2.TableFieldSchema
	if t, err := c.svc.Tables.Get(project, ds, table).Context(c.ctx).Do(); err == nil && t.Schema != nil {
		fields = t.Schema.Fields
	}
	c.schemas[key] = fields
	return fields
}

// bqNeedsQuotes reports whether name must be quoted with backticks.
func bqNeedsQuotes(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' || bqKeywordSet[strings.ToUpper(name)] {
		return true
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'z') {
			return true
		}
	}
	return false
}

func (p bigqueryProvider) studioComplete(ctx context.Context, svc *bqv2.Service, body json.RawMessage) (*bqCompleteAnswer, error) {
	var req bqCompleteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("a completion request: %w", err)
	}
	ans := &bqCompleteAnswer{Items: []bqCompletion{}}
	before := req.Before
	toks, open, openAt := sqlLex(before)
	if open == 's' {
		return ans, nil
	}
	var qual []string
	var partial string
	quoted := false
	prev := toks
	if open == '`' {
		inner := strings.ReplaceAll(before[openAt+1:], "\\`", "`")
		parts := strings.Split(inner, ".")
		qual, partial = parts[:len(parts)-1], parts[len(parts)-1]
		ans.Replace = before[openAt:]
		quoted = true
	} else {
		i := len(before)
		for i > 0 && (before[i-1] == '_' || before[i-1] >= '0' && before[i-1] <= '9' || before[i-1]|0x20 >= 'a' && before[i-1]|0x20 <= 'z') {
			i--
		}
		partial = before[i:]
		ans.Replace = partial
		if len(prev) > 0 && partial != "" && prev[len(prev)-1].end == len(before) {
			prev = prev[:len(prev)-1]
		}
		at := i
		for len(prev) >= 2 {
			dot, name := prev[len(prev)-1], prev[len(prev)-2]
			if dot.kind != 'p' || dot.text != "." || dot.end != at || (name.kind != 'i' && name.kind != 'q') || name.end != dot.start {
				break
			}
			if name.kind == 'q' {
				qual = append(strings.Split(name.text, "."), qual...)
			} else {
				qual = append([]string{name.text}, qual...)
			}
			at = name.start
			prev = prev[:len(prev)-2]
		}
	}
	// The clause the cursor is in, at its parenthesis depth.
	clauses := []string{""}
	for _, t := range prev {
		switch {
		case t.kind == 'p' && t.text == "(":
			clauses = append(clauses, "")
		case t.kind == 'p' && t.text == ")":
			if len(clauses) > 1 {
				clauses = clauses[:len(clauses)-1]
			}
		case t.kind == 'p' && t.text == ";":
			clauses = []string{""}
		case t.kind == 'i' && bqClauseWords[strings.ToUpper(t.text)]:
			clauses[len(clauses)-1] = strings.ToUpper(t.text)
		}
	}
	clause := clauses[len(clauses)-1]
	var last sqlTok
	if len(prev) > 0 {
		last = prev[len(prev)-1]
	}
	tableSpot := bqTableClauses[clause] && (strings.EqualFold(last.text, clause) || clause == "FROM" && last.text == "," ||
		clause == "INTO" && strings.EqualFold(last.text, "INTO"))

	cat := &bqCatalog{ctx: ctx, svc: svc, project: p.project, tables: map[string][]*bqv2.TableListTables{}, schemas: map[string][]*bqv2.TableFieldSchema{}}
	low := strings.ToLower(partial)
	add := func(label, kind, detail, insert string) {
		if strings.HasPrefix(strings.ToLower(label), low) {
			ans.Items = append(ans.Items, bqCompletion{Label: label, Kind: kind, Detail: detail, Insert: insert})
		}
	}
	// name inserts a path's next name: inside backticks, the whole
	// quoted path so far; otherwise the name, quoted when it must be.
	name := func(n string, more bool) string {
		if quoted {
			s := "`" + strings.Join(append(append([]string{}, qual...), n), ".")
			if more {
				return s + "."
			}
			if !strings.HasPrefix(req.After, "`") {
				s += "`"
			}
			return s
		}
		if bqNeedsQuotes(n) {
			if more {
				return "`" + n + "."
			}
			return "`" + n + "`"
		}
		if more {
			return n + "."
		}
		return n
	}
	tables := func(project, ds string) {
		for _, t := range cat.tablesOf(project, ds) {
			if t.TableReference != nil {
				add(t.TableReference.TableId, "Table", strings.ToLower(t.Type), name(t.TableReference.TableId, false))
			}
		}
	}
	if tableSpot || quoted {
		switch len(qual) {
		case 0:
			// The project name autofill: the project, then its datasets.
			add(p.project, "Project", "", name(p.project, true))
			for _, d := range cat.datasets() {
				add(d, "Dataset", "", name(d, true))
			}
			if !quoted {
				_, ctes := sqlRefs(append(toks, bqTail(req.After)...))
				for c := range ctes {
					add(c, "Table", "WITH", c)
				}
				if req.DefaultDataset != "" {
					tables(p.project, req.DefaultDataset)
				}
				add("UNNEST", "Keyword", "", "UNNEST")
			}
		case 1:
			if strings.EqualFold(qual[0], p.project) {
				for _, d := range cat.datasets() {
					add(d, "Dataset", "", name(d, true))
				}
			} else if cat.hasDataset(qual[0]) {
				tables(p.project, qual[0])
			}
		case 2:
			tables(qual[0], qual[1])
		}
		sortCompletions(ans.Items)
		return ans, nil
	}

	// An expression: the statement's tables' columns.
	stmt := bqStatementAt(toks, bqTail(req.After))
	refs, _ := sqlRefs(stmt)
	if len(qual) > 0 {
		var fields []*bqv2.TableFieldSchema
		found := false
		for _, r := range refs {
			if strings.EqualFold(r.alias, qual[0]) || r.alias == "" && strings.EqualFold(r.path[len(r.path)-1], qual[0]) {
				fields, found = cat.schema(r.path, req.DefaultDataset), true
				qual = qual[1:]
				break
			}
		}
		if !found {
			// A column of one of the tables, whose STRUCT fields follow.
			for _, r := range refs {
				for _, f := range cat.schema(r.path, req.DefaultDataset) {
					if strings.EqualFold(f.Name, qual[0]) {
						fields, found = []*bqv2.TableFieldSchema{f}, true
					}
				}
			}
			if found {
				fields = fields[0].Fields
				qual = qual[1:]
			}
		}
		for _, q := range qual {
			var next []*bqv2.TableFieldSchema
			for _, f := range fields {
				if strings.EqualFold(f.Name, q) {
					next = f.Fields
				}
			}
			fields = next
		}
		for _, f := range fields {
			add(f.Name, "Column", f.Type, name(f.Name, false))
		}
		if !found && len(qual) == 1 && cat.hasDataset(qual[0]) {
			if rs, err := svc.Routines.List(p.project, qual[0]).Context(ctx).Do(); err == nil {
				for _, rt := range rs.Routines {
					if rt.RoutineReference != nil {
						add(rt.RoutineReference.RoutineId, "Function", strings.ToLower(rt.RoutineType), rt.RoutineReference.RoutineId)
					}
				}
			}
		}
		sortCompletions(ans.Items)
		return ans, nil
	}
	if partial == "" && !(last.kind == 'p' && (last.text == "," || last.text == "(")) && !strings.EqualFold(last.text, "SELECT") &&
		!strings.EqualFold(last.text, "WHERE") && !strings.EqualFold(last.text, "BY") && !strings.EqualFold(last.text, "ON") && !strings.EqualFold(last.text, "AND") && !strings.EqualFold(last.text, "OR") {
		// Nothing typed after a name or a value: no column is due.
		for _, k := range bqKeywords {
			add(k, "Keyword", "", k)
		}
		sortCompletions(ans.Items)
		return ans, nil
	}
	seen := map[string]bool{}
	for _, r := range refs {
		table := r.path[len(r.path)-1]
		for _, f := range cat.schema(r.path, req.DefaultDataset) {
			if !seen[strings.ToLower(f.Name)] {
				seen[strings.ToLower(f.Name)] = true
				add(f.Name, "Column", f.Type+" · "+table, name(f.Name, false))
			}
		}
		if r.alias != "" {
			add(r.alias, "Alias", strings.Join(r.path, "."), r.alias)
		}
	}
	for _, f := range bqFunctions {
		add(f, "Function", "", f)
	}
	for _, k := range bqKeywords {
		add(k, "Keyword", "", k)
	}
	sortCompletions(ans.Items)
	return ans, nil
}

// bqTail is the tokens of the text after the cursor, whose positions are
// past the text before it.
func bqTail(after string) []sqlTok {
	toks, _, _ := sqlLex(after)
	for i := range toks {
		toks[i].start += 1 << 30
		toks[i].end += 1 << 30
	}
	return toks
}

// bqStatementAt is the statement the cursor is in: the tokens before it
// back to a semicolon, and after it up to one.
func bqStatementAt(before, after []sqlTok) []sqlTok {
	start := 0
	for i, t := range before {
		if t.kind == 'p' && t.text == ";" {
			start = i + 1
		}
	}
	end := len(after)
	for i, t := range after {
		if t.kind == 'p' && t.text == ";" {
			end = i
			break
		}
	}
	return append(append([]sqlTok{}, before[start:]...), after[:end]...)
}

var bqKindOrder = map[string]int{"Column": 0, "Alias": 1, "Project": 2, "Dataset": 3, "Table": 4, "Function": 5, "Keyword": 6}

func sortCompletions(items []bqCompletion) {
	sort.SliceStable(items, func(a, b int) bool {
		if bqKindOrder[items[a].Kind] != bqKindOrder[items[b].Kind] {
			return bqKindOrder[items[a].Kind] < bqKindOrder[items[b].Kind]
		}
		return strings.ToLower(items[a].Label) < strings.ToLower(items[b].Label)
	})
}
