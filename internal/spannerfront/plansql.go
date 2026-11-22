package spannerfront

// The query parser the plan builder (plan.go, #2029) reads a query with:
// GoogleSQL and PostgreSQL-dialect SELECT statements, as far as a plan
// needs them. It keeps every expression's place in the text, so the row
// counts PROFILE reports are counted by the engine on the query's own text
// (planstats.go). The query is first validated by the engine itself, so
// this parser only has to understand valid queries; a construct it does
// not know is refused by name, never planned by guess.

import (
	"fmt"
	"strings"
)

type tokKind int

const (
	tkEOF tokKind = iota
	tkIdent
	tkQIdent // a quoted identifier: `x` (GoogleSQL) or "x" (PostgreSQL)
	tkString
	tkNumber
	tkParam
	tkPunct
	tkPGHint // a PostgreSQL hint comment, /*@ ... */
)

type token struct {
	kind       tokKind
	text       string // an identifier's name, a punctuation, a hint's body
	start, end int    // its place in the source
}

// lexSQL splits a statement into tokens; comments are dropped, except a
// PostgreSQL hint comment.
func lexSQL(src string, pg bool) ([]token, error) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-', c == '#' && !pg:
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return nil, fmt.Errorf("unterminated comment")
			}
			body := src[i+2 : i+2+j]
			if pg && strings.HasPrefix(body, "@") {
				out = append(out, token{kind: tkPGHint, text: strings.TrimSpace(body[1:]), start: i, end: i + 4 + j})
			}
			i += 4 + j
		case c == '`' && !pg:
			j := strings.IndexByte(src[i+1:], '`')
			if j < 0 {
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			out = append(out, token{kind: tkQIdent, text: src[i+1 : i+1+j], start: i, end: i + 2 + j})
			i += 2 + j
		case c == '"' && pg:
			j := i + 1
			var b strings.Builder
			for {
				if j >= len(src) {
					return nil, fmt.Errorf("unterminated quoted identifier")
				}
				if src[j] == '"' {
					if j+1 < len(src) && src[j+1] == '"' {
						b.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(src[j])
				j++
			}
			out = append(out, token{kind: tkQIdent, text: b.String(), start: i, end: j + 1})
			i = j + 1
		case c == '\'' || c == '"':
			end, err := scanString(src, i, pg)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tkString, text: src[i:end], start: i, end: end})
			i = end
		case (c == 'r' || c == 'R' || c == 'b' || c == 'B') && !pg && i+1 < len(src) && strings.ContainsRune("'\"rRbB", rune(src[i+1])):
			j := i + 1
			if src[j] != '\'' && src[j] != '"' {
				j++
			}
			if j < len(src) && (src[j] == '\'' || src[j] == '"') {
				end, err := scanString(src, j, false)
				if err != nil {
					return nil, err
				}
				out = append(out, token{kind: tkString, text: src[i:end], start: i, end: end})
				i = end
				continue
			}
			end := scanIdent(src, i)
			out = append(out, token{kind: tkIdent, text: src[i:end], start: i, end: end})
			i = end
		case c == '@' && i+1 < len(src) && src[i+1] == '{':
			out = append(out, token{kind: tkPunct, text: "@{", start: i, end: i + 2})
			i += 2
		case (c == '@' && !pg) || (c == '$' && pg):
			j := i + 1
			for j < len(src) && (isIdentByte(src[j])) {
				j++
			}
			out = append(out, token{kind: tkParam, text: src[i+1 : j], start: i, end: j})
			i = j
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			j := i
			if c == '0' && i+1 < len(src) && (src[i+1] == 'x' || src[i+1] == 'X') {
				j += 2
				for j < len(src) && strings.IndexByte("0123456789abcdefABCDEF", src[j]) >= 0 {
					j++
				}
			} else {
				for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
					j++
				}
				if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
					j++
					if j < len(src) && (src[j] == '+' || src[j] == '-') {
						j++
					}
					for j < len(src) && src[j] >= '0' && src[j] <= '9' {
						j++
					}
				}
			}
			out = append(out, token{kind: tkNumber, text: src[i:j], start: i, end: j})
			i = j
		case isIdentStart(c):
			end := scanIdent(src, i)
			out = append(out, token{kind: tkIdent, text: src[i:end], start: i, end: end})
			i = end
		default:
			for _, p := range []string{"<=>", "<>", "!=", "<=", ">=", "<<", ">>", "||", "::", "=>"} {
				if strings.HasPrefix(src[i:], p) {
					out = append(out, token{kind: tkPunct, text: p, start: i, end: i + len(p)})
					i += len(p)
					goto next
				}
			}
			if strings.IndexByte("()[]{},.;=<>+-*/%&|^~!:?", c) < 0 {
				return nil, fmt.Errorf("unexpected character %q", c)
			}
			out = append(out, token{kind: tkPunct, text: string(c), start: i, end: i + 1})
			i++
		next:
		}
	}
	out = append(out, token{kind: tkEOF, start: len(src), end: len(src)})
	return out, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}
func isIdentByte(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

func scanIdent(src string, i int) int {
	for i < len(src) && isIdentByte(src[i]) {
		i++
	}
	return i
}

// scanString returns the end of the string literal that starts at i: a
// single, double or triple quoted one, with backslash escapes in GoogleSQL
// and doubled quotes in PostgreSQL.
func scanString(src string, i int, pg bool) (int, error) {
	q := src[i]
	if !pg && strings.HasPrefix(src[i:], strings.Repeat(string(q), 3)) {
		j := strings.Index(src[i+3:], strings.Repeat(string(q), 3))
		if j < 0 {
			return 0, fmt.Errorf("unterminated string literal")
		}
		return i + 6 + j, nil
	}
	j := i + 1
	for j < len(src) {
		switch {
		case src[j] == '\\' && !pg:
			j += 2
			continue
		case src[j] == q:
			if pg && j+1 < len(src) && src[j+1] == q {
				j += 2
				continue
			}
			return j + 1, nil
		}
		j++
	}
	return 0, fmt.Errorf("unterminated string literal")
}

// --- The tree -------------------------------------------------------------

type exprKind int

const (
	exCol    exprKind = iota // a column or a path: a.b
	exLit                    // a literal, NULL, TRUE, a typed literal
	exParam                  // @p or $1
	exFunc                   // a function call or a construct like CAST, CASE
	exBinary                 // a binary operator, op is its text in upper case
	exUnary                  // NOT, -, ~
	exSubq                   // a subquery: scalar, ARRAY, EXISTS, IN
	exList                   // a parenthesised list, an array literal
	exStar                   // * or a.*
)

type sqlExpr struct {
	kind exprKind
	op   string   // a function's name or an operator, in upper case
	path []string // a column's path
	args []*sqlExpr
	// subKind is a subquery's kind: SCALAR, ARRAY, EXISTS, IN, NOT IN
	subKind string
	sub     *sqlQuery
	lo, hi  int // its tokens, [lo, hi)
}

type sqlQuery struct {
	with   []sqlCTE
	body   *sqlSet
	order  []sqlOrder
	limit  *sqlExpr
	offset *sqlExpr
	lo, hi int
}

type sqlCTE struct {
	name   string
	q      *sqlQuery
	lo, hi int
}

type sqlOrder struct {
	e    *sqlExpr
	desc bool
}

// sqlSet is a set operation's tree; a leaf is a SELECT or a parenthesised
// query.
type sqlSet struct {
	op          string // UNION, INTERSECT, EXCEPT; "" for a leaf
	all         bool
	left, right *sqlSet
	sel         *sqlSelect
	paren       *sqlQuery
	lo, hi      int
}

type sqlSelect struct {
	distinct bool
	list     []sqlItem
	from     *sqlFrom
	where    *sqlExpr
	groupBy  []*sqlExpr
	having   *sqlExpr
	lo, hi   int
}

type sqlItem struct {
	e     *sqlExpr
	alias string
}

type fromKind int

const (
	fromTable fromKind = iota
	fromSub
	fromUnnest
	fromJoin
)

type sqlFrom struct {
	kind  fromKind
	name  []string // a table's (or a path's) name
	alias string
	hints map[string]string
	sub   *sqlQuery
	arr   *sqlExpr // UNNEST's array
	// a join
	jtype       string // INNER, CROSS, LEFT, RIGHT, FULL; COMMA for a comma
	left, right *sqlFrom
	on          *sqlExpr
	using       []string
	lo, hi      int
}

// --- The parser -----------------------------------------------------------

type sqlParser struct {
	src  string
	toks []token
	p    int
	pg   bool
	// stmtHints are the statement's hints.
	stmtHints map[string]string
}

type unsupportedSQL struct{ what string }

func (e unsupportedSQL) Error() string { return e.what }

// sqlDML is an INSERT, UPDATE or DELETE.
type sqlDML struct {
	kind  string // INSERT, UPDATE, DELETE
	table *sqlFrom
	where *sqlExpr
	set   []*sqlExpr // UPDATE's new values
	cols  []string   // INSERT's columns
	rows  int        // INSERT ... VALUES's rows
	query *sqlQuery  // INSERT ... SELECT's query
	// returning is THEN RETURN's (RETURNING's) list.
	returning []sqlItem
	lo, hi    int
}

type sqlStmt struct {
	query *sqlQuery
	dml   *sqlDML
}

func parseQuerySQL(src string, pg bool) (*sqlParser, *sqlQuery, error) {
	ps, st, err := parseStatementSQL(src, pg)
	if err != nil {
		return nil, nil, err
	}
	if st.query == nil {
		return nil, nil, unsupportedSQL{"a " + st.dml.kind + " statement where a query was expected"}
	}
	return ps, st.query, nil
}

// parseStatementSQL parses a query or a DML statement.
func parseStatementSQL(src string, pg bool) (*sqlParser, *sqlStmt, error) {
	toks, err := lexSQL(src, pg)
	if err != nil {
		return nil, nil, err
	}
	ps := &sqlParser{src: src, toks: toks, pg: pg, stmtHints: map[string]string{}}
	if ps.isP("@{") {
		h, err := ps.hints()
		if err != nil {
			return nil, nil, err
		}
		ps.stmtHints = h
	} else if ps.peek().kind == tkPGHint {
		ps.stmtHints = parseHintBody(ps.next().text)
	}
	st := &sqlStmt{}
	switch {
	case ps.isKw("INSERT") || ps.isKw("UPDATE") || ps.isKw("DELETE"):
		d, err := ps.dml()
		if err != nil {
			return nil, nil, err
		}
		st.dml = d
	case ps.isKw("SELECT") || ps.isKw("WITH") || ps.isP("("):
		q, err := ps.query()
		if err != nil {
			return nil, nil, err
		}
		st.query = q
	default:
		return nil, nil, unsupportedSQL{fmt.Sprintf("a %s statement", strings.ToUpper(ps.peek().text))}
	}
	if ps.isP(";") {
		ps.p++
	}
	if ps.peek().kind != tkEOF {
		return nil, nil, ps.errAt("the end of the statement")
	}
	return ps, st, nil
}

// dml parses INSERT, UPDATE and DELETE, in either dialect.
func (ps *sqlParser) dml() (*sqlDML, error) {
	d := &sqlDML{lo: ps.p, kind: strings.ToUpper(ps.next().text)}
	target := func() error {
		lo := ps.p
		name, ok := ps.ident()
		if !ok {
			return ps.errAt("a table name")
		}
		f := &sqlFrom{kind: fromTable, name: []string{name}, lo: lo}
		if err := ps.itemHints(f); err != nil {
			return err
		}
		if d.kind != "INSERT" || ps.pg {
			if ps.acceptKw("AS") {
				a, ok := ps.ident()
				if !ok {
					return ps.errAt("an alias")
				}
				f.alias = a
			} else if a, ok := ps.ident(); ok {
				f.alias = a
			}
		}
		f.hi = ps.p
		d.table = f
		return nil
	}
	switch d.kind {
	case "DELETE":
		ps.acceptKw("FROM")
		if err := target(); err != nil {
			return nil, err
		}
		if err := ps.expectKw("WHERE"); err != nil {
			return nil, err
		}
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		d.where = e
	case "UPDATE":
		if err := target(); err != nil {
			return nil, err
		}
		if err := ps.expectKw("SET"); err != nil {
			return nil, err
		}
		for {
			if _, err := ps.postfix(); err != nil {
				return nil, err
			}
			if err := ps.expectP("="); err != nil {
				return nil, err
			}
			if ps.acceptKw("DEFAULT") {
			} else {
				e, err := ps.expr()
				if err != nil {
					return nil, err
				}
				d.set = append(d.set, e)
			}
			if !ps.acceptP(",") {
				break
			}
		}
		if err := ps.expectKw("WHERE"); err != nil {
			return nil, err
		}
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		d.where = e
	case "INSERT":
		if ps.acceptKw("OR") {
			if !ps.acceptKw("IGNORE") && !ps.acceptKw("UPDATE") {
				return nil, ps.errAt("IGNORE or UPDATE")
			}
		}
		ps.acceptKw("INTO")
		if err := target(); err != nil {
			return nil, err
		}
		if err := ps.expectP("("); err != nil {
			return nil, err
		}
		for {
			c, ok := ps.ident()
			if !ok {
				return nil, ps.errAt("a column name")
			}
			d.cols = append(d.cols, c)
			if !ps.acceptP(",") {
				break
			}
		}
		if err := ps.expectP(")"); err != nil {
			return nil, err
		}
		if ps.acceptKw("VALUES") {
			for {
				if err := ps.expectP("("); err != nil {
					return nil, err
				}
				for {
					if !ps.acceptKw("DEFAULT") {
						if _, err := ps.expr(); err != nil {
							return nil, err
						}
					}
					if !ps.acceptP(",") {
						break
					}
				}
				if err := ps.expectP(")"); err != nil {
					return nil, err
				}
				d.rows++
				if !ps.acceptP(",") {
					break
				}
			}
		} else {
			q, err := ps.query()
			if err != nil {
				return nil, err
			}
			d.query = q
		}
		if ps.pg && ps.acceptKw("ON", "CONFLICT") {
			return nil, unsupportedSQL{"INSERT ... ON CONFLICT"}
		}
	}
	if ps.acceptKw("THEN", "RETURN") || ps.acceptKw("RETURNING") {
		ps.acceptKw("WITH", "ACTION")
		for {
			var it sqlItem
			lo := ps.p
			if ps.acceptP("*") {
				it.e = &sqlExpr{kind: exStar, lo: lo, hi: ps.p}
			} else {
				e, err := ps.expr()
				if err != nil {
					return nil, err
				}
				it.e = e
			}
			if ps.acceptKw("AS") {
				a, _ := ps.ident()
				it.alias = a
			} else if a, ok := ps.ident(); ok {
				it.alias = a
			}
			d.returning = append(d.returning, it)
			if !ps.acceptP(",") {
				break
			}
		}
	}
	d.hi = ps.p
	return d, nil
}

func (ps *sqlParser) peek() token { return ps.toks[ps.p] }
func (ps *sqlParser) peekN(n int) token {
	if ps.p+n < len(ps.toks) {
		return ps.toks[ps.p+n]
	}
	return ps.toks[len(ps.toks)-1]
}
func (ps *sqlParser) next() token {
	t := ps.toks[ps.p]
	if t.kind != tkEOF {
		ps.p++
	}
	return t
}
func (ps *sqlParser) isP(s string) bool {
	t := ps.peek()
	return t.kind == tkPunct && t.text == s
}
func (ps *sqlParser) isKw(s string) bool {
	t := ps.peek()
	return t.kind == tkIdent && strings.EqualFold(t.text, s)
}
func (ps *sqlParser) isKwN(n int, s string) bool {
	t := ps.peekN(n)
	return t.kind == tkIdent && strings.EqualFold(t.text, s)
}
func (ps *sqlParser) acceptKw(s ...string) bool {
	for i, k := range s {
		if !ps.isKwN(i, k) {
			return false
		}
	}
	ps.p += len(s)
	return true
}
func (ps *sqlParser) acceptP(s string) bool {
	if ps.isP(s) {
		ps.p++
		return true
	}
	return false
}
func (ps *sqlParser) errAt(want string) error {
	t := ps.peek()
	if t.kind == tkEOF {
		return unsupportedSQL{fmt.Sprintf("the query, which ends where %s was expected", want)}
	}
	return unsupportedSQL{fmt.Sprintf("the syntax at %q (offset %d), where %s was expected", ps.src[t.start:t.end], t.start, want)}
}
func (ps *sqlParser) expectP(s string) error {
	if !ps.acceptP(s) {
		return ps.errAt(fmt.Sprintf("%q", s))
	}
	return nil
}
func (ps *sqlParser) expectKw(s string) error {
	if !ps.acceptKw(s) {
		return ps.errAt(s)
	}
	return nil
}

// text is the source of tokens [lo, hi).
func (ps *sqlParser) text(lo, hi int) string {
	if hi <= lo {
		return ""
	}
	return ps.src[ps.toks[lo].start:ps.toks[hi-1].end]
}

func (ps *sqlParser) exprText(e *sqlExpr) string { return ps.text(e.lo, e.hi) }

// reserved are the keywords that end an expression or a FROM item.
var reservedSQL = map[string]bool{
	"ALL": true, "AND": true, "ANY": true, "ARRAY": true, "AS": true, "ASC": true, "BETWEEN": true, "BY": true,
	"CASE": true, "CAST": true, "CROSS": true, "DESC": true, "DISTINCT": true, "ELSE": true, "END": true,
	"EXCEPT": true, "EXISTS": true, "FALSE": true, "FETCH": true, "FOR": true, "FROM": true, "FULL": true,
	"GROUP": true, "HAVING": true, "IN": true, "INNER": true, "INTERSECT": true, "INTERVAL": true, "IS": true,
	"JOIN": true, "LEFT": true, "LIKE": true, "LIMIT": true, "NOT": true, "NULL": true, "OFFSET": true,
	"ON": true, "OR": true, "ORDER": true, "OUTER": true, "RIGHT": true, "SELECT": true, "TABLESAMPLE": true,
	"THEN": true, "TRUE": true, "UNION": true, "UNNEST": true, "USING": true, "WHEN": true, "WHERE": true,
	"WINDOW": true, "WITH": true, "QUALIFY": true, "ILIKE": true, "LATERAL": true,
	"SET": true, "RETURNING": true, "VALUES": true, "DEFAULT": true,
}

func (ps *sqlParser) isReserved() bool {
	t := ps.peek()
	return t.kind == tkIdent && reservedSQL[strings.ToUpper(t.text)]
}

func (ps *sqlParser) ident() (string, bool) {
	t := ps.peek()
	switch {
	case t.kind == tkQIdent:
		ps.p++
		return t.text, true
	case t.kind == tkIdent && !reservedSQL[strings.ToUpper(t.text)]:
		ps.p++
		if ps.pg {
			return strings.ToLower(t.text), true
		}
		return t.text, true
	}
	return "", false
}

func (ps *sqlParser) query() (*sqlQuery, error) {
	q := &sqlQuery{lo: ps.p}
	if ps.acceptKw("WITH") {
		if ps.acceptKw("RECURSIVE") {
			return nil, unsupportedSQL{"WITH RECURSIVE"}
		}
		for {
			lo := ps.p
			name, ok := ps.ident()
			if !ok {
				return nil, ps.errAt("a WITH clause's name")
			}
			if err := ps.expectKw("AS"); err != nil {
				return nil, err
			}
			if err := ps.expectP("("); err != nil {
				return nil, err
			}
			sub, err := ps.query()
			if err != nil {
				return nil, err
			}
			if err := ps.expectP(")"); err != nil {
				return nil, err
			}
			q.with = append(q.with, sqlCTE{name: name, q: sub, lo: lo, hi: ps.p})
			if !ps.acceptP(",") {
				break
			}
		}
	}
	body, err := ps.setExpr()
	if err != nil {
		return nil, err
	}
	q.body = body
	if ps.acceptKw("ORDER", "BY") {
		for {
			e, err := ps.expr()
			if err != nil {
				return nil, err
			}
			o := sqlOrder{e: e}
			if ps.acceptKw("DESC") {
				o.desc = true
			} else {
				ps.acceptKw("ASC")
			}
			if ps.acceptKw("NULLS") {
				if !ps.acceptKw("FIRST") && !ps.acceptKw("LAST") {
					return nil, ps.errAt("FIRST or LAST")
				}
			}
			q.order = append(q.order, o)
			if !ps.acceptP(",") {
				break
			}
		}
	}
	for {
		switch {
		case q.limit == nil && ps.acceptKw("LIMIT"):
			if ps.acceptKw("ALL") {
				continue
			}
			e, err := ps.expr()
			if err != nil {
				return nil, err
			}
			q.limit = e
			continue
		case q.offset == nil && ps.acceptKw("OFFSET"):
			e, err := ps.expr()
			if err != nil {
				return nil, err
			}
			q.offset = e
			if ps.pg {
				_ = ps.acceptKw("ROWS") || ps.acceptKw("ROW")
			}
			continue
		}
		break
	}
	if ps.isKw("FOR") {
		return nil, unsupportedSQL{"FOR UPDATE"}
	}
	q.hi = ps.p
	return q, nil
}

func (ps *sqlParser) setExpr() (*sqlSet, error) {
	left, err := ps.setPrimary()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case ps.isKw("UNION"):
			op = "UNION"
		case ps.isKw("INTERSECT"):
			op = "INTERSECT"
		case ps.isKw("EXCEPT"):
			op = "EXCEPT"
		default:
			return left, nil
		}
		ps.p++
		all := false
		if ps.acceptKw("ALL") {
			all = true
		} else {
			ps.acceptKw("DISTINCT")
		}
		right, err := ps.setPrimary()
		if err != nil {
			return nil, err
		}
		left = &sqlSet{op: op, all: all, left: left, right: right, lo: left.lo, hi: ps.p}
	}
}

func (ps *sqlParser) setPrimary() (*sqlSet, error) {
	lo := ps.p
	if ps.acceptP("(") {
		q, err := ps.query()
		if err != nil {
			return nil, err
		}
		if err := ps.expectP(")"); err != nil {
			return nil, err
		}
		return &sqlSet{paren: q, lo: lo, hi: ps.p}, nil
	}
	s, err := ps.selectStmt()
	if err != nil {
		return nil, err
	}
	return &sqlSet{sel: s, lo: lo, hi: ps.p}, nil
}

func (ps *sqlParser) selectStmt() (*sqlSelect, error) {
	s := &sqlSelect{lo: ps.p}
	if err := ps.expectKw("SELECT"); err != nil {
		return nil, err
	}
	if ps.isP("@{") {
		if _, err := ps.hints(); err != nil {
			return nil, err
		}
	}
	if ps.acceptKw("DISTINCT") {
		s.distinct = true
	} else {
		ps.acceptKw("ALL")
	}
	if ps.acceptKw("AS", "STRUCT") || ps.acceptKw("AS", "VALUE") {
		// the rows' shape changes, not the plan's
	}
	for {
		lo := ps.p
		var it sqlItem
		if ps.isP("*") {
			ps.p++
			it.e = &sqlExpr{kind: exStar, lo: lo, hi: ps.p}
		} else {
			e, err := ps.expr()
			if err != nil {
				return nil, err
			}
			it.e = e
		}
		if ps.isKw("EXCEPT") && ps.peekN(1).kind == tkPunct && ps.peekN(1).text == "(" || ps.isKw("REPLACE") {
			if err := ps.skipBalancedAfterKw(); err != nil {
				return nil, err
			}
			it.e.hi = ps.p
		}
		if ps.acceptKw("AS") {
			a, ok := ps.ident()
			if !ok {
				return nil, ps.errAt("an alias")
			}
			it.alias = a
		} else if a, ok := ps.ident(); ok {
			it.alias = a
		}
		s.list = append(s.list, it)
		if !ps.acceptP(",") {
			break
		}
	}
	if ps.acceptKw("FROM") {
		f, err := ps.fromList()
		if err != nil {
			return nil, err
		}
		s.from = f
	}
	if ps.acceptKw("WHERE") {
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		s.where = e
	}
	if ps.acceptKw("GROUP", "BY") {
		for {
			e, err := ps.expr()
			if err != nil {
				return nil, err
			}
			s.groupBy = append(s.groupBy, e)
			if !ps.acceptP(",") {
				break
			}
		}
	}
	if ps.acceptKw("HAVING") {
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		s.having = e
	}
	if ps.isKw("WINDOW") || ps.isKw("QUALIFY") {
		return nil, unsupportedSQL{strings.ToUpper(ps.peek().text) + " clauses"}
	}
	s.hi = ps.p
	return s, nil
}

// skipBalancedAfterKw skips a keyword and the parenthesised list after it
// (SELECT * EXCEPT (...), REPLACE (...)).
func (ps *sqlParser) skipBalancedAfterKw() error {
	ps.p++
	return ps.skipBalanced("(", ")")
}

func (ps *sqlParser) skipBalanced(open, close string) error {
	if err := ps.expectP(open); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		t := ps.next()
		switch {
		case t.kind == tkEOF:
			return ps.errAt(fmt.Sprintf("%q", close))
		case t.kind == tkPunct && t.text == open:
			depth++
		case t.kind == tkPunct && t.text == close:
			depth--
		}
	}
	return nil
}

// hints reads @{k=v, ...}.
func (ps *sqlParser) hints() (map[string]string, error) {
	lo := ps.p
	if err := ps.expectP("@{"); err != nil {
		return nil, err
	}
	for !ps.isP("}") {
		if ps.peek().kind == tkEOF {
			return nil, ps.errAt("}")
		}
		ps.p++
	}
	ps.p++
	body := ps.src[ps.toks[lo].end:ps.toks[ps.p-1].start]
	return parseHintBody(body), nil
}

// parseHintBody reads "k=v, k2=v2" into upper-case keys.
func parseHintBody(body string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(body, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), "`\"")
	}
	return out
}

func (ps *sqlParser) itemHints(f *sqlFrom) error {
	switch {
	case ps.isP("@{"):
		h, err := ps.hints()
		if err != nil {
			return err
		}
		f.hints = h
	case ps.peek().kind == tkPGHint:
		f.hints = parseHintBody(ps.next().text)
	}
	return nil
}

func (ps *sqlParser) fromList() (*sqlFrom, error) {
	left, err := ps.fromItem()
	if err != nil {
		return nil, err
	}
	for {
		j := &sqlFrom{kind: fromJoin, left: left, lo: left.lo}
		switch {
		case ps.acceptP(","):
			j.jtype = "COMMA"
		case ps.acceptKw("CROSS", "JOIN"):
			j.jtype = "CROSS"
		case ps.acceptKw("JOIN"), ps.acceptKw("INNER", "JOIN"):
			j.jtype = "INNER"
		case ps.acceptKw("LEFT", "OUTER", "JOIN"), ps.acceptKw("LEFT", "JOIN"):
			j.jtype = "LEFT"
		case ps.acceptKw("RIGHT", "OUTER", "JOIN"), ps.acceptKw("RIGHT", "JOIN"):
			j.jtype = "RIGHT"
		case ps.acceptKw("FULL", "OUTER", "JOIN"), ps.acceptKw("FULL", "JOIN"):
			j.jtype = "FULL"
		case ps.isKw("HASH") || ps.isKw("LOOKUP") || ps.isKw("NATURAL"):
			return nil, unsupportedSQL{strings.ToUpper(ps.peek().text) + " joins"}
		default:
			return left, nil
		}
		if j.jtype != "COMMA" {
			if err := ps.itemHints(j); err != nil {
				return nil, err
			}
		}
		right, err := ps.fromItem()
		if err != nil {
			return nil, err
		}
		j.right = right
		if j.jtype == "INNER" || j.jtype == "LEFT" || j.jtype == "RIGHT" || j.jtype == "FULL" {
			switch {
			case ps.acceptKw("ON"):
				e, err := ps.expr()
				if err != nil {
					return nil, err
				}
				j.on = e
			case ps.acceptKw("USING"):
				if err := ps.expectP("("); err != nil {
					return nil, err
				}
				for {
					c, ok := ps.ident()
					if !ok {
						return nil, ps.errAt("a column name")
					}
					j.using = append(j.using, c)
					if !ps.acceptP(",") {
						break
					}
				}
				if err := ps.expectP(")"); err != nil {
					return nil, err
				}
			}
		}
		j.hi = ps.p
		left = j
	}
}

func (ps *sqlParser) fromItem() (*sqlFrom, error) {
	lo := ps.p
	f := &sqlFrom{lo: lo}
	switch {
	case ps.isKw("UNNEST"):
		ps.p++
		if err := ps.expectP("("); err != nil {
			return nil, err
		}
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		if err := ps.expectP(")"); err != nil {
			return nil, err
		}
		f.kind, f.arr = fromUnnest, e
	case ps.isP("("):
		save := ps.p
		ps.p++
		if ps.isKw("SELECT") || ps.isKw("WITH") || ps.isP("(") && ps.looksLikeQuery() {
			q, err := ps.query()
			if err != nil {
				return nil, err
			}
			if err := ps.expectP(")"); err != nil {
				return nil, err
			}
			f.kind, f.sub = fromSub, q
		} else {
			ps.p = save + 1
			inner, err := ps.fromList()
			if err != nil {
				return nil, err
			}
			if err := ps.expectP(")"); err != nil {
				return nil, err
			}
			inner.lo, inner.hi = lo, ps.p
			return inner, nil
		}
	default:
		for {
			id, ok := ps.ident()
			if !ok {
				return nil, ps.errAt("a table name")
			}
			f.name = append(f.name, id)
			if !ps.acceptP(".") {
				break
			}
		}
		f.kind = fromTable
		if err := ps.itemHints(f); err != nil {
			return nil, err
		}
	}
	if ps.acceptKw("AS") {
		a, ok := ps.ident()
		if !ok {
			return nil, ps.errAt("an alias")
		}
		f.alias = a
	} else if a, ok := ps.ident(); ok {
		f.alias = a
	}
	if f.kind == fromUnnest && ps.acceptKw("WITH", "OFFSET") {
		if ps.acceptKw("AS") {
			ps.ident()
		} else {
			ps.ident()
		}
	}
	if ps.isKw("TABLESAMPLE") {
		return nil, unsupportedSQL{"TABLESAMPLE"}
	}
	f.hi = ps.p
	return f, nil
}

// looksLikeQuery says whether the parenthesis at p opens a query, rather
// than a parenthesised join.
func (ps *sqlParser) looksLikeQuery() bool {
	i := ps.p
	for i < len(ps.toks) && ps.toks[i].kind == tkPunct && ps.toks[i].text == "(" {
		i++
	}
	t := ps.toks[i]
	return t.kind == tkIdent && (strings.EqualFold(t.text, "SELECT") || strings.EqualFold(t.text, "WITH"))
}

// --- Expressions ------------------------------------------------------------

func (ps *sqlParser) expr() (*sqlExpr, error) { return ps.orExpr() }

func (ps *sqlParser) bin(op string, l, r *sqlExpr) *sqlExpr {
	return &sqlExpr{kind: exBinary, op: op, args: []*sqlExpr{l, r}, lo: l.lo, hi: r.hi}
}

func (ps *sqlParser) orExpr() (*sqlExpr, error) {
	l, err := ps.andExpr()
	if err != nil {
		return nil, err
	}
	for ps.acceptKw("OR") {
		r, err := ps.andExpr()
		if err != nil {
			return nil, err
		}
		l = ps.bin("OR", l, r)
	}
	return l, nil
}

func (ps *sqlParser) andExpr() (*sqlExpr, error) {
	l, err := ps.notExpr()
	if err != nil {
		return nil, err
	}
	for ps.acceptKw("AND") {
		r, err := ps.notExpr()
		if err != nil {
			return nil, err
		}
		l = ps.bin("AND", l, r)
	}
	return l, nil
}

func (ps *sqlParser) notExpr() (*sqlExpr, error) {
	lo := ps.p
	if ps.acceptKw("NOT") {
		e, err := ps.notExpr()
		if err != nil {
			return nil, err
		}
		return &sqlExpr{kind: exUnary, op: "NOT", args: []*sqlExpr{e}, lo: lo, hi: e.hi}, nil
	}
	return ps.cmpExpr()
}

var cmpOps = map[string]bool{"=": true, "!=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

func (ps *sqlParser) cmpExpr() (*sqlExpr, error) {
	l, err := ps.bitOr()
	if err != nil {
		return nil, err
	}
	for {
		t := ps.peek()
		switch {
		case t.kind == tkPunct && cmpOps[t.text]:
			ps.p++
			r, err := ps.bitOr()
			if err != nil {
				return nil, err
			}
			op := t.text
			if op == "<>" {
				op = "!="
			}
			l = ps.bin(op, l, r)
		case ps.isKw("IS"):
			ps.p++
			neg := ps.acceptKw("NOT")
			if ps.acceptKw("DISTINCT", "FROM") {
				r, err := ps.bitOr()
				if err != nil {
					return nil, err
				}
				op := "IS DISTINCT FROM"
				if neg {
					op = "IS NOT DISTINCT FROM"
				}
				l = ps.bin(op, l, r)
				continue
			}
			v := ps.next()
			if v.kind != tkIdent {
				return nil, ps.errAt("NULL, TRUE, FALSE or UNKNOWN")
			}
			op := "IS " + strings.ToUpper(v.text)
			if neg {
				op = "IS NOT " + strings.ToUpper(v.text)
			}
			l = &sqlExpr{kind: exUnary, op: op, args: []*sqlExpr{l}, lo: l.lo, hi: ps.p}
		case ps.isKw("NOT") && (ps.isKwN(1, "IN") || ps.isKwN(1, "LIKE") || ps.isKwN(1, "BETWEEN") || ps.isKwN(1, "ILIKE")):
			ps.p++
			e, err := ps.cmpTail(l, true)
			if err != nil {
				return nil, err
			}
			l = e
		case ps.isKw("IN") || ps.isKw("LIKE") || ps.isKw("BETWEEN") || ps.isKw("ILIKE"):
			e, err := ps.cmpTail(l, false)
			if err != nil {
				return nil, err
			}
			l = e
		default:
			return l, nil
		}
	}
}

// cmpTail reads IN, LIKE or BETWEEN after l.
func (ps *sqlParser) cmpTail(l *sqlExpr, neg bool) (*sqlExpr, error) {
	kw := strings.ToUpper(ps.next().text)
	not := ""
	if neg {
		not = "NOT "
	}
	switch kw {
	case "LIKE", "ILIKE":
		r, err := ps.bitOr()
		if err != nil {
			return nil, err
		}
		return ps.bin(not+kw, l, r), nil
	case "BETWEEN":
		a, err := ps.bitOr()
		if err != nil {
			return nil, err
		}
		if err := ps.expectKw("AND"); err != nil {
			return nil, err
		}
		b, err := ps.bitOr()
		if err != nil {
			return nil, err
		}
		return &sqlExpr{kind: exBinary, op: not + "BETWEEN", args: []*sqlExpr{l, a, b}, lo: l.lo, hi: b.hi}, nil
	}
	// IN
	if ps.isKw("UNNEST") {
		lo := ps.p
		ps.p++
		if err := ps.expectP("("); err != nil {
			return nil, err
		}
		a, err := ps.expr()
		if err != nil {
			return nil, err
		}
		if err := ps.expectP(")"); err != nil {
			return nil, err
		}
		r := &sqlExpr{kind: exFunc, op: "UNNEST", args: []*sqlExpr{a}, lo: lo, hi: ps.p}
		return &sqlExpr{kind: exBinary, op: not + "IN", args: []*sqlExpr{l, r}, lo: l.lo, hi: ps.p}, nil
	}
	lo := ps.p
	if err := ps.expectP("("); err != nil {
		return nil, err
	}
	if ps.isKw("SELECT") || ps.isKw("WITH") || ps.isP("(") && ps.looksLikeQuery() {
		q, err := ps.query()
		if err != nil {
			return nil, err
		}
		if err := ps.expectP(")"); err != nil {
			return nil, err
		}
		return &sqlExpr{kind: exSubq, subKind: not + "IN", sub: q, args: []*sqlExpr{l}, lo: l.lo, hi: ps.p}, nil
	}
	list := &sqlExpr{kind: exList, lo: lo}
	for {
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		list.args = append(list.args, e)
		if !ps.acceptP(",") {
			break
		}
	}
	if err := ps.expectP(")"); err != nil {
		return nil, err
	}
	list.hi = ps.p
	return &sqlExpr{kind: exBinary, op: not + "IN", args: []*sqlExpr{l, list}, lo: l.lo, hi: ps.p}, nil
}

// binLevels are the binary operators from the loosest to the tightest.
var binLevels = [][]string{{"|"}, {"^"}, {"&"}, {"<<", ">>"}, {"+", "-", "||"}, {"*", "/", "%"}}

func (ps *sqlParser) bitOr() (*sqlExpr, error) { return ps.binLevel(0) }

func (ps *sqlParser) binLevel(n int) (*sqlExpr, error) {
	if n == len(binLevels) {
		return ps.unary()
	}
	l, err := ps.binLevel(n + 1)
	if err != nil {
		return nil, err
	}
	for {
		t := ps.peek()
		matched := false
		if t.kind == tkPunct {
			for _, op := range binLevels[n] {
				if t.text == op {
					matched = true
				}
			}
		}
		if !matched {
			return l, nil
		}
		ps.p++
		r, err := ps.binLevel(n + 1)
		if err != nil {
			return nil, err
		}
		l = ps.bin(t.text, l, r)
	}
}

func (ps *sqlParser) unary() (*sqlExpr, error) {
	lo := ps.p
	if ps.isP("-") || ps.isP("+") || ps.isP("~") {
		op := ps.next().text
		e, err := ps.unary()
		if err != nil {
			return nil, err
		}
		return &sqlExpr{kind: exUnary, op: op, args: []*sqlExpr{e}, lo: lo, hi: e.hi}, nil
	}
	return ps.postfix()
}

func (ps *sqlParser) postfix() (*sqlExpr, error) {
	e, err := ps.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case ps.isP("["):
			ps.p++
			i, err := ps.expr()
			if err != nil {
				return nil, err
			}
			if err := ps.expectP("]"); err != nil {
				return nil, err
			}
			e = &sqlExpr{kind: exFunc, op: "[]", args: []*sqlExpr{e, i}, lo: e.lo, hi: ps.p}
		case ps.isP("."):
			ps.p++
			if ps.isP("*") {
				ps.p++
				e = &sqlExpr{kind: exStar, path: e.path, lo: e.lo, hi: ps.p}
				continue
			}
			id, ok := ps.ident()
			if !ok {
				t := ps.next()
				if t.kind != tkIdent {
					return nil, ps.errAt("a field name")
				}
				id = t.text
			}
			if e.kind == exCol {
				e.path = append(e.path, id)
				e.hi = ps.p
			} else {
				e = &sqlExpr{kind: exFunc, op: ".", args: []*sqlExpr{e}, path: []string{id}, lo: e.lo, hi: ps.p}
			}
		case ps.isP("::"):
			ps.p++
			if err := ps.skipType(); err != nil {
				return nil, err
			}
			e = &sqlExpr{kind: exFunc, op: "CAST", args: []*sqlExpr{e}, lo: e.lo, hi: ps.p}
		default:
			return e, nil
		}
	}
}

// skipType skips a type name: INT64, STRING(10), ARRAY<STRUCT<a INT64>>,
// character varying(10), timestamp with time zone, int8[].
func (ps *sqlParser) skipType() error {
	t := ps.next()
	if t.kind != tkIdent && t.kind != tkQIdent {
		return ps.errAt("a type")
	}
	for ps.peek().kind == tkIdent && (ps.isKw("VARYING") || ps.isKw("PRECISION") || ps.isKw("WITH") && ps.isKwN(1, "TIME") || ps.isKw("WITHOUT") || ps.isKw("TIME") || ps.isKw("ZONE")) {
		ps.p++
	}
	if ps.isP("<") {
		depth := 0
		for {
			t := ps.next()
			if t.kind == tkEOF {
				return ps.errAt(">")
			}
			if t.kind == tkPunct {
				switch t.text {
				case "<":
					depth++
				case ">":
					depth--
				case ">>":
					depth -= 2
				}
			}
			if depth <= 0 {
				break
			}
		}
	}
	if ps.isP("(") {
		if err := ps.skipBalanced("(", ")"); err != nil {
			return err
		}
	}
	for ps.isP("[") && ps.peekN(1).kind == tkPunct && ps.peekN(1).text == "]" {
		ps.p += 2
	}
	return nil
}

var typedLiteral = map[string]bool{"DATE": true, "TIMESTAMP": true, "NUMERIC": true, "JSON": true, "BIGNUMERIC": true, "INTERVAL": true, "TIMESTAMPTZ": true}

func (ps *sqlParser) primary() (*sqlExpr, error) {
	lo := ps.p
	t := ps.peek()
	done := func(k exprKind) *sqlExpr { return &sqlExpr{kind: k, lo: lo, hi: ps.p} }
	switch t.kind {
	case tkNumber, tkString:
		ps.p++
		return done(exLit), nil
	case tkParam:
		ps.p++
		e := done(exParam)
		e.op = t.text
		return e, nil
	case tkQIdent:
		ps.p++
		e := done(exCol)
		e.path = []string{t.text}
		return ps.maybeCall(e)
	case tkPunct:
		switch t.text {
		case "(":
			ps.p++
			if ps.isKw("SELECT") || ps.isKw("WITH") || ps.isP("(") && ps.looksLikeQuery() {
				q, err := ps.query()
				if err != nil {
					return nil, err
				}
				if err := ps.expectP(")"); err != nil {
					return nil, err
				}
				e := done(exSubq)
				e.subKind, e.sub = "SCALAR", q
				return e, nil
			}
			first, err := ps.expr()
			if err != nil {
				return nil, err
			}
			if ps.acceptP(")") {
				return &sqlExpr{kind: exFunc, op: "()", args: []*sqlExpr{first}, lo: lo, hi: ps.p}, nil
			}
			list := &sqlExpr{kind: exList, args: []*sqlExpr{first}, lo: lo}
			for ps.acceptP(",") {
				e, err := ps.expr()
				if err != nil {
					return nil, err
				}
				list.args = append(list.args, e)
			}
			if err := ps.expectP(")"); err != nil {
				return nil, err
			}
			list.hi = ps.p
			return list, nil
		case "[":
			ps.p++
			return ps.listUntil(lo, "]", exList)
		case "*":
			ps.p++
			return done(exStar), nil
		}
		return nil, ps.errAt("an expression")
	case tkIdent:
	default:
		return nil, ps.errAt("an expression")
	}
	up := strings.ToUpper(t.text)
	switch up {
	case "NULL", "TRUE", "FALSE":
		ps.p++
		return done(exLit), nil
	case "CASE":
		ps.p++
		e := &sqlExpr{kind: exFunc, op: "CASE", lo: lo}
		for !ps.acceptKw("END") {
			if ps.peek().kind == tkEOF {
				return nil, ps.errAt("END")
			}
			if ps.acceptKw("WHEN") || ps.acceptKw("THEN") || ps.acceptKw("ELSE") {
				continue
			}
			a, err := ps.expr()
			if err != nil {
				return nil, err
			}
			e.args = append(e.args, a)
		}
		e.hi = ps.p
		return e, nil
	case "EXISTS":
		ps.p++
		if err := ps.expectP("("); err != nil {
			return nil, err
		}
		q, err := ps.query()
		if err != nil {
			return nil, err
		}
		if err := ps.expectP(")"); err != nil {
			return nil, err
		}
		e := done(exSubq)
		e.subKind, e.sub = "EXISTS", q
		return e, nil
	case "ARRAY":
		ps.p++
		if ps.isP("<") {
			ps.p--
			if err := ps.skipType(); err != nil {
				return nil, err
			}
		}
		if ps.isP("(") && (ps.isKwN(1, "SELECT") || ps.isKwN(1, "WITH")) {
			ps.p++
			q, err := ps.query()
			if err != nil {
				return nil, err
			}
			if err := ps.expectP(")"); err != nil {
				return nil, err
			}
			e := done(exSubq)
			e.subKind, e.sub = "ARRAY", q
			return e, nil
		}
		if ps.acceptP("[") {
			return ps.listUntil(lo, "]", exList)
		}
		return nil, ps.errAt("an array")
	case "STRUCT":
		ps.p++
		if ps.isP("<") {
			ps.p--
			if err := ps.skipType(); err != nil {
				return nil, err
			}
		}
		if err := ps.expectP("("); err != nil {
			return nil, err
		}
		e, err := ps.args(lo, "STRUCT")
		return e, err
	case "INTERVAL":
		ps.p++
		a, err := ps.bitOr()
		if err != nil {
			return nil, err
		}
		if ps.peek().kind == tkIdent && !ps.isReserved() {
			ps.p++
			if ps.acceptKw("TO") {
				ps.p++
			}
		}
		return &sqlExpr{kind: exFunc, op: "INTERVAL", args: []*sqlExpr{a}, lo: lo, hi: ps.p}, nil
	}
	if typedLiteral[up] && ps.peekN(1).kind == tkString {
		ps.p += 2
		return done(exLit), nil
	}
	if reservedSQL[up] && !(ps.peekN(1).kind == tkPunct && ps.peekN(1).text == "(" && (up == "LEFT" || up == "RIGHT" || up == "IF")) {
		return nil, ps.errAt("an expression")
	}
	ps.p++
	e := done(exCol)
	e.path = []string{t.text}
	if ps.pg {
		e.path = []string{strings.ToLower(t.text)}
	}
	return ps.maybeCall(e)
}

func (ps *sqlParser) listUntil(lo int, close string, k exprKind) (*sqlExpr, error) {
	e := &sqlExpr{kind: k, lo: lo}
	for !ps.acceptP(close) {
		a, err := ps.expr()
		if err != nil {
			return nil, err
		}
		e.args = append(e.args, a)
		if !ps.acceptP(",") {
			if err := ps.expectP(close); err != nil {
				return nil, err
			}
			break
		}
	}
	e.hi = ps.p
	return e, nil
}

// maybeCall turns a name followed by "(" into a call.
func (ps *sqlParser) maybeCall(e *sqlExpr) (*sqlExpr, error) {
	if !ps.isP("(") {
		return e, nil
	}
	ps.p++
	return ps.args(e.lo, strings.ToUpper(strings.Join(e.path, ".")))
}

// args reads a call's arguments after "(": the special forms CAST(x AS T),
// EXTRACT(p FROM x), aggregate modifiers (DISTINCT, IGNORE NULLS, HAVING
// MAX, ORDER BY, LIMIT) and named arguments.
func (ps *sqlParser) args(lo int, name string) (*sqlExpr, error) {
	e := &sqlExpr{kind: exFunc, op: name, lo: lo}
	if ps.acceptP(")") {
		e.hi = ps.p
		return e, nil
	}
	if name == "EXTRACT" {
		ps.next()
		if err := ps.expectKw("FROM"); err != nil {
			return nil, err
		}
	}
	ps.acceptKw("DISTINCT")
	for {
		if ps.peek().kind == tkIdent && ps.peekN(1).kind == tkPunct && ps.peekN(1).text == "=>" {
			ps.p += 2
		}
		a, err := ps.expr()
		if err != nil {
			return nil, err
		}
		e.args = append(e.args, a)
		if ps.acceptKw("AS") {
			if err := ps.skipType(); err != nil {
				return nil, err
			}
		}
		for !ps.isP(",") && !ps.isP(")") {
			switch {
			case ps.acceptKw("IGNORE", "NULLS"), ps.acceptKw("RESPECT", "NULLS"):
			case ps.acceptKw("HAVING", "MAX"), ps.acceptKw("HAVING", "MIN"):
				if _, err := ps.expr(); err != nil {
					return nil, err
				}
			case ps.acceptKw("ORDER", "BY"):
				for {
					if _, err := ps.expr(); err != nil {
						return nil, err
					}
					_ = ps.acceptKw("ASC") || ps.acceptKw("DESC")
					if !ps.acceptP(",") {
						break
					}
				}
			case ps.acceptKw("LIMIT"):
				if _, err := ps.expr(); err != nil {
					return nil, err
				}
			case ps.acceptKw("AT", "TIME", "ZONE"):
				if _, err := ps.expr(); err != nil {
					return nil, err
				}
			case ps.acceptKw("FROM"), ps.acceptKw("FOR"):
				a, err := ps.expr()
				if err != nil {
					return nil, err
				}
				e.args = append(e.args, a)
			default:
				return nil, ps.errAt(fmt.Sprintf("the rest of %s's arguments", name))
			}
		}
		if !ps.acceptP(",") {
			break
		}
	}
	if err := ps.expectP(")"); err != nil {
		return nil, err
	}
	if ps.isKw("OVER") {
		return nil, unsupportedSQL{"window functions (OVER)"}
	}
	e.hi = ps.p
	return e, nil
}

// conjuncts splits e at its top-level ANDs.
func conjuncts(e *sqlExpr) []*sqlExpr {
	if e == nil {
		return nil
	}
	if e.kind == exBinary && e.op == "AND" {
		return append(conjuncts(e.args[0]), conjuncts(e.args[1])...)
	}
	if e.kind == exFunc && e.op == "()" && len(e.args) == 1 {
		if c := conjuncts(e.args[0]); len(c) > 1 {
			return c
		}
	}
	return []*sqlExpr{e}
}

// walkExpr calls f on e and on every expression in it, not going into
// subqueries.
func walkExpr(e *sqlExpr, f func(*sqlExpr)) {
	if e == nil {
		return
	}
	f(e)
	for _, a := range e.args {
		walkExpr(a, f)
	}
}

// aggregateFuncs are the aggregate functions of both dialects.
var aggregateFuncs = map[string]bool{
	"ANY_VALUE": true, "ARRAY_AGG": true, "ARRAY_CONCAT_AGG": true, "AVG": true, "BIT_AND": true, "BIT_OR": true,
	"BIT_XOR": true, "COUNT": true, "COUNTIF": true, "LOGICAL_AND": true, "LOGICAL_OR": true, "MAX": true,
	"MIN": true, "STRING_AGG": true, "SUM": true, "STDDEV": true, "STDDEV_SAMP": true, "VAR_SAMP": true,
	"VARIANCE": true, "APPROX_COUNT_DISTINCT": true, "APPROX_TOP_COUNT": true, "APPROX_TOP_SUM": true,
	"BOOL_AND": true, "BOOL_OR": true, "EVERY": true, "JSONB_AGG": true, "JSON_AGG": true, "MAX_BY": true, "MIN_BY": true,
	"APPROX_QUANTILES": true, "HLL_COUNT.INIT": true, "HLL_COUNT.MERGE": true,
}

func hasAggregate(e *sqlExpr) bool {
	found := false
	walkExpr(e, func(x *sqlExpr) {
		if x.kind == exFunc && aggregateFuncs[x.op] {
			found = true
		}
	})
	return found
}
