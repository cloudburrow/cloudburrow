package spannerfront

// The GQL translation's statements over the working table (#2029;
// plangraph2.go): projections into derived tables, LET, FOR, ORDER BY /
// LIMIT / OFFSET, the final RETURN, and rendering GQL expressions as SQL.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// joinCols are an element table's columns its element's identity and its
// edges' joins read: its key, and the columns its edges reference.
func (t *gqlTranslator) joinCols(e *graphElement) []string {
	var out []string
	add := func(cs []string) {
		for _, c := range cs {
			if !hasCol(out, c) {
				out = append(out, c)
			}
		}
	}
	add(e.KeyColumns)
	if e.Source != nil {
		add(e.Source.EdgeCols)
		add(e.Dest.EdgeCols)
		return out
	}
	for _, et := range t.g.EdgeTables {
		if strings.EqualFold(et.Source.Node, e.Name) {
			add(et.Source.NodeCols)
		}
		if strings.EqualFold(et.Dest.Node, e.Name) {
			add(et.Dest.NodeCols)
		}
	}
	return out
}

// gqlExport is a name a derived table carries, and its columns.
type gqlExport struct {
	name, from string // its name, and the name it has in the branches
	kind       gqlBindKind
	edge       bool
	tables     []*graphElement
	suffixes   []string
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9_]`)

func gqlPrefix(name string) string {
	return "cb_" + nonIdent.ReplaceAllString(strings.ToLower(name), "_")
}

// exportOf describes a name's columns over every branch.
func (t *gqlTranslator) exportOf(name, from string) (*gqlExport, error) {
	x := &gqlExport{name: name, from: from}
	seen := map[*graphElement]bool{}
	found := false
	for _, br := range t.branches {
		b := br.binds[from]
		if b == nil {
			continue
		}
		if found && b.kind != x.kind {
			return nil, unsupportedSQL{fmt.Sprintf("%s, a graph element in one branch and a value in another", name)}
		}
		found = true
		x.kind, x.edge = b.kind, b.edge
		for _, e := range b.candidates() {
			if !seen[e] {
				seen[e] = true
				x.tables = append(x.tables, e)
			}
		}
	}
	if !found {
		return nil, unsupportedSQL{fmt.Sprintf("name %s, which the working table does not have", name)}
	}
	props := map[string]bool{}
	for p := range t.refProps[strings.ToLower(name)] {
		props[p] = true
	}
	for p := range t.refProps[strings.ToLower(from)] {
		props[p] = true
	}
	switch x.kind {
	case bElem:
		x.suffixes = append(x.suffixes, "t")
		var cols []string
		for _, e := range x.tables {
			for _, c := range t.joinCols(e) {
				if !hasCol(cols, c) {
					cols = append(cols, c)
				}
			}
		}
		for _, c := range cols {
			x.suffixes = append(x.suffixes, "c_"+strings.ToLower(c))
		}
		for _, p := range sortedKeys(props) {
			for _, e := range x.tables {
				if e.hasProperty(p) {
					x.suffixes = append(x.suffixes, "p_"+p)
					break
				}
			}
		}
	case bGroup:
		x.suffixes = append(x.suffixes, "len")
		ids := false
		for _, br := range t.branches {
			if b := br.binds[from]; b != nil && b.quant != nil {
				b.quant.withLen = true
				ids = ids || b.quant.withIDs
			}
		}
		if ids {
			x.suffixes = append(x.suffixes, "ids")
		}
		for _, p := range sortedKeys(props) {
			x.suffixes = append(x.suffixes, "a_"+p)
		}
	case bScalar:
		x.suffixes = []string{"s"}
	case bPath:
		x.suffixes = []string{"len"}
	}
	return x, nil
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// exportValue is the SQL of one of an export's columns in a branch.
func (t *gqlTranslator) exportValue(x *gqlExport, b *gqlBind, suffix string) (string, error) {
	if b == nil {
		return "NULL", nil
	}
	if b.materialized() {
		switch {
		case suffix == "t" && b.table != nil:
			return "'" + b.table.Name + "'", nil
		case b.kind == bScalar:
			return b.sql, nil
		}
		n := b.prefix + "__" + suffix
		if !b.cols[n] {
			return "NULL", nil
		}
		return b.qual + "." + n, nil
	}
	switch b.kind {
	case bScalar:
		return b.sql, nil
	case bPath:
		return b.pathLen, nil
	case bGroup:
		if suffix == "len" {
			b.quant.withLen = true
			return b.alias + ".cb_len", nil
		}
		if suffix == "ids" {
			if !b.quant.withIDs {
				return "NULL", nil
			}
			return b.alias + ".cb_ids", nil
		}
		p := strings.TrimPrefix(suffix, "a_")
		if !hasCol(b.quant.arrs, p) {
			return "NULL", nil
		}
		return b.alias + ".cb_arr_" + p, nil
	}
	switch {
	case suffix == "t":
		return "'" + b.table.Name + "'", nil
	case strings.HasPrefix(suffix, "c_"):
		c := strings.TrimPrefix(suffix, "c_")
		for _, jc := range t.joinCols(b.table) {
			if strings.EqualFold(jc, c) {
				return b.alias + "." + jc, nil
			}
		}
		return "NULL", nil
	}
	return b.prop(strings.TrimPrefix(suffix, "p_"))
}

// materialize is the bind of an export in the derived table qual.
func (x *gqlExport) materialize(qual string) *gqlBind {
	b := &gqlBind{kind: x.kind, edge: x.edge, qual: qual, prefix: gqlPrefix(x.name), tables: x.tables, cols: map[string]bool{}}
	if len(x.tables) == 1 {
		b.table = x.tables[0]
	}
	for _, s := range x.suffixes {
		b.cols[b.prefix+"__"+s] = true
	}
	if x.kind == bScalar {
		b.sql = qual + "." + b.prefix + "__s"
	}
	if x.kind == bPath {
		b.pathLen = qual + "." + b.prefix + "__len"
	}
	return b
}

// selectSQL is SELECT list FROM ... WHERE ... of a branch.
func selectSQL(list []string, br *gqlBranch) string {
	s := "SELECT " + strings.Join(list, ", ")
	if len(br.from) > 0 {
		s += " FROM " + strings.Join(br.from, ", ")
		if len(br.conds) > 0 {
			s += " WHERE " + strings.Join(br.conds, " AND ")
		}
	}
	return s
}

// allNames are the names visible in any branch, in order.
func (t *gqlTranslator) allNames() []string {
	var out []string
	for _, br := range t.branches {
		for _, n := range br.names {
			if !hasCol(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// collapse makes the working table one derived table of the names (every
// visible one when nil), and returns its branch.
func (t *gqlTranslator) collapse(names []string) *gqlBranch {
	if names == nil {
		names = t.allNames()
	}
	qual := fmt.Sprintf("cb_w%d", t.derived)
	t.derived++
	nb := &gqlBranch{binds: map[string]*gqlBind{}}
	var xs []*gqlExport
	for _, n := range names {
		x, err := t.exportOf(n, n)
		if err != nil {
			t.err = err
			continue
		}
		xs = append(xs, x)
	}
	var parts []string
	for _, br := range t.branches {
		var list []string
		for _, x := range xs {
			for _, s := range x.suffixes {
				v, err := t.exportValue(x, br.binds[x.from], s)
				if err != nil {
					t.err = err
				}
				list = append(list, v+" AS "+gqlPrefix(x.name)+"__"+s)
			}
		}
		if len(list) == 0 {
			list = []string{"TRUE AS cb_unit"}
		}
		parts = append(parts, selectSQL(list, br))
	}
	nb.from = []string{"(" + strings.Join(parts, " UNION ALL ") + ") AS " + qual}
	for _, x := range xs {
		nb.bind(x.name, x.materialize(qual))
	}
	t.branches = []*gqlBranch{nb}
	return nb
}

// one makes the working table one branch.
func (t *gqlTranslator) one() *gqlBranch {
	if len(t.branches) > 1 {
		return t.collapse(nil)
	}
	return t.branches[0]
}

// --- LET, FOR, ORDER BY / LIMIT / OFFSET --------------------------------------------

func (t *gqlTranslator) let() error {
	ps := t.ps
	type def struct {
		name   string
		lo, hi int
	}
	var defs []def
	for {
		n, ok := ps.ident()
		if !ok {
			return ps.errAt("a LET variable")
		}
		if err := ps.expectP("="); err != nil {
			return err
		}
		e, err := ps.expr()
		if err != nil {
			return err
		}
		defs = append(defs, def{n, e.lo, e.hi})
		if !ps.acceptP(",") {
			break
		}
	}
	for _, br := range t.branches {
		var sqls []string
		for _, d := range defs {
			s, err := t.render(br, d.lo, d.hi)
			if err != nil {
				return err
			}
			sqls = append(sqls, s)
		}
		for i, d := range defs {
			br.bind(d.name, &gqlBind{kind: bScalar, sql: "(" + sqls[i] + ")"})
		}
	}
	return nil
}

func (t *gqlTranslator) forStmt() error {
	ps := t.ps
	n, ok := ps.ident()
	if !ok {
		return ps.errAt("a FOR variable")
	}
	if err := ps.expectKw("IN"); err != nil {
		return err
	}
	e, err := ps.expr()
	if err != nil {
		return err
	}
	offset := ""
	if ps.isKw("WITH") && ps.isKwN(1, "OFFSET") {
		ps.p += 2
		offset = "offset"
		if ps.acceptKw("AS") {
			o, ok := ps.ident()
			if !ok {
				return ps.errAt("an offset's name")
			}
			offset = o
		}
	}
	for _, br := range t.branches {
		s, err := t.render(br, e.lo, e.hi)
		if err != nil {
			return err
		}
		alias := fmt.Sprintf("cb_f%d", t.next)
		t.next++
		item := "UNNEST(" + s + ") AS " + alias
		if offset != "" {
			item += " WITH OFFSET AS " + alias + "_o"
		}
		br.from = append(br.from, item)
		br.bind(n, &gqlBind{kind: bScalar, sql: alias})
		if offset != "" {
			br.bind(offset, &gqlBind{kind: bScalar, sql: alias + "_o"})
		}
	}
	return nil
}

// gqlTail is ORDER BY, LIMIT and OFFSET (SKIP).
type gqlTail struct {
	order         []gqlOrderItem
	limit, offset [2]int
}

type gqlOrderItem struct {
	lo, hi int
	dir    string
}

func (t *gqlTranslator) tail(tl *gqlTail) error {
	ps := t.ps
	expr := func() ([2]int, error) {
		e, err := ps.expr()
		if err != nil {
			return [2]int{}, err
		}
		return [2]int{e.lo, e.hi}, nil
	}
	var err error
	for {
		switch {
		case ps.isKw("ORDER") && ps.isKwN(1, "BY"):
			ps.p += 2
			for {
				e, err := ps.expr()
				if err != nil {
					return err
				}
				it := gqlOrderItem{lo: e.lo, hi: e.hi}
				if ps.isKw("COLLATE") {
					return unsupportedSQL{"COLLATE in a graph query's ORDER BY"}
				}
				switch {
				case ps.acceptKw("ASC"), ps.acceptKw("ASCENDING"):
					it.dir = " ASC"
				case ps.acceptKw("DESC"), ps.acceptKw("DESCENDING"):
					it.dir = " DESC"
				}
				tl.order = append(tl.order, it)
				if !ps.acceptP(",") {
					break
				}
			}
		case ps.acceptKw("LIMIT"):
			if tl.limit, err = expr(); err != nil {
				return err
			}
		case ps.acceptKw("OFFSET"), ps.acceptKw("SKIP"):
			if tl.offset, err = expr(); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// tailSQL renders a tail in br; alias maps an output alias to its column.
func (t *gqlTranslator) tailSQL(tl *gqlTail, br *gqlBranch) (string, error) {
	var b strings.Builder
	if len(tl.order) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range tl.order {
			s, err := t.render(br, o.lo, o.hi)
			if err != nil {
				return "", err
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(s + o.dir)
		}
	}
	if tl.limit[1] > 0 || tl.offset[1] > 0 {
		lim := "9223372036854775807"
		if tl.limit[1] > 0 {
			s, err := t.render(br, tl.limit[0], tl.limit[1])
			if err != nil {
				return "", err
			}
			lim = s
		}
		b.WriteString(" LIMIT " + lim)
		if tl.offset[1] > 0 {
			s, err := t.render(br, tl.offset[0], tl.offset[1])
			if err != nil {
				return "", err
			}
			b.WriteString(" OFFSET " + s)
		}
	}
	return b.String(), nil
}

// orderLimitStmt is ORDER BY, LIMIT or OFFSET as statements: the working
// table, sorted and cut. An ORDER BY no LIMIT or OFFSET follows does
// nothing (Google's page).
func (t *gqlTranslator) orderLimitStmt() error {
	var tl gqlTail
	if err := t.tail(&tl); err != nil {
		return err
	}
	if tl.limit[1] == 0 && tl.offset[1] == 0 {
		return nil
	}
	br := t.one()
	names := br.names
	tail, err := t.tailSQL(&tl, br)
	if err != nil {
		return err
	}
	return t.projectInto(br, names, nil, nil, false, tail)
}

// projectInto makes br's names (renamed by alias: new name -> old) and
// the scalar columns cols (new name -> SQL) one derived table, grouped by
// group, with a tail.
func (t *gqlTranslator) projectInto(br *gqlBranch, names []string, alias map[string]string, cols map[string]string, distinct bool, tail string, group ...string) error {
	qual := fmt.Sprintf("cb_w%d", t.derived)
	t.derived++
	saved := t.branches
	t.branches = []*gqlBranch{br}
	nb := &gqlBranch{binds: map[string]*gqlBind{}}
	var list []string
	for _, n := range names {
		from := n
		if alias != nil && alias[n] != "" {
			from = alias[n]
		}
		var x *gqlExport
		var err error
		if s, ok := cols[n]; ok {
			x = &gqlExport{name: n, from: n, kind: bScalar, suffixes: []string{"s"}}
			list = append(list, s+" AS "+gqlPrefix(n)+"__s")
		} else {
			if x, err = t.exportOf(n, from); err != nil {
				t.branches = saved
				return err
			}
			for _, s := range x.suffixes {
				v, err := t.exportValue(x, br.binds[from], s)
				if err != nil {
					t.branches = saved
					return err
				}
				list = append(list, v+" AS "+gqlPrefix(n)+"__"+s)
			}
		}
		nb.bind(n, x.materialize(qual))
	}
	t.branches = saved
	if len(list) == 0 {
		list = []string{"TRUE AS cb_unit"}
	}
	s := selectSQL(list, br)
	if distinct {
		s = "SELECT DISTINCT" + strings.TrimPrefix(s, "SELECT")
	}
	if group = dedupe(group); len(group) > 0 {
		s += " GROUP BY " + strings.Join(group, ", ")
	}
	s += tail
	nb.from = []string{"(" + s + ") AS " + qual}
	t.branches = []*gqlBranch{nb}
	return nil
}

// --- RETURN and WITH -------------------------------------------------------------------

type gqlItem struct {
	e     *sqlExpr
	alias string
	star  bool
}

// items reads a RETURN's, WITH's or COLUMNS' list.
func (t *gqlTranslator) items() (distinct bool, out []gqlItem, err error) {
	ps := t.ps
	if ps.acceptKw("DISTINCT") {
		distinct = true
	} else {
		ps.acceptKw("ALL")
	}
	for {
		if ps.acceptP("*") {
			out = append(out, gqlItem{star: true})
		} else {
			e, err := ps.expr()
			if err != nil {
				return false, nil, err
			}
			it := gqlItem{e: e}
			if ps.acceptKw("AS") {
				a, ok := ps.ident()
				if !ok {
					return false, nil, ps.errAt("an alias")
				}
				it.alias = a
			} else if e.kind == exCol && len(e.path) == 2 {
				// a property's column is named after the property
				it.alias = e.path[1]
			} else if e.kind == exCol && len(e.path) == 1 {
				it.alias = e.path[0]
			}
			out = append(out, it)
		}
		if !ps.acceptP(",") {
			return distinct, out, nil
		}
	}
}

// groupBy reads GROUP BY's items.
func (t *gqlTranslator) groupBy() ([]*sqlExpr, error) {
	ps := t.ps
	if !(ps.isKw("GROUP") && ps.isKwN(1, "BY")) {
		return nil, nil
	}
	ps.p += 2
	var out []*sqlExpr
	for {
		e, err := ps.expr()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if !ps.acceptP(",") {
			return out, nil
		}
	}
}

// bareName is the name an expression is, when it is only a name.
func (t *gqlTranslator) bareName(e *sqlExpr) string {
	if e != nil && e.kind == exCol && len(e.path) == 1 {
		return strings.ToLower(e.path[0])
	}
	return ""
}

// verticalAggregate says whether e aggregates rows (an aggregate over a
// group variable's property aggregates the path's values instead).
func (t *gqlTranslator) verticalAggregate(e *sqlExpr, br *gqlBranch) bool {
	agg := false
	walkExpr(e, func(x *sqlExpr) {
		if x.kind != exFunc || !aggregateFuncs[x.op] {
			return
		}
		horizontal := false
		walkExpr(x, func(y *sqlExpr) {
			if y.kind == exCol && len(y.path) >= 1 {
				if b := br.binds[strings.ToLower(y.path[0])]; b != nil && b.kind == bGroup {
					horizontal = true
				}
			}
		})
		if !horizontal {
			agg = true
		}
	})
	return agg
}

// groupKeys are the SQL GROUP BY items of an expression: an element's
// columns, or the expression.
func (t *gqlTranslator) groupKeys(br *gqlBranch, e *sqlExpr) ([]string, error) {
	if n := t.bareName(e); n != "" {
		if b := br.binds[n]; b != nil && b.kind != bScalar {
			x, err := t.exportOf(n, n)
			if err != nil {
				return nil, err
			}
			var out []string
			for _, s := range x.suffixes {
				v, err := t.exportValue(x, b, s)
				if err != nil {
					return nil, err
				}
				if v != "NULL" && !strings.HasPrefix(v, "'") {
					out = append(out, v)
				}
			}
			return out, nil
		}
	}
	if e.kind == exLit {
		return nil, nil
	}
	s, err := t.render(br, e.lo, e.hi)
	if err != nil {
		return nil, err
	}
	return []string{s}, nil
}

// project is WITH, or a RETURN NEXT follows: the working table becomes
// its list's columns.
func (t *gqlTranslator) project(with bool) error {
	distinct, items, err := t.items()
	if err != nil {
		return err
	}
	group, err := t.groupBy()
	if err != nil {
		return err
	}
	var tl gqlTail
	if !with {
		if err := t.tail(&tl); err != nil {
			return err
		}
	}
	br := t.one()
	var names []string
	alias := map[string]string{}
	cols := map[string]string{}
	aggregating := false
	for _, it := range items {
		if !it.star && t.verticalAggregate(it.e, br) {
			aggregating = true
		}
	}
	var keys []string
	for i, it := range items {
		if it.star {
			for _, n := range br.names {
				if !hasCol(names, n) {
					names = append(names, n)
				}
			}
			continue
		}
		name := strings.ToLower(it.alias)
		if name == "" {
			name = fmt.Sprintf("cb_col%d", i)
		}
		if n := t.bareName(it.e); n != "" && br.binds[n] != nil {
			names = append(names, name)
			alias[name] = n
			if aggregating && len(group) == 0 {
				k, err := t.groupKeys(br, it.e)
				if err != nil {
					return err
				}
				keys = append(keys, k...)
			}
			continue
		}
		s, err := t.render(br, it.e.lo, it.e.hi)
		if err != nil {
			return err
		}
		names = append(names, name)
		cols[name] = s
		if aggregating && len(group) == 0 && !t.verticalAggregate(it.e, br) && it.e.kind != exLit {
			keys = append(keys, s)
		}
	}
	for _, g := range group {
		k, err := t.groupKeys(br, g)
		if err != nil {
			return err
		}
		keys = append(keys, k...)
	}
	// ORDER BY may name the list's aliases
	ob := br.copy()
	for _, n := range names {
		if _, ok := cols[n]; ok && ob.binds[n] == nil {
			ob.binds[n] = &gqlBind{kind: bScalar, sql: gqlPrefix(n) + "__s"}
		}
	}
	tail, err := t.tailSQL(&tl, ob)
	if err != nil {
		return err
	}
	return t.projectInto(br, names, alias, cols, distinct, tail, keys...)
}

// finalReturn is the last RETURN (or GRAPH_TABLE's COLUMNS) as the SQL
// query's select list.
func (t *gqlTranslator) finalReturn() (string, error) {
	ps := t.ps
	if t.parens {
		if err := ps.expectP("("); err != nil {
			return "", err
		}
	}
	distinct, items, err := t.items()
	if err != nil {
		return "", err
	}
	if t.parens {
		if err := ps.expectP(")"); err != nil {
			return "", err
		}
	}
	var group []*sqlExpr
	var tl gqlTail
	if !t.parens {
		if group, err = t.groupBy(); err != nil {
			return "", err
		}
		if err := t.tail(&tl); err != nil {
			return "", err
		}
		if !ps.isP(";") && !ps.isP(")") && ps.peek().kind != tkEOF && !ps.isKw("UNION") && !ps.isKw("INTERSECT") && !ps.isKw("EXCEPT") {
			return "", unsupportedSQL{fmt.Sprintf("the graph query statement %s", strings.ToUpper(ps.peek().text))}
		}
	}
	aggregating := len(group) > 0
	for _, it := range items {
		if !it.star && t.verticalAggregate(it.e, t.branches[0]) {
			aggregating = true
		}
	}
	plain := !aggregating && !distinct && len(tl.order) == 0 && tl.limit[1] == 0 && tl.offset[1] == 0
	if len(t.branches) > 1 && plain {
		var parts []string
		for _, br := range t.branches {
			list, err := t.returnList(br, items)
			if err != nil {
				return "", err
			}
			parts = append(parts, selectSQL(list, br))
		}
		return t.finish(strings.Join(parts, " UNION ALL ")), nil
	}
	br := t.one()
	list, err := t.returnList(br, items)
	if err != nil {
		return "", err
	}
	s := selectSQL(list, br)
	if distinct {
		s = "SELECT DISTINCT" + strings.TrimPrefix(s, "SELECT")
	}
	var keys []string
	for _, g := range group {
		k, err := t.groupKeys(br, g)
		if err != nil {
			return "", err
		}
		keys = append(keys, k...)
	}
	if aggregating && len(group) == 0 {
		for _, it := range items {
			if it.star || t.verticalAggregate(it.e, br) {
				continue
			}
			k, err := t.groupKeys(br, it.e)
			if err != nil {
				return "", err
			}
			keys = append(keys, k...)
		}
	}
	if keys = dedupe(keys); len(keys) > 0 {
		s += " GROUP BY " + strings.Join(keys, ", ")
	}
	tail, err := t.tailSQL(&tl, br)
	if err != nil {
		return "", err
	}
	return t.finish(s + tail), nil
}

// returnList renders the final list in a branch.
func (t *gqlTranslator) returnList(br *gqlBranch, items []gqlItem) ([]string, error) {
	var list []string
	for _, it := range items {
		if it.star {
			for _, n := range br.names {
				b := br.binds[n]
				if b.kind != bScalar {
					return nil, unsupportedSQL{fmt.Sprintf("graph element %s used as a value", n)}
				}
				list = append(list, b.sql+" AS "+n)
			}
			continue
		}
		s, err := t.render(br, it.e.lo, it.e.hi)
		if err != nil {
			return nil, err
		}
		if it.alias != "" {
			s += " AS " + it.alias
		}
		list = append(list, s)
	}
	return list, nil
}

var quantRef = regexp.MustCompile(`\(\?(cb_q[0-9]+)\)`)

// finish writes each quantified edge's FROM item, now its uses are known.
func (t *gqlTranslator) finish(s string) string {
	return quantRef.ReplaceAllStringFunc(s, func(m string) string {
		q := t.quants[quantRef.FindStringSubmatch(m)[1]]
		return "(" + q.pathsSQL(q.min, q.max) + ")"
	})
}

// --- expressions ---------------------------------------------------------------------

func isNameTok(tk token) bool { return tk.kind == tkIdent || tk.kind == tkQIdent }

// render is GQL tokens [lo, hi) as SQL in a branch: var.property becomes
// the column it reads, a LET or FOR variable its value, an element
// comparison a comparison of keys, and the functions of a group variable
// (ARRAY_LENGTH, an aggregate of its property) and of a path variable
// (PATH_LENGTH) their values.
func (t *gqlTranslator) render(br *gqlBranch, lo, hi int) (string, error) {
	ps := t.ps
	if hi <= lo {
		return "", nil
	}
	if t.err != nil {
		return "", t.err
	}
	var b strings.Builder
	pos := ps.toks[lo].start
	emit := func(from, to int, s string) {
		b.WriteString(ps.src[pos:ps.toks[from].start])
		b.WriteString(s)
		pos = ps.toks[to].end
	}
	bindAt := func(i int) *gqlBind {
		if i >= hi || !isNameTok(ps.toks[i]) || i > lo && ps.toks[i-1].text == "." {
			return nil
		}
		return br.binds[strings.ToLower(ps.toks[i].text)]
	}
	for i := lo; i < hi; i++ {
		tk := ps.toks[i]
		if !isNameTok(tk) || i > lo && ps.toks[i-1].text == "." {
			continue
		}
		// functions of graph values
		if i+1 < hi && ps.toks[i+1].text == "(" && tk.kind == tkIdent {
			fn := strings.ToUpper(tk.text)
			arg := bindAt(i + 2)
			switch {
			case fn == "ARRAY_LENGTH" && arg != nil && arg.kind == bGroup && i+3 < hi && ps.toks[i+3].text == ")":
				v, err := t.exportValue(&gqlExport{kind: bGroup}, arg, "len")
				if err != nil {
					return "", err
				}
				emit(i, i+3, v)
				i += 3
				continue
			case fn == "PATH_LENGTH" && arg != nil && arg.kind == bPath && i+3 < hi && ps.toks[i+3].text == ")":
				emit(i, i+3, arg.pathLen)
				i += 3
				continue
			case fn == "LABELS" && arg != nil && arg.kind == bElem && i+3 < hi && ps.toks[i+3].text == ")":
				emit(i, i+3, labelsSQL(arg))
				i += 3
				continue
			case aggregateFuncs[fn]:
				j := i + 2
				dist := ""
				if j < hi && strings.EqualFold(ps.toks[j].text, "DISTINCT") {
					dist = "DISTINCT "
					j++
				}
				g := bindAt(j)
				if g != nil && g.kind == bGroup && j+3 < hi && ps.toks[j+1].text == "." && isNameTok(ps.toks[j+2]) && ps.toks[j+3].text == ")" {
					arr, err := t.exportValue(&gqlExport{kind: bGroup}, g, "a_"+strings.ToLower(ps.toks[j+2].text))
					if err != nil {
						return "", err
					}
					emit(i, j+3, "(SELECT "+fn+"("+dist+"cb_x) FROM UNNEST("+arr+") AS cb_x)")
					i = j + 3
					continue
				}
			}
			continue
		}
		v := bindAt(i)
		if v == nil {
			continue
		}
		switch v.kind {
		case bScalar:
			emit(i, i, v.sql)
			continue
		case bGroup:
			return "", unsupportedSQL{fmt.Sprintf("group variable %s, the edges of a quantified path, used other than in ARRAY_LENGTH or an aggregate of its property", tk.text)}
		case bPath:
			return "", unsupportedSQL{fmt.Sprintf("path variable %s used other than in PATH_LENGTH", tk.text)}
		}
		// an element: its property, or its comparison with another element
		if i+2 < hi && ps.toks[i+1].text == "." && isNameTok(ps.toks[i+2]) {
			col, err := v.prop(ps.toks[i+2].text)
			if err != nil {
				return "", err
			}
			emit(i, i+2, col)
			i += 2
			continue
		}
		if i+2 < hi && (ps.toks[i+1].text == "=" || ps.toks[i+1].text == "!=" || ps.toks[i+1].text == "<>") {
			if w := bindAt(i + 2); w != nil && w.kind == bElem && !(i+3 < hi && ps.toks[i+3].text == ".") {
				eq := elemEqual(v, w)
				if ps.toks[i+1].text != "=" {
					eq = "(NOT " + eq + ")"
				}
				emit(i, i+2, eq)
				i += 2
				continue
			}
		}
		return "", unsupportedSQL{fmt.Sprintf("graph element %s used as a value", tk.text)}
	}
	b.WriteString(ps.src[pos:ps.toks[hi-1].end])
	return b.String(), nil
}

// labelsSQL is LABELS(element).
func labelsSQL(b *gqlBind) string {
	arr := func(e *graphElement) string {
		var ls []string
		for _, l := range e.LabelNames {
			ls = append(ls, "'"+l+"'")
		}
		return "ARRAY<STRING>[" + strings.Join(ls, ", ") + "]"
	}
	if b.table != nil {
		return arr(b.table)
	}
	var w []string
	for _, e := range b.tables {
		w = append(w, "WHEN '"+e.Name+"' THEN "+arr(e))
	}
	return "(CASE " + b.qual + "." + b.prefix + "__t " + strings.Join(w, " ") + " END)"
}

func dedupe(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !hasCol(out, x) {
			out = append(out, x)
		}
	}
	return out
}
