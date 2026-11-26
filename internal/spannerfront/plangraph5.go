package spannerfront

// Graph elements, group variables and paths as values in graph queries'
// plans (#2029). Spanner Graph's functions of an element (ELEMENT_ID,
// SOURCE_NODE_ID, DESTINATION_NODE_ID, LABELS, PROPERTY_NAMES, TO_JSON)
// and its IS LABELED predicate read the element's table and key; a path's
// (NODES, EDGES, IS_ACYCLIC, IS_TRAIL, IS_SIMPLE, PATH_FIRST, PATH_LAST)
// read its elements in order. The plan reads the same rows whatever these
// values' text, so the translation writes an element's value as its
// identity, a string of its element table and key: equal for the same
// element and different for others, as ELEMENT_ID is, so a comparison, a
// DISTINCT or a GROUP BY over it keeps the rows Spanner keeps. A path's
// nodes and edges are arrays of those identities, its quantified edges'
// (cb_pn, cb_ps, cb_pe) written by graphQuant.
//
// The path modes of a path with a quantified edge and other edges, and
// SIMPLE, hold the path's identities distinct: TRAIL its edges, ACYCLIC
// its nodes, SIMPLE its nodes but for the first being the last.

import (
	"strings"
)

// idSQL is the identity of the element of table name whose key is vals.
func idSQL(name string, vals []string) string {
	parts := []string{"'" + name + "'"}
	for _, v := range vals {
		parts = append(parts, "FORMAT('%T', "+v+")")
	}
	return "CONCAT(" + strings.Join(parts, ", '|', ") + ")"
}

// emptyIDs is an empty array of identities.
const emptyIDs = "ARRAY<STRING>[]"

// elemIDSQL is an element's identity.
func elemIDSQL(b *gqlBind) string {
	one := func(e *graphElement) string {
		var vals []string
		for _, c := range e.KeyColumns {
			vals = append(vals, b.col(c))
		}
		return idSQL(e.Name, vals)
	}
	return perTable(b, one)
}

// endpointKeyVals are the values of node table n's key, in its order, an
// edge's endpoint ep references, read by col.
func endpointKeyVals(n *graphElement, ep *graphEndpoint, col func(string) string) []string {
	var vals []string
	if n != nil && len(n.KeyColumns) == len(ep.NodeCols) {
		for _, k := range n.KeyColumns {
			for j, nc := range ep.NodeCols {
				if strings.EqualFold(nc, k) {
					vals = append(vals, col(ep.EdgeCols[j]))
				}
			}
		}
		if len(vals) == len(n.KeyColumns) {
			return vals
		}
		vals = nil
	}
	for _, c := range ep.EdgeCols {
		vals = append(vals, col(c))
	}
	return vals
}

// endpointIDSQL is SOURCE_NODE_ID(edge) (source) or DESTINATION_NODE_ID.
func (t *gqlTranslator) endpointIDSQL(b *gqlBind, source bool) string {
	one := func(e *graphElement) string {
		ep := e.Dest
		if source {
			ep = e.Source
		}
		if ep == nil {
			return "NULL"
		}
		return idSQL(ep.Node, endpointKeyVals(t.nodeTable(ep.Node), ep, b.col))
	}
	return perTable(b, one)
}

// perTable is the SQL f writes for each of an element's tables.
func perTable(b *gqlBind, f func(e *graphElement) string) string {
	if b.table != nil {
		return f(b.table)
	}
	if len(b.tables) == 0 {
		// an OPTIONAL MATCH's element that nothing matched: NULL
		return "NULL"
	}
	var w []string
	for _, e := range b.tables {
		w = append(w, "WHEN '"+e.Name+"' THEN "+f(e))
	}
	return "(CASE " + b.qual + "." + b.prefix + "__t " + strings.Join(w, " ") + " END)"
}

// propertyNamesSQL is PROPERTY_NAMES(element).
func propertyNamesSQL(b *gqlBind) string {
	return perTable(b, func(e *graphElement) string {
		var ns []string
		for _, p := range e.PropertyDefs {
			ns = append(ns, "'"+p.Name+"'")
		}
		return "ARRAY<STRING>[" + strings.Join(ns, ", ") + "]"
	})
}

// isLabeledSQL is element IS LABELED label.
func isLabeledSQL(b *gqlBind, l *gqlLabel) string {
	return perTable(b, func(e *graphElement) string {
		if l.matches(e) {
			return "TRUE"
		}
		return "FALSE"
	})
}

// elemJSONSQL is TO_JSON's argument for an element: its identity and its
// labels.
func elemJSONSQL(b *gqlBind) string {
	return "STRUCT(" + elemIDSQL(b) + " AS identifier, " + labelsSQL(b) + " AS labels)"
}

// idItem is one of a path's elements' identities (one), or a quantified
// edge's run of them (arr), whose own all being different is flag.
type idItem struct {
	one, arr, flag string
}

// idList is a path's nodes' or edges' identities, in order.
type idList []idItem

// array is the identities as one array.
func (l idList) array() string {
	var parts []string
	for _, x := range l {
		if x.one != "" {
			parts = append(parts, "["+x.one+"]")
		} else {
			parts = append(parts, x.arr)
		}
	}
	return concatIDs(parts)
}

// distinct is the condition that no identity is there twice, written
// with no subquery (a GROUP BY of it must match its select list's).
func (l idList) distinct() string {
	var cs []string
	for i, x := range l {
		if x.arr != "" {
			cs = append(cs, x.flag)
		}
		for _, y := range l[i+1:] {
			switch {
			case x.one != "" && y.one != "":
				cs = append(cs, x.one+" != "+y.one)
			case x.one != "":
				cs = append(cs, "NOT ARRAY_INCLUDES("+y.arr+", "+x.one+")")
			case y.one != "":
				cs = append(cs, "NOT ARRAY_INCLUDES("+x.arr+", "+y.one+")")
			default:
				cs = append(cs, "NOT ARRAY_INCLUDES_ANY("+x.arr+", "+y.arr+")")
			}
		}
	}
	if len(cs) == 0 {
		return "TRUE"
	}
	return "(" + strings.Join(cs, " AND ") + ")"
}

// simple is SIMPLE's condition on a path's nodes, whose first and last
// are first and last: all different, or the first the last and the rest
// all different.
func (l idList) simple(first, last string) string {
	if len(l) < 2 {
		return "TRUE"
	}
	return "(" + l.distinct() + " OR " + first + " = " + last + " AND " + l[1:].distinct() + ")"
}

// pairwiseDistinct is the condition that ids are all different.
func pairwiseDistinct(ids []string) string {
	var l idList
	for _, x := range ids {
		l = append(l, idItem{one: x})
	}
	return l.distinct()
}

// concatIDs is the identities of parts, in order.
func concatIDs(parts []string) string {
	if len(parts) == 0 {
		return emptyIDs
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "ARRAY_CONCAT(" + strings.Join(parts, ", ") + ")"
}

// pathElements are a path's nodes' and edges' identities, in order: each
// node pattern's, but for the node a quantified edge reaches, which its
// identities end with (or, with no edge, is the node before it).
func (t *gqlTranslator) pathElements(pat *gqlPattern, p gqlPath, bindOf func(*gqlPatVar) *gqlBind) (nodes, edges idList) {
	for k, n := range p.nodes {
		reached := false
		if k > 0 && k-1 < len(p.edges) {
			if gb := bindOf(pat.edges[p.edges[k-1]].e); gb != nil && gb.kind == bGroup {
				reached = true
			}
		}
		if !reached {
			if b := bindOf(n); b != nil && b.kind == bElem {
				nodes = append(nodes, idItem{one: elemIDSQL(b)})
			}
		}
		if k >= len(p.edges) {
			continue
		}
		pe := pat.edges[p.edges[k]]
		gb := bindOf(pe.e)
		if gb == nil {
			continue
		}
		if gb.kind == bGroup {
			gb.quant.withPath = true
			edges = append(edges, idItem{arr: gb.alias + ".cb_pe", flag: gb.alias + ".cb_ped"})
			if pe.dir == -1 && gb.quant.seg == nil {
				// written right to left: its hops leave the nodes after the start
				nodes = append(nodes, idItem{arr: gb.alias + ".cb_ps", flag: gb.alias + ".cb_psd"})
			} else {
				nodes = append(nodes, idItem{arr: gb.alias + ".cb_pn", flag: gb.alias + ".cb_pnd"})
			}
			continue
		}
		edges = append(edges, idItem{one: elemIDSQL(gb)})
	}
	return nodes, edges
}

// pathMode holds a path's elements to its mode by their identities.
func (t *gqlTranslator) pathMode(mode string, pat *gqlPattern, p gqlPath, bindOf func(*gqlPatVar) *gqlBind) string {
	nodes, edges := t.pathElements(pat, p, bindOf)
	switch mode {
	case "TRAIL":
		return edges.distinct()
	case "ACYCLIC":
		return nodes.distinct()
	case "SIMPLE":
		first, last := bindOf(p.nodes[0]), bindOf(p.nodes[len(p.nodes)-1])
		return nodes.simple(elemIDSQL(first), elemIDSQL(last))
	}
	return "TRUE"
}

// pathFuncsSQL are a path's NODES, EDGES, IS_ACYCLIC, IS_TRAIL and
// IS_SIMPLE.
func (t *gqlTranslator) pathFuncsSQL(pat *gqlPattern, p gqlPath, bindOf func(*gqlPatVar) *gqlBind) map[string]string {
	nodes, edges := t.pathElements(pat, p, bindOf)
	return map[string]string{"NODES": nodes.array(), "EDGES": edges.array(), "IS_ACYCLIC": t.pathMode("ACYCLIC", pat, p, bindOf),
		"IS_TRAIL": t.pathMode("TRAIL", pat, p, bindOf), "IS_SIMPLE": t.pathMode("SIMPLE", pat, p, bindOf)}
}

// pathFuncSuffix is each path function's column of a derived table.
var pathFuncSuffix = map[string]string{"NODES": "fn", "EDGES": "fe", "IS_ACYCLIC": "fa", "IS_TRAIL": "ft", "IS_SIMPLE": "fs"}

// pathElementsUsed says whether the query reads a path variable's
// elements under either of its names (NODES(p), IS_TRAIL(p), ...).
func (t *gqlTranslator) pathElementsUsed(names ...string) bool {
	toks := t.ps.toks
	for i := 0; i+3 < len(toks); i++ {
		if pathFuncs[strings.ToUpper(toks[i].text)] && toks[i+1].text == "(" && toks[i+3].text == ")" {
			for _, n := range names {
				if strings.EqualFold(toks[i+2].text, n) {
					return true
				}
			}
		}
	}
	return false
}

// replaceQual moves a derived table's columns to another alias.
func replaceQual(m map[string]string, from, to string) map[string]string {
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		out[k] = strings.Replace(v, from+".", to+".", 1)
	}
	return out
}

// pathFuncs are the functions of a path that read its elements.
var pathFuncs = map[string]bool{"NODES": true, "EDGES": true, "IS_ACYCLIC": true, "IS_TRAIL": true, "IS_SIMPLE": true}

// subPattern is the pattern of some of a MATCH's path patterns: their
// variables, edges and inline filters, and, with conds, the MATCH's
// conditions.
func (pat *gqlPattern) subPattern(paths []int, conds bool) *gqlPattern {
	sub := &gqlPattern{byName: map[string]*gqlPatVar{}}
	in := map[*gqlPatVar]bool{}
	add := func(v *gqlPatVar) {
		if !in[v] {
			in[v] = true
			sub.vars = append(sub.vars, v)
			if v.name != "" {
				sub.byName[v.name] = v
			}
		}
	}
	for _, i := range paths {
		p := pat.paths[i]
		np := p
		np.edges = nil
		for _, n := range p.nodes {
			add(n)
		}
		for _, ei := range p.edges {
			e := pat.edges[ei]
			add(e.e)
			add(e.left)
			add(e.right)
			sub.edges = append(sub.edges, e)
			np.edges = append(np.edges, len(sub.edges)-1)
		}
		sub.paths = append(sub.paths, np)
	}
	for _, p := range pat.props {
		if in[p.v] {
			sub.props = append(sub.props, p)
		}
	}
	for _, s := range pat.same {
		if in[s[0]] && in[s[1]] {
			sub.same = append(sub.same, s)
		}
	}
	for _, sp := range pat.subModes {
		ok := true
		for _, n := range sp.nodes {
			ok = ok && in[n]
		}
		if ok {
			// its edges, by their index in the sub-pattern
			np := sp
			np.edges = nil
			for _, ei := range sp.edges {
				for k, e := range sub.edges {
					if e == pat.edges[ei] {
						np.edges = append(np.edges, k)
					}
				}
			}
			sub.subModes = append(sub.subModes, np)
		}
	}
	if conds {
		sub.conds = pat.conds
	}
	return sub
}
