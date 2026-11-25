package spannerfront

// Quantified path patterns, path costs and path values in graph queries'
// plans (#2029).
//
// A quantified path pattern of one edge, ((src)-[e]->(dst) WHERE c){m,n}
// (Google's graph patterns page: "((a:Account)-[t:Transfers]->
// (b:Account)){1,3}"), matches the same paths as the quantified edge
// -[e]->{m,n} whose every edge has its endpoints held to the node
// patterns' labels and filters and to the subpath's WHERE; the planner
// draws it as a quantified edge's Recursive Union, each edge's conditions
// on its endpoints a semi join (EXISTS) to their node tables. Its nodes
// are group variables outside it; a query that reads them there is
// refused by name, as is a subpath of more than one edge.
//
// ANY CHEAPEST keeps, per pair of endpoints, a path of the least total
// COST, as ANY SHORTEST keeps one of the fewest edges. TO_JSON(p) of a
// path variable reads the path's elements' keys.

import (
	"fmt"
	"strings"
)

// quantifiedSubpath says whether the "(" ahead opens a parenthesized path
// that a quantifier follows.
func (t *gqlTranslator) quantifiedSubpath() bool {
	ps := t.ps
	depth := 0
	for i := ps.p; i < len(ps.toks); i++ {
		switch ps.toks[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i+1 < len(ps.toks) && ps.toks[i+1].kind != tkEOF && ps.toks[i+1].text == "{"
			}
		}
		if ps.toks[i].kind == tkEOF {
			return false
		}
	}
	return false
}

// quantSubpath reads ((src)-[e]->(dst) [WHERE c]){m,n} as the quantified
// edge e, its endpoints' patterns and the WHERE held on each of its edges.
func (t *gqlTranslator) quantSubpath(pat *gqlPattern, path *gqlPath, last **gqlPatVar) error {
	ps := t.ps
	lo := ps.p
	nv, np, nc := len(pat.vars), len(pat.props), len(pat.conds)
	ps.p++ // (
	if ps.peek().kind == tkIdent && gqlPathModes[strings.ToUpper(ps.peek().text)] {
		return unsupportedSQL{"a path mode in a quantified path pattern"}
	}
	if !ps.isP("(") {
		return unsupportedSQL{"a quantified path pattern that does not begin with a node pattern"}
	}
	x, err := t.element(pat, false, "(", ")")
	if err != nil {
		return err
	}
	if !ps.isP("-") && !ps.isP("<") {
		return unsupportedSQL{"a quantified path pattern of no edge"}
	}
	e, dir, err := t.edge(pat)
	if err != nil {
		return err
	}
	if e.quant != nil {
		return unsupportedSQL{"a quantified edge in a quantified path pattern"}
	}
	if dir == 0 {
		return unsupportedSQL{"an edge in either direction in a quantified path pattern"}
	}
	if !ps.isP("(") {
		return unsupportedSQL{"a quantified path pattern that does not end with a node pattern"}
	}
	y, err := t.element(pat, false, "(", ")")
	if err != nil {
		return err
	}
	var where [][2]int
	if ps.acceptKw("WHERE") {
		w, err := ps.expr()
		if err != nil {
			return err
		}
		where = append(where, [2]int{w.lo, w.hi})
	}
	if !ps.isP(")") {
		return unsupportedSQL{"a quantified path pattern of more than one edge"}
	}
	ps.p++
	q, err := t.quantifier()
	if err != nil {
		return err
	}
	hi := ps.p
	// its variables are its own: they are group variables outside it
	for _, v := range []*gqlPatVar{x, e, y} {
		if indexOfVar(pat.vars[:nv], v) >= 0 {
			return unsupportedSQL{fmt.Sprintf("variable %s, in a quantified path pattern and outside it", v.name)}
		}
	}
	for _, v := range []*gqlPatVar{x, y} {
		if v.name != "" && t.namedOutside(v.name, lo, hi) {
			return unsupportedSQL{fmt.Sprintf("group variable %s, a node of a quantified path pattern, used outside it", v.name)}
		}
	}
	for _, p := range pat.props[np:] {
		if p.v == e {
			q.props = append(q.props, p)
		} else {
			q.nodeProps = append(q.nodeProps, p)
		}
	}
	q.nodeConds = append(append(q.nodeConds, pat.conds[nc:]...), where...)
	q.cost = e.cost
	q.src, q.dst = x, y
	pat.props, pat.conds = pat.props[:np], pat.conds[:nc]
	e.cost = [2]int{}
	if dir == -1 {
		// written right to left: the edges run from dst to src
		q.src, q.dst = y, x
	}
	e.quant = q
	var kept []*gqlPatVar
	for _, v := range pat.vars {
		if v != x && v != y {
			kept = append(kept, v)
		}
	}
	pat.vars = kept
	for _, v := range []*gqlPatVar{x, y} {
		if v.name != "" {
			delete(pat.byName, v.name)
		}
	}
	// the node before it is its first edge's: juxtaposed, or a new one
	if *last == nil || t.precededByEdge {
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
	t.pendingEdge = &gqlPatEdge{e: e, left: *last, dir: dir}
	t.precededByEdge = true
	return nil
}

func indexOfVar(vs []*gqlPatVar, v *gqlPatVar) int {
	for i, w := range vs {
		if w == v {
			return i
		}
	}
	return -1
}

// namedOutside says whether the statement names name outside tokens
// [lo, hi), other than as a property after ".".
func (t *gqlTranslator) namedOutside(name string, lo, hi int) bool {
	for i, tk := range t.ps.toks {
		if i >= lo && i < hi || !isNameTok(tk) || i > 0 && t.ps.toks[i-1].text == "." {
			continue
		}
		if strings.EqualFold(tk.text, name) {
			return true
		}
	}
	return false
}

// labelsMatch says whether element table e has every label expression.
func labelsMatch(ls []*gqlLabel, e *graphElement) bool {
	for _, l := range ls {
		if !l.matches(e) {
			return false
		}
	}
	return true
}

// subpathHopCond is the condition each edge of a quantified path pattern
// (as cb_hop) holds its endpoints to: a semi join to their node tables,
// where the node patterns' filters and the subpath's WHERE are evaluated.
func (t *gqlTranslator) subpathHopCond(hb *gqlBranch, gb *gqlBind, q *gqlQuantSpec) (string, error) {
	et := gb.table
	src, dst := t.nodeTable(et.Source.Node), t.nodeTable(et.Dest.Node)
	if src == nil || dst == nil || !labelsMatch(q.src.labels, src) || !labelsMatch(q.dst.labels, dst) {
		return "FALSE", nil
	}
	sb := &gqlBind{kind: bElem, alias: "cb_hsrc", table: src}
	db := &gqlBind{kind: bElem, alias: "cb_hdst", table: dst}
	nb := hb.copy()
	if q.src.name != "" {
		nb.binds[q.src.name] = sb
	}
	if q.dst.name != "" && q.dst != q.src {
		nb.binds[q.dst.name] = db
	}
	var conds []string
	for j, c := range et.Source.EdgeCols {
		conds = append(conds, "cb_hsrc."+et.Source.NodeCols[j]+" = cb_hop."+c)
	}
	for j, c := range et.Dest.EdgeCols {
		conds = append(conds, "cb_hdst."+et.Dest.NodeCols[j]+" = cb_hop."+c)
	}
	if q.src == q.dst {
		// one node pattern at both ends: each edge is a loop
		for j, c := range et.Source.EdgeCols {
			conds = append(conds, "cb_hop."+c+" = cb_hop."+et.Dest.EdgeCols[j])
		}
	}
	for _, p := range q.nodeProps {
		b := sb
		if p.v == q.dst && q.dst != q.src {
			b = db
		}
		s, err := t.propCond(nb, b, p)
		if err != nil {
			return "", err
		}
		conds = append(conds, s)
	}
	for _, c := range q.nodeConds {
		s, err := t.render(nb, c[0], c[1])
		if err != nil {
			return "", err
		}
		conds = append(conds, "("+s+")")
	}
	return "EXISTS (SELECT 1 FROM " + src.BaseTableName + " AS cb_hsrc, " + dst.BaseTableName + " AS cb_hdst WHERE " +
		strings.Join(conds, " AND ") + ")", nil
}

// hasCost says whether an edge pattern has a COST expression.
func hasCost(c [2]int) bool { return c[1] > c[0] }

// elemKeySQL is an element's identity: its table and key, as a string.
func elemKeySQL(b *gqlBind) string {
	var ks []string
	for _, c := range b.table.KeyColumns {
		ks = append(ks, b.col(c))
	}
	return "FORMAT('%T', STRUCT('" + b.table.Name + "' AS t, " + strings.Join(ks, ", ") + "))"
}

// pathValueSQL is a path's value, TO_JSON(p)'s argument as the plan reads
// it: its elements' identities, in order.
func pathValueSQL(parts []string) string {
	return "ARRAY_CONCAT(" + strings.Join(parts, ", ") + ")"
}

// propertyExistsSQL is PROPERTY_EXISTS(element, property): whether its
// element table defines the property (the engine has already refused a
// property the graph does not have).
func propertyExistsSQL(b *gqlBind, prop string) string {
	has := func(e *graphElement) string {
		if e.hasProperty(prop) {
			return "TRUE"
		}
		return "FALSE"
	}
	if b.table != nil {
		return has(b.table)
	}
	var w []string
	for _, e := range b.tables {
		w = append(w, "WHEN '"+e.Name+"' THEN "+has(e))
	}
	return "(CASE " + b.qual + "." + b.prefix + "__t " + strings.Join(w, " ") + " END)"
}

// pathUsedAsValue says whether the query reads a path variable's value
// (TO_JSON(p)), under either of its names.
func (t *gqlTranslator) pathUsedAsValue(names ...string) bool {
	toks := t.ps.toks
	for i := 0; i+3 < len(toks); i++ {
		fn := strings.ToUpper(toks[i].text)
		if (fn == "TO_JSON" || fn == "SAFE_TO_JSON") && toks[i+1].text == "(" && toks[i+3].text == ")" {
			for _, n := range names {
				if strings.EqualFold(toks[i+2].text, n) {
					return true
				}
			}
		}
	}
	return false
}

// sameSQL is SAME(a, b, ...) (every element the same) or ALL_DIFFERENT
// (no two the same).
func sameSQL(els []*gqlBind, different bool) string {
	var cs []string
	for i := range els {
		for j := i + 1; j < len(els); j++ {
			if different {
				cs = append(cs, "NOT "+elemEqual(els[i], els[j]))
			} else if j == i+1 {
				cs = append(cs, elemEqual(els[i], els[j]))
			}
		}
	}
	if len(cs) == 0 {
		return "TRUE"
	}
	return "(" + strings.Join(cs, " AND ") + ")"
}

// endpointSQL is node IS SOURCE OF edge (source) or IS DESTINATION OF.
func endpointSQL(n, e *gqlBind, source bool) (string, error) {
	if n.table == nil || e.table == nil {
		return "", unsupportedSQL{"IS SOURCE OF or IS DESTINATION OF an element of several element tables"}
	}
	ep := e.table.Dest
	if source {
		ep = e.table.Source
	}
	if ep == nil || !strings.EqualFold(ep.Node, n.table.Name) {
		return "FALSE", nil
	}
	var cs []string
	for j, c := range ep.EdgeCols {
		cs = append(cs, e.col(c)+" = "+n.col(ep.NodeCols[j]))
	}
	return "(" + strings.Join(cs, " AND ") + ")", nil
}
