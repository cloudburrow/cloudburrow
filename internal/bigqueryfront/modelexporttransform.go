package bigqueryfront

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// EXPORT MODEL of a model with a TRANSFORM clause, and of a TRANSFORM_ONLY
// model (#2210), as "Export models"
// (https://docs.cloud.google.com/bigquery/docs/exporting-models, read
// 2026-11-24) documents it: "an additional preprocessing model performs the
// same logic in the TRANSFORM clause and is saved in the TensorFlow
// SavedModel format under the subdirectory transform" (the model's own
// files at the URI, as without a TRANSFORM); a TRANSFORM_ONLY model is that
// preprocessing SavedModel alone, at the URI, whose outputs are "the same
// as the columns specified in the model's TRANSFORM clause".
//
// The preprocessing SavedModel's serving_default signature takes each
// column the TRANSFORM reads, as the page's "Supported data types" table
// gives them: INT64 an int64 tensor; NUMERIC, BIGNUMERIC and FLOAT64
// float64; BOOL bool; STRING and BYTES string; DATE a string in "%F";
// DATETIME "%F %H:%M:%E6S"; TIME "%H:%M:%E6S"; TIMESTAMP
// "%F %H:%M:%E1S %z"; and NULL as 123456789.0e10 (float64),
// 1234567890000000000 (int64) or "__MISSING__" (string). It answers one
// tensor for each TRANSFORM column, named as the column, of the dtype the
// model's own export takes it in (an INT64 column int64, FLOAT64 float64,
// STRING string, BOOL bool; a DATE, DATETIME or TIME the string of its
// input format), so the two chain. "During model export,
// ARRAY<STRUCT<INT64, FLOAT64>>, ARRAY and TIMESTAMP are supported as
// pre-transformed data, but are not supported as post-transformed data."
//
// The page lists the SQL a TRANSFORM can use for it to export: the
// operators, conditional expressions, mathematical, conversion, string,
// date, datetime, time and timestamp functions it names, and the manual
// preprocessing functions. The front writes the ML functions of a
// TRANSFORM out as SQL at CREATE MODEL (modelpreprocess.go, modelvocab.go),
// with what they learnt as constants, so the model keeps plain SQL; this
// file compiles that SQL into an expression graph (transformGraph) of a
// few primitive operations, which the converter image's export
// (deploy/tfconvert/convert.py) builds in TensorFlow, a value and a NULL
// mask a column, as GoogleSQL computes it. A scalar subquery over a
// constant array (ML.BUCKETIZE's split points, ML.LABEL_ENCODER's
// vocabulary) is a lookup or an unrolled sum; ML.HASH_BUCKETIZE's
// FARM_FINGERPRINT is TensorFlow's tf.fingerprint, FarmHash's
// Fingerprint64, as BigQuery's is.
//
// UNVERIFIED (the page does not say; re-read 2026-11-24): the
// preprocessing signature's output names and dtypes beyond "the same as the columns" (CloudBurrow's are
// above); how a NULL output is written (CloudBurrow writes the input
// table's NULL values; a BOOL NULL is false); NUMERIC and BIGNUMERIC are
// computed in float64, as the page says the signatures carry them. A
// time zone argument is computed from the tz database's offsets, written
// into the graph (a local time in a transition's gap or fold takes the
// offset before it, UNVERIFIED; %Z is the zone's abbreviation, as absl's
// FormatTime writes it, UNVERIFIED). Every function the page lists
// compiles; one it does not list, and that ML preprocessing's SQL does not
// use, is refused 400, as the page's list is all a TRANSFORM can use. An
// ARRAY input is in the signature as the page's table gives it (an
// ARRAY<STRUCT<INT64, FLOAT64>> a SparseTensor, NAME, NAME_1 and NAME_2
// its indices, values and shape, UNVERIFIED); an output that reads one is
// refused, the functions the page lists of one giving an ARRAY. ERROR()
// in the TRANSFORM gives NULL.

// Missing values of the preprocessing model's inputs, as the page's NULL row
// gives them.
const (
	transformNullFloat  = 123456789.0e10
	transformNullInt    = int64(1234567890000000000)
	transformNullString = "__MISSING__"
)

// tgNode is one operation of a transform graph: Op on Args, its result of
// TF dtype T (i int64, f float64, s string, b bool; DATE, DATETIME, TIME
// and TIMESTAMP are int64 days or microseconds), with Op's own attributes.
type tgNode struct {
	Op   string         `json:"op"`
	T    string         `json:"t"`
	Args []*tgNode      `json:"args,omitempty"`
	A    map[string]any `json:"a,omitempty"`
}

// tgVal is a compiled expression: its GoogleSQL type, the node computing
// it, and its value when it is a constant (lit; isLit), or its elements
// when it is a constant array.
type tgVal struct {
	typ   string
	node  *tgNode
	lit   any
	isLit bool
	arr   []tgVal
	isArr bool
	elem  string
}

// tgFail is a compile refusal.
type tgFail struct {
	code int
	msg  string
}

func (e *tgFail) Error() string { return e.msg }

// tgArrayOut is the refusal of an ARRAY in a TRANSFORM output, which
// transformSpec names the column of.
var tgArrayOut = &tgFail{400, "ARRAY"}

func tgUnsupported(format string, a ...any) *tgFail {
	return &tgFail{501, "Not implemented here: EXPORT MODEL of this TRANSFORM: " + fmt.Sprintf(format, a...) + ". Nothing was written."}
}

// tfDtypeOf is the graph dtype of GoogleSQL type typ.
func tfDtypeOf(typ string) string {
	switch typ {
	case "INT64", "DATE", "DATETIME", "TIME", "TIMESTAMP":
		return "i"
	case "FLOAT64", "NUMERIC", "BIGNUMERIC":
		return "f"
	case "STRING", "BYTES":
		return "s"
	case "BOOL":
		return "b"
	}
	return "f"
}

// sqlTypeOfColumn is a REST schema type as a GoogleSQL type name.
func sqlTypeOfColumn(t string) string {
	switch u := strings.ToUpper(t); u {
	case "INTEGER", "INT64":
		return "INT64"
	case "FLOAT", "FLOAT64":
		return "FLOAT64"
	case "BOOLEAN", "BOOL":
		return "BOOL"
	default:
		return u
	}
}

func isNumericType(t string) bool {
	return t == "INT64" || t == "FLOAT64" || t == "NUMERIC" || t == "BIGNUMERIC"
}

func node(op, t string, a map[string]any, args ...*tgNode) *tgNode {
	return &tgNode{Op: op, T: t, Args: args, A: a}
}

func litVal(typ string, v any) tgVal {
	n := node("lit", tfDtypeOf(typ), map[string]any{"v": v})
	if v == nil {
		n = node("null", tfDtypeOf(typ), nil)
	}
	return tgVal{typ: typ, node: n, lit: v, isLit: true}
}

// tgCompiler compiles one TRANSFORM item's SQL.
type tgCompiler struct {
	toks []token
	i    int
	src  string
	cols map[string]tgVal // the inputs, by lower-case name
	env  []map[string]tgVal
}

func (c *tgCompiler) peek() token {
	if c.i < len(c.toks) {
		return c.toks[c.i]
	}
	return token{kind: tokPunct, text: "\x00", pos: len(c.src), end: len(c.src)}
}

func (c *tgCompiler) peekAt(k int) token {
	if c.i+k < len(c.toks) {
		return c.toks[c.i+k]
	}
	return token{kind: tokPunct, text: "\x00", pos: len(c.src), end: len(c.src)}
}

func (c *tgCompiler) next() token { t := c.peek(); c.i++; return t }

func (c *tgCompiler) accept(word string) bool {
	if c.peek().is(word) {
		c.i++
		return true
	}
	return false
}

func (c *tgCompiler) acceptPunct(p string) bool {
	if c.peek().punct(p) {
		c.i++
		return true
	}
	return false
}

func (c *tgCompiler) expectPunct(p string) {
	if !c.acceptPunct(p) {
		panic(&tgFail{400, fmt.Sprintf("cloudburrow: could not read the TRANSFORM for export: expected %q at %q", p, c.rest())})
	}
}

func (c *tgCompiler) expect(word string) {
	if !c.accept(word) {
		panic(&tgFail{400, fmt.Sprintf("cloudburrow: could not read the TRANSFORM for export: expected %s at %q", word, c.rest())})
	}
}

func (c *tgCompiler) rest() string {
	p := c.peek().pos
	s := c.src[p:]
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// compileTransformItem compiles expression sql over the inputs cols.
func compileTransformItem(sql string, cols map[string]tgVal) (v tgVal, err *tgFail) {
	toks, ok := lex(sql)
	if !ok {
		return v, &tgFail{400, "cloudburrow: could not read the TRANSFORM item " + sql}
	}
	c := &tgCompiler{toks: toks, src: sql, cols: cols}
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(*tgFail)
			if !ok {
				panic(r)
			}
			err = f
		}
	}()
	v = c.expr(0)
	if c.i != len(c.toks) {
		panic(&tgFail{400, "cloudburrow: could not read the TRANSFORM item at " + c.rest()})
	}
	return v, nil
}

// Binary operator precedences, GoogleSQL's, low to high.
const (
	precOr = iota + 1
	precAnd
	precNot
	precCmp
	precBitOr
	precBitXor
	precBitAnd
	precShift
	precAdd
	precMul
	precUnary
)

func (c *tgCompiler) binaryPrec() (string, int) {
	t := c.peek()
	switch {
	case t.is("OR"):
		return "OR", precOr
	case t.is("AND"):
		return "AND", precAnd
	case t.punct("="):
		return "=", precCmp
	case t.punct("<"):
		switch n := c.peekAt(1); {
		case n.punct("=") && n.pos == t.end:
			return "<=", precCmp
		case n.punct(">") && n.pos == t.end:
			return "!=", precCmp
		case n.punct("<") && n.pos == t.end:
			return "<<", precShift
		}
		return "<", precCmp
	case t.punct(">"):
		switch n := c.peekAt(1); {
		case n.punct("=") && n.pos == t.end:
			return ">=", precCmp
		case n.punct(">") && n.pos == t.end:
			return ">>", precShift
		}
		return ">", precCmp
	case t.punct("!") && c.peekAt(1).punct("="):
		return "!=", precCmp
	case t.is("IS"), t.is("BETWEEN"), t.is("IN"), t.is("LIKE"):
		return strings.ToUpper(t.text), precCmp
	case t.is("NOT") && (c.peekAt(1).is("BETWEEN") || c.peekAt(1).is("IN") || c.peekAt(1).is("LIKE")):
		return "NOT " + strings.ToUpper(c.peekAt(1).text), precCmp
	case t.punct("|"):
		if n := c.peekAt(1); n.punct("|") && n.pos == t.end {
			return "||", precMul
		}
		return "|", precBitOr
	case t.punct("^"):
		return "^", precBitXor
	case t.punct("&"):
		return "&", precBitAnd
	case t.punct("+"), t.punct("-"):
		return t.text, precAdd
	case t.punct("*"), t.punct("/"):
		return t.text, precMul
	}
	return "", 0
}

func (c *tgCompiler) expr(minPrec int) tgVal {
	var left tgVal
	switch {
	case c.peek().is("NOT"):
		c.i++
		x := c.expr(precNot)
		left = c.not(x)
	default:
		left = c.unary()
	}
	for {
		op, prec := c.binaryPrec()
		if prec == 0 || prec <= minPrec {
			return left
		}
		// consume the operator's tokens
		switch op {
		case "<=", ">=", "!=", "<<", ">>", "||":
			c.i += 2
		case "NOT BETWEEN", "NOT IN", "NOT LIKE":
			c.i += 2
		default:
			c.i++
		}
		switch op {
		case "IS":
			neg := c.accept("NOT")
			var r tgVal
			switch {
			case c.accept("NULL"):
				r = tgVal{typ: "BOOL", node: node("isnull", "b", nil, left.node)}
			case c.accept("TRUE"):
				r = tgVal{typ: "BOOL", node: node("istrue", "b", nil, c.boolOf(left).node)}
			case c.accept("FALSE"):
				r = tgVal{typ: "BOOL", node: node("isfalse", "b", nil, c.boolOf(left).node)}
			default:
				panic(tgUnsupported("IS %s", c.rest()))
			}
			if neg {
				r = tgVal{typ: "BOOL", node: node("not", "b", nil, r.node)}
			}
			left = r
		case "BETWEEN", "NOT BETWEEN":
			lo := c.expr(precCmp)
			c.expect("AND")
			hi := c.expr(precCmp)
			r := c.and(c.compare(">=", left, lo), c.compare("<=", left, hi))
			if op == "NOT BETWEEN" {
				r = c.not(r)
			}
			left = r
		case "IN", "NOT IN":
			var items []tgVal
			if c.accept("UNNEST") {
				c.expectPunct("(")
				a := c.expr(0)
				c.expectPunct(")")
				if !a.isArr {
					panic(tgUnsupported("IN UNNEST of an array that is not constant"))
				}
				items = a.arr
			} else {
				c.expectPunct("(")
				if c.peek().is("SELECT") {
					panic(tgUnsupported("IN (SELECT ...)"))
				}
				for {
					items = append(items, c.expr(0))
					if !c.acceptPunct(",") {
						break
					}
				}
				c.expectPunct(")")
			}
			r := litVal("BOOL", false)
			for k, it := range items {
				eq := c.compare("=", left, it)
				if k == 0 {
					r = eq
				} else {
					r = c.or(r, eq)
				}
			}
			if op == "NOT IN" {
				r = c.not(r)
			}
			left = r
		case "LIKE", "NOT LIKE":
			p := c.expr(precCmp)
			if !p.isLit || p.typ != "STRING" {
				panic(tgUnsupported("LIKE of a pattern that is not a string literal"))
			}
			re := likeRegex(fmt.Sprint(p.lit))
			r := tgVal{typ: "BOOL", node: node("regex_full_match", "b", map[string]any{"pattern": re}, c.str(left).node)}
			if op == "NOT LIKE" {
				r = c.not(r)
			}
			left = r
		default:
			right := c.expr(prec)
			left = c.binary(op, left, right)
		}
	}
}

func likeRegex(p string) string {
	var b strings.Builder
	b.WriteString("(?s)")
	for k := 0; k < len(p); k++ {
		switch ch := p[k]; ch {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		case '\\':
			if k+1 < len(p) {
				k++
				b.WriteString(regexp.QuoteMeta(string(p[k])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	return b.String()
}

func (c *tgCompiler) unary() tgVal {
	t := c.peek()
	switch {
	case t.punct("-"):
		c.i++
		x := c.expr(precUnary)
		if x.isLit {
			switch v := x.lit.(type) {
			case int64:
				return litVal("INT64", -v)
			case float64:
				return litVal(x.typ, -v)
			}
		}
		if !isNumericType(x.typ) {
			panic(tgUnsupported("unary minus of %s", x.typ))
		}
		return tgVal{typ: x.typ, node: node("neg", x.node.T, nil, x.node)}
	case t.punct("+"):
		c.i++
		return c.expr(precUnary)
	case t.punct("~"):
		c.i++
		x := c.expr(precUnary)
		return tgVal{typ: "INT64", node: node("bitnot", "i", nil, c.toInt(x).node)}
	}
	return c.postfix(c.primary())
}

func (c *tgCompiler) postfix(v tgVal) tgVal {
	for c.peek().punct("[") {
		c.i++
		mode := "OFFSET"
		var idx tgVal
		if t := c.peek(); (t.is("OFFSET") || t.is("ORDINAL") || t.is("SAFE_OFFSET") || t.is("SAFE_ORDINAL")) && c.peekAt(1).punct("(") {
			mode = strings.ToUpper(t.text)
			c.i += 2
			idx = c.expr(0)
			c.expectPunct(")")
		} else {
			idx = c.expr(0)
		}
		c.expectPunct("]")
		safe := strings.HasPrefix(mode, "SAFE_")
		base := 0
		if strings.HasSuffix(mode, "ORDINAL") {
			base = 1
		}
		switch {
		case v.isArr && idx.isLit:
			k := int(idx.lit.(int64)) - base
			if k < 0 || k >= len(v.arr) {
				if !safe {
					panic(&tgFail{400, fmt.Sprintf("Array index %d is out of bounds", k+base)})
				}
				v = litVal(v.elem, nil)
			} else {
				v = v.arr[k]
			}
		case v.typ == "ARRAY<STRING>" && v.node.Op == "split":
			v = tgVal{typ: "STRING", node: node("split_get", "s", map[string]any{"base": base}, v.node, c.toInt(idx).node)}
		default:
			panic(tgUnsupported("an array subscript of an array that is not constant or SPLIT's"))
		}
	}
	return v
}

// number reads a numeric literal from the tokens: the lexer splits 1.5e-3
// at its punctuation.
func (c *tgCompiler) number() (tgVal, bool) {
	t := c.peek()
	startsDigit := t.kind == tokWord && t.text[0] >= '0' && t.text[0] <= '9'
	startsDot := t.punct(".") && c.peekAt(1).kind == tokWord && c.peekAt(1).pos == t.end && c.peekAt(1).text[0] >= '0' && c.peekAt(1).text[0] <= '9'
	if !startsDigit && !startsDot {
		return tgVal{}, false
	}
	start, end := t.pos, t.end
	c.i++
	for {
		n := c.peek()
		if n.pos != end {
			break
		}
		prev := c.src[start:end]
		last := prev[len(prev)-1]
		switch {
		case n.punct("."):
		case n.kind == tokWord && (n.text[0] >= '0' && n.text[0] <= '9' || strings.HasSuffix(prev, ".")):
		case (n.punct("+") || n.punct("-")) && (last == 'e' || last == 'E') && !strings.HasPrefix(strings.ToLower(prev), "0x"):
		default:
			goto done
		}
		end = n.end
		c.i++
	}
done:
	s := c.src[start:end]
	ls := strings.ToLower(s)
	if strings.HasPrefix(ls, "0x") {
		v, err := strconv.ParseInt(s[2:], 16, 64)
		if err != nil {
			panic(&tgFail{400, "Invalid integer literal " + s})
		}
		return litVal("INT64", v), true
	}
	if !strings.ContainsAny(ls, ".e") {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			panic(&tgFail{400, "Invalid integer literal " + s})
		}
		return litVal("INT64", v), true
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(&tgFail{400, "Invalid floating point literal " + s})
	}
	return litVal("FLOAT64", v), true
}

func (c *tgCompiler) stringLit(t token) string {
	s := t.text
	low := strings.ToLower(s)
	raw := false
	for len(s) > 0 && (low[0] == 'r' || low[0] == 'b') {
		if low[0] == 'r' {
			raw = true
		}
		s, low = s[1:], low[1:]
	}
	if raw {
		q := 1
		if strings.HasPrefix(s, `'''`) || strings.HasPrefix(s, `"""`) {
			q = 3
		}
		return s[q : len(s)-q]
	}
	if strings.HasPrefix(s, `'''`) || strings.HasPrefix(s, `"""`) {
		return unescapeSQL(s[3 : len(s)-3])
	}
	return unescapeSQL(s[1 : len(s)-1])
}

// unescapeSQL undoes GoogleSQL's string escapes.
func unescapeSQL(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for k := 0; k < len(s); k++ {
		if s[k] != '\\' || k+1 >= len(s) {
			b.WriteByte(s[k])
			continue
		}
		k++
		switch s[k] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(s[k])
		}
	}
	return b.String()
}

var tgTypeNames = map[string]bool{"INT64": true, "FLOAT64": true, "NUMERIC": true, "BIGNUMERIC": true, "STRING": true, "BYTES": true, "BOOL": true,
	"DATE": true, "DATETIME": true, "TIME": true, "TIMESTAMP": true, "INTEGER": true, "FLOAT": true, "BOOLEAN": true, "DECIMAL": true, "BIGDECIMAL": true,
	"INT": true, "SMALLINT": true, "BIGINT": true, "TINYINT": true, "BYTEINT": true}

func normType(t string) string {
	switch t = strings.ToUpper(t); t {
	case "INTEGER", "INT", "SMALLINT", "BIGINT", "TINYINT", "BYTEINT":
		return "INT64"
	case "FLOAT":
		return "FLOAT64"
	case "BOOLEAN":
		return "BOOL"
	case "DECIMAL":
		return "NUMERIC"
	case "BIGDECIMAL":
		return "BIGNUMERIC"
	}
	return t
}

// typeName reads a type name: a scalar one, or ARRAY<scalar>.
func (c *tgCompiler) typeName() string {
	t := c.next()
	if t.is("ARRAY") && c.acceptPunct("<") {
		e := c.typeName()
		c.expectPunct(">")
		return "ARRAY<" + e + ">"
	}
	if t.kind != tokWord || !tgTypeNames[strings.ToUpper(t.text)] {
		panic(tgUnsupported("the type %s", t.text))
	}
	// Parameterized types: NUMERIC(10, 2), STRING(10).
	if c.peek().punct("(") {
		depth := 0
		for {
			x := c.next()
			if x.punct("(") {
				depth++
			} else if x.punct(")") {
				depth--
				if depth == 0 {
					break
				}
			}
		}
	}
	return normType(t.text)
}

func (c *tgCompiler) lookupName(name string) (tgVal, bool) {
	l := strings.ToLower(name)
	for k := len(c.env) - 1; k >= 0; k-- {
		if v, ok := c.env[k][l]; ok {
			return v, true
		}
	}
	v, ok := c.cols[l]
	return v, ok
}

func (c *tgCompiler) primary() tgVal {
	if v, ok := c.number(); ok {
		return v
	}
	t := c.peek()
	switch {
	case t.kind == tokString:
		c.i++
		s := c.stringLit(t)
		if strings.HasPrefix(strings.ToLower(t.text), "b") || strings.HasPrefix(strings.ToLower(t.text), "rb") {
			return litVal("BYTES", s)
		}
		return litVal("STRING", s)
	case t.punct("("):
		c.i++
		if c.peek().is("SELECT") {
			v := c.subquery()
			c.expectPunct(")")
			return v
		}
		v := c.expr(0)
		c.expectPunct(")")
		return v
	case t.punct("["):
		c.i++
		if c.peek().is("STRUCT") {
			panic(tgArrayOut)
		}
		return c.arrayLit("")
	case t.kind == tokQuoted:
		c.i++
		return c.column(t.text)
	case t.kind != tokWord:
		panic(tgUnsupported("%q", c.rest()))
	}
	u := strings.ToUpper(t.text)
	switch u {
	case "TRUE", "FALSE":
		c.i++
		return litVal("BOOL", u == "TRUE")
	case "NULL":
		c.i++
		return tgVal{typ: "NULL", node: node("null", "f", nil), isLit: true}
	case "CASE":
		c.i++
		return c.caseExpr()
	case "CAST", "SAFE_CAST":
		c.i++
		c.expectPunct("(")
		x := c.expr(0)
		c.expect("AS")
		to := c.typeName()
		format := ""
		if c.accept("FORMAT") {
			panic(tgUnsupported("CAST ... FORMAT"))
		}
		_ = format
		c.expectPunct(")")
		return c.cast(x, to, u == "SAFE_CAST")
	case "EXTRACT":
		c.i++
		c.expectPunct("(")
		part := strings.ToUpper(c.next().text)
		if c.acceptPunct("(") {
			part += "(" + strings.ToUpper(c.next().text) + ")"
			c.expectPunct(")")
		}
		c.expect("FROM")
		x := c.expr(0)
		zone := ""
		if c.accept("AT") {
			c.expect("TIME")
			c.expect("ZONE")
			z := c.expr(0)
			zone = c.constString(z, "the time zone")
		}
		c.expectPunct(")")
		return c.extract(part, x, zone)
	case "ARRAY":
		c.i++
		elem := ""
		if c.acceptPunct("<") {
			elem = c.typeName()
			c.expectPunct(">")
		}
		if c.acceptPunct("[") {
			return c.arrayLit(elem)
		}
		panic(tgArrayOut)
	case "STRUCT":
		panic(&tgFail{400, "Model export does not support a STRUCT in the TRANSFORM output: expand its fields into separate columns"})
	case "INTERVAL":
		c.i++
		n := c.expr(precAdd)
		unit := strings.ToUpper(c.next().text)
		return tgVal{typ: "INTERVAL", node: c.toInt(n).node, lit: unit}
	case "DATE", "DATETIME", "TIME", "TIMESTAMP", "NUMERIC", "BIGNUMERIC", "JSON":
		if n := c.peekAt(1); n.kind == tokString {
			c.i += 2
			return c.cast(litVal("STRING", c.stringLit(n)), u, false)
		}
	}
	// A function call, maybe of a dotted name (SAFE.PARSE_DATE).
	name := u
	k := 1
	for c.peekAt(k).punct(".") && c.peekAt(k+1).kind == tokWord && c.peekAt(k+2).punct("(") {
		name += "." + strings.ToUpper(c.peekAt(k+1).text)
		k += 2
	}
	if c.peekAt(k).punct("(") {
		c.i += k + 1
		return c.call(name)
	}
	c.i++
	// table.column: the last part names the column.
	col := t.text
	for c.peek().punct(".") && (c.peekAt(1).kind == tokWord || c.peekAt(1).kind == tokQuoted) {
		c.i++
		col = c.next().text
	}
	return c.column(col)
}

func (c *tgCompiler) column(name string) tgVal {
	v, ok := c.lookupName(name)
	if !ok {
		panic(&tgFail{400, "cloudburrow: the TRANSFORM reads " + name + ", which is not one of the model's input columns"})
	}
	if v.node == nil && !v.isArr && strings.HasPrefix(v.typ, "ARRAY") {
		// An ARRAY input: the functions the page lists for a TRANSFORM
		// that take one (ML.MULTI_HOT_ENCODER, ML.NGRAMS, ML.NORMALIZER)
		// give an ARRAY, and the page lists no other function of one.
		panic(&tgFail{400, "an ARRAY input"})
	}
	return v
}

// structIntFloat is ARRAY<STRUCT<INT64, FLOAT64>>'s element type, its
// fields named.
var structIntFloat = regexp.MustCompile(`^STRUCT<\s*(\w+\s+)?(INT64|INTEGER)\s*,\s*(\w+\s+)?(FLOAT64|FLOAT)\s*>$`)

func (c *tgCompiler) arrayLit(elem string) tgVal {
	var items []tgVal
	for !c.acceptPunct("]") {
		items = append(items, c.expr(0))
		if !c.acceptPunct(",") {
			c.expectPunct("]")
			break
		}
	}
	for _, it := range items {
		if !it.isLit {
			panic(tgUnsupported("an array of values that are not constants: an ARRAY is not supported as post-transformed data"))
		}
		if elem == "" && it.typ != "NULL" {
			elem = it.typ
		}
		if elem == "INT64" && it.typ == "FLOAT64" {
			elem = "FLOAT64"
		}
	}
	if elem == "" {
		elem = "INT64"
	}
	for k, it := range items {
		items[k] = c.coerceLit(it, elem)
	}
	return tgVal{typ: "ARRAY<" + elem + ">", isArr: true, arr: items, elem: elem, isLit: true}
}

// coerceLit is constant v as type t.
func (c *tgCompiler) coerceLit(v tgVal, t string) tgVal {
	if v.typ == t {
		return v
	}
	if v.lit == nil {
		return litVal(t, nil)
	}
	if t == "FLOAT64" || t == "NUMERIC" || t == "BIGNUMERIC" {
		if i, ok := v.lit.(int64); ok {
			return litVal(t, float64(i))
		}
		if f, ok := v.lit.(float64); ok {
			return litVal(t, f)
		}
	}
	return c.cast(v, t, false)
}

func (c *tgCompiler) caseExpr() tgVal {
	var subject *tgVal
	if !c.peek().is("WHEN") {
		s := c.expr(0)
		subject = &s
	}
	var conds, results []tgVal
	for c.accept("WHEN") {
		w := c.expr(0)
		c.expect("THEN")
		r := c.expr(0)
		if subject != nil {
			w = c.compare("=", *subject, w)
		}
		conds, results = append(conds, c.boolOf(w)), append(results, r)
	}
	var els tgVal
	if c.accept("ELSE") {
		els = c.expr(0)
	} else {
		els = tgVal{typ: "NULL", node: node("null", "f", nil), isLit: true}
	}
	c.expect("END")
	return c.caseOf(conds, results, els)
}

// caseOf is CASE WHEN conds[k] THEN results[k] ... ELSE els END.
func (c *tgCompiler) caseOf(conds, results []tgVal, els tgVal) tgVal {
	all := append(append([]tgVal(nil), results...), els)
	t := c.unify(all...)
	out := c.as(els, t)
	for k := len(conds) - 1; k >= 0; k-- {
		out = tgVal{typ: t, node: node("if", tfDtypeOf(t), nil, conds[k].node, c.as(results[k], t).node, out.node)}
	}
	return out
}

// unify is the common supertype of vs, GoogleSQL's: INT64 and FLOAT64
// unify to FLOAT64; NULL and ERROR take the other's.
func (c *tgCompiler) unify(vs ...tgVal) string {
	t := ""
	for _, v := range vs {
		switch {
		case v.typ == "NULL" || v.typ == "ANY":
		case t == "":
			t = v.typ
		case t == v.typ:
		case isNumericType(t) && isNumericType(v.typ):
			if t == "FLOAT64" || v.typ == "FLOAT64" {
				t = "FLOAT64"
			} else if t == "INT64" {
				t = v.typ
			}
		case v.isLit && v.typ == "STRING":
			// a string literal coerces to a date or time type
		case t == "STRING" && vs[0].isLit:
			t = v.typ
		default:
			panic(&tgFail{400, fmt.Sprintf("No matching signature: the values have types %s and %s", t, v.typ)})
		}
	}
	if t == "" {
		t = "INT64"
	}
	return t
}

// as is v of type t: a NULL or ERROR typed, an INT64 widened, a literal
// coerced.
func (c *tgCompiler) as(v tgVal, t string) tgVal {
	switch {
	case v.typ == t:
		return v
	case v.typ == "NULL":
		return litVal(t, nil)
	case v.typ == "ANY":
		n := *v.node
		n.T = tfDtypeOf(t)
		return tgVal{typ: t, node: &n}
	case isNumericType(v.typ) && isNumericType(t):
		if tfDtypeOf(v.typ) == tfDtypeOf(t) {
			return tgVal{typ: t, node: v.node, lit: v.lit, isLit: v.isLit}
		}
		if v.isLit {
			return c.coerceLit(v, t)
		}
		return tgVal{typ: t, node: node("cast", tfDtypeOf(t), nil, v.node)}
	case v.isLit && v.typ == "STRING":
		return c.cast(v, t, false)
	}
	panic(&tgFail{400, fmt.Sprintf("No matching signature: %s cannot be coerced to %s", v.typ, t)})
}

func (c *tgCompiler) boolOf(v tgVal) tgVal {
	if v.typ == "BOOL" {
		return v
	}
	return c.as(v, "BOOL")
}

func (c *tgCompiler) not(x tgVal) tgVal {
	x = c.boolOf(x)
	return tgVal{typ: "BOOL", node: node("not", "b", nil, x.node)}
}

func (c *tgCompiler) and(a, b tgVal) tgVal {
	return tgVal{typ: "BOOL", node: node("and", "b", nil, c.boolOf(a).node, c.boolOf(b).node)}
}

func (c *tgCompiler) or(a, b tgVal) tgVal {
	return tgVal{typ: "BOOL", node: node("or", "b", nil, c.boolOf(a).node, c.boolOf(b).node)}
}

func (c *tgCompiler) compare(op string, a, b tgVal) tgVal {
	t := c.unify(a, b)
	a, b = c.as(a, t), c.as(b, t)
	name := map[string]string{"=": "eq", "!=": "ne", "<": "lt", "<=": "le", ">": "gt", ">=": "ge"}[op]
	return tgVal{typ: "BOOL", node: node("cmp", "b", map[string]any{"op": name}, a.node, b.node)}
}

func (c *tgCompiler) toInt(v tgVal) tgVal {
	if v.typ == "INT64" {
		return v
	}
	if v.typ == "NULL" {
		return litVal("INT64", nil)
	}
	return c.cast(v, "INT64", false)
}

func (c *tgCompiler) toFloat(v tgVal) tgVal {
	if v.typ == "FLOAT64" {
		return v
	}
	return c.as(v, "FLOAT64")
}

func (c *tgCompiler) str(v tgVal) tgVal {
	if v.typ == "STRING" || v.typ == "BYTES" {
		return v
	}
	if v.typ == "NULL" {
		return litVal("STRING", nil)
	}
	panic(&tgFail{400, "No matching signature: a STRING is expected; the value is " + v.typ})
}

func (c *tgCompiler) binary(op string, a, b tgVal) tgVal {
	switch op {
	case "AND":
		return c.and(a, b)
	case "OR":
		return c.or(a, b)
	case "=", "!=", "<", "<=", ">", ">=":
		return c.compare(op, a, b)
	case "||":
		return c.concat([]tgVal{a, b})
	case "&", "|", "^", "<<", ">>":
		a, b = c.toInt(a), c.toInt(b)
		name := map[string]string{"&": "bitand", "|": "bitor", "^": "bitxor", "<<": "shl", ">>": "shr"}[op]
		return tgVal{typ: "INT64", node: node(name, "i", nil, a.node, b.node)}
	}
	if b.typ == "INTERVAL" && (op == "+" || op == "-") {
		fn := map[string]string{"DATE": "DATE", "DATETIME": "DATETIME", "TIMESTAMP": "TIMESTAMP", "TIME": "TIME"}[a.typ]
		if fn == "" {
			panic(tgUnsupported("%s %s INTERVAL", a.typ, op))
		}
		return c.dateAdd(a.typ, a, b, op == "-")
	}
	t := c.unify(a, b)
	if !isNumericType(t) {
		panic(&tgFail{400, fmt.Sprintf("No matching signature for operator %s for argument types: %s, %s", op, a.typ, b.typ)})
	}
	if op == "/" {
		t = "FLOAT64"
	}
	a, b = c.as(a, t), c.as(b, t)
	name := map[string]string{"+": "add", "-": "sub", "*": "mul", "/": "div"}[op]
	return tgVal{typ: t, node: node(name, tfDtypeOf(t), nil, a.node, b.node)}
}

func (c *tgCompiler) concat(args []tgVal) tgVal {
	var ns []*tgNode
	for _, a := range args {
		ns = append(ns, c.str(a).node)
	}
	return tgVal{typ: "STRING", node: node("concat", "s", nil, ns...)}
}

func (c *tgCompiler) constString(v tgVal, what string) string {
	s, ok := v.lit.(string)
	if !v.isLit || !ok {
		panic(tgUnsupported("%s that is not a string literal", what))
	}
	return s
}

func (c *tgCompiler) constInt(v tgVal, what string) int64 {
	s, ok := v.lit.(int64)
	if !v.isLit || !ok {
		panic(tgUnsupported("%s that is not an integer literal", what))
	}
	return s
}

// cast is CAST(x AS to), or SAFE_CAST.
func (c *tgCompiler) cast(x tgVal, to string, safe bool) tgVal {
	to = normType(to)
	if x.typ == to {
		return x
	}
	if x.typ == "NULL" {
		return litVal(to, nil)
	}
	if strings.HasPrefix(to, "ARRAY") {
		panic(tgUnsupported("CAST to %s", to))
	}
	a := map[string]any{"from": x.typ, "to": to, "safe": safe}
	if x.isLit {
		if v, ok := foldCast(x, to); ok {
			return v
		}
	}
	return tgVal{typ: to, node: node("cast", tfDtypeOf(to), a, x.node)}
}

// foldCast casts constant x, when Go computes it as GoogleSQL does.
func foldCast(x tgVal, to string) (tgVal, bool) {
	switch v := x.lit.(type) {
	case int64:
		switch to {
		case "FLOAT64", "NUMERIC", "BIGNUMERIC":
			return litVal(to, float64(v)), true
		case "STRING":
			return litVal(to, strconv.FormatInt(v, 10)), true
		}
	case float64:
		switch to {
		case "FLOAT64", "NUMERIC", "BIGNUMERIC":
			return litVal(to, v), true
		case "INT64":
			if math.IsNaN(v) || math.Abs(v) > 9.2e18 {
				return tgVal{}, false
			}
			return litVal(to, int64(math.Round(v))), true
		}
	case string:
		switch to {
		case "INT64":
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return litVal(to, n), true
			}
		case "FLOAT64", "NUMERIC", "BIGNUMERIC":
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return litVal(to, f), true
			}
		case "STRING", "BYTES":
			return litVal(to, v), true
		}
	case bool:
		if to == "STRING" {
			return litVal(to, strconv.FormatBool(v)), true
		}
	}
	return tgVal{}, false
}

// args reads a call's arguments, up to its ).
func (c *tgCompiler) args() []tgVal {
	var out []tgVal
	if c.acceptPunct(")") {
		return out
	}
	for {
		out = append(out, c.expr(0))
		if !c.acceptPunct(",") {
			break
		}
	}
	c.expectPunct(")")
	return out
}

var tgMath1 = map[string]string{"ABS": "abs", "SIGN": "sign", "SQRT": "sqrt", "EXP": "exp", "LN": "ln", "LOG10": "log10",
	"FLOOR": "floor", "CEIL": "ceil", "CEILING": "ceil", "SIN": "sin", "COS": "cos", "TAN": "tan", "ASIN": "asin", "ACOS": "acos",
	"ATAN": "atan", "SINH": "sinh", "COSH": "cosh", "TANH": "tanh", "ASINH": "asinh", "ACOSH": "acosh", "ATANH": "atanh",
	"CBRT": "cbrt", "COT": "cot", "COTH": "coth", "CSC": "csc", "CSCH": "csch", "SEC": "sec", "SECH": "sech"}

func (c *tgCompiler) nargs(name string, got []tgVal, lo, hi int) {
	if len(got) < lo || len(got) > hi {
		panic(&tgFail{400, fmt.Sprintf("No matching signature for function %s with %d arguments", name, len(got))})
	}
}

func (c *tgCompiler) call(name string) tgVal {
	safe := false
	if strings.HasPrefix(name, "SAFE.") {
		safe, name = true, strings.TrimPrefix(name, "SAFE.")
	}
	// Functions whose arguments are not plain expressions.
	switch name {
	case "DATE_ADD", "DATE_SUB", "DATETIME_ADD", "DATETIME_SUB", "TIME_ADD", "TIME_SUB", "TIMESTAMP_ADD", "TIMESTAMP_SUB":
		x := c.expr(0)
		c.expectPunct(",")
		iv := c.expr(0)
		c.expectPunct(")")
		if iv.typ != "INTERVAL" {
			panic(&tgFail{400, name + " takes an INTERVAL"})
		}
		typ := strings.SplitN(name, "_", 2)[0]
		return c.dateAdd(typ, c.as(x, typ), iv, strings.HasSuffix(name, "_SUB"))
	case "DATE_DIFF", "DATETIME_DIFF", "TIME_DIFF", "TIMESTAMP_DIFF":
		a := c.expr(0)
		c.expectPunct(",")
		b := c.expr(0)
		c.expectPunct(",")
		unit := c.partName()
		c.expectPunct(")")
		typ := strings.SplitN(name, "_", 2)[0]
		return tgVal{typ: "INT64", node: node("date_diff", "i", map[string]any{"type": typ, "unit": unit}, c.as(a, typ).node, c.as(b, typ).node)}
	case "DATE_TRUNC", "DATETIME_TRUNC", "TIME_TRUNC", "TIMESTAMP_TRUNC":
		a := c.expr(0)
		c.expectPunct(",")
		unit := c.partName()
		var zn *tgZone
		if c.acceptPunct(",") {
			z := c.expr(0)
			zn = c.zoneOf(c.constString(z, "the time zone"))
		}
		c.expectPunct(")")
		typ := strings.SplitN(name, "_", 2)[0]
		if typ == "TIMESTAMP" && zn != nil {
			// Truncated in the zone's civil time, as a DATETIME is.
			local := toLocal(c.as(a, typ).node, zn)
			return tgVal{typ: typ, node: fromLocal(node("date_trunc", "i", map[string]any{"type": "DATETIME", "unit": unit}, local), zn)}
		}
		return tgVal{typ: typ, node: node("date_trunc", "i", map[string]any{"type": typ, "unit": unit}, c.as(a, typ).node)}
	}
	args := c.args()
	switch name {
	case "IF":
		c.nargs(name, args, 3, 3)
		return c.caseOf([]tgVal{c.boolOf(args[0])}, []tgVal{args[1]}, args[2])
	case "IFNULL", "COALESCE":
		if name == "IFNULL" {
			c.nargs(name, args, 2, 2)
		}
		t := c.unify(args...)
		var ns []*tgNode
		for _, a := range args {
			ns = append(ns, c.as(a, t).node)
		}
		return tgVal{typ: t, node: node("coalesce", tfDtypeOf(t), nil, ns...)}
	case "NULLIF":
		c.nargs(name, args, 2, 2)
		t := c.unify(args...)
		return c.caseOf([]tgVal{c.compare("=", args[0], args[1])}, []tgVal{litVal(t, nil)}, c.as(args[0], t))
	case "ERROR":
		return tgVal{typ: "ANY", node: node("null", "f", nil)}
	case "SAFE_DIVIDE":
		c.nargs(name, args, 2, 2)
		a, b := c.toFloat(args[0]), c.toFloat(args[1])
		return tgVal{typ: "FLOAT64", node: node("safe_div", "f", nil, a.node, b.node)}
	case "IEEE_DIVIDE":
		c.nargs(name, args, 2, 2)
		return tgVal{typ: "FLOAT64", node: node("ieee_div", "f", nil, c.toFloat(args[0]).node, c.toFloat(args[1]).node)}
	case "DIV":
		c.nargs(name, args, 2, 2)
		return tgVal{typ: "INT64", node: node("intdiv", "i", nil, c.toInt(args[0]).node, c.toInt(args[1]).node)}
	case "MOD":
		c.nargs(name, args, 2, 2)
		t := c.unify(args...)
		if t == "FLOAT64" {
			panic(&tgFail{400, "No matching signature for function MOD for argument types: FLOAT64"})
		}
		return tgVal{typ: t, node: node("mod", tfDtypeOf(t), nil, c.as(args[0], t).node, c.as(args[1], t).node)}
	case "GREATEST", "LEAST":
		t := c.unify(args...)
		var ns []*tgNode
		for _, a := range args {
			ns = append(ns, c.as(a, t).node)
		}
		return tgVal{typ: t, node: node(strings.ToLower(name), tfDtypeOf(t), nil, ns...)}
	case "POW", "POWER", "ATAN2", "LOG":
		lo := 2
		if name == "LOG" {
			lo = 1
		}
		c.nargs(name, args, lo, 2)
		if name == "LOG" && len(args) == 1 {
			return tgVal{typ: "FLOAT64", node: node("math", "f", map[string]any{"fn": "ln"}, c.toFloat(args[0]).node)}
		}
		fn := map[string]string{"POW": "pow", "POWER": "pow", "ATAN2": "atan2", "LOG": "log"}[name]
		return tgVal{typ: "FLOAT64", node: node("math2", "f", map[string]any{"fn": fn}, c.toFloat(args[0]).node, c.toFloat(args[1]).node)}
	case "ROUND", "TRUNC":
		c.nargs(name, args, 1, 2)
		digits := int64(0)
		if len(args) == 2 {
			digits = c.constInt(args[1], name+"'s digits")
		}
		t := args[0].typ
		if t == "INT64" {
			t = "FLOAT64"
		}
		return tgVal{typ: t, node: node(strings.ToLower(name), tfDtypeOf(t), map[string]any{"digits": digits}, c.as(args[0], t).node)}
	case "IS_INF", "IS_NAN":
		c.nargs(name, args, 1, 1)
		return tgVal{typ: "BOOL", node: node(strings.ToLower(name), "b", nil, c.toFloat(args[0]).node)}
	case "FARM_FINGERPRINT":
		c.nargs(name, args, 1, 1)
		return tgVal{typ: "INT64", node: node("farm_fingerprint", "i", nil, c.str(args[0]).node)}
	case "CONCAT":
		return c.concat(args)
	case "LOWER", "UPPER":
		c.nargs(name, args, 1, 1)
		s := c.str(args[0])
		return tgVal{typ: s.typ, node: node(strings.ToLower(name), "s", nil, s.node)}
	case "LENGTH", "CHAR_LENGTH", "CHARACTER_LENGTH", "BYTE_LENGTH":
		c.nargs(name, args, 1, 1)
		s := c.str(args[0])
		unit := "char"
		if s.typ == "BYTES" || name == "BYTE_LENGTH" {
			unit = "byte"
		}
		return tgVal{typ: "INT64", node: node("length", "i", map[string]any{"unit": unit}, s.node)}
	case "SUBSTR", "SUBSTRING":
		c.nargs(name, args, 2, 3)
		s := c.str(args[0])
		ns := []*tgNode{s.node, c.toInt(args[1]).node}
		if len(args) == 3 {
			ns = append(ns, c.toInt(args[2]).node)
		}
		return tgVal{typ: s.typ, node: node("substr", "s", map[string]any{"unit": unitOf(s)}, ns...)}
	case "LEFT", "RIGHT":
		c.nargs(name, args, 2, 2)
		s := c.str(args[0])
		return tgVal{typ: s.typ, node: node(strings.ToLower(name), "s", map[string]any{"unit": unitOf(s)}, s.node, c.toInt(args[1]).node)}
	case "TRIM", "LTRIM", "RTRIM":
		c.nargs(name, args, 1, 2)
		s := c.str(args[0])
		chars := ""
		if len(args) == 2 {
			chars = c.constString(args[1], name+"'s characters")
		}
		pat := `\s`
		if len(args) == 2 {
			var b strings.Builder
			for _, r := range chars {
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
			pat = "[" + strings.ReplaceAll(b.String(), "]", `\]`) + "]"
			if chars == "" {
				return s
			}
		}
		re := map[string]string{"TRIM": "^(?:" + pat + ")+|(?:" + pat + ")+$", "LTRIM": "^(?:" + pat + ")+", "RTRIM": "(?:" + pat + ")+$"}[name]
		return tgVal{typ: s.typ, node: node("regex_replace", "s", map[string]any{"pattern": "(?s)" + re, "rewrite": ""}, s.node)}
	case "REGEXP_REPLACE":
		c.nargs(name, args, 3, 3)
		s := c.str(args[0])
		pat := c.constString(args[1], "REGEXP_REPLACE's regular expression")
		rep := c.constString(args[2], "REGEXP_REPLACE's replacement")
		if _, err := regexp.Compile(pat); err != nil {
			panic(&tgFail{400, "Cannot parse regular expression: " + err.Error()})
		}
		return tgVal{typ: s.typ, node: node("regex_replace", "s", map[string]any{"pattern": pat, "rewrite": rep}, s.node)}
	case "REGEXP_CONTAINS":
		c.nargs(name, args, 2, 2)
		pat := c.constString(args[1], "REGEXP_CONTAINS's regular expression")
		return tgVal{typ: "BOOL", node: node("regex_full_match", "b", map[string]any{"pattern": "(?s).*(?:" + pat + ").*"}, c.str(args[0]).node)}
	case "STARTS_WITH", "ENDS_WITH":
		c.nargs(name, args, 2, 2)
		p := regexp.QuoteMeta(c.constString(args[1], name+"'s prefix"))
		re := "(?s)" + p + ".*"
		if name == "ENDS_WITH" {
			re = "(?s).*" + p
		}
		return tgVal{typ: "BOOL", node: node("regex_full_match", "b", map[string]any{"pattern": re}, c.str(args[0]).node)}
	case "SPLIT":
		c.nargs(name, args, 1, 2)
		sep := ","
		if len(args) == 2 {
			sep = c.constString(args[1], "SPLIT's delimiter")
		}
		return tgVal{typ: "ARRAY<STRING>", node: node("split", "s", map[string]any{"sep": sep}, c.str(args[0]).node)}
	case "ARRAY_LENGTH":
		c.nargs(name, args, 1, 1)
		if args[0].isArr {
			return litVal("INT64", int64(len(args[0].arr)))
		}
		if args[0].node != nil && args[0].node.Op == "split" {
			return tgVal{typ: "INT64", node: node("split_len", "i", nil, args[0].node)}
		}
		panic(tgUnsupported("ARRAY_LENGTH of an array that is not constant or SPLIT's"))
	case "STRING":
		c.nargs(name, args, 1, 2)
		var zn *tgZone
		if len(args) == 2 {
			zn = c.zoneOf(c.constString(args[1], "the time zone"))
		}
		return c.formatDate(c.as(args[0], "TIMESTAMP"), "%Y-%m-%d %H:%M:%E*S%\x00", zn)
	case "DATE", "DATETIME", "TIME", "TIMESTAMP":
		return c.dateCtor(name, args)
	case "UNIX_SECONDS", "UNIX_MILLIS", "UNIX_MICROS", "UNIX_DATE":
		c.nargs(name, args, 1, 1)
		if name == "UNIX_DATE" {
			return tgVal{typ: "INT64", node: c.as(args[0], "DATE").node}
		}
		div := map[string]int64{"UNIX_SECONDS": 1000000, "UNIX_MILLIS": 1000, "UNIX_MICROS": 1}[name]
		return tgVal{typ: "INT64", node: node("floordiv_const", "i", map[string]any{"d": div}, c.as(args[0], "TIMESTAMP").node)}
	case "TIMESTAMP_SECONDS", "TIMESTAMP_MILLIS", "TIMESTAMP_MICROS", "DATE_FROM_UNIX_DATE":
		c.nargs(name, args, 1, 1)
		if name == "DATE_FROM_UNIX_DATE" {
			return tgVal{typ: "DATE", node: c.toInt(args[0]).node}
		}
		m := map[string]int64{"TIMESTAMP_SECONDS": 1000000, "TIMESTAMP_MILLIS": 1000, "TIMESTAMP_MICROS": 1}[name]
		return tgVal{typ: "TIMESTAMP", node: node("mul", "i", nil, c.toInt(args[0]).node, litVal("INT64", m).node)}
	case "FORMAT_DATE", "FORMAT_DATETIME", "FORMAT_TIME", "FORMAT_TIMESTAMP":
		c.nargs(name, args, 2, 3)
		var zn *tgZone
		if len(args) == 3 {
			zn = c.zoneOf(c.constString(args[2], "the time zone"))
		}
		typ := strings.TrimPrefix(name, "FORMAT_")
		return c.formatDate(c.as(args[1], typ), c.constString(args[0], name+"'s format"), zn)
	case "PARSE_DATE", "PARSE_DATETIME", "PARSE_TIME", "PARSE_TIMESTAMP":
		c.nargs(name, args, 2, 3)
		var zn *tgZone
		if len(args) == 3 {
			zn = c.zoneOf(c.constString(args[2], "the time zone"))
		}
		typ := strings.TrimPrefix(name, "PARSE_")
		return c.parseDate(typ, c.str(args[1]), c.constString(args[0], name+"'s format"), safe, zn)
	}
	if fn, ok := tgMath1[name]; ok {
		c.nargs(name, args, 1, 1)
		x := args[0]
		if (name == "ABS" || name == "SIGN") && x.typ == "INT64" {
			return tgVal{typ: "INT64", node: node("math", "i", map[string]any{"fn": fn}, x.node)}
		}
		if (name == "FLOOR" || name == "CEIL" || name == "CEILING") && x.typ == "INT64" {
			x = c.toFloat(x)
		}
		t := "FLOAT64"
		if x.typ == "NUMERIC" || x.typ == "BIGNUMERIC" {
			t = x.typ
		}
		return tgVal{typ: t, node: node("math", "f", map[string]any{"fn": fn}, c.toFloat(x).node)}
	}
	if strings.HasPrefix(name, "ML.") {
		panic(tgUnsupported("%s, which CloudBurrow does not write out as SQL", name))
	}
	// Not one of the functions "Export models" lists for a TRANSFORM (all
	// of which compile), nor one ML preprocessing's SQL uses.
	panic(&tgFail{400, "Model export does not support the function " + name + " in a TRANSFORM clause: the TRANSFORM can use the SQL functions listed in \"Export models\" (https://cloud.google.com/bigquery/docs/exporting-models)"})
}

func unitOf(s tgVal) string {
	if s.typ == "BYTES" {
		return "byte"
	}
	return "char"
}

// partName reads a date part: DAY, WEEK(MONDAY), ...
func (c *tgCompiler) partName() string {
	p := strings.ToUpper(c.next().text)
	if c.acceptPunct("(") {
		p += "(" + strings.ToUpper(c.next().text) + ")"
		c.expectPunct(")")
	}
	return p
}

// tgZone is a time zone as the graph computes it: the UTC instants (in
// microseconds) each of its offsets starts at, the offsets (microseconds
// east of UTC) and their abbreviations, and the local times each starts
// at as the zone's local-to-UTC conversion reads them.
type tgZone struct {
	starts, lstarts, offs []int64
	abbrs                 []string
}

// zoneOf is time zone z, a tz database name or an offset from UTC, as
// BigQuery takes them; nil is UTC. A named zone's offsets are written out
// from the tz database (Go's), every transition from year 1 to 9999, so
// the graph needs no database of its own.
func (c *tgCompiler) zoneOf(z string) *tgZone {
	if !validTimeZone(z) {
		panic(&tgFail{400, "Invalid time zone: " + z})
	}
	if m := offsetZone.FindStringSubmatch(z); m != nil {
		hh, _ := strconv.Atoi(m[2])
		mm, _ := strconv.Atoi(m[4])
		off := int64(hh*3600+mm*60) * 1000000
		if m[1] == "-" {
			off = -off
		}
		if off == 0 {
			return nil
		}
		return &tgZone{starts: []int64{math.MinInt64}, lstarts: []int64{math.MinInt64}, offs: []int64{off},
			abbrs: []string{"UTC" + engineZone(z)}}
	}
	loc, err := time.LoadLocation(z)
	if err != nil {
		panic(&tgFail{400, "Invalid time zone: " + z})
	}
	zn := &tgZone{}
	add := func(start int64, off int, name string) {
		o := int64(off) * 1000000
		n := len(zn.offs)
		if n > 0 && zn.offs[n-1] == o && zn.abbrs[n-1] == name {
			return // a transition that changes nothing (its DST flag only)
		}
		ls := int64(math.MinInt64)
		if n > 0 {
			// A local time in the gap or the fold of a transition takes
			// the offset before it: absl's TimeZone::At(...).pre, which
			// GoogleSQL's conversions use
			// (UNVERIFIED; re-read 2026-11-24, the timestamp functions page).
			ls = start + max(o, zn.offs[n-1])
		}
		zn.starts = append(zn.starts, start)
		zn.lstarts = append(zn.lstarts, ls)
		zn.offs = append(zn.offs, o)
		zn.abbrs = append(zn.abbrs, name)
	}
	end := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	t := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).In(loc)
	name, off := t.Zone()
	add(math.MinInt64, off, name)
	// The tz database's own transitions, while Go's ZoneBounds gives them.
	for {
		_, next := t.ZoneBounds()
		if next.IsZero() || !next.Before(end) {
			return zn.orUTC()
		}
		if !next.After(t) {
			break // past them, into the zone's rule
		}
		t = next
		name, off = t.Zone()
		add(t.UnixMicro(), off, name)
	}
	// The zone's rule (its TZ string) repeats every 400 Gregorian years,
	// 146097 days, a whole number of weeks: its transitions are found a
	// day at a time over one cycle, to the second, then repeated.
	const cycleUS = int64(146097) * 86400 * 1000000
	from := len(zn.starts)
	stop := t.UTC().AddDate(400, 0, 0)
	for t.Before(stop) {
		n := t.Add(24 * time.Hour)
		nn, no := n.Zone()
		if nn != name || no != off {
			lo, hi := t, n // the change is in (lo, hi]
			for hi.Sub(lo) > time.Second {
				mid := lo.Add(hi.Sub(lo) / 2).Truncate(time.Second)
				if mn, mo := mid.Zone(); mn == name && mo == off {
					lo = mid
				} else {
					hi = mid
				}
			}
			name, off = hi.Zone()
			add(hi.UnixMicro(), off, name)
			t = hi
			continue
		}
		t = n
	}
	us := cycleUS
	one := len(zn.starts) - from
	for k := int64(1); one > 0; k++ {
		for i := from; i < from+one; i++ {
			st := zn.starts[i] + k*us
			if st >= end.UnixMicro() {
				return zn.orUTC()
			}
			add(st, int(zn.offs[i]/1000000), zn.abbrs[i])
		}
	}
	return zn.orUTC()
}

// orUTC is z, or nil when it is UTC throughout.
func (z *tgZone) orUTC() *tgZone {
	if len(z.offs) == 1 && z.offs[0] == 0 {
		return nil
	}
	return z
}

func (z *tgZone) attrs(local bool) map[string]any {
	if local {
		return map[string]any{"starts": z.lstarts, "offs": z.offs}
	}
	return map[string]any{"starts": z.starts, "offs": z.offs}
}

// toLocal is TIMESTAMP ts as its civil time in z, in microseconds.
func toLocal(ts *tgNode, z *tgZone) *tgNode {
	if z == nil {
		return ts
	}
	return node("add", "i", nil, ts, node("tz_offset", "i", z.attrs(false), ts))
}

// fromLocal is civil time local in z as a TIMESTAMP.
func fromLocal(local *tgNode, z *tgZone) *tgNode {
	if z == nil {
		return local
	}
	return node("sub", "i", nil, local, node("tz_offset", "i", z.attrs(true), local))
}

var tgDateUnits = map[string]bool{"MICROSECOND": true, "MILLISECOND": true, "SECOND": true, "MINUTE": true, "HOUR": true, "DAY": true,
	"WEEK": true, "MONTH": true, "QUARTER": true, "YEAR": true}

func (c *tgCompiler) dateAdd(typ string, x, iv tgVal, sub bool) tgVal {
	unit, _ := iv.lit.(string)
	if !tgDateUnits[unit] {
		panic(&tgFail{400, "Unsupported date part " + unit})
	}
	n := iv.node
	if sub {
		n = node("neg", "i", nil, n)
	}
	return tgVal{typ: typ, node: node("date_add", "i", map[string]any{"type": typ, "unit": unit}, x.node, n)}
}

func (c *tgCompiler) extract(part string, x tgVal, zone string) tgVal {
	if !strings.Contains(x.typ, "DATE") && !strings.Contains(x.typ, "TIME") {
		x = c.as(x, "TIMESTAMP")
	}
	if zone != "" && x.typ == "TIMESTAMP" {
		// The part of the zone's civil time.
		x = tgVal{typ: "DATETIME", node: toLocal(x.node, c.zoneOf(zone))}
	}
	switch part {
	case "DATE", "TIME", "DATETIME":
		return c.dateCtor(part, []tgVal{x})
	}
	return tgVal{typ: "INT64", node: node("extract", "i", map[string]any{"type": x.typ, "part": part}, x.node)}
}

// dateCtor is DATE(...), DATETIME(...), TIME(...) or TIMESTAMP(...).
func (c *tgCompiler) dateCtor(name string, args []tgVal) tgVal {
	if len(args) == 0 {
		panic(&tgFail{400, "No matching signature for function " + name})
	}
	a0 := args[0]
	switch name {
	case "DATE":
		switch {
		case len(args) == 3:
			return tgVal{typ: "DATE", node: node("make_date", "i", nil, c.toInt(args[0]).node, c.toInt(args[1]).node, c.toInt(args[2]).node)}
		case a0.typ == "DATETIME" || a0.typ == "TIMESTAMP":
			return tgVal{typ: "DATE", node: node("floordiv_const", "i", map[string]any{"d": int64(86400000000)}, c.civil(args))}
		case a0.typ == "DATE":
			return a0
		case a0.typ == "STRING":
			return c.cast(a0, "DATE", false)
		}
	case "DATETIME":
		switch {
		case len(args) == 6:
			var ns []*tgNode
			for _, a := range args {
				ns = append(ns, c.toInt(a).node)
			}
			return tgVal{typ: "DATETIME", node: node("make_datetime", "i", nil, ns...)}
		case len(args) == 2 && a0.typ == "DATE" && args[1].typ == "TIME":
			day := node("mul", "i", nil, a0.node, litVal("INT64", int64(86400000000)).node)
			return tgVal{typ: "DATETIME", node: node("add", "i", nil, day, args[1].node)}
		case a0.typ == "DATE":
			return tgVal{typ: "DATETIME", node: node("mul", "i", nil, a0.node, litVal("INT64", int64(86400000000)).node)}
		case a0.typ == "TIMESTAMP":
			return tgVal{typ: "DATETIME", node: c.civil(args)}
		case a0.typ == "DATETIME":
			return a0
		case a0.typ == "STRING":
			return c.cast(a0, "DATETIME", false)
		}
	case "TIME":
		switch {
		case len(args) == 3:
			return tgVal{typ: "TIME", node: node("make_time", "i", nil, c.toInt(args[0]).node, c.toInt(args[1]).node, c.toInt(args[2]).node)}
		case a0.typ == "DATETIME" || a0.typ == "TIMESTAMP":
			return tgVal{typ: "TIME", node: node("floormod_const", "i", map[string]any{"d": int64(86400000000)}, c.civil(args))}
		case a0.typ == "TIME":
			return a0
		case a0.typ == "STRING":
			return c.cast(a0, "TIME", false)
		}
	case "TIMESTAMP":
		var zn *tgZone
		if len(args) == 2 {
			zn = c.zoneOf(c.constString(args[1], "the time zone"))
		}
		switch a0.typ {
		case "DATE":
			return tgVal{typ: "TIMESTAMP", node: fromLocal(node("mul", "i", nil, a0.node, litVal("INT64", int64(86400000000)).node), zn)}
		case "DATETIME":
			return tgVal{typ: "TIMESTAMP", node: fromLocal(a0.node, zn)}
		case "TIMESTAMP":
			return a0
		case "STRING":
			v := c.cast(a0, "TIMESTAMP", false)
			if zn != nil && v.node.Op == "cast" {
				// A string with no offset of its own is in the zone.
				v.node.A["tz"] = zn.attrs(true)
			}
			return v
		}
	}
	panic(&tgFail{400, fmt.Sprintf("No matching signature for function %s for argument types: %s", name, a0.typ)})
}

// tgFormatSpec is one element of a date format: a literal or a specifier.
var tgFormatSpecs = map[string]bool{"Y": true, "m": true, "d": true, "H": true, "M": true, "S": true, "F": true, "T": true, "D": true, "R": true,
	"y": true, "j": true, "e": true, "I": true, "p": true, "a": true, "A": true, "b": true, "B": true, "h": true, "u": true, "w": true, "Q": true,
	"C": true, "k": true, "l": true, "s": true, "%": true, "n": true, "t": true, "z": true, "Z": true}

// formatParts also reads "%\x00", which no format element is: STRING(ts)'s
// offset, "+HH", or "+HH:MM" when it has minutes, as "%cbz".

// formatParts splits a format string into literals and specifiers
// ("%Y", "%E6S", "%E*S", "%Ez").
func formatParts(f string) ([]string, *tgFail) {
	var out []string
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, "="+lit.String())
			lit.Reset()
		}
	}
	for k := 0; k < len(f); k++ {
		if f[k] != '%' || k+1 >= len(f) {
			lit.WriteByte(f[k])
			continue
		}
		k++
		switch {
		case f[k] == 'E' && k+2 < len(f) && (f[k+1] >= '0' && f[k+1] <= '9' || f[k+1] == '*') && f[k+2] == 'S':
			flush()
			out = append(out, "%E"+string(f[k+1])+"S")
			k += 2
		case f[k] == 0:
			flush()
			out = append(out, "%cbz")
		case f[k] == 'E' && k+1 < len(f) && f[k+1] == 'z':
			flush()
			out = append(out, "%Ez")
			k++
		case f[k] == '%':
			lit.WriteByte('%')
		case f[k] == 'n':
			lit.WriteByte('\n')
		case f[k] == 't':
			lit.WriteByte('\t')
		case tgFormatSpecs[string(f[k])]:
			flush()
			switch f[k] {
			case 'F':
				out = append(out, "%Y", "=-", "%m", "=-", "%d")
			case 'T':
				out = append(out, "%H", "=:", "%M", "=:", "%S")
			case 'D':
				out = append(out, "%m", "=/", "%d", "=/", "%y")
			case 'R':
				out = append(out, "%H", "=:", "%M")
			default:
				out = append(out, "%"+string(f[k]))
			}
		default:
			return nil, tgUnsupported("the format element %%%c", f[k])
		}
	}
	flush()
	return out, nil
}

// formatDate is FORMAT_<type>(f, x): the parts the convert.py side
// writes, a specifier each.
// A TIMESTAMP in time zone zn (nil UTC) is written in the zone's civil
// time, %z, %Ez and %Z its offset and abbreviation there.
func (c *tgCompiler) formatDate(x tgVal, f string, zn *tgZone) tgVal {
	parts, err := formatParts(f)
	if err != nil {
		panic(err)
	}
	a := map[string]any{"type": x.typ, "parts": parts}
	if x.typ != "TIMESTAMP" || zn == nil {
		return tgVal{typ: "STRING", node: node("format_date", "s", a, x.node)}
	}
	za := zn.attrs(false)
	za["abbrs"] = zn.abbrs
	return tgVal{typ: "STRING", node: node("format_date", "s", a, toLocal(x.node, zn), node("tz_offset", "i", zn.attrs(false), x.node),
		node("tz_abbr", "s", za, x.node))}
}

// civil is DATE, DATETIME or TIME(args...)'s first argument as civil
// microseconds: a TIMESTAMP in its time zone argument (UTC by default).
func (c *tgCompiler) civil(args []tgVal) *tgNode {
	if args[0].typ != "TIMESTAMP" {
		return args[0].node
	}
	var zn *tgZone
	if len(args) == 2 {
		zn = c.zoneOf(c.constString(args[1], "the time zone"))
	}
	return toLocal(args[0].node, zn)
}

// parseDate is PARSE_<type>(f, s).
func (c *tgCompiler) parseDate(typ string, s tgVal, f string, safe bool, zn *tgZone) tgVal {
	parts, err := formatParts(f)
	if err != nil {
		panic(err)
	}
	for _, p := range parts {
		switch p {
		case "%Y", "%m", "%d", "%H", "%M", "%S", "%E6S", "%E*S", "%E3S", "%y", "%j", "%e", "%b", "%B", "%h", "%z", "%Ez", "%I", "%p", "%k", "%l", "%s":
		default:
			if !strings.HasPrefix(p, "=") {
				panic(tgUnsupported("PARSE_%s of the format element %s", typ, p))
			}
		}
	}
	v := node("parse_date", "i", map[string]any{"type": typ, "parts": parts, "safe": safe}, s.node)
	if typ == "TIMESTAMP" && zn != nil && !slices.Contains(parts, "%z") && !slices.Contains(parts, "%Ez") && !slices.Contains(parts, "%s") {
		// No offset in the string: its time is the zone's.
		v = fromLocal(v, zn)
	}
	return tgVal{typ: typ, node: v}
}

// subquery compiles (SELECT ...): over UNNEST of a constant array, or of
// a FROM-less SELECT, as ML preprocessing's SQL writes them.
func (c *tgCompiler) subquery() tgVal {
	c.expect("SELECT")
	selStart := c.i
	// Skip the select item to read the FROM first: its aliases bind it.
	depth := 0
	for ; c.i < len(c.toks); c.i++ {
		t := c.toks[c.i]
		if t.punct("(") || t.punct("[") {
			depth++
		} else if t.punct(")") || t.punct("]") {
			if depth == 0 {
				break
			}
			depth--
		} else if depth == 0 && (t.is("FROM") || t.punct(",")) {
			break
		}
	}
	selEnd := c.i
	if c.peek().punct(",") {
		panic(tgUnsupported("a subquery of more than one column"))
	}
	if !c.accept("FROM") {
		// SELECT expr
		save := c.i
		c.i = selStart
		v := c.expr(0)
		c.i = save
		return v
	}
	if c.peek().punct("(") && c.peekAt(1).is("SELECT") {
		// FROM (SELECT e AS a, ...)
		c.i += 2
		bind := map[string]tgVal{}
		for {
			e := c.expr(0)
			c.expect("AS")
			bind[strings.ToLower(c.next().text)] = e
			if !c.acceptPunct(",") {
				break
			}
		}
		c.expectPunct(")")
		save := c.i
		c.env = append(c.env, bind)
		c.i = selStart
		v := c.expr(0)
		if c.i != selEnd {
			panic(tgUnsupported("the subquery's select list"))
		}
		c.env = c.env[:len(c.env)-1]
		c.i = save
		return v
	}
	c.expect("UNNEST")
	c.expectPunct("(")
	arr := c.expr(0)
	c.expectPunct(")")
	if !arr.isArr {
		panic(tgUnsupported("a subquery over an array that is not constant: an ARRAY is not supported as post-transformed data"))
	}
	c.accept("AS")
	alias := strings.ToLower(c.next().text)
	off := ""
	if c.accept("WITH") {
		c.expect("OFFSET")
		c.accept("AS")
		off = strings.ToLower(c.next().text)
	}
	whereStart, whereEnd := -1, -1
	if c.accept("WHERE") {
		whereStart = c.i
		depth := 0
		for ; c.i < len(c.toks); c.i++ {
			t := c.toks[c.i]
			if t.punct("(") {
				depth++
			} else if t.punct(")") {
				if depth == 0 {
					break
				}
				depth--
			}
		}
		whereEnd = c.i
	}
	if !c.peek().punct(")") {
		panic(tgUnsupported("the subquery's %q", c.rest()))
	}
	after := c.i
	evalAt := func(start, end int, bind map[string]tgVal) tgVal {
		c.env = append(c.env, bind)
		c.i = start
		v := c.expr(0)
		if c.i != end {
			panic(tgUnsupported("the subquery at %q", c.rest()))
		}
		c.env = c.env[:len(c.env)-1]
		return v
	}
	elemBind := func(k int) map[string]tgVal {
		b := map[string]tgVal{alias: arr.arr[k]}
		if off != "" {
			b[off] = litVal("INT64", int64(k))
		}
		return b
	}
	sel := c.toks[selStart:selEnd]
	isCount := len(sel) >= 3 && sel[0].is("COUNT") && sel[1].punct("(")
	defer func() { c.i = after }()
	if isCount {
		var total tgVal
		for k := range arr.arr {
			one := litVal("INT64", int64(1))
			if whereStart >= 0 {
				cond := evalAt(whereStart, whereEnd, elemBind(k))
				one = c.caseOf([]tgVal{c.boolOf(cond)}, []tgVal{litVal("INT64", int64(1))}, litVal("INT64", int64(0)))
			}
			if k == 0 {
				total = one
			} else {
				total = tgVal{typ: "INT64", node: node("add", "i", nil, total.node, one.node)}
			}
		}
		if len(arr.arr) == 0 {
			return litVal("INT64", int64(0))
		}
		return total
	}
	// WHERE alias = key: a lookup of key in the array.
	if whereStart >= 0 {
		w := c.toks[whereStart:whereEnd]
		keyStart := -1
		if len(w) >= 3 && strings.EqualFold(w[0].text, alias) && w[1].punct("=") {
			keyStart = whereStart + 2
			for _, t := range w[2:] {
				if t.is("AND") || t.is("OR") {
					keyStart = -1
				}
			}
		}
		if keyStart >= 0 {
			key := evalAt(keyStart, whereEnd, map[string]tgVal{})
			et := arr.elem
			key = c.as(key, et)
			var keys []any
			for _, e := range arr.arr {
				keys = append(keys, e.lit)
			}
			idx := tgVal{typ: "INT64", node: node("lookup", "i", map[string]any{"keys": keys}, key.node)}
			bind := map[string]tgVal{alias: key}
			if off != "" {
				bind[off] = idx
			}
			v := evalAt(selStart, selEnd, bind)
			// No match: NULL.
			found := tgVal{typ: "BOOL", node: node("not", "b", nil, node("isnull", "b", nil, idx.node))}
			return c.caseOf([]tgVal{found}, []tgVal{v}, tgVal{typ: "NULL", node: node("null", "f", nil), isLit: true})
		}
	}
	if len(arr.arr) > 512 {
		panic(tgUnsupported("a subquery over an array of %d elements", len(arr.arr)))
	}
	var conds, results []tgVal
	for k := range arr.arr {
		cond := litVal("BOOL", true)
		if whereStart >= 0 {
			cond = c.boolOf(evalAt(whereStart, whereEnd, elemBind(k)))
		}
		conds = append(conds, cond)
		results = append(results, evalAt(selStart, selEnd, elemBind(k)))
	}
	return c.caseOf(conds, results, tgVal{typ: "NULL", node: node("null", "f", nil), isLit: true})
}

// transformSpec is the converter's spec of md's preprocessing SavedModel:
// its inputs, the columns the TRANSFORM reads, and its outputs, a graph a
// TRANSFORM column.
func transformSpec(md bqModel) (map[string]any, int, string) {
	if len(md.Transform) == 0 {
		return nil, 400, "the model has no TRANSFORM clause"
	}
	if len(md.Inputs) == 0 && len(md.TransformSQL) > 0 {
		return nil, 400, fmt.Sprintf("Model %s:%s.%s cannot be exported: it was created before CloudBurrow kept its TRANSFORM's input columns; create it again", md.Project, md.Dataset, md.ID)
	}
	cols := map[string]tgVal{}
	var inputs []map[string]any
	for _, in := range md.Inputs {
		t := sqlTypeOfColumn(in.Type)
		dtype := ""
		switch t {
		case "INT64":
			dtype = "int64"
		case "FLOAT64", "NUMERIC", "BIGNUMERIC":
			dtype = "float64"
		case "BOOL":
			dtype = "bool"
		case "STRING", "BYTES", "DATE", "DATETIME", "TIME", "TIMESTAMP":
			dtype = "string"
		}
		if strings.HasPrefix(t, "ARRAY<") {
			// Pre-transformed data only: in the signature, as the page's
			// table gives it, and refused where an output reads it.
			elem := sqlTypeOfColumn(strings.TrimSuffix(strings.TrimPrefix(t, "ARRAY<"), ">"))
			in := map[string]any{"name": in.Name, "type": t}
			switch {
			case elem == "RECORD" || structIntFloat.MatchString(elem):
				// The schema of a query's REPEATED RECORD does not carry
				// its fields here: ARRAY<STRUCT<INT64, FLOAT64>> is the one
				// the page takes.
				in["dtype"], in["sparse"] = "float64", true
			case isNumericType(elem) || elem == "BOOL" || elem == "STRING" || elem == "BYTES":
				in["dtype"], in["rank"] = map[string]string{"i": "int64", "f": "float64", "s": "string", "b": "bool"}[tfDtypeOf(elem)], 2
			case elem == "DATE" || elem == "DATETIME" || elem == "TIME" || elem == "TIMESTAMP":
				in["dtype"], in["rank"] = "string", 2
			default:
				return nil, 400, fmt.Sprintf("Model %s:%s.%s cannot be exported: its TRANSFORM reads %s, of type %s; during model export, ARRAY<STRUCT<INT64, FLOAT64>>, ARRAY and TIMESTAMP are supported as pre-transformed data", md.Project, md.Dataset, md.ID, in["name"], t)
			}
			cols[strings.ToLower(in["name"].(string))] = tgVal{typ: t}
			inputs = append(inputs, in)
			continue
		}
		if dtype == "" {
			return nil, 400, fmt.Sprintf("Model %s:%s.%s cannot be exported: its TRANSFORM reads %s, of type %s, which model export does not take", md.Project, md.Dataset, md.ID, in.Name, in.Type)
		}
		cols[strings.ToLower(in.Name)] = tgVal{typ: t, node: node("input", tfDtypeOf(t), map[string]any{"name": in.Name, "type": t})}
		inputs = append(inputs, map[string]any{"name": in.Name, "dtype": dtype, "type": t})
	}
	var outputs []map[string]any
	for _, it := range md.predictTransform() {
		expr, name := transformItemName(it)
		v, fail := compileTransformItem(expr, cols)
		if fail == tgArrayOut {
			v, fail = tgVal{typ: "ARRAY<>"}, nil
		}
		if fail != nil && fail.msg == "an ARRAY input" {
			return nil, 400, fmt.Sprintf("Model %s:%s.%s cannot be exported: its TRANSFORM column %s reads an ARRAY input; the TRANSFORM functions model export supports give an ARRAY of an ARRAY, and during model export, ARRAY<STRUCT<INT64, FLOAT64>>, ARRAY and TIMESTAMP are supported as pre-transformed data, but are not supported as post-transformed data", md.Project, md.Dataset, md.ID, name)
		}
		if fail != nil {
			return nil, fail.code, fail.msg
		}
		t := v.typ
		if t == "NULL" || t == "ANY" {
			t = "INT64"
			v = c0().as(v, t)
		}
		switch {
		case strings.HasPrefix(t, "ARRAY"):
			return nil, 400, fmt.Sprintf("Model %s:%s.%s cannot be exported: its TRANSFORM column %s is an ARRAY; during model export, ARRAY<STRUCT<INT64, FLOAT64>>, ARRAY and TIMESTAMP are supported as pre-transformed data, but are not supported as post-transformed data", md.Project, md.Dataset, md.ID, name)
		case t == "TIMESTAMP":
			return nil, 400, fmt.Sprintf("Model %s:%s.%s cannot be exported: its TRANSFORM column %s is a TIMESTAMP; during model export, ARRAY<STRUCT<INT64, FLOAT64>>, ARRAY and TIMESTAMP are supported as pre-transformed data, but are not supported as post-transformed data", md.Project, md.Dataset, md.ID, name)
		}
		outputs = append(outputs, map[string]any{"name": name, "type": t, "graph": v.node})
	}
	return map[string]any{"transform": map[string]any{"inputs": inputs, "outputs": outputs,
		"nulls": map[string]any{"float64": transformNullFloat, "int64": transformNullInt, "string": transformNullString}}}, 0, ""
}

func c0() *tgCompiler { return &tgCompiler{} }
