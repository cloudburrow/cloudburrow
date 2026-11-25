package spannerfront

// Quantified path patterns of several edges in graph queries' plans
// (#2029). Google's graph patterns page: a quantified path pattern,
// ((a)-[t]->(b)-[u]->(c) WHERE ...){m,n}, repeats its subpath m to n
// times, each repetition starting at the node the one before reached;
// its variables are group variables outside it, one element per
// repetition; a path mode inside it (TRAIL, ACYCLIC, SIMPLE) holds each
// repetition's own elements (measured on the emulator: the mode inside
// the parentheses does not reach across repetitions, as one before the
// path does).
//
// The translation reads each repetition as a row of the subpath's matches
// (quantSeg.rowsSQL): its node and edge tables joined on their keys, with
// its filters and mode, one SELECT per consistent choice of element
// tables; the quantified path is then a quantified edge over those rows,
// a graphQuant whose table is a stand-in element table (cb_seg) with the
// first node's key as its source and the last's as its destination. The
// planner draws it as it draws a quantified edge, a Recursive Union, whose
// base and recursive inputs read the subpath's tables in turn: the first
// node, then each edge and the node it reaches (a Cross Apply seeking on
// the key each reads), a Union All of those reads for several choices.

import (
	"fmt"
	"sort"
	"strings"
)

// quantSeg is a quantified path pattern read as rows of its subpath.
type quantSeg struct {
	mode  string       // the path mode inside it; "" for WALK
	nodes []*gqlPatVar // its node patterns, n0 ... nL
	edges []*gqlPatVar // its edge patterns, e1 ... eL
	dirs  []int        // each edge's direction: 1, -1 or 0 (either)
	props []gqlProp
	conds [][2]int
	names []string // its named variables, group variables outside it
	// variants are its consistent choices of element tables, edges
	// oriented
	variants []segVariant
	t        *gqlTranslator
}

type segVariant struct {
	nodes []*graphElement
	edges []*graphElement
	dirs  []int // 1 or -1
	el    *graphElement
}

// segRow is a stand-in element table: a variant group's rows.
type segRow struct {
	variants []*segVariant
}

func segNodeAlias(j int) string { return fmt.Sprintf("cb_sn%d", j) }
func segEdgeAlias(j int) string { return fmt.Sprintf("cb_sx%d", j) }

// isQuantSegment says whether a parsed quantified path pattern needs the
// subpath reading: more than one edge, a path mode, an edge either way, or
// a node variable read outside it.
func (t *gqlTranslator) isQuantSegment(mode string, nodes, edges []*gqlPatVar, dirs []int, lo, hi int) bool {
	if mode != "" || len(edges) != 1 || dirs[0] == 0 {
		return true
	}
	for _, n := range nodes {
		if n.name != "" && t.namedOutside(n.name, lo, hi) {
			return true
		}
	}
	return false
}

// buildSeg chooses the subpath's element tables and makes its stand-in
// tables, one for each pair of first and last node tables.
func (t *gqlTranslator) buildSeg(sg *quantSeg) ([]*graphElement, error) {
	L := len(sg.edges)
	nodeCands := make([][]*graphElement, L+1)
	for j, n := range sg.nodes {
		for _, e := range t.tablesOf(false) {
			if labelsMatch(n.labels, e) {
				nodeCands[j] = append(nodeCands[j], e)
			}
		}
	}
	edgeCands := make([][]*graphElement, L)
	for j, e := range sg.edges {
		for _, x := range t.tablesOf(true) {
			if labelsMatch(e.labels, x) {
				edgeCands[j] = append(edgeCands[j], x)
			}
		}
	}
	// the same variable twice in the subpath is one element table
	same := func(v segVariant) bool {
		seen := map[*gqlPatVar]*graphElement{}
		for j, n := range sg.nodes {
			if n.name == "" {
				continue
			}
			if e, ok := seen[n]; ok && e != v.nodes[j] {
				return false
			}
			seen[n] = v.nodes[j]
		}
		return true
	}
	var out []segVariant
	cur := segVariant{nodes: make([]*graphElement, L+1), edges: make([]*graphElement, L), dirs: make([]int, L)}
	var walk func(j int)
	walk = func(j int) {
		if len(out) > 64 {
			return
		}
		if j == L {
			if same(cur) {
				out = append(out, segVariant{nodes: append([]*graphElement(nil), cur.nodes...), edges: append([]*graphElement(nil), cur.edges...),
					dirs: append([]int(nil), cur.dirs...)})
			}
			return
		}
		for _, x := range edgeCands[j] {
			ds := []int{sg.dirs[j]}
			if ds[0] == 0 {
				ds = []int{1, -1}
			}
			for _, d := range ds {
				src, dst := x.Source.Node, x.Dest.Node
				if d == -1 {
					src, dst = dst, src
				}
				if !strings.EqualFold(cur.nodes[j].Name, src) {
					continue
				}
				for _, n := range nodeCands[j+1] {
					if !strings.EqualFold(n.Name, dst) {
						continue
					}
					cur.edges[j], cur.dirs[j], cur.nodes[j+1] = x, d, n
					walk(j + 1)
				}
			}
		}
	}
	for _, n := range nodeCands[0] {
		cur.nodes[0] = n
		walk(0)
	}
	if len(out) > 64 {
		return nil, unsupportedSQL{"a pattern that matches more than 64 combinations of element tables"}
	}
	sg.variants = out
	// the properties each group variable's values carry: every property
	// the query reads of any name its tables have
	read := map[string]bool{}
	for _, ps := range t.refProps {
		for p := range ps {
			read[p] = true
		}
	}
	var els []*graphElement
	byPair := map[[2]string]*graphElement{}
	for i := range sg.variants {
		v := &sg.variants[i]
		key := [2]string{v.nodes[0].Name, v.nodes[L].Name}
		el := byPair[key]
		if el == nil {
			el = &graphElement{Name: fmt.Sprintf("cb_seg%d", t.next), KeyColumns: []string{"cb_id"}}
			t.next++
			src, dst := v.nodes[0], v.nodes[L]
			el.Source = &graphEndpoint{Node: src.Name, NodeCols: src.KeyColumns}
			for j := range src.KeyColumns {
				el.Source.EdgeCols = append(el.Source.EdgeCols, fmt.Sprintf("cb_s_%d", j))
			}
			el.Dest = &graphEndpoint{Node: dst.Name, NodeCols: dst.KeyColumns}
			for j := range dst.KeyColumns {
				el.Dest.EdgeCols = append(el.Dest.EdgeCols, fmt.Sprintf("cb_d_%d", j))
			}
			byPair[key] = el
			els = append(els, el)
		}
		v.el = el
	}
	// the group variables' values, in every stand-in table alike
	for _, el := range els {
		for _, g := range sg.names {
			props := map[string]bool{}
			for i := range sg.variants {
				for _, e := range sg.elemsOf(&sg.variants[i], g) {
					for _, p := range e.PropertyDefs {
						if read[strings.ToLower(p.Name)] {
							if _, err := e.column(p.Name, "cb_x"); err != nil {
								return nil, err
							}
							props[strings.ToLower(p.Name)] = true
						}
					}
				}
			}
			for _, p := range sortedKeys(props) {
				el.PropertyDefs = append(el.PropertyDefs, struct {
					Name string `json:"propertyDeclarationName"`
					SQL  string `json:"valueExpressionSql"`
				}{g + "__" + p, "cb_v_" + g + "__" + p})
			}
			el.PropertyDefs = append(el.PropertyDefs, struct {
				Name string `json:"propertyDeclarationName"`
				SQL  string `json:"valueExpressionSql"`
			}{g + "__cb_id", "cb_v_" + g + "__cb_id"})
		}
	}
	for _, el := range els {
		var vs []*segVariant
		for i := range sg.variants {
			if sg.variants[i].el == el {
				vs = append(vs, &sg.variants[i])
			}
		}
		s, err := sg.rowsSQL(vs, el)
		if err != nil {
			return nil, err
		}
		el.BaseTableName = "(" + s + ")"
	}
	return els, nil
}

// elemsOf are the element tables a named variable is in a variant.
func (sg *quantSeg) elemsOf(v *segVariant, name string) []*graphElement {
	var out []*graphElement
	for j, n := range sg.nodes {
		if n.name == name {
			out = append(out, v.nodes[j])
		}
	}
	for j, e := range sg.edges {
		if e.name == name {
			out = append(out, v.edges[j])
		}
	}
	return out
}

// isEdgeName says whether a named variable of the subpath is an edge.
func (sg *quantSeg) isEdgeName(name string) bool {
	for _, e := range sg.edges {
		if e.name == name {
			return true
		}
	}
	return false
}

// segBinds are a variant's elements as binds, in a branch of their own.
func (sg *quantSeg) segBinds(v *segVariant) (*gqlBranch, map[*gqlPatVar]*gqlBind) {
	br := &gqlBranch{binds: map[string]*gqlBind{}}
	by := map[*gqlPatVar]*gqlBind{}
	for j, n := range sg.nodes {
		if b := by[n]; b != nil {
			continue
		}
		b := &gqlBind{kind: bElem, alias: segNodeAlias(j), table: v.nodes[j]}
		by[n] = b
		if n.name != "" {
			br.bind(n.name, b)
		}
	}
	for j, e := range sg.edges {
		b := &gqlBind{kind: bElem, edge: true, alias: segEdgeAlias(j + 1), table: v.edges[j]}
		by[e] = b
		if e.name != "" {
			br.bind(e.name, b)
		}
	}
	return br, by
}

// segElems are a variant's elements in path order (n0, e1, n1, ...): each
// one's FROM item, the conditions that join it to the ones before, and
// the pattern variable it is.
type segElem struct {
	from  string
	links []string
	v     *gqlPatVar
	table *graphElement
	edge  bool
	j     int
}

func (sg *quantSeg) elems(v *segVariant, by map[*gqlPatVar]*gqlBind) []segElem {
	var out []segElem
	first := map[*gqlPatVar]int{}
	for j := 0; j <= len(sg.edges); j++ {
		n := sg.nodes[j]
		ne := segElem{v: n, table: v.nodes[j], j: j, from: v.nodes[j].BaseTableName + " AS " + segNodeAlias(j)}
		if k, ok := first[n]; ok && k != j {
			// a variable twice: the same node
			for _, c := range v.nodes[j].KeyColumns {
				ne.links = append(ne.links, segNodeAlias(j)+"."+c+" = "+segNodeAlias(k)+"."+c)
			}
		} else {
			first[n] = j
		}
		if j > 0 {
			x, d := v.edges[j-1], v.dirs[j-1]
			ea := segEdgeAlias(j)
			// the edge's end at the node before it, and at this node
			before, after := x.Source, x.Dest
			if d == -1 {
				before, after = after, before
			}
			// the edge, joined to the node before it, then this node
			ee := segElem{v: sg.edges[j-1], table: x, edge: true, j: j, from: x.BaseTableName + " AS " + ea}
			for i, c := range before.EdgeCols {
				ee.links = append(ee.links, ea+"."+c+" = "+segNodeAlias(j-1)+"."+before.NodeCols[i])
			}
			for i, c := range after.NodeCols {
				ne.links = append(ne.links, segNodeAlias(j)+"."+c+" = "+ea+"."+after.EdgeCols[i])
			}
			out = append(out, ee)
		}
		out = append(out, ne)
	}
	_ = by
	return out
}

// condVars are the subpath's variables a condition reads.
func (sg *quantSeg) condVars(c [2]int) []*gqlPatVar {
	var out []*gqlPatVar
	ps := sg.t.ps
	for i := c[0]; i < c[1]; i++ {
		tk := ps.toks[i]
		if !isNameTok(tk) || i > c[0] && ps.toks[i-1].text == "." {
			continue
		}
		for _, v := range append(append([]*gqlPatVar(nil), sg.nodes...), sg.edges...) {
			if v.name != "" && strings.EqualFold(v.name, tk.text) {
				out = append(out, v)
			}
		}
	}
	return out
}

// conditions are a variant's conditions, each with the index in its
// elements after which every element it reads is there.
func (sg *quantSeg) conditions(v *segVariant, br *gqlBranch, by map[*gqlPatVar]*gqlBind, els []segElem) ([]string, []int, error) {
	pos := map[*gqlPatVar]int{}
	for i, e := range els {
		if _, ok := pos[e.v]; !ok {
			pos[e.v] = i
		}
	}
	var conds []string
	var at []int
	t := sg.t
	for _, p := range sg.props {
		s, err := t.propCond(br, by[p.v], p)
		if err != nil {
			return nil, nil, err
		}
		conds, at = append(conds, s), append(at, pos[p.v])
	}
	for _, c := range sg.conds {
		s, err := t.render(br, c[0], c[1])
		if err != nil {
			return nil, nil, err
		}
		last := 0
		for _, x := range sg.condVars(c) {
			if pos[x] > last {
				last = pos[x]
			}
		}
		conds, at = append(conds, "("+s+")"), append(at, last)
	}
	// the mode inside the parentheses: this repetition's own elements
	if sg.mode == "TRAIL" || sg.mode == "ACYCLIC" || sg.mode == "SIMPLE" {
		var idx []int
		for i, e := range els {
			if e.edge == (sg.mode == "TRAIL") {
				idx = append(idx, i)
			}
		}
		for a := 0; a < len(idx); a++ {
			for b := a + 1; b < len(idx); b++ {
				if sg.mode == "SIMPLE" && a == 0 && b == len(idx)-1 {
					continue
				}
				x, y := els[idx[a]], els[idx[b]]
				if x.v == y.v {
					conds, at = append(conds, "FALSE"), append(at, idx[b])
					continue
				}
				conds, at = append(conds, "NOT "+elemEqual(by[x.v], by[y.v])), append(at, idx[b])
			}
		}
	}
	return conds, at, nil
}

// rowsSQL is the rows of a stand-in table's variants: each repetition's
// first and last nodes' keys (cb_s_, cb_d_), its identity (cb_id), its
// edges' and nodes' identities (cb_eids, cb_nids: the nodes it reaches,
// cb_sids: the ones it leaves, cb_n0id: its first), its edges' COST sum
// (cb_c) and its group variables' values (cb_v_<name>__<property>,
// cb_v_<name>__cb_id).
func (sg *quantSeg) rowsSQL(vs []*segVariant, el *graphElement) (string, error) {
	var parts []string
	for _, v := range vs {
		s, err := sg.prefixSQL(v, -1, el)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "SELECT 1 AS cb_none FROM UNNEST(ARRAY<INT64>[]) AS cb_none", nil
	}
	return strings.Join(parts, " UNION ALL "), nil
}

// prefixSQL is a variant's rows: of all its elements (upto -1), with the
// stand-in table el's columns, or of its first upto+1 elements, with its
// first node's key alone.
func (sg *quantSeg) prefixSQL(v *segVariant, upto int, el *graphElement) (string, error) {
	br, by := sg.segBinds(v)
	els := sg.elems(v, by)
	conds, at, err := sg.conditions(v, br, by, els)
	if err != nil {
		return "", err
	}
	if upto < 0 {
		upto = len(els) - 1
	}
	var froms, where []string
	for i := 0; i <= upto; i++ {
		froms = append(froms, els[i].from)
		where = append(where, els[i].links...)
	}
	for i, c := range conds {
		if at[i] <= upto {
			where = append(where, c)
		}
	}
	L := len(sg.edges)
	n0 := v.nodes[0]
	var cols []string
	for j, c := range n0.KeyColumns {
		cols = append(cols, fmt.Sprintf("%s.%s AS cb_s_%d", segNodeAlias(0), c, j))
	}
	if el != nil {
		nL := v.nodes[L]
		for j, c := range nL.KeyColumns {
			cols = append(cols, fmt.Sprintf("%s.%s AS cb_d_%d", segNodeAlias(L), c, j))
		}
		var eids, nids, sids, costs []string
		for j := 1; j <= L; j++ {
			eids = append(eids, elemIDSQL(&gqlBind{alias: segEdgeAlias(j), table: v.edges[j-1]}))
			if c := sg.edges[j-1].cost; hasCost(c) {
				s, err := sg.t.render(br, c[0], c[1])
				if err != nil {
					return "", err
				}
				costs = append(costs, "("+s+")")
			}
		}
		for j := 0; j <= L; j++ {
			id := elemIDSQL(&gqlBind{alias: segNodeAlias(j), table: v.nodes[j]})
			if j > 0 {
				nids = append(nids, id)
			}
			if j < L {
				sids = append(sids, id)
			}
		}
		if len(costs) == 0 {
			costs = []string{"0"}
		}
		cols = append(cols, "ARRAY_TO_STRING(["+strings.Join(eids, ", ")+"], ',') AS cb_id",
			"["+strings.Join(eids, ", ")+"] AS cb_eids", "["+strings.Join(nids, ", ")+"] AS cb_nids",
			"["+strings.Join(sids, ", ")+"] AS cb_sids", sids[0]+" AS cb_n0id", "("+strings.Join(costs, " + ")+") AS cb_c")
		for _, p := range el.PropertyDefs {
			g, prop, _ := strings.Cut(p.Name, "__")
			var b *gqlBind
			for x, xb := range by {
				if x.name == g {
					b = xb
				}
			}
			val := "NULL"
			switch {
			case b == nil:
			case prop == "cb_id":
				val = elemIDSQL(b)
			default:
				pv, err := b.prop(prop)
				if err != nil {
					return "", err
				}
				val = pv
			}
			cols = append(cols, val+" AS "+p.SQL)
		}
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + strings.Join(froms, ", ") + whereSQL(where), nil
}

// segHopsSQL is the SQL for the paths of k > 0 repetitions.
func (q *graphQuant) segHopsSQL(k int) string {
	e := q.table
	var cols, froms, conds []string
	for _, c := range e.Source.EdgeCols {
		cols = append(cols, "cb_e1."+c+" AS cb_src_"+c)
	}
	for _, c := range e.Dest.EdgeCols {
		cols = append(cols, fmt.Sprintf("cb_e%d.%s AS cb_dst_%s", k, c, c))
	}
	if q.withLen {
		cols = append(cols, fmt.Sprintf("%d AS cb_len", k))
	}
	each := func(f string) []string {
		var out []string
		for i := 1; i <= k; i++ {
			out = append(out, fmt.Sprintf(f, i))
		}
		return out
	}
	for _, p := range q.arrs {
		var vals []string
		for i := 1; i <= k; i++ {
			v, _ := e.column(p, fmt.Sprintf("cb_e%d", i))
			vals = append(vals, v)
		}
		cols = append(cols, "["+strings.Join(vals, ", ")+"] AS cb_arr_"+p)
	}
	eids := concatIDs(each("cb_e%d.cb_eids"))
	// each repetition's elements' identities, one by one
	L := len(q.seg.edges)
	flat := func(col string) []string {
		var out []string
		for i := 1; i <= k; i++ {
			for j := 0; j < L; j++ {
				out = append(out, fmt.Sprintf("cb_e%d.%s[OFFSET(%d)]", i, col, j))
			}
		}
		return out
	}
	if q.withIDs {
		cols = append(cols, eids+" AS cb_ids")
	}
	if q.cost != "" {
		cols = append(cols, "("+strings.Join(each("cb_e%d.cb_c"), " + ")+") AS cb_cost")
	}
	if q.withPath {
		cols = append(cols, eids+" AS cb_pe", concatIDs(each("cb_e%d.cb_nids"))+" AS cb_pn", concatIDs(each("cb_e%d.cb_sids"))+" AS cb_ps",
			pairwiseDistinct(flat("cb_eids"))+" AS cb_ped", pairwiseDistinct(flat("cb_nids"))+" AS cb_pnd", pairwiseDistinct(flat("cb_sids"))+" AS cb_psd")
	}
	for i := 1; i <= k; i++ {
		froms = append(froms, fmt.Sprintf("%s AS cb_e%d", q.segName(), i))
		if i > 1 {
			for j, c := range e.Source.EdgeCols {
				conds = append(conds, fmt.Sprintf("cb_e%d.%s = cb_e%d.%s", i, c, i-1, e.Dest.EdgeCols[j]))
			}
		}
	}
	if q.trail {
		conds = append(conds, pairwiseDistinct(flat("cb_eids")))
	}
	if q.acyc {
		conds = append(conds, pairwiseDistinct(append([]string{"cb_e1.cb_n0id"}, flat("cb_nids")...)))
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + strings.Join(froms, ", ") + whereSQL(conds)
}

// segCountSQL is the SQL of the paths of k-1 repetitions and the first
// upto+1 elements of a k-th, of variant v (one row each), for counting.
func (q *graphQuant) segCountSQL(k int, v *segVariant, upto int) string {
	e := q.table
	pre, err := q.seg.prefixSQL(v, upto, nil)
	if err != nil {
		return "SELECT 1 FROM UNNEST(ARRAY<INT64>[]) AS cb_none"
	}
	if k <= 1 {
		return "SELECT 1 FROM (" + pre + ") AS cb_p"
	}
	var froms, conds []string
	for i := 1; i < k; i++ {
		froms = append(froms, fmt.Sprintf("%s AS cb_e%d", q.segName(), i))
		if i > 1 {
			for j, c := range e.Source.EdgeCols {
				conds = append(conds, fmt.Sprintf("cb_e%d.%s = cb_e%d.%s", i, c, i-1, e.Dest.EdgeCols[j]))
			}
		}
	}
	froms = append(froms, "("+pre+") AS cb_p")
	for j, c := range e.Source.EdgeCols {
		conds = append(conds, fmt.Sprintf("cb_p.%s = cb_e%d.%s", c, k-1, e.Dest.EdgeCols[j]))
	}
	return "SELECT 1 FROM " + strings.Join(froms, ", ") + whereSQL(conds)
}

// segName is the WITH clause's name for the subpath's rows.
func (q *graphQuant) segName() string { return "cb_sg_" + q.alias }

// segWith is the WITH clause of the subpath's rows.
func (q *graphQuant) segWith() string {
	return "WITH " + q.segName() + " AS " + q.table.BaseTableName + " "
}

// variantsOf are the variants a stand-in table reads.
func (q *graphQuant) variantsOf() []*segVariant {
	var out []*segVariant
	for i := range q.seg.variants {
		if q.seg.variants[i].el == q.table {
			out = append(out, &q.seg.variants[i])
		}
	}
	return out
}

// segRecursiveUnion plans a quantified path pattern of several edges: a
// Recursive Union of its first repetitions and, for each further one, a
// Cross Apply of the previous repetition's rows (a Recursive Spool Scan)
// to the subpath's reads, seeking from the node it reached.
func (b *planBuilder) segRecursiveUnion(f *sqlFrom, it *planItem, q *graphQuant, cc *countCtx) *frag {
	count := func(lo, hi int) string {
		if lo > hi {
			return "SELECT 0"
		}
		return cc.count(q.pathsSQL(lo, hi))
	}
	rootOf := func(e *graphElement) string {
		if t := b.schema.table(e.BaseTableName); t != nil {
			return b.schema.root(t)
		}
		return e.BaseTableName
	}
	scanOf := func(e *graphElement, full bool) *planOp {
		var links []planLink
		for _, c := range e.KeyColumns {
			links = append(links, planLink{sc: &planScalar{name: "Reference", desc: c}, variable: c})
		}
		meta := map[string]string{"scan_type": "TableScan", "scan_target": e.BaseTableName, "scan_method": "Automatic"}
		if full {
			meta["Full scan"] = "true"
		}
		s := op("Scan", meta, links...)
		s.scan = true
		return s
	}
	// chain draws a variant's reads from element from on: the first read
	// (a full scan, or a seek from the node the spool reached), then a
	// Cross Apply to each next element's seek. rows(i) and execs(i) count
	// the rows after element i and the executions of its read.
	chain := func(v *segVariant, spool bool, rows func(i int) string) *planOp {
		br, by := q.seg.segBinds(v)
		els := q.seg.elems(v, by)
		_, at, _ := q.seg.conditions(v, br, by, els)
		filtered := func(i int) bool {
			for _, a := range at {
				if a == i {
					return true
				}
			}
			return false
		}
		read := func(i int) *planOp {
			el := els[i]
			full := i == 0 && !spool
			s := scanOf(el.table, full)
			var o *planOp
			var seek []string
			switch {
			case i == 0 && spool:
				for j, c := range el.table.KeyColumns {
					seek = append(seek, fmt.Sprintf("($%s = $%s.cb_d_%d)", c, q.alias, j))
				}
			case i > 0:
				for _, l := range el.links {
					a, bb, _ := strings.Cut(l, " = ")
					_, ac, _ := strings.Cut(a, ".")
					seek = append(seek, "($"+ac+" = $"+strings.ReplaceAll(bb, ".", "_")+")")
				}
			}
			if len(seek) > 0 || filtered(i) {
				var links []planLink
				links = append(links, rel(s, ""))
				if len(seek) > 0 {
					links = append(links, planLink{sc: &planScalar{name: "Function", desc: strings.Join(seek, " AND ")}, typ: "Seek Condition"})
				}
				if filtered(i) {
					links = append(links, planLink{sc: &planScalar{name: "Function", desc: "($subpath_condition)"}, typ: "Residual Condition"})
				}
				o = op("Filter Scan", nil, links...)
				o.filtered = filtered(i)
			} else {
				o = s
			}
			o.rows = rows(i)
			if o != s {
				// a seek reads the rows it returns, before the residual
				s.rows = o.rows
			}
			return b.distUnion(o, &frag{root: rootOf(el.table)})
		}
		cur := read(0)
		for i := 1; i < len(els); i++ {
			m := read(i)
			ca := op("Cross Apply", nil, rel(cur, "Input"), rel(m, "Map"))
			ca.rows = rows(i)
			cur = ca
		}
		return cur
	}
	vs := q.variantsOf()
	union := func(ops []*planOp, rows string) *planOp {
		if len(ops) == 1 {
			return ops[0]
		}
		var links []planLink
		for _, o := range ops {
			ui := op("Union Input", nil, rel(o, ""))
			links = append(links, rel(ui, ""))
		}
		u := op("Union All", nil, links...)
		u.rows = rows
		return u
	}
	first := 1
	var base *planOp
	if q.min == 0 && q.node != nil {
		first = 0
		ns := op("Scan", map[string]string{"scan_type": "TableScan", "scan_target": q.node.BaseTableName, "scan_method": "Automatic", "Full scan": "true"})
		ns.scan = true
		ns.rows = count(0, 0)
		base = b.distUnion(ns, &frag{root: rootOf(q.node)})
	} else {
		var ops []*planOp
		for _, v := range vs {
			v := v
			ops = append(ops, chain(v, false, func(i int) string { return cc.count(q.segCountSQL(1, v, i)) }))
		}
		base = union(ops, count(1, 1))
	}
	spool := op("Scan", map[string]string{"scan_type": "RecursiveSpoolScan", "scan_target": "$" + q.alias})
	spool.rows = count(first, q.max-1)
	spool.execs = fmt.Sprintf("SELECT %d", q.max-first)
	var ops []*planOp
	for _, v := range vs {
		v := v
		ops = append(ops, chain(v, true, func(i int) string {
			var parts []string
			for k := first + 1; k <= q.max; k++ {
				parts = append(parts, q.segCountSQL(k, v, i))
			}
			if len(parts) == 0 {
				return "SELECT 0"
			}
			return cc.count(q.segWith() + strings.Join(parts, " UNION ALL "))
		}))
	}
	m := union(ops, count(first+1, q.max))
	ca := op("Cross Apply", nil, rel(spool, "Input"), rel(m, "Map"))
	ca.rows, ca.execs = count(first+1, q.max), spool.execs
	ru := op("Recursive Union", map[string]string{"upper_bound": fmt.Sprint(q.max)}, rel(base, "Base"), rel(ca, "Recursive"))
	ru.rows = count(first, q.max)
	o := ru
	if q.min > 1 {
		o = op("Filter", nil, rel(ru, ""), planLink{sc: &planScalar{name: "Function", desc: fmt.Sprintf("($repetitions >= %d)", q.min)}, typ: "Condition"})
		o.rows = count(q.min, q.max)
	}
	return &frag{op: o, items: []*planItem{it}, from: b.itemSQL(f)}
}

// segGroupArrs are the arrays a stand-in table's group variables carry
// for the properties the query reads of them, and their identities.
func segGroupArrs(t *gqlTranslator, sg *quantSeg, el *graphElement) []string {
	var out []string
	for _, g := range sg.names {
		for p := range t.refProps[g] {
			if el.hasProperty(g + "__" + p) {
				out = append(out, g+"__"+p)
			}
		}
		out = append(out, g+"__cb_id")
	}
	sort.Strings(out)
	return out
}

// hoistWith moves the WITH clauses of quantified path patterns' subpath
// rows (WITH cb_sg_... AS (...), written inside the subqueries that read
// them) to the start of a statement the engine runs: Spanner takes a WITH
// clause only there. Each one's rows read no outer column, so it means the
// same there.
func hoistWith(sql string) string {
	const mark = "WITH cb_sg_"
	if !strings.Contains(sql, mark) {
		return sql
	}
	var defs []string
	seen := map[string]bool{}
	var out strings.Builder
	for {
		i := strings.Index(sql, mark)
		if i < 0 {
			out.WriteString(sql)
			break
		}
		out.WriteString(sql[:i])
		rest := sql[i+len("WITH "):]
		as := strings.Index(rest, " AS (")
		if as < 0 {
			out.WriteString(sql[i:])
			break
		}
		name := rest[:as]
		body := rest[as+len(" AS "):]
		n := balancedLen(body)
		if n < 0 {
			out.WriteString(sql[i:])
			break
		}
		if !seen[name] {
			seen[name] = true
			defs = append(defs, name+" AS "+body[:n])
		}
		sql = strings.TrimPrefix(body[n:], " ")
	}
	s := out.String()
	if len(defs) == 0 {
		return s
	}
	if t := strings.TrimLeft(s, " "); len(t) > 5 && strings.EqualFold(t[:5], "WITH ") {
		return "WITH " + strings.Join(defs, ", ") + ", " + t[5:]
	}
	return "WITH " + strings.Join(defs, ", ") + " " + s
}

// balancedLen is the length of the parenthesized text s starts with, or
// -1; it skips quoted strings and identifiers.
func balancedLen(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'', '"', '`':
			for i++; i < len(s) && s[i] != c; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}
