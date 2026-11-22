package spannerfront

// Graph queries' plans (#2029). Spanner runs a graph query (GQL: GRAPH g
// MATCH ... RETURN ..., and GRAPH_TABLE in a SQL query's FROM clause) on
// the relational operators of its query plans: each node and edge pattern
// reads its element table (a Scan, or a Filter Scan seeking on its key),
// and an edge joins its source and destination nodes on the element keys
// the property graph declares (Google's binary operators page draws a
// graph query's plan with the same Distributed Cross Apply, Cross Apply
// and Filter Scan operators as SQL's). So the front plans a graph query
// as the SQL query that reads the same rows: one FROM item per element
// variable, the edges' key equalities and the patterns' conditions in its
// WHERE clause, RETURN's (COLUMNS') list as its select list. The property
// graph's element tables, labels, keys and properties come from
// INFORMATION_SCHEMA.PROPERTY_GRAPHS (PROPERTY_GRAPH_METADATA_JSON). What
// the translation does not cover (a label on more than
// one element table, OPTIONAL MATCH, NEXT, ...) is refused by name.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// graphMeta is a property graph's PROPERTY_GRAPH_METADATA_JSON.
type graphMeta struct {
	Name       string         `json:"name"`
	NodeTables []graphElement `json:"nodeTables"`
	EdgeTables []graphElement `json:"edgeTables"`
}

type graphElement struct {
	Name          string   `json:"name"`
	BaseTableName string   `json:"baseTableName"`
	KeyColumns    []string `json:"keyColumns"`
	LabelNames    []string `json:"labelNames"`
	PropertyDefs  []struct {
		Name string `json:"propertyDeclarationName"`
		SQL  string `json:"valueExpressionSql"`
	} `json:"propertyDefinitions"`
	Source *graphEndpoint `json:"sourceNodeTable"`
	Dest   *graphEndpoint `json:"destinationNodeTable"`
}

type graphEndpoint struct {
	EdgeCols []string `json:"edgeTableColumns"`
	NodeCols []string `json:"nodeTableColumns"`
	Node     string   `json:"nodeTableName"`
}

func parseGraphMeta(js string) (*graphMeta, error) {
	var g graphMeta
	if err := json.Unmarshal([]byte(js), &g); err != nil {
		return nil, fmt.Errorf("reading a property graph's metadata: %w", err)
	}
	return &g, nil
}

func (e *graphElement) hasLabel(l string) bool {
	for _, x := range e.LabelNames {
		if strings.EqualFold(x, l) {
			return true
		}
	}
	return false
}

// column is the column a property reads.
func (e *graphElement) column(prop string) (string, error) {
	for _, p := range e.PropertyDefs {
		if strings.EqualFold(p.Name, prop) {
			c := strings.Trim(p.SQL, "`")
			if !isPlainName(c) {
				return "", unsupportedSQL{fmt.Sprintf("property %s, which element table %s defines by the expression %s", prop, e.Name, p.SQL)}
			}
			return c, nil
		}
	}
	return "", unsupportedSQL{fmt.Sprintf("property %s, which element table %s does not have", prop, e.Name)}
}

func isPlainName(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return true
}

// mentionsGraph says whether a GoogleSQL statement is a graph query or
// reads GRAPH_TABLE.
func mentionsGraph(src string) bool {
	toks, err := lexSQL(src, false)
	if err != nil {
		return false
	}
	for _, t := range toks {
		if t.kind == tkIdent && (strings.EqualFold(t.text, "GRAPH") || strings.EqualFold(t.text, "GRAPH_TABLE")) {
			return true
		}
	}
	return false
}

// graphQuant is a quantified edge pattern, -[:Label]->{m,n}: the paths of
// m to n edges of one edge table, as a FROM item whose columns are each
// path's first source key (cb_src_<column>) and last destination key
// (cb_dst_<column>). The planner draws it as Google's binary operators
// page draws a quantified path: a Recursive Union of the first edges (its
// base input) and, for each further edge, a Cross Apply of the previous
// iteration's rows (a Recursive Spool Scan) to the edge table.
type graphQuant struct {
	alias    string
	table    *graphElement
	min, max int
}

// hopsSQL is the SQL for the paths of k edges.
func (q *graphQuant) hopsSQL(k int) string {
	e := q.table
	var cols, froms, conds []string
	for _, c := range e.Source.EdgeCols {
		cols = append(cols, "cb_e1."+c+" AS cb_src_"+c)
	}
	for _, c := range e.Dest.EdgeCols {
		cols = append(cols, fmt.Sprintf("cb_e%d.%s AS cb_dst_%s", k, c, c))
	}
	for i := 1; i <= k; i++ {
		froms = append(froms, fmt.Sprintf("%s AS cb_e%d", e.BaseTableName, i))
		if i > 1 {
			// the edge leaves the node the one before reached
			for j, c := range e.Source.EdgeCols {
				conds = append(conds, fmt.Sprintf("cb_e%d.%s = cb_e%d.%s", i, c, i-1, e.Dest.EdgeCols[j]))
			}
		}
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + strings.Join(froms, ", ") + whereSQL(conds)
}

// pathsSQL is the SQL for the paths of lo to hi edges.
func (q *graphQuant) pathsSQL(lo, hi int) string {
	var parts []string
	for k := lo; k <= hi; k++ {
		parts = append(parts, q.hopsSQL(k))
	}
	return strings.Join(parts, " UNION ALL ")
}

// graphToSQL rewrites a graph query, or each GRAPH_TABLE of a SQL query,
// as SQL.
func graphToSQL(src string, graphs map[string]*graphMeta) (string, error) {
	s, _, err := graphToSQLQuant(src, graphs)
	return s, err
}

// graphToSQLQuant is graphToSQL, with the quantified edge patterns it
// wrote as FROM items, by their lower-case aliases.
func graphToSQLQuant(src string, graphs map[string]*graphMeta) (string, map[string]*graphQuant, error) {
	quants := map[string]*graphQuant{}
	s, err := graphRewrite(src, graphs, quants)
	return s, quants, err
}

func graphRewrite(src string, graphs map[string]*graphMeta, quants map[string]*graphQuant) (string, error) {
	toks, err := lexSQL(src, false)
	if err != nil {
		return "", err
	}
	ps := &sqlParser{src: src, toks: toks, stmtHints: map[string]string{}}
	var out strings.Builder
	pos := 0
	if ps.isP("@{") {
		if _, err := ps.hints(); err != nil {
			return "", err
		}
	}
	if ps.isKw("GRAPH") {
		out.WriteString(src[:ps.peek().start])
		gq := &gqlTranslator{ps: ps, graphs: graphs, quants: quants}
		sql, err := gq.statement()
		if err != nil {
			return "", err
		}
		if ps.isP(";") {
			ps.p++
		}
		if ps.peek().kind != tkEOF {
			return "", ps.errAt("the end of the graph query")
		}
		out.WriteString(sql)
		return out.String(), nil
	}
	for ps.peek().kind != tkEOF {
		if ps.isKw("GRAPH_TABLE") && ps.peekN(1).text == "(" {
			out.WriteString(src[pos:ps.peek().start])
			ps.p += 2
			gq := &gqlTranslator{ps: ps, graphs: graphs, columns: true, quants: quants}
			sql, err := gq.statement()
			if err != nil {
				return "", err
			}
			if err := ps.expectP(")"); err != nil {
				return "", err
			}
			out.WriteString("(" + sql + ")")
			pos = ps.toks[ps.p-1].end
			continue
		}
		ps.p++
	}
	out.WriteString(src[pos:])
	return out.String(), nil
}

// gqlVar is a graph pattern's element variable.
type gqlVar struct {
	name  string // the variable, or a generated one
	alias string // its FROM item's alias
	edge  bool
	label string
	table *graphElement
	quant *graphQuant
}

// gqlEdge links an edge variable to its source and destination nodes.
type gqlEdge struct {
	e, src, dst *gqlVar
}

type gqlTranslator struct {
	ps      *sqlParser
	graphs  map[string]*graphMeta
	columns bool // GRAPH_TABLE: COLUMNS ends the query
	g       *graphMeta
	vars    []*gqlVar
	byName  map[string]*gqlVar
	edges   []gqlEdge
	// conds are SQL conditions on the variables' aliases; pending ones are
	// GQL token ranges rendered once every variable is resolved
	pending [][2]int
	props   []gqlProp
	next    int
	quants  map[string]*graphQuant
	group   map[string]bool // quantified edges' group variables
}

// gqlProp is an inline property filter, {prop: value}.
type gqlProp struct {
	v      *gqlVar
	prop   string
	lo, hi int
}

func (t *gqlTranslator) statement() (string, error) {
	ps := t.ps
	t.byName = map[string]*gqlVar{}
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
	matched := false
	for {
		switch {
		case ps.acceptKw("MATCH"):
			if err := t.match(); err != nil {
				return "", err
			}
			matched = true
		case ps.acceptKw("FILTER"):
			ps.acceptKw("WHERE")
			e, err := ps.expr()
			if err != nil {
				return "", err
			}
			t.pending = append(t.pending, [2]int{e.lo, e.hi})
		case !t.columns && ps.acceptKw("RETURN"), t.columns && ps.acceptKw("COLUMNS"):
			if !matched {
				return "", unsupportedSQL{"a graph query with no MATCH statement"}
			}
			return t.finish()
		default:
			t := ps.peek()
			if t.kind == tkEOF {
				return "", ps.errAt("RETURN")
			}
			return "", unsupportedSQL{fmt.Sprintf("the graph query statement %s", strings.ToUpper(ps.src[t.start:t.end]))}
		}
	}
}

// match reads a MATCH statement's patterns and WHERE clause.
func (t *gqlTranslator) match() error {
	ps := t.ps
	for {
		if err := t.pathPattern(); err != nil {
			return err
		}
		if !ps.acceptP(",") {
			break
		}
	}
	if ps.acceptKw("WHERE") {
		e, err := ps.expr()
		if err != nil {
			return err
		}
		t.pending = append(t.pending, [2]int{e.lo, e.hi})
	}
	return nil
}

var gqlPathPrefixes = map[string]bool{"ANY": true, "ALL": true, "SHORTEST": true, "WALK": true, "TRAIL": true, "ACYCLIC": true, "SIMPLE": true}

func (t *gqlTranslator) pathPattern() error {
	ps := t.ps
	if ps.peek().kind == tkIdent && ps.peekN(1).text == "=" {
		return unsupportedSQL{"a path variable"}
	}
	if ps.peek().kind == tkIdent && gqlPathPrefixes[strings.ToUpper(ps.peek().text)] {
		return unsupportedSQL{"a path search prefix or path mode (" + strings.ToUpper(ps.peek().text) + ")"}
	}
	left, err := t.node()
	if err != nil {
		return err
	}
	for ps.isP("-") || ps.isP("<") {
		pend := len(t.pending)
		e, toRight, err := t.edge()
		if err != nil {
			return err
		}
		if ps.isP("{") || ps.isP("+") || ps.isP("*") {
			if len(t.pending) > pend {
				return unsupportedSQL{"a WHERE clause in a quantified edge pattern"}
			}
			if err := t.quantifier(e); err != nil {
				return err
			}
		}
		right, err := t.node()
		if err != nil {
			return err
		}
		ge := gqlEdge{e: e, src: left, dst: right}
		if !toRight {
			ge.src, ge.dst = right, left
		}
		t.edges = append(t.edges, ge)
		left = right
	}
	return nil
}

// quantifier reads an edge pattern's {m,n} or {n}.
func (t *gqlTranslator) quantifier(e *gqlVar) error {
	ps := t.ps
	if !ps.acceptP("{") {
		return unsupportedSQL{"a path quantifier with no upper bound"}
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
		return ps.errAt("a path quantifier's bound")
	}
	hi := lo
	if ps.acceptP(",") {
		if hi, ok = num(); !ok {
			return unsupportedSQL{"a path quantifier with no upper bound"}
		}
	}
	if err := ps.expectP("}"); err != nil {
		return err
	}
	switch {
	case lo < 1:
		return unsupportedSQL{"a path quantifier with a lower bound of 0"}
	case hi < lo:
		return ps.errAt("a path quantifier's bounds in order")
	}
	if e.name != "" {
		// its group variable is the path's edges, an array of them
		delete(t.byName, strings.ToLower(e.name))
		if t.group == nil {
			t.group = map[string]bool{}
		}
		t.group[strings.ToLower(e.name)] = true
	}
	for _, p := range t.props {
		if p.v == e {
			return unsupportedSQL{"a property filter on a quantified edge pattern"}
		}
	}
	e.quant = &graphQuant{alias: fmt.Sprintf("cb_q%d", len(t.quants)), min: lo, max: hi}
	t.quants[e.quant.alias] = e.quant
	e.alias = e.quant.alias
	return nil
}

// element reads a node's or an edge's filler: [var] [:Label | IS Label]
// [{prop: value, ...}] [WHERE cond], up to close.
func (t *gqlTranslator) element(edge bool, close string) (*gqlVar, error) {
	ps := t.ps
	name, _ := ps.ident()
	label := ""
	if ps.acceptP(":") || ps.acceptKw("IS") {
		l, ok := ps.ident()
		if !ok {
			return nil, unsupportedSQL{"a label expression"}
		}
		label = l
		if ps.isP("|") || ps.isP("&") || ps.isP("!") || ps.isP("%") {
			return nil, unsupportedSQL{"a label expression"}
		}
	} else if ps.isP("!") || ps.isP("%") {
		return nil, unsupportedSQL{"a label expression"}
	}
	v := t.byName[strings.ToLower(name)]
	if name == "" || v == nil {
		v = &gqlVar{name: name, edge: edge, label: label}
		if name == "" {
			v.alias = fmt.Sprintf("cb_g%d", t.next)
			t.next++
		} else {
			v.alias = name
			t.byName[strings.ToLower(name)] = v
		}
		t.vars = append(t.vars, v)
	} else {
		if v.edge != edge {
			return nil, unsupportedSQL{fmt.Sprintf("variable %s, used as a node and an edge", name)}
		}
		if edge {
			return nil, unsupportedSQL{fmt.Sprintf("edge variable %s, used twice", name)}
		}
		if label != "" && v.label != "" && !strings.EqualFold(label, v.label) {
			return nil, unsupportedSQL{fmt.Sprintf("variable %s, with two labels", name)}
		}
		if v.label == "" {
			v.label = label
		}
	}
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
			t.props = append(t.props, gqlProp{v: v, prop: p, lo: e.lo, hi: e.hi})
			if !ps.acceptP(",") {
				if err := ps.expectP("}"); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	if ps.acceptKw("WHERE") {
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		t.pending = append(t.pending, [2]int{e.lo, e.hi})
	}
	if err := ps.expectP(close); err != nil {
		return nil, err
	}
	return v, nil
}

func (t *gqlTranslator) node() (*gqlVar, error) {
	if err := t.ps.expectP("("); err != nil {
		return nil, err
	}
	return t.element(false, ")")
}

// edge reads -[...]->, <-[...]-, -> or <-; it says whether the edge points
// to the right.
func (t *gqlTranslator) edge() (*gqlVar, bool, error) {
	ps := t.ps
	fromLeft := ps.acceptP("-")
	if !fromLeft {
		ps.p++ // <
		if err := ps.expectP("-"); err != nil {
			return nil, false, err
		}
	}
	var v *gqlVar
	if ps.acceptP("[") {
		var err error
		if v, err = t.element(true, "]"); err != nil {
			return nil, false, err
		}
		if err := ps.expectP("-"); err != nil {
			return nil, false, err
		}
	} else {
		v = &gqlVar{edge: true, alias: fmt.Sprintf("cb_g%d", t.next)}
		t.next++
		t.vars = append(t.vars, v)
	}
	toRight := ps.acceptP(">")
	if fromLeft != toRight {
		// -[]- and <-[]->
		return nil, false, unsupportedSQL{"an edge pattern in either direction"}
	}
	return v, toRight, nil
}

// resolve gives every variable its element table: by its label, else by
// the edges it is on.
func (t *gqlTranslator) resolve() error {
	tables := func(edge bool) []graphElement {
		if edge {
			return t.g.EdgeTables
		}
		return t.g.NodeTables
	}
	byName := func(edge bool, name string) *graphElement {
		ts := tables(edge)
		for i := range ts {
			if strings.EqualFold(ts[i].Name, name) {
				return &ts[i]
			}
		}
		return nil
	}
	for _, v := range t.vars {
		if v.label == "" {
			continue
		}
		var found []*graphElement
		ts := tables(v.edge)
		for i := range ts {
			if ts[i].hasLabel(v.label) {
				found = append(found, &ts[i])
			}
		}
		switch len(found) {
		case 0:
			return unsupportedSQL{fmt.Sprintf("label %s, which no element table has", v.label)}
		case 1:
			v.table = found[0]
		default:
			return unsupportedSQL{fmt.Sprintf("label %s, which more than one element table has", v.label)}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, e := range t.edges {
			if e.e.table == nil && (e.src.table != nil || e.dst.table != nil) {
				var found []*graphElement
				for i := range t.g.EdgeTables {
					et := &t.g.EdgeTables[i]
					if (e.src.table == nil || strings.EqualFold(et.Source.Node, e.src.table.Name)) &&
						(e.dst.table == nil || strings.EqualFold(et.Dest.Node, e.dst.table.Name)) {
						found = append(found, et)
					}
				}
				if len(found) == 1 {
					e.e.table, changed = found[0], true
				}
			}
			if e.e.table != nil {
				if n := byName(false, e.e.table.Source.Node); e.src.table == nil && n != nil {
					e.src.table, changed = n, true
				}
				if n := byName(false, e.e.table.Dest.Node); e.dst.table == nil && n != nil {
					e.dst.table, changed = n, true
				}
			}
		}
	}
	for _, v := range t.vars {
		if v.table == nil && !v.edge && len(t.g.NodeTables) == 1 {
			v.table = &t.g.NodeTables[0]
		}
		if v.table == nil {
			what := "node"
			if v.edge {
				what = "edge"
			}
			return unsupportedSQL{fmt.Sprintf("an unlabeled %s pattern that matches more than one element table", what)}
		}
	}
	for _, e := range t.edges {
		if !strings.EqualFold(e.e.table.Source.Node, e.src.table.Name) || !strings.EqualFold(e.e.table.Dest.Node, e.dst.table.Name) {
			return unsupportedSQL{fmt.Sprintf("an edge pattern of %s between nodes it does not link", e.e.table.Name)}
		}
	}
	return nil
}

// render is GQL tokens [lo, hi) as SQL: var.property becomes the FROM
// item's column.
func (t *gqlTranslator) render(lo, hi int) (string, error) {
	ps := t.ps
	if hi <= lo {
		return "", nil
	}
	var b strings.Builder
	pos := ps.toks[lo].start
	for i := lo; i < hi; i++ {
		tk := ps.toks[i]
		if tk.kind != tkIdent && tk.kind != tkQIdent {
			continue
		}
		v := t.byName[strings.ToLower(tk.text)]
		if t.group[strings.ToLower(tk.text)] && !(i > lo && ps.toks[i-1].text == ".") {
			return "", unsupportedSQL{fmt.Sprintf("group variable %s, the edges of a quantified path", tk.text)}
		}
		if v == nil {
			continue
		}
		// a field of something else, s.x.y's x
		if i > lo && ps.toks[i-1].text == "." {
			continue
		}
		if i+2 >= hi || ps.toks[i+1].text != "." || ps.toks[i+2].kind != tkIdent && ps.toks[i+2].kind != tkQIdent {
			return "", unsupportedSQL{fmt.Sprintf("graph element %s used as a value", tk.text)}
		}
		col, err := v.table.column(ps.toks[i+2].text)
		if err != nil {
			return "", err
		}
		b.WriteString(ps.src[pos:tk.start])
		b.WriteString(v.alias + "." + col)
		pos = ps.toks[i+2].end
		i += 2
	}
	b.WriteString(ps.src[pos:ps.toks[hi-1].end])
	return b.String(), nil
}

// finish reads RETURN's (COLUMNS') list and what follows it, and writes
// the SQL query.
func (t *gqlTranslator) finish() (string, error) {
	ps := t.ps
	if err := t.resolve(); err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("SELECT ")
	if t.columns {
		if err := ps.expectP("("); err != nil {
			return "", err
		}
	} else if ps.acceptKw("DISTINCT") {
		sb.WriteString("DISTINCT ")
	}
	if ps.isP("*") {
		return "", unsupportedSQL{"RETURN *"}
	}
	for i := 0; ; i++ {
		e, err := ps.expr()
		if err != nil {
			return "", err
		}
		s, err := t.render(e.lo, e.hi)
		if err != nil {
			return "", err
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(s)
		alias := ""
		if ps.acceptKw("AS") {
			a, ok := ps.ident()
			if !ok {
				return "", ps.errAt("an alias")
			}
			alias = a
		} else if e.kind == exCol && len(e.path) == 2 {
			// a property's column is named after the property
			alias = e.path[1]
		}
		if alias != "" {
			sb.WriteString(" AS " + alias)
		}
		if !ps.acceptP(",") {
			break
		}
	}
	if t.columns {
		if err := ps.expectP(")"); err != nil {
			return "", err
		}
	}
	sb.WriteString(" FROM ")
	for i, v := range t.vars {
		if i > 0 {
			sb.WriteString(", ")
		}
		if q := v.quant; q != nil {
			q.table = v.table
			if !strings.EqualFold(v.table.Source.Node, v.table.Dest.Node) && q.max > 1 {
				return "", unsupportedSQL{fmt.Sprintf("a quantified edge pattern of %s, whose edges do not chain", v.table.Name)}
			}
			sb.WriteString("(" + q.pathsSQL(q.min, q.max) + ") AS " + v.alias)
			continue
		}
		sb.WriteString(v.table.BaseTableName + " AS " + v.alias)
	}
	var conds []string
	for _, e := range t.edges {
		src, dst := e.e.alias+".", e.e.alias+"."
		if e.e.quant != nil {
			src, dst = e.e.alias+".cb_src_", e.e.alias+".cb_dst_"
		}
		for i, c := range e.e.table.Source.EdgeCols {
			conds = append(conds, src+c+" = "+e.src.alias+"."+e.e.table.Source.NodeCols[i])
		}
		for i, c := range e.e.table.Dest.EdgeCols {
			conds = append(conds, dst+c+" = "+e.dst.alias+"."+e.e.table.Dest.NodeCols[i])
		}
	}
	for _, p := range t.props {
		col, err := p.v.table.column(p.prop)
		if err != nil {
			return "", err
		}
		val, err := t.render(p.lo, p.hi)
		if err != nil {
			return "", err
		}
		conds = append(conds, p.v.alias+"."+col+" = ("+val+")")
	}
	for _, r := range t.pending {
		s, err := t.render(r[0], r[1])
		if err != nil {
			return "", err
		}
		conds = append(conds, "("+s+")")
	}
	if len(conds) > 0 {
		sb.WriteString(" WHERE " + strings.Join(conds, " AND "))
	}
	if t.columns {
		return sb.String(), nil
	}
	tail := func(kw ...string) (bool, error) {
		if !ps.acceptKw(kw...) {
			return false, nil
		}
		sb.WriteString(" " + strings.Join(kw, " ") + " ")
		for {
			e, err := ps.expr()
			if err != nil {
				return true, err
			}
			s, err := t.render(e.lo, e.hi)
			if err != nil {
				return true, err
			}
			sb.WriteString(s)
			if ps.acceptKw("ASC") {
				sb.WriteString(" ASC")
			} else if ps.acceptKw("DESC") {
				sb.WriteString(" DESC")
			}
			if !ps.acceptP(",") {
				return true, nil
			}
			sb.WriteString(", ")
		}
	}
	for _, kw := range [][]string{{"GROUP", "BY"}, {"ORDER", "BY"}, {"LIMIT"}, {"OFFSET"}} {
		if _, err := tail(kw...); err != nil {
			return "", err
		}
	}
	if ps.isKw("SKIP") {
		if _, err := tail("SKIP"); err != nil {
			return "", err
		}
		return strings.Replace(sb.String(), " SKIP ", " OFFSET ", 1), nil
	}
	if !ps.isP(";") && ps.peek().kind != tkEOF {
		return "", unsupportedSQL{fmt.Sprintf("the graph query statement %s", strings.ToUpper(ps.peek().text))}
	}
	return sb.String(), nil
}
