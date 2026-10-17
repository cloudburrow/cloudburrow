package bigtablefront

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// The parse of GoogleSQL for Bigtable (#1511): the lexer, and a
// precedence-climbing parser into the expression tree sqlexpr.go types and
// evaluates. The statement it reads is the one the reference documents:
//
//	SELECT [DISTINCT] item [, ...] [FROM name [(arg => value, ...)] [[AS] alias]]
//	  [WHERE condition] [GROUP BY expr [, ...]] [ORDER BY _key [ASC]]
//	  [LIMIT count]
//
// Statements the reference does not have (DML, DDL, joins, subqueries, set
// operations) are refused INVALID_ARGUMENT, and documented features the
// emulator's SQL lacks (UNPACK, window functions) UNIMPLEMENTED.

type token struct {
	kind string // "ident", "qident", "string", "bytes", "param", "int", "float", "op"
	text string
}

func lex(src string) ([]token, error) {
	var out []token
	r := []rune(src)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '-' && i+1 < len(r) && r[i+1] == '-', c == '#':
			for i < len(r) && r[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(r) && r[i+1] == '*':
			j := strings.Index(string(r[i+2:]), "*/")
			if j < 0 {
				return nil, fmt.Errorf("unterminated comment")
			}
			i += 2 + len([]rune(string(r[i+2:])[:j])) + 2
		case c == '`':
			j := i + 1
			for j < len(r) && r[j] != '`' {
				j++
			}
			if j == len(r) {
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			out = append(out, token{"qident", string(r[i+1 : j])})
			i = j + 1
		case (c == 'b' || c == 'B') && i+1 < len(r) && (r[i+1] == '\'' || r[i+1] == '"'):
			s, n, err := quoted(r[i+1:], true)
			if err != nil {
				return nil, err
			}
			out = append(out, token{"bytes", s})
			i += 1 + n
		case (c == 'r' || c == 'R') && i+1 < len(r) && (r[i+1] == '\'' || r[i+1] == '"'):
			q := r[i+1]
			j := i + 2
			for j < len(r) && r[j] != q {
				j++
			}
			if j == len(r) {
				return nil, fmt.Errorf("unterminated string literal")
			}
			out = append(out, token{"string", string(r[i+2 : j])})
			i = j + 1
		case c == '\'' || c == '"':
			s, n, err := quoted(r[i:], false)
			if err != nil {
				return nil, err
			}
			out = append(out, token{"string", s})
			i += n
		case c == '@':
			j := i + 1
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') {
				j++
			}
			if j == i+1 {
				return nil, fmt.Errorf("expected a parameter name after @")
			}
			out = append(out, token{"param", string(r[i+1 : j])})
			i = j
		case unicode.IsDigit(c) || c == '.' && i+1 < len(r) && unicode.IsDigit(r[i+1]):
			j := i
			kind := "int"
			if c == '0' && i+1 < len(r) && (r[i+1] == 'x' || r[i+1] == 'X') {
				j += 2
				for j < len(r) && strings.ContainsRune("0123456789abcdefABCDEF", r[j]) {
					j++
				}
				out = append(out, token{"int", string(r[i:j])})
				i = j
				continue
			}
			for j < len(r) && (unicode.IsDigit(r[j]) || r[j] == '.') {
				if r[j] == '.' {
					kind = "float"
				}
				j++
			}
			if j < len(r) && (r[j] == 'e' || r[j] == 'E') {
				kind = "float"
				j++
				if j < len(r) && (r[j] == '+' || r[j] == '-') {
					j++
				}
				for j < len(r) && unicode.IsDigit(r[j]) {
					j++
				}
			}
			out = append(out, token{kind, string(r[i:j])})
			i = j
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') {
				j++
			}
			out = append(out, token{"ident", string(r[i:j])})
			i = j
		default:
			for _, op := range []string{"=>", "<=", ">=", "!=", "<>", "||", "<<", ">>", "=", "<", ">", ",", "(", ")", "[", "]", "*", ";", "+", "-", "/", ".", "&", "|", "^", "~"} {
				if strings.HasPrefix(string(r[i:]), op) {
					out = append(out, token{"op", op})
					i += len([]rune(op))
					goto next
				}
			}
			return nil, fmt.Errorf("Syntax error: unexpected %q", string(c))
		next:
		}
	}
	return out, nil
}

// quoted reads a quoted literal at r[0], returning its value and length.
// Triple-quoted literals are read too.
func quoted(r []rune, isBytes bool) (string, int, error) {
	q := r[0]
	start, closeLen := 1, 1
	if len(r) >= 3 && r[1] == q && r[2] == q {
		start, closeLen = 3, 3
	}
	var b strings.Builder
	for i := start; i < len(r); i++ {
		switch {
		case r[i] == '\\' && i+1 < len(r):
			i++
			switch r[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '0':
				b.WriteByte(0)
			case 'x', 'X':
				if i+2 < len(r) {
					n, err := strconv.ParseUint(string(r[i+1:i+3]), 16, 8)
					if err != nil {
						return "", 0, fmt.Errorf("invalid \\x escape")
					}
					if isBytes {
						b.WriteByte(byte(n))
					} else {
						b.WriteRune(rune(n))
					}
					i += 2
				}
			default:
				b.WriteRune(r[i])
			}
		case r[i] == q && (closeLen == 1 || i+2 < len(r) && r[i+1] == q && r[i+2] == q):
			return b.String(), i + closeLen, nil
		default:
			b.WriteRune(r[i])
		}
	}
	return "", 0, fmt.Errorf("unterminated string literal")
}

// selectItem is one item of the SELECT list.
type selectItem struct {
	e     expr
	alias string
	star  bool   // * or alias.*
	pos   string // the source text, for errors
}

// tableArg is one of the FROM table's temporal arguments.
type tableArg struct {
	name string
	e    expr
}

type query struct {
	distinct bool
	items    []selectItem
	from     string
	alias    string
	args     []tableArg
	where    expr
	groupBy  []expr
	orderBy  bool
	limit    expr
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return token{}
}

func (p *parser) peekAt(n int) token {
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	return token{}
}

func (p *parser) next() token { t := p.peek(); p.pos++; return t }

func (p *parser) isKeyword(k string) bool {
	t := p.peek()
	return t.kind == "ident" && strings.EqualFold(t.text, k)
}

func (p *parser) keyword(k string) bool {
	if p.isKeyword(k) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) op(o string) bool {
	if t := p.peek(); t.kind == "op" && t.text == o {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expect(o string) error {
	if !p.op(o) {
		return fmt.Errorf("Syntax error: expected %q but got %s", o, p.describe())
	}
	return nil
}

func (p *parser) describe() string {
	t := p.peek()
	if t.kind == "" {
		return "end of input"
	}
	return fmt.Sprintf("%q", t.text)
}

// reserved are the keywords that end an expression or a select item.
var reserved = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "GROUP": true, "ORDER": true, "LIMIT": true, "AS": true,
	"AND": true, "OR": true, "NOT": true, "IS": true, "IN": true, "BETWEEN": true, "LIKE": true, "CASE": true,
	"WHEN": true, "THEN": true, "ELSE": true, "END": true, "BY": true, "HAVING": true, "JOIN": true, "UNION": true,
	"ON": true, "CROSS": true, "INNER": true, "LEFT": true, "RIGHT": true, "FULL": true, "WINDOW": true,
	"WITH": true, "OFFSET": true, "ASC": true, "DESC": true, "DISTINCT": true, "INTERVAL": true, "EXCEPT": true,
	"INTERSECT": true, "QUALIFY": true, "UNNEST": true, "OVER": true,
}

// unsupportedErr is an error the front answers UNIMPLEMENTED: a part of
// the documented language the emulator's SQL does not run.
type unsupportedErr struct{ what string }

func (e unsupportedErr) Error() string {
	return e.what + " is not supported by the emulator's SQL"
}

func parseQuery(src string) (*query, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	for _, k := range []string{"INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "MERGE"} {
		if p.isKeyword(k) {
			return nil, fmt.Errorf("Statement not supported: %s: GoogleSQL for Bigtable runs SELECT queries alone", strings.ToUpper(k))
		}
	}
	if p.isKeyword("WITH") {
		return nil, fmt.Errorf("Syntax error: WITH (common table expressions) is not supported by GoogleSQL for Bigtable")
	}
	if !p.keyword("SELECT") {
		return nil, fmt.Errorf("Syntax error: expected SELECT but got %s", p.describe())
	}
	q := &query{}
	if p.keyword("DISTINCT") {
		q.distinct = true
	} else {
		p.keyword("ALL")
	}
	if p.isKeyword("AS") && (strings.EqualFold(p.peekAt(1).text, "STRUCT") || strings.EqualFold(p.peekAt(1).text, "VALUE")) {
		return nil, unsupportedErr{"SELECT AS " + strings.ToUpper(p.peekAt(1).text)}
	}
	for {
		start := p.pos
		if p.op("*") {
			if p.isKeyword("EXCEPT") || p.isKeyword("REPLACE") {
				return nil, unsupportedErr{"SELECT * " + strings.ToUpper(p.peek().text)}
			}
			q.items = append(q.items, selectItem{star: true, pos: "*"})
		} else if t := p.peek(); (t.kind == "ident" || t.kind == "qident") && p.peekAt(1).text == "." && p.peekAt(2).text == "*" {
			p.pos += 3
			q.items = append(q.items, selectItem{star: true, alias: t.text, pos: t.text + ".*"})
		} else {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			it := selectItem{e: e}
			if p.keyword("AS") {
				t := p.next()
				if t.kind != "ident" && t.kind != "qident" {
					return nil, fmt.Errorf("Syntax error: expected an alias after AS")
				}
				it.alias = t.text
			} else if t := p.peek(); t.kind == "qident" || t.kind == "ident" && !reserved[strings.ToUpper(t.text)] {
				p.pos++
				it.alias = t.text
			}
			it.pos = tokenText(p.toks[start:p.pos])
			q.items = append(q.items, it)
		}
		if !p.op(",") {
			break
		}
	}
	if p.keyword("FROM") {
		if p.op("(") {
			if p.isKeyword("SELECT") {
				return nil, fmt.Errorf("Syntax error: subqueries are not supported by GoogleSQL for Bigtable")
			}
			return nil, fmt.Errorf("Syntax error: expected a table name after FROM")
		}
		if p.isKeyword("UNPACK") {
			return nil, unsupportedErr{"UNPACK"}
		}
		if p.isKeyword("UNNEST") {
			return nil, unsupportedErr{"UNNEST"}
		}
		t := p.next()
		if t.kind != "ident" && t.kind != "qident" {
			return nil, fmt.Errorf("Syntax error: expected a table name after FROM")
		}
		q.from = t.text
		if p.op("(") {
			for !p.op(")") {
				n := p.next()
				if n.kind != "ident" {
					return nil, fmt.Errorf("Syntax error: expected a named argument of %s", q.from)
				}
				if err := p.expect("=>"); err != nil {
					return nil, err
				}
				e, err := p.expr()
				if err != nil {
					return nil, err
				}
				q.args = append(q.args, tableArg{strings.ToLower(n.text), e})
				if !p.op(",") {
					if err := p.expect(")"); err != nil {
						return nil, err
					}
					break
				}
			}
		}
		if p.keyword("AS") {
			t := p.next()
			if t.kind != "ident" && t.kind != "qident" {
				return nil, fmt.Errorf("Syntax error: expected an alias after AS")
			}
			q.alias = t.text
		} else if t := p.peek(); t.kind == "qident" || t.kind == "ident" && !reserved[strings.ToUpper(t.text)] {
			p.pos++
			q.alias = t.text
		}
		if p.op(",") || p.isKeyword("JOIN") || p.isKeyword("CROSS") || p.isKeyword("INNER") || p.isKeyword("LEFT") || p.isKeyword("RIGHT") || p.isKeyword("FULL") {
			return nil, fmt.Errorf("Syntax error: joins are not supported by GoogleSQL for Bigtable")
		}
	}
	if p.keyword("WHERE") {
		if q.where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.keyword("GROUP") {
		if !p.keyword("BY") {
			return nil, fmt.Errorf("Syntax error: expected BY after GROUP")
		}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			q.groupBy = append(q.groupBy, e)
			if !p.op(",") {
				break
			}
		}
	}
	if p.isKeyword("HAVING") {
		return nil, fmt.Errorf("Syntax error: HAVING is not part of GoogleSQL for Bigtable")
	}
	if p.isKeyword("WINDOW") || p.isKeyword("QUALIFY") {
		return nil, unsupportedErr{strings.ToUpper(p.peek().text)}
	}
	if p.keyword("ORDER") {
		if !p.keyword("BY") {
			return nil, fmt.Errorf("Syntax error: expected BY after ORDER")
		}
		t := p.next()
		if q.alias != "" && t.text == q.alias && p.op(".") {
			t = p.next()
		}
		if t.text != "_key" {
			return nil, fmt.Errorf("ORDER BY: only the _key column can be used with the ORDER BY clause")
		}
		if p.keyword("DESC") {
			return nil, fmt.Errorf("ORDER BY _key DESC: only ascending order (ASC) is supported")
		}
		p.keyword("ASC")
		if p.op(",") {
			return nil, fmt.Errorf("ORDER BY: only the _key column can be used with the ORDER BY clause")
		}
		q.orderBy = true
	}
	if p.keyword("LIMIT") {
		if q.limit, err = p.expr(); err != nil {
			return nil, err
		}
		if p.isKeyword("OFFSET") {
			return nil, fmt.Errorf("Syntax error: OFFSET is not part of GoogleSQL for Bigtable")
		}
	}
	if p.isKeyword("UNION") || p.isKeyword("INTERSECT") || p.isKeyword("EXCEPT") {
		return nil, fmt.Errorf("Syntax error: set operations (%s) are not supported by GoogleSQL for Bigtable", strings.ToUpper(p.peek().text))
	}
	p.op(";")
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("Syntax error: unexpected %s", p.describe())
	}
	return q, nil
}

func tokenText(ts []token) string {
	var parts []string
	for _, t := range ts {
		switch t.kind {
		case "string":
			parts = append(parts, strconv.Quote(t.text))
		case "bytes":
			parts = append(parts, "b"+strconv.Quote(t.text))
		case "param":
			parts = append(parts, "@"+t.text)
		case "qident":
			parts = append(parts, "`"+t.text+"`")
		default:
			parts = append(parts, t.text)
		}
	}
	return strings.Join(parts, " ")
}

// --- expressions, by precedence ---

func (p *parser) expr() (expr, error) { return p.or() }

func (p *parser) or() (expr, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.keyword("OR") {
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = &binaryOp{op: "OR", l: l, r: r}
	}
	return l, nil
}

func (p *parser) and() (expr, error) {
	l, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.keyword("AND") {
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		l = &binaryOp{op: "AND", l: l, r: r}
	}
	return l, nil
}

func (p *parser) not() (expr, error) {
	if p.keyword("NOT") {
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		return &unaryOp{op: "NOT", x: x}, nil
	}
	return p.comparison()
}

func (p *parser) comparison() (expr, error) {
	l, err := p.bitOr()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.kind == "op" && (t.text == "=" || t.text == "!=" || t.text == "<>" || t.text == "<" || t.text == "<=" || t.text == ">" || t.text == ">="):
			p.pos++
			r, err := p.bitOr()
			if err != nil {
				return nil, err
			}
			op := t.text
			if op == "<>" {
				op = "!="
			}
			l = &binaryOp{op: op, l: l, r: r}
		case p.isKeyword("IS"):
			p.pos++
			not := p.keyword("NOT")
			var what string
			switch {
			case p.keyword("NULL"):
				what = "NULL"
			case p.keyword("TRUE"):
				what = "TRUE"
			case p.keyword("FALSE"):
				what = "FALSE"
			default:
				return nil, fmt.Errorf("Syntax error: expected NULL, TRUE or FALSE after IS")
			}
			l = &isExpr{x: l, not: not, what: what}
		case p.isKeyword("NOT") && (strings.EqualFold(p.peekAt(1).text, "IN") || strings.EqualFold(p.peekAt(1).text, "LIKE") || strings.EqualFold(p.peekAt(1).text, "BETWEEN")):
			p.pos++
			if l, err = p.postComparison(l, true); err != nil {
				return nil, err
			}
		case p.isKeyword("IN") || p.isKeyword("LIKE") || p.isKeyword("BETWEEN"):
			if l, err = p.postComparison(l, false); err != nil {
				return nil, err
			}
		default:
			return l, nil
		}
	}
}

func (p *parser) postComparison(l expr, not bool) (expr, error) {
	switch {
	case p.keyword("IN"):
		if p.isKeyword("UNNEST") {
			p.pos++
			if err := p.expect("("); err != nil {
				return nil, err
			}
			a, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return &inExpr{x: l, array: a, not: not}, nil
		}
		if err := p.expect("("); err != nil {
			return nil, err
		}
		if p.isKeyword("SELECT") {
			return nil, fmt.Errorf("Syntax error: subqueries are not supported by GoogleSQL for Bigtable")
		}
		var list []expr
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			list = append(list, e)
			if !p.op(",") {
				break
			}
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		return &inExpr{x: l, list: list, not: not}, nil
	case p.keyword("LIKE"):
		r, err := p.bitOr()
		if err != nil {
			return nil, err
		}
		var e expr = &binaryOp{op: "LIKE", l: l, r: r}
		if not {
			e = &unaryOp{op: "NOT", x: e}
		}
		return e, nil
	case p.keyword("BETWEEN"):
		lo, err := p.bitOr()
		if err != nil {
			return nil, err
		}
		if !p.keyword("AND") {
			return nil, fmt.Errorf("Syntax error: expected AND in BETWEEN")
		}
		hi, err := p.bitOr()
		if err != nil {
			return nil, err
		}
		var e expr = &binaryOp{op: "AND", l: &binaryOp{op: ">=", l: l, r: lo}, r: &binaryOp{op: "<=", l: l, r: hi}}
		if not {
			e = &unaryOp{op: "NOT", x: e}
		}
		return e, nil
	}
	return nil, fmt.Errorf("Syntax error: unexpected %s", p.describe())
}

func (p *parser) binaryLevel(ops []string, next func() (expr, error)) (expr, error) {
	l, err := next()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		matched := ""
		for _, o := range ops {
			if t.kind == "op" && t.text == o {
				matched = o
			}
		}
		if matched == "" {
			return l, nil
		}
		p.pos++
		r, err := next()
		if err != nil {
			return nil, err
		}
		l = &binaryOp{op: matched, l: l, r: r}
	}
}

func (p *parser) bitOr() (expr, error)  { return p.binaryLevel([]string{"|"}, p.bitXor) }
func (p *parser) bitXor() (expr, error) { return p.binaryLevel([]string{"^"}, p.bitAnd) }
func (p *parser) bitAnd() (expr, error) { return p.binaryLevel([]string{"&"}, p.shift) }
func (p *parser) shift() (expr, error)  { return p.binaryLevel([]string{"<<", ">>"}, p.additive) }
func (p *parser) additive() (expr, error) {
	return p.binaryLevel([]string{"+", "-"}, p.multiplicative)
}
func (p *parser) multiplicative() (expr, error) {
	return p.binaryLevel([]string{"*", "/", "||"}, p.unaryExpr)
}

func (p *parser) unaryExpr() (expr, error) {
	if p.op("-") {
		x, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		if l, ok := x.(*literal); ok {
			switch v := l.v.(type) {
			case int64:
				return &literal{v: -v, t: int64Type}, nil
			case float64:
				return &literal{v: -v, t: float64Type}, nil
			}
		}
		return &unaryOp{op: "-", x: x}, nil
	}
	if p.op("+") {
		return p.unaryExpr()
	}
	if p.op("~") {
		x, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		return &unaryOp{op: "~", x: x}, nil
	}
	return p.postfix()
}

func (p *parser) postfix() (expr, error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.op("["):
			mode := "key"
			wrapped := false
			for _, m := range []string{"OFFSET", "ORDINAL", "SAFE_OFFSET", "SAFE_ORDINAL"} {
				if p.isKeyword(m) && p.peekAt(1).text == "(" {
					mode = strings.ToLower(m)
					p.pos += 2
					wrapped = true
					break
				}
			}
			k, err := p.expr()
			if err != nil {
				return nil, err
			}
			if wrapped {
				if err := p.expect(")"); err != nil {
					return nil, err
				}
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			x = &index{x: x, k: k, mode: mode}
		case p.peek().text == "." && p.peek().kind == "op":
			p.pos++
			t := p.next()
			if t.kind != "ident" && t.kind != "qident" {
				return nil, fmt.Errorf("Syntax error: expected a field name after .")
			}
			// alias.column: the table alias qualifying a column.
			if c, ok := x.(*colRef); ok && !c.quoted && c.qualifier == "" {
				x = &colRef{name: t.text, qualifier: c.name}
				continue
			}
			x = &field{x: x, name: t.text}
		default:
			return x, nil
		}
	}
}

// typeNames are the types CAST and typed literals name.
func (p *parser) typeName() (string, error) {
	t := p.next()
	if t.kind != "ident" {
		return "", fmt.Errorf("Syntax error: expected a type name")
	}
	name := strings.ToUpper(t.text)
	if name == "ARRAY" || name == "MAP" || name == "STRUCT" {
		if err := p.expect("<"); err != nil {
			return "", err
		}
		var parts []string
		depth := 1
		for depth > 0 {
			n := p.next()
			switch n.text {
			case "<":
				depth++
			case ">":
				depth--
			case ">>":
				depth -= 2
			case "":
				return "", fmt.Errorf("Syntax error: unterminated type")
			}
			if depth > 0 {
				parts = append(parts, n.text)
			}
		}
		return name + "<" + strings.Join(parts, " ") + ">", nil
	}
	return name, nil
}

func (p *parser) primary() (expr, error) {
	t := p.next()
	switch t.kind {
	case "string":
		return &literal{v: t.text, t: stringType}, nil
	case "bytes":
		return &literal{v: []byte(t.text), t: bytesType}, nil
	case "int":
		n, err := strconv.ParseInt(t.text, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("Invalid integer literal: %s", t.text)
		}
		return &literal{v: n, t: int64Type}, nil
	case "float":
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("Invalid floating point literal: %s", t.text)
		}
		return &literal{v: f, t: float64Type}, nil
	case "param":
		return &param{name: t.text}, nil
	case "qident":
		return &colRef{name: t.text, quoted: true}, nil
	case "op":
		switch t.text {
		case "(":
			if p.isKeyword("SELECT") {
				return nil, fmt.Errorf("Syntax error: subqueries are not supported by GoogleSQL for Bigtable")
			}
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			return e, p.expect(")")
		case "[":
			return p.arrayLiteral()
		}
		return nil, fmt.Errorf("Syntax error: unexpected %q", t.text)
	case "ident":
	default:
		return nil, fmt.Errorf("Syntax error: unexpected end of input")
	}
	up := strings.ToUpper(t.text)
	switch up {
	case "NULL":
		return &literal{v: nil}, nil
	case "TRUE":
		return &literal{v: true, t: boolType}, nil
	case "FALSE":
		return &literal{v: false, t: boolType}, nil
	case "CASE":
		return p.caseExpr()
	case "ARRAY":
		if p.op("[") {
			return p.arrayLiteral()
		}
		if p.peek().text == "<" {
			p.pos--
			tn, err := p.typeName()
			if err != nil {
				return nil, err
			}
			if err := p.expect("["); err != nil {
				return nil, err
			}
			a, err := p.arrayLiteral()
			if err != nil {
				return nil, err
			}
			return &cast{x: a, to: tn}, nil
		}
		if p.op("(") {
			return nil, fmt.Errorf("Syntax error: subqueries are not supported by GoogleSQL for Bigtable")
		}
	case "STRUCT":
		return nil, unsupportedErr{"STRUCT constructors"}
	case "INTERVAL":
		n, err := p.additive()
		if err != nil {
			return nil, err
		}
		u := p.next()
		if u.kind != "ident" {
			return nil, fmt.Errorf("Syntax error: expected a date part after INTERVAL")
		}
		return &interval{n: n, unit: strings.ToUpper(u.text)}, nil
	case "TIMESTAMP", "DATE", "NUMERIC", "JSON", "BIGNUMERIC", "DATETIME", "TIME", "RANGE":
		if s := p.peek(); s.kind == "string" {
			p.pos++
			if up != "TIMESTAMP" && up != "DATE" {
				return nil, unsupportedErr{up + " literals"}
			}
			return &cast{x: &literal{v: s.text, t: stringType}, to: up, literal: true}, nil
		}
	case "CAST", "SAFE_CAST":
		if err := p.expect("("); err != nil {
			return nil, err
		}
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		if !p.keyword("AS") {
			return nil, fmt.Errorf("Syntax error: expected AS in %s", up)
		}
		tn, err := p.typeName()
		if err != nil {
			return nil, err
		}
		if p.keyword("FORMAT") {
			return nil, unsupportedErr{up + " with FORMAT"}
		}
		return &cast{x: x, to: tn, safe: up == "SAFE_CAST"}, p.expect(")")
	case "EXTRACT":
		if err := p.expect("("); err != nil {
			return nil, err
		}
		part := p.next()
		if part.kind != "ident" {
			return nil, fmt.Errorf("Syntax error: expected a date part in EXTRACT")
		}
		if !p.keyword("FROM") {
			return nil, fmt.Errorf("Syntax error: expected FROM in EXTRACT")
		}
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		tz := expr(nil)
		if p.keyword("AT") {
			if !p.keyword("TIME") || !p.keyword("ZONE") {
				return nil, fmt.Errorf("Syntax error: expected AT TIME ZONE")
			}
			if tz, err = p.expr(); err != nil {
				return nil, err
			}
		}
		return &extract{part: strings.ToUpper(part.text), x: x, tz: tz}, p.expect(")")
	}
	if p.peek().text == "(" && p.peek().kind == "op" {
		p.pos++
		c := &call{name: up}
		if p.keyword("DISTINCT") {
			c.distinct = true
		}
		if up == "COUNT" && p.op("*") {
			c.star = true
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return c.over(p)
		}
		if !p.op(")") {
			for {
				// date parts are bare identifiers in the timestamp functions.
				if a := p.peek(); a.kind == "ident" && datePart[strings.ToUpper(a.text)] && (p.peekAt(1).text == ")" || p.peekAt(1).text == ",") && (strings.HasPrefix(up, "TIMESTAMP_") || up == "DATE_TRUNC") {
					p.pos++
					c.args = append(c.args, &literal{v: strings.ToUpper(a.text), t: datePartType})
				} else {
					a, err := p.expr()
					if err != nil {
						return nil, err
					}
					c.args = append(c.args, a)
				}
				if p.keyword("IGNORE") || p.keyword("RESPECT") {
					if !p.keyword("NULLS") {
						return nil, fmt.Errorf("Syntax error: expected NULLS")
					}
					if strings.EqualFold(p.toks[p.pos-2].text, "IGNORE") {
						c.ignoreNulls = true
					}
				}
				if p.isKeyword("ORDER") || p.isKeyword("LIMIT") || p.isKeyword("HAVING") {
					return nil, unsupportedErr{"ORDER BY, LIMIT and HAVING inside " + up}
				}
				if !p.op(",") {
					break
				}
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
		}
		return c.over(p)
	}
	return &colRef{name: t.text}, nil
}

// over refuses window functions: an OVER clause after a call.
func (c *call) over(p *parser) (expr, error) {
	if p.isKeyword("OVER") {
		return nil, unsupportedErr{"window function calls (OVER)"}
	}
	return c, nil
}

func (p *parser) arrayLiteral() (expr, error) {
	a := &arrayLit{}
	if p.op("]") {
		return a, nil
	}
	for {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		a.elems = append(a.elems, e)
		if !p.op(",") {
			break
		}
	}
	return a, p.expect("]")
}

func (p *parser) caseExpr() (expr, error) {
	c := &caseExpr{}
	if !p.isKeyword("WHEN") {
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		c.operand = x
	}
	for p.keyword("WHEN") {
		w, err := p.expr()
		if err != nil {
			return nil, err
		}
		if !p.keyword("THEN") {
			return nil, fmt.Errorf("Syntax error: expected THEN")
		}
		th, err := p.expr()
		if err != nil {
			return nil, err
		}
		c.whens = append(c.whens, w)
		c.thens = append(c.thens, th)
	}
	if len(c.whens) == 0 {
		return nil, fmt.Errorf("Syntax error: CASE needs a WHEN")
	}
	if p.keyword("ELSE") {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		c.els = e
	}
	if !p.keyword("END") {
		return nil, fmt.Errorf("Syntax error: expected END")
	}
	return c, nil
}

var datePart = map[string]bool{
	"NANOSECOND": true, "MICROSECOND": true, "MILLISECOND": true, "SECOND": true, "MINUTE": true, "HOUR": true,
	"DAY": true, "WEEK": true, "MONTH": true, "QUARTER": true, "YEAR": true, "DAYOFWEEK": true, "DAYOFYEAR": true,
	"ISOWEEK": true, "ISOYEAR": true, "DATE": true,
}
