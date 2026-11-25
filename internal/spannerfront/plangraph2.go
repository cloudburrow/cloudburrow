package spannerfront

// The GQL-to-SQL translation the graph planner reads (#2029; plangraph.go).
//
// A linear graph query is a sequence of statements over a working table
// (Google's GQL linear query statements page). The translation keeps that
// working table as a set of branches, each a flat FROM list and WHERE
// clause over base tables, in which every graph variable reads one element
// table: a pattern whose variable can match several element tables (a
// label expression with |, ! or %, an unlabeled pattern) or whose edge can
// go either way (-[e]-) yields one branch per consistent choice, the
// branches' rows together being the pattern's matches, as Spanner reads a
// label on several element tables (a Union All of their scans). MATCH adds
// to every branch; FILTER, LET and FOR change every branch; OPTIONAL MATCH
// left-joins the working table to the optional pattern's rows; NEXT, WITH,
// a RETURN before NEXT, and ORDER BY / LIMIT / OFFSET as statements
// project the working table into a derived table (its columns
// cb_<var>__...); the final RETURN is the select list over the working
// table, or one select list per branch when nothing aggregates or sorts.
// A quantified edge is a FROM item of the paths it matches (graphQuant).
// What the translation does not cover is refused by name.

import (
	"fmt"
	"sort"
	"strings"
)

type gqlBindKind int

const (
	bElem   gqlBindKind = iota // a node or an edge
	bGroup                     // a quantified edge's group variable
	bScalar                    // a value (LET, FOR, a projected column)
	bPath                      // a path variable
)

// gqlBind is what a name is in one branch.
type gqlBind struct {
	kind gqlBindKind
	edge bool
	// an element read in the branch: its FROM item's alias and table
	alias string
	table *graphElement
	// an element of a derived table: the table's alias, the element's
	// column prefix, the element tables it can be, and its columns
	qual   string
	prefix string
	tables []*graphElement
	cols   map[string]bool
	// a value: its SQL
	sql string
	// a group variable: its quantified path
	quant *graphQuant
	// a path variable: its length, its cost (ANY CHEAPEST's sum of its
	// edges' COST) and its value (TO_JSON(p): its elements' keys, in order)
	pathLen  string
	pathCost string
	pathVal  string
	quants   []*graphQuant // its quantified edges, whose keys pathVal reads
}

func (b *gqlBind) materialized() bool { return b.qual != "" }

// candidates are the element tables it can be.
func (b *gqlBind) candidates() []*graphElement {
	if b.table != nil {
		return []*graphElement{b.table}
	}
	return b.tables
}

// col is the SQL of one of its table's columns.
func (b *gqlBind) col(c string) string {
	if b.materialized() {
		n := b.prefix + "__c_" + strings.ToLower(c)
		if !b.cols[n] {
			return "NULL"
		}
		return b.qual + "." + n
	}
	return b.alias + "." + c
}

// prop is the SQL of one of its properties.
func (b *gqlBind) prop(p string) (string, error) {
	if b.materialized() {
		n := b.prefix + "__p_" + strings.ToLower(p)
		if !b.cols[n] {
			return "NULL", nil
		}
		return b.qual + "." + n, nil
	}
	if !b.table.hasProperty(p) {
		return "NULL", nil
	}
	c, err := b.table.column(p)
	if err != nil {
		return "", err
	}
	return b.alias + "." + c, nil
}

// isTable is the SQL condition that it is of element table t.
func (b *gqlBind) isTable(t *graphElement) string {
	if b.materialized() && b.table == nil && len(b.tables) > 1 {
		return b.qual + "." + b.prefix + "__t = '" + t.Name + "'"
	}
	for _, c := range b.candidates() {
		if c == t {
			return "TRUE"
		}
	}
	return "FALSE"
}

type gqlBranch struct {
	from  []string
	conds []string
	binds map[string]*gqlBind
	names []string // the visible names, in order
}

func (br *gqlBranch) copy() *gqlBranch {
	c := &gqlBranch{from: append([]string(nil), br.from...), conds: append([]string(nil), br.conds...),
		binds: map[string]*gqlBind{}, names: append([]string(nil), br.names...)}
	for k, v := range br.binds {
		c.binds[k] = v
	}
	return c
}

func (br *gqlBranch) bind(name string, b *gqlBind) {
	k := strings.ToLower(name)
	if _, ok := br.binds[k]; !ok && !strings.HasPrefix(k, "\x00") {
		br.names = append(br.names, k)
	}
	br.binds[k] = b
}

// --- label expressions ---------------------------------------------------------

type gqlLabel struct {
	op    string // "|", "&", "!", "%", "" (a label)
	name  string
	l, r  *gqlLabel
	inner *gqlLabel
}

func (l *gqlLabel) matches(e *graphElement) bool {
	switch {
	case l == nil:
		return true
	case l.op == "%":
		return len(e.LabelNames) > 0
	case l.op == "|":
		return l.l.matches(e) || l.r.matches(e)
	case l.op == "&":
		return l.l.matches(e) && l.r.matches(e)
	case l.op == "!":
		return !l.inner.matches(e)
	}
	return e.hasLabel(l.name)
}

func (t *gqlTranslator) labelExpr() (*gqlLabel, error) {
	l, err := t.labelTerm()
	for err == nil && t.ps.acceptP("|") {
		var r *gqlLabel
		if r, err = t.labelTerm(); err == nil {
			l = &gqlLabel{op: "|", l: l, r: r}
		}
	}
	return l, err
}

func (t *gqlTranslator) labelTerm() (*gqlLabel, error) {
	l, err := t.labelFactor()
	for err == nil && t.ps.acceptP("&") {
		var r *gqlLabel
		if r, err = t.labelFactor(); err == nil {
			l = &gqlLabel{op: "&", l: l, r: r}
		}
	}
	return l, err
}

func (t *gqlTranslator) labelFactor() (*gqlLabel, error) {
	ps := t.ps
	switch {
	case ps.acceptP("!"):
		in, err := t.labelFactor()
		return &gqlLabel{op: "!", inner: in}, err
	case ps.acceptP("%"):
		return &gqlLabel{op: "%"}, nil
	case ps.acceptP("("):
		in, err := t.labelExpr()
		if err == nil {
			err = ps.expectP(")")
		}
		return in, err
	}
	tk := ps.peek()
	if tk.kind != tkIdent && tk.kind != tkQIdent {
		return nil, ps.errAt("a label")
	}
	ps.p++
	return &gqlLabel{name: tk.text}, nil
}

// --- patterns -------------------------------------------------------------------

// gqlPatVar is a variable of a MATCH statement's pattern.
type gqlPatVar struct {
	name   string // lower case; "" for an anonymous one
	alias  string // its FROM item's alias
	edge   bool
	labels []*gqlLabel
	quant  *gqlQuantSpec
	cost   [2]int // an edge pattern's COST expression, {0, 0} without one
	// restrict are the element tables an OPTIONAL MATCH's variable can be:
	// those it is in the working table
	restrict []*graphElement
}

type gqlQuantSpec struct {
	lo, hi int
	props  []gqlProp
	conds  [][2]int
	cost   [2]int // the edge's COST expression, per edge
	// a quantified path pattern, ((src)-[e]->(dst) WHERE ...){m,n}: its
	// node patterns, their inline filters, and the conditions (the nodes'
	// and the subpath's WHERE) each edge's endpoints are held to
	src, dst  *gqlPatVar
	nodeProps []gqlProp
	nodeConds [][2]int
}

// gqlProp is an inline property filter, {prop: value}.
type gqlProp struct {
	v      *gqlPatVar
	prop   string
	lo, hi int
}

// gqlPatEdge links an edge variable to the nodes before and after it.
type gqlPatEdge struct {
	e, left, right *gqlPatVar
	dir            int // 1: left to right, -1: right to left, 0: either
}

type gqlPath struct {
	search  string // ANY, ANY SHORTEST, ANY CHEAPEST: one path per pair of endpoints
	mode    string // WALK, TRAIL, ACYCLIC
	edges   []int  // its edges, in t.pat.edges
	nodes   []*gqlPatVar
	pathVar string
}

type gqlPattern struct {
	vars   []*gqlPatVar
	byName map[string]*gqlPatVar
	edges  []gqlPatEdge
	props  []gqlProp
	conds  [][2]int
	paths  []gqlPath
}

type gqlTranslator struct {
	ps       *sqlParser
	graphs   map[string]*graphMeta
	columns  bool // GRAPH_TABLE: COLUMNS ends the query
	g        *graphMeta
	quants   map[string]*graphQuant
	refProps map[string]map[string]bool // the properties read of each name
	next     int                        // generated aliases
	derived  int                        // derived tables
	branches []*gqlBranch
	// reading a path
	pendingEdge    *gqlPatEdge
	precededByEdge bool
	lastProps      []gqlProp
	lastConds      [][2]int
	lastCost       [2]int
	err            error // a failure while collapsing
	parens         bool  // COLUMNS (...): the final list is parenthesized
}

func newGQL(ps *sqlParser, graphs map[string]*graphMeta, quants map[string]*graphQuant, columns bool) *gqlTranslator {
	return &gqlTranslator{ps: ps, graphs: graphs, quants: quants, columns: columns}
}

// scanRefs records, for each name, the properties the query reads of it
// (name.property).
func (t *gqlTranslator) scanRefs() {
	t.refProps = map[string]map[string]bool{}
	toks := t.ps.toks
	for i := t.ps.p; i+2 < len(toks); i++ {
		if (toks[i].kind == tkIdent || toks[i].kind == tkQIdent) && toks[i+1].text == "." && (toks[i+2].kind == tkIdent || toks[i+2].kind == tkQIdent) &&
			(i == 0 || toks[i-1].text != ".") {
			n := strings.ToLower(toks[i].text)
			if t.refProps[n] == nil {
				t.refProps[n] = map[string]bool{}
			}
			t.refProps[n][strings.ToLower(toks[i+2].text)] = true
		}
	}
}

func (t *gqlTranslator) statement() (string, error) {
	ps := t.ps
	if !t.columns {
		if err := ps.expectKw("GRAPH"); err != nil {
			return "", err
		}
	}
	name, ok := ps.ident()
	if !ok {
		return "", ps.errAt("a property graph's name")
	}
	t.g = t.graphs[strings.ToLower(name)]
	if t.g == nil {
		return "", unsupportedSQL{fmt.Sprintf("property graph %s, which is not in the schema", name)}
	}
	t.scanRefs()
	return t.compositeQuery()
}

// compositeQuery is linear queries joined by set operations.
func (t *gqlTranslator) compositeQuery() (string, error) {
	ps := t.ps
	var parts []string
	for {
		t.branches = []*gqlBranch{{binds: map[string]*gqlBind{}}}
		sql, err := t.linearQuery()
		if err != nil {
			return "", err
		}
		parts = append(parts, sql)
		op := ""
		for _, kw := range []string{"UNION", "INTERSECT", "EXCEPT"} {
			if ps.acceptKw(kw) {
				op = kw
			}
		}
		if op == "" || t.columns {
			break
		}
		switch {
		case ps.acceptKw("ALL"):
			op += " ALL"
		case ps.acceptKw("DISTINCT"):
			op += " DISTINCT"
		}
		if ps.isKw("CORRESPONDING") || ps.isKw("STRICT") || ps.isKw("FULL") || ps.isKw("LEFT") || ps.isKw("INNER") {
			return "", unsupportedSQL{"a graph set operation's column propagation mode"}
		}
		parts = append(parts, op)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 0 {
			b.WriteString("(" + p + ")")
		} else {
			b.WriteString(" " + p + " ")
		}
	}
	return b.String(), nil
}

func (t *gqlTranslator) linearQuery() (string, error) {
	ps := t.ps
	matched := false
	for {
		switch {
		case ps.isKw("MATCH"), ps.isKw("OPTIONAL") && ps.isKwN(1, "MATCH"):
			optional := ps.acceptKw("OPTIONAL")
			ps.p++
			if ps.isP("@{") {
				if _, err := ps.hints(); err != nil {
					return "", err
				}
			}
			pat, err := t.pattern()
			if err != nil {
				return "", err
			}
			if optional {
				err = t.optionalMatch(pat)
			} else {
				err = t.match(pat)
			}
			if err != nil {
				return "", err
			}
			matched = true
		case ps.acceptKw("FILTER"):
			ps.acceptKw("WHERE")
			e, err := ps.expr()
			if err != nil {
				return "", err
			}
			for _, br := range t.branches {
				s, err := t.render(br, e.lo, e.hi)
				if err != nil {
					return "", err
				}
				br.conds = append(br.conds, s)
			}
		case ps.acceptKw("LET"):
			if err := t.let(); err != nil {
				return "", err
			}
		case ps.acceptKw("FOR"):
			if err := t.forStmt(); err != nil {
				return "", err
			}
		case ps.isKw("ORDER") || ps.isKw("LIMIT") || ps.isKw("OFFSET") || ps.isKw("SKIP"):
			if err := t.orderLimitStmt(); err != nil {
				return "", err
			}
		case ps.acceptKw("WITH"):
			if err := t.project(true); err != nil {
				return "", err
			}
		case ps.isKw("RETURN"), t.columns && ps.isKw("COLUMNS"):
			t.parens = ps.isKw("COLUMNS")
			ps.p++
			if !matched {
				return "", unsupportedSQL{"a graph query with no MATCH statement"}
			}
			// a RETURN NEXT is a projection
			save := ps.p
			if err := t.skipReturn(); err != nil {
				return "", err
			}
			isNext := ps.isKw("NEXT")
			ps.p = save
			if isNext {
				if err := t.project(false); err != nil {
					return "", err
				}
				ps.p++ // NEXT
				continue
			}
			return t.finalReturn()
		case ps.acceptKw("NEXT"):
			// NEXT after a statement other than RETURN: nothing to project
		default:
			tk := ps.peek()
			if tk.kind == tkEOF {
				return "", ps.errAt("RETURN")
			}
			return "", unsupportedSQL{fmt.Sprintf("the graph query statement %s", strings.ToUpper(ps.src[tk.start:tk.end]))}
		}
	}
}

// skipReturn moves past a RETURN's list and clauses.
func (t *gqlTranslator) skipReturn() error {
	ps := t.ps
	depth := 0
	for {
		tk := ps.peek()
		if tk.kind == tkEOF {
			return nil
		}
		if depth == 0 && tk.kind == tkIdent && (ps.isKw("NEXT") || ps.isKw("UNION") || ps.isKw("INTERSECT") || ps.isKw("EXCEPT")) {
			return nil
		}
		if depth == 0 && (tk.text == ";" || tk.text == ")") {
			return nil
		}
		switch tk.text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		}
		ps.p++
	}
}

// --- MATCH ----------------------------------------------------------------------

func (t *gqlTranslator) pattern() (*gqlPattern, error) {
	ps := t.ps
	pat := &gqlPattern{byName: map[string]*gqlPatVar{}}
	for {
		if err := t.pathPattern(pat); err != nil {
			return nil, err
		}
		if !ps.acceptP(",") {
			break
		}
	}
	if ps.acceptKw("WHERE") {
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		pat.conds = append(pat.conds, [2]int{e.lo, e.hi})
	}
	return pat, nil
}

var gqlPathModes = map[string]bool{"WALK": true, "TRAIL": true, "ACYCLIC": true, "SIMPLE": true}

func (t *gqlTranslator) pathPattern(pat *gqlPattern) error {
	ps := t.ps
	path := gqlPath{mode: "WALK"}
	if (ps.peek().kind == tkIdent || ps.peek().kind == tkQIdent) && ps.peekN(1).text == "=" {
		path.pathVar = strings.ToLower(ps.peek().text)
		ps.p += 2
	}
	switch {
	case ps.acceptKw("ALL"):
		// every path: the default
		if ps.isKw("SHORTEST") {
			return unsupportedSQL{"the path search prefix ALL SHORTEST"}
		}
	case ps.isKw("ANY") && ps.isKwN(1, "SHORTEST"):
		ps.p += 2
		path.search = "ANY SHORTEST"
	case ps.isKw("ANY") && ps.isKwN(1, "CHEAPEST") && ps.peekN(2).kind != tkNumber:
		ps.p += 2
		path.search = "ANY CHEAPEST"
	case ps.isKw("ANY") && ps.peekN(1).kind != tkNumber && !ps.isKwN(1, "CHEAPEST"):
		ps.p++
		path.search = "ANY"
	case ps.isKw("ANY") || ps.isKw("SHORTEST") || ps.isKw("CHEAPEST"):
		return unsupportedSQL{"the path search prefix " + strings.ToUpper(ps.peek().text) + " " + strings.ToUpper(ps.peekN(1).text)}
	}
	if ps.peek().kind == tkIdent && gqlPathModes[strings.ToUpper(ps.peek().text)] {
		path.mode = strings.ToUpper(ps.next().text)
		if path.mode == "SIMPLE" {
			return unsupportedSQL{"the SIMPLE path mode"}
		}
		ps.acceptKw("PATH")
		ps.acceptKw("PATHS")
	}
	var last *gqlPatVar // the node the path is at
	t.pendingEdge, t.precededByEdge = nil, false
	if err := t.pathSeq(pat, &path, &last, 0); err != nil {
		return err
	}
	if last == nil {
		return ps.errAt("a path pattern")
	}
	pat.paths = append(pat.paths, path)
	return nil
}

// pathSeq reads a path's node and edge patterns, and parenthesized
// subpaths, up to the end of the path (or of the subpath, at depth > 0).
func (t *gqlTranslator) pathSeq(pat *gqlPattern, path *gqlPath, last **gqlPatVar, depth int) error {
	ps := t.ps
	for {
		switch {
		case ps.isP("(") && t.isSubpath() && t.quantifiedSubpath():
			if err := t.quantSubpath(pat, path, last); err != nil {
				return err
			}
		case ps.isP("(") && t.isSubpath():
			ps.p++
			mode := ""
			whole := len(path.edges) == 0 && *last == nil
			if ps.peek().kind == tkIdent && gqlPathModes[strings.ToUpper(ps.peek().text)] {
				mode = strings.ToUpper(ps.next().text)
				ps.acceptKw("PATH")
				switch {
				case mode == "WALK" || mode == path.mode:
				case whole && depth == 0 && path.mode == "WALK" && mode != "SIMPLE":
					// the subpath is the whole path: its mode is the path's
					path.mode = mode
				default:
					return unsupportedSQL{"a path mode on a part of a path"}
				}
			}
			if err := t.pathSeq(pat, path, last, depth+1); err != nil {
				return err
			}
			if ps.acceptKw("WHERE") {
				e, err := ps.expr()
				if err != nil {
					return err
				}
				pat.conds = append(pat.conds, [2]int{e.lo, e.hi})
			}
			if err := ps.expectP(")"); err != nil {
				return err
			}
			if ps.isP("{") || ps.isP("+") || ps.isP("*") {
				return unsupportedSQL{"a quantified path pattern of more than one edge"}
			}
			if mode != "" && mode != "WALK" && whole && (ps.isP("-") || ps.isP("<") || ps.isP("(")) {
				return unsupportedSQL{"a path mode on a part of a path"}
			}
		case ps.isP("("):
			n, err := t.element(pat, false, "(", ")")
			if err != nil {
				return err
			}
			if *last != nil && !t.precededByEdge {
				// juxtaposed node patterns are one node
				if n != *last {
					return unsupportedSQL{"two node patterns side by side"}
				}
			}
			if t.pendingEdge != nil {
				t.pendingEdge.right = n
				pat.edges = append(pat.edges, *t.pendingEdge)
				path.edges = append(path.edges, len(pat.edges)-1)
				t.pendingEdge = nil
			}
			path.nodes = append(path.nodes, n)
			*last = n
			t.precededByEdge = false
		case ps.isP("-") || ps.isP("<"):
			if *last == nil || t.precededByEdge {
				// an edge with no node before it: an anonymous one
				n := t.anonNode(pat)
				if t.pendingEdge != nil {
					t.pendingEdge.right = n
					pat.edges = append(pat.edges, *t.pendingEdge)
					path.edges = append(path.edges, len(pat.edges)-1)
					t.pendingEdge = nil
				}
				path.nodes = append(path.nodes, n)
				*last = n
			}
			e, dir, err := t.edge(pat)
			if err != nil {
				return err
			}
			t.pendingEdge = &gqlPatEdge{e: e, left: *last, dir: dir}
			t.precededByEdge = true
		default:
			if t.pendingEdge != nil {
				n := t.anonNode(pat)
				t.pendingEdge.right = n
				pat.edges = append(pat.edges, *t.pendingEdge)
				path.edges = append(path.edges, len(pat.edges)-1)
				t.pendingEdge = nil
				path.nodes = append(path.nodes, n)
				*last = n
				t.precededByEdge = false
			}
			return nil
		}
	}
}

// isSubpath says whether the "(" ahead opens a parenthesized path rather
// than a node pattern.
func (t *gqlTranslator) isSubpath() bool {
	n := t.ps.peekN(1)
	if n.text == "(" || n.text == "-" || n.text == "<" {
		return true
	}
	return n.kind == tkIdent && gqlPathModes[strings.ToUpper(n.text)] && t.ps.peekN(2).text != ")" && t.ps.peekN(2).text != ":"
}

func (t *gqlTranslator) anonNode(pat *gqlPattern) *gqlPatVar {
	v := &gqlPatVar{alias: fmt.Sprintf("cb_g%d", t.next)}
	t.next++
	pat.vars = append(pat.vars, v)
	return v
}

// element reads a node's or an edge's filler: [var] [:Label | IS Label]
// [{prop: value, ...}] [WHERE cond], between open and close.
func (t *gqlTranslator) element(pat *gqlPattern, edge bool, open, close string) (*gqlPatVar, error) {
	ps := t.ps
	if err := ps.expectP(open); err != nil {
		return nil, err
	}
	name := ""
	if !ps.isKw("IS") && !ps.isKw("WHERE") {
		if n, ok := ps.ident(); ok {
			name = strings.ToLower(n)
		}
	}
	var label *gqlLabel
	if ps.acceptP(":") || ps.acceptKw("IS") {
		l, err := t.labelExpr()
		if err != nil {
			return nil, err
		}
		label = l
	}
	var v *gqlPatVar
	if name != "" {
		v = pat.byName[name]
	}
	if v == nil {
		v = &gqlPatVar{name: name, edge: edge}
		if name == "" {
			v.alias = fmt.Sprintf("cb_g%d", t.next)
			t.next++
		} else {
			v.alias = name
			pat.byName[name] = v
		}
		pat.vars = append(pat.vars, v)
	} else {
		if v.edge != edge {
			return nil, unsupportedSQL{fmt.Sprintf("variable %s, used as a node and an edge", name)}
		}
	}
	if label != nil {
		v.labels = append(v.labels, label)
	}
	var props []gqlProp
	if ps.acceptP("{") {
		for !ps.acceptP("}") {
			p, ok := ps.ident()
			if !ok {
				return nil, ps.errAt("a property name")
			}
			if err := ps.expectP(":"); err != nil {
				return nil, err
			}
			e, err := ps.expr()
			if err != nil {
				return nil, err
			}
			props = append(props, gqlProp{v: v, prop: p, lo: e.lo, hi: e.hi})
			if !ps.acceptP(",") {
				if err := ps.expectP("}"); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	var conds [][2]int
	if ps.acceptKw("WHERE") {
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		conds = append(conds, [2]int{e.lo, e.hi})
	}
	var cost [2]int
	if edge && ps.acceptKw("COST") {
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		cost = [2]int{e.lo, e.hi}
	}
	if err := ps.expectP(close); err != nil {
		return nil, err
	}
	t.lastProps, t.lastConds, t.lastCost = props, conds, cost
	if !edge {
		pat.props = append(pat.props, props...)
		pat.conds = append(pat.conds, conds...)
	}
	return v, nil
}

// edge reads -[...]->, <-[...]-, -[...]-, ->, <- or -, and a quantifier
// after it; it says which way the edge goes.
func (t *gqlTranslator) edge(pat *gqlPattern) (*gqlPatVar, int, error) {
	ps := t.ps
	fromLeft := ps.acceptP("-")
	if !fromLeft {
		ps.p++ // <
		if err := ps.expectP("-"); err != nil {
			return nil, 0, err
		}
	}
	var v *gqlPatVar
	var props []gqlProp
	var conds [][2]int
	var cost [2]int
	if ps.isP("[") {
		var err error
		if v, err = t.element(pat, true, "[", "]"); err != nil {
			return nil, 0, err
		}
		props, conds, cost = t.lastProps, t.lastConds, t.lastCost
		if err := ps.expectP("-"); err != nil {
			return nil, 0, err
		}
	} else {
		v = &gqlPatVar{edge: true, alias: fmt.Sprintf("cb_g%d", t.next)}
		t.next++
		pat.vars = append(pat.vars, v)
	}
	toRight := ps.acceptP(">")
	dir := 0
	switch {
	case fromLeft && toRight:
		dir = 1
	case !fromLeft && !toRight:
		dir = -1
	}
	if ps.isP("{") || ps.isP("+") || ps.isP("*") {
		q, err := t.quantifier()
		if err != nil {
			return nil, 0, err
		}
		q.props, q.conds, q.cost = props, conds, cost
		v.quant = q
		return v, dir, nil
	}
	v.cost = cost
	pat.props = append(pat.props, props...)
	pat.conds = append(pat.conds, conds...)
	return v, dir, nil
}

// quantifier reads an edge pattern's {m,n}, {,n} or {n}. The engine has
// already refused the unbounded ones (+, *, {m,}) and the bounds Google
// does not allow.
func (t *gqlTranslator) quantifier() (*gqlQuantSpec, error) {
	ps := t.ps
	if !ps.acceptP("{") {
		return nil, unsupportedSQL{"a path quantifier with no upper bound"}
	}
	num := func() (int, bool) {
		tk := ps.peek()
		if tk.kind != tkNumber {
			return 0, false
		}
		var n int
		if _, err := fmt.Sscan(tk.text, &n); err != nil {
			return 0, false
		}
		ps.p++
		return n, true
	}
	lo, ok := num()
	if !ok && !ps.isP(",") {
		return nil, ps.errAt("a path quantifier's bound")
	}
	hi := lo
	if ps.acceptP(",") {
		if hi, ok = num(); !ok {
			return nil, unsupportedSQL{"a path quantifier with no upper bound"}
		}
	}
	if err := ps.expectP("}"); err != nil {
		return nil, err
	}
	if hi < lo || hi < 1 {
		return nil, ps.errAt("a path quantifier's bounds in order")
	}
	return &gqlQuantSpec{lo: lo, hi: hi}, nil
}

// --- choosing element tables ------------------------------------------------------

// gqlChoice is one consistent choice of element tables for a pattern.
type gqlChoice struct {
	table map[*gqlPatVar]*graphElement
	dir   []int // each edge's direction: 1 or -1
}

func (t *gqlTranslator) tablesOf(edge bool) []*graphElement {
	var out []*graphElement
	ts := t.g.NodeTables
	if edge {
		ts = t.g.EdgeTables
	}
	for i := range ts {
		out = append(out, &ts[i])
	}
	return out
}

func (t *gqlTranslator) nodeTable(name string) *graphElement {
	for i := range t.g.NodeTables {
		if strings.EqualFold(t.g.NodeTables[i].Name, name) {
			return &t.g.NodeTables[i]
		}
	}
	return nil
}

// choices are the pattern's consistent choices in branch br.
func (t *gqlTranslator) choices(pat *gqlPattern, br *gqlBranch) ([]gqlChoice, error) {
	cands := map[*gqlPatVar][]*graphElement{}
	for _, v := range pat.vars {
		var base []*graphElement
		if b := br.binds[v.name]; v.name != "" && b != nil {
			if b.kind != bElem || b.edge != v.edge {
				return nil, unsupportedSQL{fmt.Sprintf("variable %s, which is not a graph element here", v.name)}
			}
			base = b.candidates()
		} else {
			base = t.tablesOf(v.edge)
		}
		if v.restrict != nil {
			base = v.restrict
		}
		var ok []*graphElement
		for _, e := range base {
			match := true
			for _, l := range v.labels {
				if !l.matches(e) {
					match = false
				}
			}
			if match {
				ok = append(ok, e)
			}
		}
		cands[v] = ok
	}
	var out []gqlChoice
	cur := gqlChoice{table: map[*gqlPatVar]*graphElement{}, dir: make([]int, len(pat.edges))}
	consistent := func() bool {
		for i, e := range pat.edges {
			et, l, r := cur.table[e.e], cur.table[e.left], cur.table[e.right]
			if et == nil || l == nil || r == nil {
				continue
			}
			src, dst := l, r
			if cur.dir[i] == -1 {
				src, dst = r, l
			}
			if !strings.EqualFold(et.Source.Node, src.Name) || !strings.EqualFold(et.Dest.Node, dst.Name) {
				return false
			}
			if q := e.e.quant; q != nil && (q.hi > 1 || q.lo == 0) && !strings.EqualFold(et.Source.Node, et.Dest.Node) {
				return false
			}
		}
		return true
	}
	var walk func(i int)
	walk = func(i int) {
		if len(out) > 64 {
			return
		}
		if i == len(pat.vars) {
			// then each either-way edge's direction
			var dirs func(j int)
			dirs = func(j int) {
				if j == len(pat.edges) {
					if consistent() {
						c := gqlChoice{table: map[*gqlPatVar]*graphElement{}, dir: append([]int(nil), cur.dir...)}
						for k, v := range cur.table {
							c.table[k] = v
						}
						out = append(out, c)
					}
					return
				}
				ds := []int{pat.edges[j].dir}
				if ds[0] == 0 {
					ds = []int{1, -1}
				}
				for _, d := range ds {
					cur.dir[j] = d
					dirs(j + 1)
				}
			}
			dirs(0)
			return
		}
		v := pat.vars[i]
		for _, e := range cands[v] {
			cur.table[v] = e
			walk(i + 1)
		}
		delete(cur.table, v)
	}
	walk(0)
	if len(out) > 64 {
		return nil, unsupportedSQL{"a pattern that matches more than 64 combinations of element tables"}
	}
	return out, nil
}

// --- building branches -----------------------------------------------------------

// match adds a MATCH statement's pattern to every branch.
func (t *gqlTranslator) match(pat *gqlPattern) error {
	for _, p := range pat.paths {
		if p.search != "" {
			return t.searchMatch(pat)
		}
	}
	var out []*gqlBranch
	for _, br := range t.branches {
		bs, err := t.matchIn(pat, br)
		if err != nil {
			return err
		}
		out = append(out, bs...)
	}
	if len(out) == 0 {
		return unsupportedSQL{"a pattern that no element table matches"}
	}
	t.branches = out
	return nil
}

// matchIn is the branches of the pattern's matches in br.
func (t *gqlTranslator) matchIn(pat *gqlPattern, br *gqlBranch) ([]*gqlBranch, error) {
	chs, err := t.choices(pat, br)
	if err != nil {
		return nil, err
	}
	var out []*gqlBranch
	for _, ch := range chs {
		nb := br.copy()
		for _, v := range pat.vars {
			tab := ch.table[v]
			if old := br.binds[v.name]; v.name != "" && old != nil {
				if old.table == nil {
					// a derived table's element, of this table in this branch
					nb.conds = append(nb.conds, old.isTable(tab))
					fixed := *old
					fixed.table = tab
					nb.binds[v.name] = &fixed
				}
				continue
			}
			b := &gqlBind{kind: bElem, edge: v.edge, alias: v.alias, table: tab}
			if q := v.quant; q != nil {
				gq := &graphQuant{alias: fmt.Sprintf("cb_q%d", len(t.quants)), table: tab, node: t.nodeTable(tab.Source.Node), min: q.lo, max: q.hi}
				t.quants[gq.alias] = gq
				b = &gqlBind{kind: bGroup, edge: true, alias: gq.alias, table: tab, quant: gq}
				nb.from = append(nb.from, "(?"+gq.alias+") AS "+gq.alias)
			} else {
				nb.from = append(nb.from, tab.BaseTableName+" AS "+v.alias)
			}
			if v.name != "" {
				nb.bind(v.name, b)
			} else {
				nb.binds["\x00"+v.alias] = b
			}
		}
		bindOf := func(v *gqlPatVar) *gqlBind {
			if v.name != "" {
				return nb.binds[v.name]
			}
			return nb.binds["\x00"+v.alias]
		}
		for i, e := range pat.edges {
			eb, l, r := bindOf(e.e), bindOf(e.left), bindOf(e.right)
			src, dst := l, r
			if ch.dir[i] == -1 {
				src, dst = r, l
			}
			et := eb.table
			srcCol, dstCol := eb.col, eb.col
			if eb.kind == bGroup {
				srcCol = func(c string) string { return eb.alias + ".cb_src_" + c }
				dstCol = func(c string) string { return eb.alias + ".cb_dst_" + c }
			}
			for j, c := range et.Source.EdgeCols {
				nb.conds = append(nb.conds, srcCol(c)+" = "+src.col(et.Source.NodeCols[j]))
			}
			for j, c := range et.Dest.EdgeCols {
				nb.conds = append(nb.conds, dstCol(c)+" = "+dst.col(et.Dest.NodeCols[j]))
			}
		}
		for _, p := range pat.props {
			s, err := t.propCond(nb, bindOf(p.v), p)
			if err != nil {
				return nil, err
			}
			nb.conds = append(nb.conds, s)
		}
		for _, c := range pat.conds {
			s, err := t.render(nb, c[0], c[1])
			if err != nil {
				return nil, err
			}
			nb.conds = append(nb.conds, "("+s+")")
		}
		// quantified edges: their own conditions, on each edge
		for _, v := range pat.vars {
			if v.quant == nil {
				continue
			}
			gb := bindOf(v)
			gq := gb.quant
			hop := &gqlBind{kind: bElem, edge: true, alias: "cb_hop", table: gb.table}
			hb := nb.copy()
			if v.name != "" {
				hb.binds[v.name] = hop
			}
			for _, p := range v.quant.props {
				p.v = v
				s, err := t.propCond(hb, hop, p)
				if err != nil {
					return nil, err
				}
				gq.hopConds = append(gq.hopConds, s)
			}
			for _, c := range v.quant.conds {
				s, err := t.render(hb, c[0], c[1])
				if err != nil {
					return nil, err
				}
				gq.hopConds = append(gq.hopConds, "("+s+")")
			}
			if v.quant.src != nil {
				s, err := t.subpathHopCond(hb, gb, v.quant)
				if err != nil {
					return nil, err
				}
				gq.hopConds = append(gq.hopConds, s)
			}
			if c := v.quant.cost; hasCost(c) {
				s, err := t.render(hb, c[0], c[1])
				if err != nil {
					return nil, err
				}
				gq.cost = "(" + s + ")"
			}
			for p := range t.refProps[v.name] {
				if gb.table.hasProperty(p) {
					if _, err := gb.table.column(p); err != nil {
						return nil, err
					}
					gq.arrs = append(gq.arrs, p)
				}
			}
			sort.Strings(gq.arrs)
		}
		if err := t.pathModes(pat, nb, bindOf); err != nil {
			return nil, err
		}
		for _, p := range pat.paths {
			if p.pathVar == "" {
				continue
			}
			var parts, costs, vals []string
			var quants []*graphQuant
			fixed := 0
			for k, n := range p.nodes {
				if nbd := bindOf(n); nbd != nil && nbd.table != nil && !nbd.materialized() {
					vals = append(vals, "["+elemKeySQL(nbd)+"]")
				}
				if k >= len(p.edges) {
					continue
				}
				pe := pat.edges[p.edges[k]]
				if gb := bindOf(pe.e); gb.kind == bGroup {
					gb.quant.withLen = true
					parts = append(parts, gb.alias+".cb_len")
					if gb.quant.cost != "" {
						costs = append(costs, gb.alias+".cb_cost")
					}
					quants = append(quants, gb.quant)
					vals = append(vals, gb.alias+".cb_ids")
				} else {
					fixed++
					if hasCost(pe.e.cost) {
						s, err := t.render(nb, pe.e.cost[0], pe.e.cost[1])
						if err != nil {
							return nil, err
						}
						costs = append(costs, "("+s+")")
					}
					if gb.table != nil && !gb.materialized() {
						vals = append(vals, "["+elemKeySQL(gb)+"]")
					}
				}
			}
			parts = append(parts, fmt.Sprint(fixed))
			pb := &gqlBind{kind: bPath, pathLen: "(" + strings.Join(parts, " + ") + ")", quants: quants, pathVal: pathValueSQL(vals)}
			if len(costs) > 0 {
				pb.pathCost = "(" + strings.Join(costs, " + ") + ")"
			}
			nb.bind(p.pathVar, pb)
		}
		out = append(out, nb)
	}
	return out, nil
}

// propCond is an inline property filter's condition.
func (t *gqlTranslator) propCond(br *gqlBranch, b *gqlBind, p gqlProp) (string, error) {
	col, err := b.prop(p.prop)
	if err != nil {
		return "", err
	}
	val, err := t.render(br, p.lo, p.hi)
	if err != nil {
		return "", err
	}
	return col + " = (" + val + ")", nil
}

// pathModes adds TRAIL's (no edge twice) and ACYCLIC's (no node twice)
// conditions.
func (t *gqlTranslator) pathModes(pat *gqlPattern, br *gqlBranch, bindOf func(*gqlPatVar) *gqlBind) error {
	for _, p := range pat.paths {
		if p.mode == "WALK" {
			continue
		}
		var quants int
		for _, i := range p.edges {
			if pat.edges[i].e.quant != nil {
				quants++
			}
		}
		if quants > 0 {
			if quants > 1 || len(p.edges) > 1 {
				return unsupportedSQL{"a " + p.mode + " path with a quantified edge and other edges"}
			}
			gb := bindOf(pat.edges[p.edges[0]].e)
			gb.quant.trail = p.mode == "TRAIL"
			gb.quant.acyc = p.mode == "ACYCLIC"
			continue
		}
		var elems []*gqlBind
		if p.mode == "TRAIL" {
			for _, i := range p.edges {
				elems = append(elems, bindOf(pat.edges[i].e))
			}
		} else {
			for _, n := range p.nodes {
				elems = append(elems, bindOf(n))
			}
		}
		for i := range elems {
			for j := i + 1; j < len(elems); j++ {
				if elems[i] == elems[j] {
					// a variable twice on the path: the same element
					br.conds = append(br.conds, "FALSE")
					continue
				}
				br.conds = append(br.conds, "NOT "+elemEqual(elems[i], elems[j]))
			}
		}
	}
	return nil
}

// elemEqual is the condition that two elements are the same one.
func elemEqual(a, b *gqlBind) string {
	var ors []string
	for _, ta := range a.candidates() {
		for _, tb := range b.candidates() {
			if ta != tb {
				continue
			}
			parts := []string{}
			for _, s := range []string{a.isTable(ta), b.isTable(ta)} {
				if s != "TRUE" {
					parts = append(parts, s)
				}
			}
			for _, c := range ta.KeyColumns {
				parts = append(parts, a.col(c)+" = "+b.col(c))
			}
			ors = append(ors, "("+strings.Join(parts, " AND ")+")")
		}
	}
	if len(ors) == 0 {
		return "(FALSE)"
	}
	return "(" + strings.Join(ors, " OR ") + ")"
}

// optionalMatch left-joins the working table to the pattern's matches:
// the pattern's own rows (its variables the working table has restricted
// to the element tables they are there), on those variables being the
// same elements and the MATCH's WHERE.
func (t *gqlTranslator) optionalMatch(pat *gqlPattern) error {
	wb := t.collapse(nil)
	conds := pat.conds
	pat.conds = nil
	for _, v := range pat.vars {
		if old := wb.binds[v.name]; v.name != "" && old != nil {
			if old.kind != bElem || old.edge != v.edge {
				return unsupportedSQL{fmt.Sprintf("variable %s, which is not a graph element here", v.name)}
			}
			v.restrict = old.candidates()
		}
	}
	obs, err := t.matchIn(pat, &gqlBranch{binds: map[string]*gqlBind{}})
	pat.conds = conds
	if err != nil {
		return err
	}
	if len(obs) == 0 {
		return unsupportedSQL{"an OPTIONAL MATCH that no element table matches"}
	}
	var names []string
	for _, v := range pat.vars {
		if v.name != "" {
			names = append(names, v.name)
		}
	}
	for _, p := range pat.paths {
		if p.pathVar != "" {
			names = append(names, p.pathVar)
		}
	}
	t.branches = obs
	ob := t.collapse(names)
	nb := &gqlBranch{binds: map[string]*gqlBind{}}
	for _, n := range wb.names {
		nb.bind(n, wb.binds[n])
	}
	var on []string
	for _, n := range names {
		if old := wb.binds[n]; old != nil {
			on = append(on, elemEqual(old, ob.binds[n]))
			continue
		}
		nb.bind(n, ob.binds[n])
	}
	for _, c := range conds {
		s, err := t.render(nb, c[0], c[1])
		if err != nil {
			return err
		}
		on = append(on, "("+s+")")
	}
	if len(on) == 0 {
		on = []string{"TRUE"}
	}
	nb.from = []string{wb.from[0] + " LEFT JOIN " + ob.from[0] + " ON " + strings.Join(on, " AND ")}
	t.branches = []*gqlBranch{nb}
	return nil
}

// searchMatch is a MATCH whose path has a search prefix: the pattern's
// own matches (its variables the working table has restricted to their
// element tables there), of which each pair of endpoints keeps one path
// (ANY), or one of its shortest (ANY SHORTEST); then joined to the working
// table on the variables they share.
func (t *gqlTranslator) searchMatch(pat *gqlPattern) error {
	if len(pat.paths) != 1 {
		return unsupportedSQL{"a path search prefix in a MATCH of more than one path pattern"}
	}
	path := &pat.paths[0]
	var wb *gqlBranch
	if len(t.allNames()) > 0 {
		wb = t.collapse(nil)
		for _, v := range pat.vars {
			if old := wb.binds[v.name]; v.name != "" && old != nil {
				if old.kind != bElem || old.edge != v.edge {
					return unsupportedSQL{fmt.Sprintf("variable %s, which is not a graph element here", v.name)}
				}
				v.restrict = old.candidates()
			}
		}
	}
	if path.pathVar == "" {
		path.pathVar = "\x00plen"
	}
	conds := pat.conds
	pat.conds = nil
	obs, err := t.matchIn(pat, &gqlBranch{binds: map[string]*gqlBind{}})
	pat.conds = conds
	if err != nil {
		return err
	}
	if len(obs) == 0 {
		return unsupportedSQL{"a pattern that no element table matches"}
	}
	key := func(v *gqlPatVar) string {
		if v.name != "" {
			return v.name
		}
		return "\x00" + v.alias
	}
	var names []string
	for _, v := range pat.vars {
		names = append(names, key(v))
	}
	names = append(names, path.pathVar)
	// group variables carry their edges' keys, to tell paths apart
	for _, br := range obs {
		for _, n := range names {
			if b := br.binds[n]; b != nil && b.kind == bGroup {
				b.quant.withIDs = true
			}
		}
	}
	t.branches = obs
	ob := t.collapse(names)
	inner := strings.TrimSuffix(ob.from[0], " AS "+ob.binds[path.pathVar].qual)
	qa := ob.binds[path.pathVar].qual
	qb := qa + "_b"
	other := func(b *gqlBind) *gqlBind {
		c := *b
		c.qual = qb
		if c.kind == bScalar {
			c.sql = strings.Replace(c.sql, qa+".", qb+".", 1)
		}
		if c.kind == bPath {
			c.pathLen = strings.Replace(c.pathLen, qa+".", qb+".", 1)
			c.pathCost = strings.Replace(c.pathCost, qa+".", qb+".", 1)
			c.pathVal = strings.Replace(c.pathVal, qa+".", qb+".", 1)
		}
		return &c
	}
	first, last := ob.binds[key(path.nodes[0])], ob.binds[key(path.nodes[len(path.nodes)-1])]
	// a path's identity: its elements' keys, in order
	var ida, idb []string
	for _, n := range names {
		b := ob.binds[n]
		for c := range b.cols {
			if strings.HasSuffix(c, "__t") || strings.Contains(c, "__c_") || strings.HasSuffix(c, "__ids") {
				ida = append(ida, "FORMAT('%T', "+qa+"."+c+")")
				idb = append(idb, "FORMAT('%T', "+qb+"."+c+")")
			}
		}
	}
	sortPairs(ida, idb)
	idA, idB := "CONCAT("+strings.Join(ida, ", '|', ")+")", "CONCAT("+strings.Join(idb, ", '|', ")+")"
	better := idB + " < " + idA
	switch path.search {
	case "ANY SHORTEST":
		la, lb := ob.binds[path.pathVar].pathLen, other(ob.binds[path.pathVar]).pathLen
		better = "(" + lb + " < " + la + " OR " + lb + " = " + la + " AND " + idB + " < " + idA + ")"
	case "ANY CHEAPEST":
		ca, cb := ob.binds[path.pathVar].pathCost, other(ob.binds[path.pathVar]).pathCost
		if ca == "" {
			return unsupportedSQL{"ANY CHEAPEST with no edge COST"}
		}
		better = "(" + cb + " < " + ca + " OR " + cb + " = " + ca + " AND " + idB + " < " + idA + ")"
	}
	var cols []string
	for _, n := range names {
		for c := range ob.binds[n].cols {
			cols = append(cols, qa+"."+c)
		}
	}
	sort.Strings(cols)
	sel := "SELECT " + strings.Join(cols, ", ") + " FROM " + ob.from[0] + " WHERE NOT EXISTS (SELECT 1 FROM " + inner + " AS " + qb +
		" WHERE " + elemEqual(first, other(first)) + " AND " + elemEqual(last, other(last)) + " AND " + better + ")"
	qs := fmt.Sprintf("cb_w%d", t.derived)
	t.derived++
	nb := &gqlBranch{binds: map[string]*gqlBind{}}
	if wb != nil {
		for _, n := range wb.names {
			nb.bind(n, wb.binds[n])
		}
	}
	var on []string
	for _, n := range names {
		b := *ob.binds[n]
		b.qual = qs
		if b.kind == bPath {
			b.pathLen = strings.Replace(b.pathLen, qa+".", qs+".", 1)
			b.pathCost = strings.Replace(b.pathCost, qa+".", qs+".", 1)
			b.pathVal = strings.Replace(b.pathVal, qa+".", qs+".", 1)
		}
		if wb != nil && wb.binds[n] != nil {
			on = append(on, elemEqual(wb.binds[n], &b))
			continue
		}
		nb.bind(n, &b)
	}
	from := "(" + sel + ") AS " + qs
	if wb != nil {
		nb.from = []string{wb.from[0], from}
	} else {
		nb.from = []string{from}
	}
	nb.conds = on
	for _, c := range conds {
		s, err := t.render(nb, c[0], c[1])
		if err != nil {
			return err
		}
		nb.conds = append(nb.conds, "("+s+")")
	}
	t.branches = []*gqlBranch{nb}
	return nil
}

// sortPairs sorts two lists alike, by the first.
func sortPairs(a, b []string) {
	idx := make([]int, len(a))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return a[idx[i]] < a[idx[j]] })
	a2, b2 := make([]string, len(a)), make([]string, len(b))
	for i, k := range idx {
		a2[i], b2[i] = a[k], b[k]
	}
	copy(a, a2)
	copy(b, b2)
}
