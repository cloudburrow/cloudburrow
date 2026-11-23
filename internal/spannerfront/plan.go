package spannerfront

// Query plans (#2029). Google documents a query plan
// (google/spanner/v1/query_plan.proto, and the pages query-execution-plans
// and query-execution-operators) as a tree of PlanNodes: RELATIONAL
// operators (Distributed Union, Serialize Result, Scan with scan_type
// TableScan or IndexScan, Filter Scan, Filter, Aggregate, Sort, Limit, Cross
// Apply, Hash Join, ...) and the SCALAR expressions they evaluate, linked by
// childLinks whose type names the role (Seek Condition, Residual
// Condition, Split Range, Build, Probe, Input, Map, Condition).
//
// The Spanner emulator, CloudBurrow's Spanner engine, has no planner: it
// hands a query to GoogleSQL's reference evaluator and returns a placeholder
// plan ("No query plan"). So the front plans the query itself, from the
// query the engine is asked to run and the database's schema, which it
// reads from the engine (INFORMATION_SCHEMA): every table reference becomes
// a Scan of that table, or of the index FORCE_INDEX names (or of a
// covering index whose key the WHERE clause seeks on); the WHERE clause's
// conjuncts on a prefix of that key become the Seek Condition and the
// others the Residual Condition or a Filter; a join becomes a Cross Apply
// (co-located when the tables share their interleaving root), a
// Distributed Cross Apply, or the Hash Join or Merge Join its JOIN_METHOD
// hint asks for; GROUP BY and aggregates an Aggregate (Stream when the
// grouping keys are the scan's key prefix, else Hash, split into a local
// and a global one when it cannot run below the Distributed Union);
// ORDER BY a Sort, LIMIT a Limit (Sort Limit with ORDER BY), each split
// into the local-global pair the operator pages describe; UNION ALL a
// Union All of Union Inputs. Nothing in a plan is fixed in advance: a
// query the parser does not understand is refused by name (UNIMPLEMENTED),
// never given a guessed plan.
//
// PROFILE's statistics are counted by the engine (planstats.go): each
// operator's rows are what the engine returns for that operator's part of
// the query, run on the same data in the same transaction.

import (
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- The schema -------------------------------------------------------------

type keyCol struct {
	name string
	desc bool
}

type planTable struct {
	name   string
	parent string
	cols   []string
	pk     []keyCol
}

type planIndex struct {
	name    string
	table   string
	keys    []keyCol
	storing []string
	parent  string // the table it is interleaved in
}

type planSchema struct {
	pg      bool
	tables  map[string]*planTable // by lower-case name
	indexes map[string]*planIndex // by lower-case name
	views   map[string]string     // definitions, by lower-case name
}

func (s *planSchema) table(name string) *planTable { return s.tables[strings.ToLower(name)] }

// root is the top of a table's interleaving.
func (s *planSchema) root(t *planTable) string {
	for i := 0; t != nil && t.parent != "" && i < 16; i++ {
		p := s.table(t.parent)
		if p == nil {
			break
		}
		t = p
	}
	if t == nil {
		return ""
	}
	return t.name
}

func hasCol(cols []string, c string) bool {
	for _, x := range cols {
		if strings.EqualFold(x, c) {
			return true
		}
	}
	return false
}

// --- The plan's operators ----------------------------------------------------

type planLink struct {
	rel      *planOp
	sc       *planScalar
	typ      string
	variable string
}

type planOp struct {
	name string
	meta map[string]string
	kids []planLink
	// rows is SQL counting the rows the operator returns; "" for the rows
	// of its first relational child.
	rows string
	// execs is SQL counting the operator's executions; "" for one.
	execs string
	// scan is set on a table or index scan, whose rows are rows scanned.
	scan bool
	// filtered is set on a Filter Scan with a residual condition.
	filtered bool
	// affected is set on Apply Mutations, whose rows are the rows the
	// statement changed.
	affected bool
}

type planScalar struct {
	name, desc string
	kids       []planLink
}

func (o *planOp) firstRel() *planOp {
	for _, l := range o.kids {
		if l.rel != nil {
			return l.rel
		}
	}
	return nil
}

func op(name string, meta map[string]string, kids ...planLink) *planOp {
	if meta == nil {
		meta = map[string]string{}
	}
	if _, ok := meta["execution_method"]; !ok {
		meta["execution_method"] = "Row"
	}
	return &planOp{name: name, meta: meta, kids: kids}
}

func rel(o *planOp, typ string) planLink { return planLink{rel: o, typ: typ} }

// --- The builder --------------------------------------------------------------

type planItem struct {
	id    int
	alias string // what columns are qualified with
	table *planTable
	from  *sqlFrom
	cols  []string // the columns it offers, for a subquery, a view or UNNEST
	scope *planScope
	view  *planView
}

// planView is a view's definition, parsed from its own text.
type planView struct {
	ps *sqlParser
	q  *sqlQuery
}

type planScope struct {
	items  []*planItem
	parent *planScope
}

// cond is a WHERE or ON conjunct.
type cond struct {
	e       *sqlExpr
	items   map[*planItem]bool // the items of the current block it reads
	subq    bool
	applied bool
}

type planBuilder struct {
	ps     *sqlParser
	schema *planSchema
	ctes   map[string]*sqlCTE
	// cols resolves a column expression to its item and column name.
	cols map[*sqlExpr]colRef
	// subPlans are the plans of the subqueries in expressions.
	subPlans map[*sqlExpr]*planOp
	// orderFor are the ORDER BY expressions of the query a SELECT is the
	// body of, which read its columns too.
	orderFor map[*sqlSelect][]sqlOrder
	// dmlTarget is the table a DML statement changes: its scan reads its
	// key, which the mutations name.
	dmlTarget *sqlFrom
	// quant are a graph query's quantified edge patterns, by the
	// lower-case alias of the FROM item they became (plangraph.go)
	quant map[string]*graphQuant
	// topSel is the top-level query's SELECT, when the query returns all
	// its rows (no LIMIT or OFFSET)
	topSel *sqlSelect
	next   int
}

type colRef struct {
	item *planItem
	name string
}

// countCtx turns a rows query into a count query, for the operators of a
// subquery evaluated once per row of an outer one.
type countCtx struct {
	sumOver func(expr string) string
}

var topCount = &countCtx{sumOver: func(expr string) string { return "SELECT " + expr }}

func (c *countCtx) count(rowsSQL string) string {
	return c.sumOver("(SELECT COUNT(*) FROM (" + rowsSQL + ") AS cb_rows)")
}

// sub is the context of a subquery run once per row of rowsFrom.
func (c *countCtx) sub(from string, where []string) *countCtx {
	if from == "" {
		return c
	}
	return &countCtx{sumOver: func(expr string) string {
		return c.sumOver("COALESCE((SELECT SUM(" + expr + ") FROM " + from + whereSQL(where) + "), 0)")
	}}
}

// groups is the context of a subquery run once per group of a SELECT
// that aggregates: per GROUP BY group (after HAVING), or once for a
// SELECT with no GROUP BY.
func (c *countCtx) groups(from string, where, groupBy []string, having string) *countCtx {
	if from == "" || len(groupBy) == 0 && having == "" {
		return c
	}
	inner := "SELECT 1 AS cb_one FROM " + from + whereSQL(where)
	if len(groupBy) > 0 {
		inner += " GROUP BY " + strings.Join(groupBy, ", ")
	}
	if having != "" {
		inner += " HAVING " + having
	}
	return &countCtx{sumOver: func(expr string) string {
		if len(groupBy) == 0 {
			return c.sumOver("COALESCE((SELECT SUM(" + expr + ") FROM (" + inner + ") AS cb_grp), 0)")
		}
		sel := "SELECT " + expr + " AS cb_n FROM " + from + whereSQL(where) + " GROUP BY " + strings.Join(groupBy, ", ")
		if having != "" {
			sel += " HAVING " + having
		}
		return c.sumOver("COALESCE((SELECT SUM(cb_n) FROM (" + sel + ") AS cb_grp), 0)")
	}}
}

func whereSQL(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE (" + strings.Join(conds, ") AND (") + ")"
}

// frag is a block's plan so far.
type frag struct {
	op    *planOp
	dist  bool   // still below the Distributed Union it needs
	root  string // the interleaving root its scans read
	items []*planItem
	from  string   // its FROM, as SQL
	conds []string // the conditions it applied, as SQL
	split []*planScalar
	// keyOrder is the key its rows come in, when it is one scan.
	keyOrder []string
	// out is SQL for its output rows once it aggregated; "" while they are
	// rowsSQL's.
	out string
}

func (f *frag) rowsSQL() string {
	if f.from == "" {
		return "SELECT 1"
	}
	return "SELECT 1 FROM " + f.from + whereSQL(f.conds)
}

func (f *frag) outSQL() string {
	if f.out != "" {
		return f.out
	}
	return f.rowsSQL()
}

func (b *planBuilder) text(e *sqlExpr) string { return b.ps.exprText(e) }

// rowsReturned, as an operator's rows, is the rows the query returned.
const rowsReturned = "@returned"

func (b *planBuilder) aggregates(sel *sqlSelect) bool {
	for _, it := range sel.list {
		if hasAggregate(it.e) {
			return true
		}
	}
	return false
}

// buildPlan plans the parsed top-level query.
func (b *planBuilder) buildPlan(q *sqlQuery) (*planOp, error) {
	if q.body.sel != nil && q.limit == nil && q.offset == nil {
		b.topSel = q.body.sel
	}
	for i := range q.with {
		b.ctes[strings.ToLower(q.with[i].name)] = &q.with[i]
	}
	f, err := b.planQuery(q, nil, topCount)
	if err != nil {
		return nil, err
	}
	return b.serialize(f.items, q, f), nil
}

// buildDML plans an INSERT, UPDATE or DELETE: Apply Mutations (Google's
// unary operators page: "the top operator in a query plan for a DML
// statement") over the rows it changes or inserts, and Serialize Result
// over it when THEN RETURN returns them.
func (b *planBuilder) buildDML(d *sqlDML) (*planOp, error) {
	var in *planOp
	var items []*planItem
	switch {
	case d.kind == "INSERT" && d.query != nil:
		f, err := b.planQuery(d.query, nil, topCount)
		if err != nil {
			return nil, err
		}
		in = b.gather(f)
	case d.kind == "INSERT":
		var inputs []planLink
		for i := 0; i < d.rows; i++ {
			u := op("Unit Relation", nil)
			u.rows = topCount.count("SELECT 1")
			inputs = append(inputs, rel(op("Union Input", nil, rel(u, "")), ""))
		}
		in = inputs[0].rel.firstRel()
		if len(inputs) > 1 {
			in = op("Union All", nil, inputs...)
			in.rows = "SELECT " + fmt.Sprint(d.rows)
		}
	default:
		// the rows a DELETE or UPDATE changes: its WHERE clause's
		b.dmlTarget = d.table
		var list []sqlItem
		for _, e := range d.set {
			list = append(list, sqlItem{e: e})
		}
		sel := &sqlSelect{list: list, from: d.table, where: d.where, lo: d.lo, hi: d.hi}
		f, err := b.planSelect(sel, nil, topCount)
		if err != nil {
			return nil, err
		}
		items = f.items
		in = b.gather(f)
	}
	am := op("Apply Mutations", nil, rel(in, ""))
	am.affected = true
	if len(d.returning) == 0 {
		return am, nil
	}
	var outs []planLink
	for _, it := range d.returning {
		if it.e.kind == exStar {
			if t := b.schema.table(d.table.name[0]); t != nil {
				for _, c := range t.cols {
					outs = append(outs, planLink{sc: &planScalar{name: "Reference", desc: "$" + c}})
				}
			}
			continue
		}
		if items != nil {
			b.resolve(it.e, items[0].scope)
		}
		outs = append(outs, planLink{sc: b.scalar(it.e, nil)})
	}
	return op("Serialize Result", nil, append([]planLink{rel(am, "")}, outs...)...), nil
}

// serialize puts Serialize Result on top of the query's plan: below its
// Distributed Union when everything runs there, else at the root.
func (b *planBuilder) serialize(items []*planItem, q *sqlQuery, f *frag) *planOp {
	var outs []planLink
	if sel := leafSelect(q); sel != nil {
		for _, it := range sel.list {
			if it.e.kind == exStar {
				for _, c := range b.starCols(it.e, items) {
					outs = append(outs, planLink{sc: &planScalar{name: "Reference", desc: "$" + c}})
				}
				continue
			}
			outs = append(outs, planLink{sc: b.scalar(it.e, f)})
		}
	}
	if f.dist {
		sr := op("Serialize Result", nil, append([]planLink{rel(f.op, "")}, outs...)...)
		du := b.distUnion(sr, f)
		f.op, f.dist = du, false
		return du
	}
	return op("Serialize Result", nil, append([]planLink{rel(f.op, "")}, outs...)...)
}

func leafSelect(q *sqlQuery) *sqlSelect {
	if q.body.sel != nil {
		return q.body.sel
	}
	if q.body.paren != nil {
		return leafSelect(q.body.paren)
	}
	if q.body.left != nil {
		s := q.body.left
		for s.sel == nil && s.left != nil {
			s = s.left
		}
		return s.sel
	}
	return nil
}

func (b *planBuilder) starCols(e *sqlExpr, items []*planItem) []string {
	var out []string
	for _, it := range items {
		if len(e.path) > 0 && !strings.EqualFold(e.path[0], it.alias) {
			continue
		}
		if it.table != nil {
			out = append(out, it.table.cols...)
		} else {
			out = append(out, it.cols...)
		}
	}
	return out
}

// distUnion puts the global and local Distributed Union over a fragment.
func (b *planBuilder) distUnion(o *planOp, f *frag) *planOp {
	local := op("Distributed Union", map[string]string{"call_type": "Local", "distribution_table": f.root}, rel(o, ""))
	links := []planLink{rel(local, "")}
	for _, s := range f.split {
		links = append(links, planLink{sc: s, typ: "Split Range"})
	}
	return op("Distributed Union", map[string]string{"call_type": "Global", "distribution_table": f.root}, links...)
}

// gather ends a fragment's distribution: its plan as the root sees it.
func (b *planBuilder) gather(f *frag) *planOp {
	if !f.dist {
		return f.op
	}
	f.op, f.dist = b.distUnion(f.op, f), false
	return f.op
}

// planQuery plans a query: its set operation, ORDER BY and LIMIT.
func (b *planBuilder) planQuery(q *sqlQuery, outer *planScope, cc *countCtx) (*frag, error) {
	for i := range q.with {
		b.ctes[strings.ToLower(q.with[i].name)] = &q.with[i]
	}
	if q.body.sel != nil && len(q.order) > 0 {
		b.orderFor[q.body.sel] = q.order
	}
	f, err := b.planSet(q.body, outer, cc)
	if err != nil {
		return nil, err
	}
	qText := b.ps.text(q.lo, q.hi)
	full := cc.count("SELECT 1 FROM (" + qText + ") AS cb_q")
	limit0 := q.limit != nil && b.text(q.limit) == "0"
	switch {
	case limit0:
		f.op = op("Empty Relation", nil)
		f.op.rows = full
		f.dist = false
	case len(q.order) > 0:
		keys := make([]planLink, 0, len(q.order))
		for _, o := range q.order {
			s := b.scalar(o.e, f)
			if o.desc {
				s.desc += " DESC"
			} else {
				s.desc += " ASC"
			}
			keys = append(keys, planLink{sc: s, typ: "Key"})
		}
		name := "Sort"
		if q.limit != nil {
			name = "Sort Limit"
			keys = append(keys, planLink{sc: b.scalar(q.limit, f), typ: "Limit"})
			if q.offset != nil {
				keys = append(keys, planLink{sc: b.scalar(q.offset, f), typ: "Offset"})
			}
		}
		if f.dist && q.limit != nil {
			local := op(name, map[string]string{"call_type": "Local"}, append([]planLink{rel(f.op, "")}, keys...)...)
			local.rows = b.atMost(cc.count(f.outSQL()), q)
			f.op = local
		}
		in := b.gather(f)
		meta := map[string]string{}
		if q.limit != nil {
			meta["call_type"] = "Global"
		}
		f.op = op(name, meta, append([]planLink{rel(in, "")}, keys...)...)
		if q.limit != nil {
			f.op.rows = full
		}
	case q.limit != nil || q.offset != nil:
		var lim []planLink
		if q.limit != nil {
			lim = append(lim, planLink{sc: b.scalar(q.limit, f), typ: "Limit"})
		}
		if q.offset != nil {
			lim = append(lim, planLink{sc: b.scalar(q.offset, f), typ: "Offset"})
		}
		if f.dist && q.limit != nil {
			local := op("Limit", map[string]string{"call_type": "Local"}, rel(f.op, ""), planLink{sc: b.scalar(q.limit, f), typ: "Limit"})
			local.rows = b.atMost(cc.count(f.outSQL()), q)
			f.op = local
		}
		in := b.gather(f)
		f.op = op("Limit", map[string]string{"call_type": "Global"}, append([]planLink{rel(in, "")}, lim...)...)
		f.op.rows = full
	}
	return f, nil
}

// atMost is the rows a local limit returns: its input's, up to LIMIT plus
// OFFSET (each split keeps that many for the global limit to skip and cut).
func (b *planBuilder) atMost(count string, q *sqlQuery) string {
	n := "(" + b.text(q.limit) + ")"
	if q.offset != nil {
		n += " + (" + b.text(q.offset) + ")"
	}
	return "SELECT CASE WHEN (" + count + ") < " + n + " THEN (" + count + ") ELSE " + n + " END"
}

func (b *planBuilder) planSet(s *sqlSet, outer *planScope, cc *countCtx) (*frag, error) {
	switch {
	case s.sel != nil:
		return b.planSelect(s.sel, outer, cc)
	case s.paren != nil:
		f, err := b.planQuery(s.paren, outer, cc)
		if err != nil {
			return nil, err
		}
		b.gather(f)
		return f, nil
	}
	if s.op != "UNION" {
		return b.planIntersectExcept(s, outer, cc)
	}
	var inputs []*planOp
	var flatten func(x *sqlSet) error
	flatten = func(x *sqlSet) error {
		if x.op == "UNION" && x.all == s.all {
			if err := flatten(x.left); err != nil {
				return err
			}
			return flatten(x.right)
		}
		f, err := b.planSet(x, outer, cc)
		if err != nil {
			return err
		}
		in := op("Union Input", nil, rel(b.gather(f), ""))
		in.rows = cc.count(b.setRows(x))
		inputs = append(inputs, in)
		return nil
	}
	if err := flatten(s); err != nil {
		return nil, err
	}
	var links []planLink
	for _, in := range inputs {
		links = append(links, rel(in, ""))
	}
	u := op("Union All", nil, links...)
	all := s.all
	if !all {
		// the rows of UNION ALL over the same inputs
		u.rows = cc.count(strings.Replace(b.setRows(s), "UNION DISTINCT", "UNION ALL", -1))
		u.rows = ""
		var sum []string
		for _, in := range inputs {
			sum = append(sum, "("+in.rows+")")
		}
		u.rows = "SELECT " + strings.Join(sum, " + ")
		agg := op("Aggregate", map[string]string{"iterator_type": "Hash", "call_type": "Global"}, rel(u, ""))
		agg.rows = cc.count(b.setRows(s))
		return &frag{op: agg}, nil
	}
	var sum []string
	for _, in := range inputs {
		sum = append(sum, "("+in.rows+")")
	}
	u.rows = "SELECT " + strings.Join(sum, " + ")
	return &frag{op: u}, nil
}

// planIntersectExcept plans INTERSECT and EXCEPT. Google's operator pages
// give neither an operator of its own, so the plan is made of the ones
// they document: the inputs' Union All, each Union Input tagging its rows
// with its side ($set_input); a Hash Aggregate counting, for each distinct
// row, the rows of each side; a Filter keeping the rows the operation
// keeps; and, for ALL, a Cross Apply over an Array Unnest that returns
// each kept row as many times as the operation keeps it (INTERSECT ALL:
// MIN(m, n), EXCEPT ALL: MAX(m - n, 0), the query syntax pages).
func (b *planBuilder) planIntersectExcept(s *sqlSet, outer *planScope, cc *countCtx) (*frag, error) {
	var inputs []planLink
	var sum []string
	for i, side := range []*sqlSet{s.left, s.right} {
		f, err := b.planSet(side, outer, cc)
		if err != nil {
			return nil, err
		}
		in := op("Union Input", nil, rel(b.gather(f), ""), planLink{sc: &planScalar{name: "Constant", desc: fmt.Sprint(i)}, variable: "set_input"})
		in.rows = cc.count(b.setRows(side))
		inputs = append(inputs, rel(in, ""))
		sum = append(sum, "("+in.rows+")")
	}
	u := op("Union All", nil, inputs...)
	u.rows = "SELECT " + strings.Join(sum, " + ")
	var keys []planLink
	for _, c := range b.setCols(s) {
		keys = append(keys, planLink{sc: &planScalar{name: "Reference", desc: "$" + c}, typ: "Key"})
	}
	aggs := []planLink{
		{sc: &planScalar{name: "Aggregate", desc: "COUNTIF($set_input = 0)"}, typ: "Agg", variable: "left_count"},
		{sc: &planScalar{name: "Aggregate", desc: "COUNTIF($set_input = 1)"}, typ: "Agg", variable: "right_count"},
	}
	agg := op("Aggregate", map[string]string{"iterator_type": "Hash", "call_type": "Global"}, append(append([]planLink{rel(u, "Input")}, keys...), aggs...)...)
	l, r := b.ps.text(s.left.lo, s.left.hi), b.ps.text(s.right.lo, s.right.hi)
	agg.rows = cc.count("SELECT 1 FROM ((" + l + ") UNION DISTINCT (" + r + ")) AS cb_s")
	cond := "($left_count > 0) AND ($right_count > 0)"
	switch {
	case s.op == "EXCEPT" && s.all:
		cond = "($left_count > $right_count)"
	case s.op == "EXCEPT":
		cond = "($left_count > 0) AND ($right_count = 0)"
	}
	fl := op("Filter", nil, rel(agg, ""), planLink{sc: &planScalar{name: "Function", desc: cond}, typ: "Condition"})
	all := b.setRows(s)
	if !s.all {
		fl.rows = cc.count(all)
		return &frag{op: fl}, nil
	}
	// the distinct rows the operation keeps
	st := b.ps.text(s.lo, s.hi)
	fl.rows = cc.count("SELECT 1 FROM ((" + st + ") INTERSECT DISTINCT (" + st + ")) AS cb_s")
	times := "LEAST($left_count, $right_count)"
	if s.op == "EXCEPT" {
		times = "$left_count - $right_count"
	}
	un := op("Array Unnest", nil, planLink{sc: &planScalar{name: "Function", desc: "GENERATE_ARRAY(1, " + times + ")"}})
	un.rows, un.execs = cc.count(all), fl.rows
	ca := op("Cross Apply", nil, rel(fl, "Input"), rel(un, "Map"))
	ca.rows = cc.count(all)
	return &frag{op: ca}, nil
}

// setCols are a set operation's column names: its first input's.
func (b *planBuilder) setCols(s *sqlSet) []string {
	for s.sel == nil && s.paren == nil && s.left != nil {
		s = s.left
	}
	var cols []string
	switch {
	case s.sel != nil:
		cols = b.queryCols(&sqlQuery{body: s})
	case s.paren != nil:
		cols = b.queryCols(s.paren)
	}
	return cols
}

func (b *planBuilder) setRows(s *sqlSet) string {
	return "SELECT 1 FROM (" + b.ps.text(s.lo, s.hi) + ") AS cb_s"
}

// --- A SELECT ------------------------------------------------------------------

func (b *planBuilder) planSelect(sel *sqlSelect, outer *planScope, cc *countCtx) (*frag, error) {
	scope := &planScope{parent: outer}
	if sel.from == nil {
		f := &frag{op: op("Unit Relation", nil), from: "", conds: nil}
		f.op.rows = cc.count("SELECT 1")
		if sel.where != nil || len(sel.groupBy) > 0 || sel.having != nil {
			return nil, unsupportedSQL{"a SELECT with no FROM clause and a WHERE, GROUP BY or HAVING clause"}
		}
		f.items = nil
		return b.planOutput(sel, f, scope, cc)
	}
	if err := b.collectItems(sel.from, scope); err != nil {
		return nil, err
	}
	// resolve every column the block names
	exprs := []*sqlExpr{sel.where, sel.having}
	for _, it := range sel.list {
		exprs = append(exprs, it.e)
	}
	exprs = append(exprs, sel.groupBy...)
	for _, o := range b.orderFor[sel] {
		exprs = append(exprs, o.e)
	}
	b.joinConds(sel.from, &exprs)
	for _, e := range exprs {
		b.resolve(e, scope)
	}
	var conds []*cond
	for _, c := range conjuncts(sel.where) {
		conds = append(conds, b.newCond(c, scope))
	}
	needed := b.neededCols(sel, scope, exprs)
	f, err := b.planFrom(sel.from, scope, &conds, needed, cc, nil)
	if err != nil {
		return nil, err
	}
	// BERNOULLI keeps a different share of the rows each run, so when a
	// sample is all the query returns, its rows are the rows returned
	if sel == b.topSel && sel.from.kind == fromTable && sel.from.sample != nil && sel.from.sample.method == "BERNOULLI" &&
		sel.where == nil && len(sel.groupBy) == 0 && sel.having == nil && !sel.distinct && !b.aggregates(sel) {
		f.op.rows = rowsReturned
	}
	// what the FROM did not apply
	var semis, filters []*cond
	for _, c := range conds {
		if c.applied {
			continue
		}
		if c.e.kind == exSubq && (c.e.subKind == "EXISTS" || strings.HasSuffix(c.e.subKind, "IN")) ||
			c.e.kind == exUnary && c.e.op == "NOT" && c.e.args[0].kind == exSubq && c.e.args[0].subKind == "EXISTS" {
			semis = append(semis, c)
		} else {
			filters = append(filters, c)
		}
	}
	if len(filters) > 0 {
		var links []planLink
		for _, c := range filters {
			if err := b.planSubqueries(c.e, scope, cc, f); err != nil {
				return nil, err
			}
			links = append(links, planLink{sc: b.scalar(c.e, f), typ: "Condition"})
			f.conds = append(f.conds, b.text(c.e))
			c.applied = true
		}
		fl := op("Filter", nil, append([]planLink{rel(f.op, "")}, links...)...)
		fl.rows = cc.count(f.rowsSQL())
		f.op = fl
	}
	for _, c := range semis {
		e, anti := c.e, false
		if e.kind == exUnary {
			e, anti = e.args[0], true
		}
		if strings.HasPrefix(e.subKind, "NOT") {
			anti = true
		}
		in := b.gather(f)
		sub, err := b.planQuery(e.sub, scope, cc.sub(f.from, f.conds))
		if err != nil {
			return nil, err
		}
		name := "Semi Apply"
		if anti {
			name = "Anti Semi Apply"
		}
		links := []planLink{rel(in, "Input"), rel(b.gather(sub), "Map")}
		if strings.HasSuffix(e.subKind, "IN") {
			links = append(links, planLink{sc: b.scalar(e.args[0], f), typ: "Condition"})
		}
		f.op = op(name, nil, links...)
		f.conds = append(f.conds, b.text(c.e))
		f.op.rows = cc.count(f.rowsSQL())
		c.applied = true
	}
	return b.planOutput(sel, f, scope, cc)
}

// planOutput plans the aggregation, HAVING and DISTINCT of a SELECT.
func (b *planBuilder) planOutput(sel *sqlSelect, f *frag, scope *planScope, cc *countCtx) (*frag, error) {
	aggregating := len(sel.groupBy) > 0 || sel.having != nil
	for _, it := range sel.list {
		if hasAggregate(it.e) {
			aggregating = true
		}
	}
	selText := b.ps.text(sel.lo, sel.hi)
	if !aggregating {
		for _, it := range sel.list {
			if err := b.planSubqueries(it.e, scope, cc, f); err != nil {
				return nil, err
			}
		}
	}
	if aggregating {
		// a subquery in an aggregate's argument runs once per input row,
		// one elsewhere in the select list once per group
		inAgg, perGroup := aggSubqueries(sel.list)
		for _, x := range inAgg {
			if err := b.planSubqueries(x, scope, cc, f); err != nil {
				return nil, err
			}
		}
		groupFrom, groupConds := f.from, append([]string(nil), f.conds...)
		upTo := sel.hi
		if sel.having != nil {
			upTo = sel.having.lo - 1
		}
		groupRows := cc.count("SELECT 1 FROM (" + b.ps.text(sel.lo, upTo) + ") AS cb_g")
		var keys []planLink
		var keyCols []string
		for _, g := range sel.groupBy {
			keys = append(keys, planLink{sc: b.scalar(g, f), typ: "Key"})
			if r, ok := b.cols[g]; ok {
				keyCols = append(keyCols, r.name)
			}
		}
		var aggs []planLink
		for _, it := range sel.list {
			walkExpr(it.e, func(x *sqlExpr) {
				if x.kind == exFunc && aggregateFuncs[x.op] {
					aggs = append(aggs, planLink{sc: &planScalar{name: "Aggregate", desc: b.render(x)}, typ: "Agg"})
				}
			})
		}
		iter := "Hash"
		if len(sel.groupBy) == 0 || prefixOf(keyCols, f.keyOrder) {
			iter = "Stream"
		}
		meta := func(call string) map[string]string {
			m := map[string]string{"iterator_type": iter}
			if call != "" {
				m["call_type"] = call
			}
			return m
		}
		links := append(append([]planLink{}, keys...), aggs...)
		if f.dist && len(sel.groupBy) > 0 && b.groupsOnRoot(sel.groupBy, f) {
			a := op("Aggregate", meta(""), append([]planLink{rel(f.op, "Input")}, links...)...)
			a.rows = groupRows
			f.op = a
		} else {
			if f.dist {
				local := op("Aggregate", meta("Local"), append([]planLink{rel(f.op, "Input")}, links...)...)
				local.rows = groupRows
				f.op = local
			}
			in := b.gather(f)
			g := op("Aggregate", meta("Global"), append([]planLink{rel(in, "Input")}, links...)...)
			g.rows = groupRows
			f.op = g
		}
		if sel.having != nil {
			fl := op("Filter", nil, rel(f.op, ""), planLink{sc: b.scalar(sel.having, f), typ: "Condition"})
			fl.rows = cc.count("SELECT 1 FROM (" + selText + ") AS cb_h")
			f.op = fl
		}
		if len(perGroup) > 0 {
			var group []string
			for _, g := range sel.groupBy {
				group = append(group, b.text(g))
			}
			having := ""
			if sel.having != nil {
				having = b.text(sel.having)
			}
			gcc := cc.groups(groupFrom, groupConds, group, having)
			for _, x := range perGroup {
				if err := b.planSubqueriesIn(x, scope, gcc, f); err != nil {
					return nil, err
				}
			}
		}
	}
	if sel.distinct {
		var keys []planLink
		for _, it := range sel.list {
			keys = append(keys, planLink{sc: b.scalar(it.e, f), typ: "Key"})
		}
		rows := cc.count("SELECT 1 FROM (" + selText + ") AS cb_d")
		if f.dist {
			local := op("Aggregate", map[string]string{"iterator_type": "Hash", "call_type": "Local"}, append([]planLink{rel(f.op, "Input")}, keys...)...)
			local.rows = rows
			f.op = local
		}
		in := b.gather(f)
		f.op = op("Aggregate", map[string]string{"iterator_type": "Hash", "call_type": "Global"}, append([]planLink{rel(in, "Input")}, keys...)...)
		f.op.rows = rows
	}
	f.items = scope.items
	return f, nil
}

// groupsOnRoot says whether the groups are within one split: the grouping
// keys include the interleaving root's first key column.
func (b *planBuilder) groupsOnRoot(group []*sqlExpr, f *frag) bool {
	root := b.schema.table(f.root)
	if root == nil || len(root.pk) == 0 {
		return false
	}
	for _, g := range group {
		r, ok := b.cols[g]
		if ok && r.item.table != nil && b.schema.root(r.item.table) == root.name && strings.EqualFold(r.name, root.pk[0].name) {
			return true
		}
	}
	return false
}

func prefixOf(cols, order []string) bool {
	if len(cols) == 0 || len(cols) > len(order) {
		return false
	}
	for _, c := range cols {
		if !hasCol(order[:len(cols)], c) {
			return false
		}
	}
	return true
}

// planSubqueries plans the subqueries in e, each run once per row of f.
func (b *planBuilder) planSubqueries(e *sqlExpr, scope *planScope, cc *countCtx, f *frag) error {
	return b.planSubqueriesIn(e, scope, cc.sub(f.from, f.conds), f)
}

// planSubqueriesIn plans the subqueries in e, counting their rows in cc.
func (b *planBuilder) planSubqueriesIn(e *sqlExpr, scope *planScope, cc *countCtx, f *frag) error {
	var err error
	walkExpr(e, func(x *sqlExpr) {
		if err != nil || x.kind != exSubq || b.subPlans[x] != nil {
			return
		}
		sub, e2 := b.planQuery(x.sub, scope, cc)
		if e2 != nil {
			err = e2
			return
		}
		b.subPlans[x] = b.gather(sub)
	})
	return err
}

// aggSubqueries splits an aggregating select list's subqueries: those in
// an aggregate's arguments, and the others.
func aggSubqueries(list []sqlItem) (inAgg, perGroup []*sqlExpr) {
	var walk func(e *sqlExpr, agg bool)
	walk = func(e *sqlExpr, agg bool) {
		if e == nil {
			return
		}
		if e.kind == exSubq {
			if agg {
				inAgg = append(inAgg, e)
			} else {
				perGroup = append(perGroup, e)
			}
			return
		}
		inner := agg || e.kind == exFunc && aggregateFuncs[e.op]
		for _, a := range e.args {
			walk(a, inner)
		}
	}
	for _, it := range list {
		walk(it.e, false)
	}
	return inAgg, perGroup
}

func hasSubquery(e *sqlExpr) bool {
	found := false
	walkExpr(e, func(x *sqlExpr) {
		if x.kind == exSubq {
			found = true
		}
	})
	return found
}

// --- Items and columns -------------------------------------------------------

func (b *planBuilder) collectItems(f *sqlFrom, scope *planScope) error {
	if f.kind == fromJoin {
		if err := b.collectItems(f.left, scope); err != nil {
			return err
		}
		return b.collectItems(f.right, scope)
	}
	it := &planItem{id: b.next, from: f, scope: scope}
	b.next++
	switch f.kind {
	case fromTable:
		name := f.name[len(f.name)-1]
		it.alias = name
		if len(f.name) == 1 {
			if t := b.schema.table(name); t != nil && b.ctes[strings.ToLower(name)] == nil {
				it.table = t
			} else if cte := b.ctes[strings.ToLower(name)]; cte != nil {
				it.cols = b.queryCols(cte.q)
			} else if v, ok := b.schema.views[strings.ToLower(name)]; ok {
				// a view is planned as its definition, inlined
				vps, vq, err := parseQuerySQL(v, b.schema.pg)
				if err != nil {
					if u, ok := err.(unsupportedSQL); ok {
						return unsupportedSQL{fmt.Sprintf("view %s, whose definition has %s", name, u.what)}
					}
					return err
				}
				it.view = &planView{ps: vps, q: vq}
				it.cols = b.queryCols(vq)
			} else {
				return unsupportedSQL{fmt.Sprintf("table %s, which is not in the schema", name)}
			}
		} else if r, ok := b.lookup(f.name[0], scope); ok && r.item != nil {
			// a path into an earlier item's array: an implicit UNNEST
			f.kind = fromUnnest
			f.arr = &sqlExpr{kind: exCol, path: f.name, lo: f.lo, hi: f.lo + 2*len(f.name) - 1}
			it.cols = []string{f.name[len(f.name)-1]}
			it.alias = f.name[len(f.name)-1]
		} else {
			return unsupportedSQL{fmt.Sprintf("table %s", strings.Join(f.name, "."))}
		}
	case fromSub:
		it.cols = b.queryCols(f.sub)
	case fromUnnest:
		it.cols = []string{f.alias}
	}
	if f.alias != "" {
		it.alias = f.alias
	}
	scope.items = append(scope.items, it)
	return nil
}

// queryCols are the names of a query's output columns.
func (b *planBuilder) queryCols(q *sqlQuery) []string {
	sel := leafSelect(q)
	if sel == nil {
		return nil
	}
	var out []string
	for _, it := range sel.list {
		switch {
		case it.alias != "":
			out = append(out, it.alias)
		case it.e.kind == exCol:
			out = append(out, it.e.path[len(it.e.path)-1])
		case it.e.kind == exStar && sel.from != nil:
			// the columns of the tables it reads
			var walk func(f *sqlFrom)
			walk = func(f *sqlFrom) {
				switch f.kind {
				case fromJoin:
					walk(f.left)
					walk(f.right)
				case fromTable:
					if t := b.schema.table(f.name[len(f.name)-1]); t != nil {
						out = append(out, t.cols...)
					}
				}
			}
			walk(sel.from)
		}
	}
	return out
}

func (b *planBuilder) joinConds(f *sqlFrom, out *[]*sqlExpr) {
	if f.kind != fromJoin {
		if f.kind == fromUnnest {
			*out = append(*out, f.arr)
		}
		return
	}
	b.joinConds(f.left, out)
	b.joinConds(f.right, out)
	if f.on != nil {
		*out = append(*out, f.on)
	}
}

// lookup finds the item a qualifier names, in scope or an outer one.
func (b *planBuilder) lookup(name string, scope *planScope) (colRef, bool) {
	for s := scope; s != nil; s = s.parent {
		for _, it := range s.items {
			if strings.EqualFold(it.alias, name) {
				return colRef{item: it}, true
			}
		}
	}
	return colRef{}, false
}

func (it *planItem) has(col string) bool {
	if it.table != nil {
		return hasCol(it.table.cols, col)
	}
	return hasCol(it.cols, col)
}

func (it *planItem) canonical(col string) string {
	list := it.cols
	if it.table != nil {
		list = it.table.cols
	}
	for _, c := range list {
		if strings.EqualFold(c, col) {
			return c
		}
	}
	return col
}

// resolve records the item and column of every column expression in e,
// and resolves the subqueries' own blocks lazily (when they are planned).
func (b *planBuilder) resolve(e *sqlExpr, scope *planScope) {
	walkExpr(e, func(x *sqlExpr) {
		if x.kind != exCol || len(x.path) == 0 {
			return
		}
		if len(x.path) >= 2 {
			if r, ok := b.lookup(x.path[0], scope); ok && r.item.has(x.path[1]) {
				b.cols[x] = colRef{item: r.item, name: r.item.canonical(x.path[1])}
				return
			}
		}
		for s := scope; s != nil; s = s.parent {
			for _, it := range s.items {
				if it.has(x.path[0]) {
					b.cols[x] = colRef{item: it, name: it.canonical(x.path[0])}
					return
				}
			}
		}
	})
}

func (b *planBuilder) newCond(e *sqlExpr, scope *planScope) *cond {
	c := &cond{e: e, items: map[*planItem]bool{}}
	walkExpr(e, func(x *sqlExpr) {
		if r, ok := b.cols[x]; ok && r.item.scope == scope {
			c.items[r.item] = true
		}
		if x.kind == exSubq {
			c.subq = true
		}
	})
	return c
}

// neededCols are the columns each item must return.
func (b *planBuilder) neededCols(sel *sqlSelect, scope *planScope, exprs []*sqlExpr) map[*planItem][]string {
	out := map[*planItem][]string{}
	add := func(it *planItem, c string) {
		if !hasCol(out[it], c) {
			out[it] = append(out[it], c)
		}
	}
	for _, it := range sel.list {
		if it.e.kind == exStar {
			for _, item := range scope.items {
				if len(it.e.path) > 0 && !strings.EqualFold(it.e.path[0], item.alias) {
					continue
				}
				if item.table != nil {
					for _, c := range item.table.cols {
						add(item, c)
					}
				}
			}
		}
	}
	for _, e := range exprs {
		walkExpr(e, func(x *sqlExpr) {
			if r, ok := b.cols[x]; ok && r.item.scope == scope {
				add(r.item, r.name)
			}
		})
	}
	for it := range out {
		sort.SliceStable(out[it], func(i, j int) bool { return colPos(it, out[it][i]) < colPos(it, out[it][j]) })
	}
	return out
}

func colPos(it *planItem, c string) int {
	list := it.cols
	if it.table != nil {
		list = it.table.cols
	}
	for i, x := range list {
		if strings.EqualFold(x, c) {
			return i
		}
	}
	return len(list)
}

// --- The FROM clause ------------------------------------------------------------

// itemSQL is a FROM item's text, for the counts.
func (b *planBuilder) itemSQL(f *sqlFrom) string { return b.ps.text(f.lo, f.hi) }

func (b *planBuilder) planFrom(f *sqlFrom, scope *planScope, conds *[]*cond, needed map[*planItem][]string, cc *countCtx, bound *frag) (*frag, error) {
	if f.kind != fromJoin {
		return b.planLeaf(f, scope, conds, needed, cc, bound)
	}
	l, err := b.planFrom(f.left, scope, conds, needed, cc, nil)
	if err != nil {
		return nil, err
	}
	// an inner join's ON conjuncts are the WHERE clause's
	var onConds []*cond
	inner := f.jtype == "INNER" || f.jtype == "CROSS" || f.jtype == "COMMA"
	for _, c := range conjuncts(f.on) {
		nc := b.newCond(c, scope)
		onConds = append(onConds, nc)
	}
	if inner {
		*conds = append(*conds, onConds...)
	}
	method := strings.ToUpper(f.hints["JOIN_METHOD"])
	if method == "" {
		method = strings.ToUpper(f.hints["JOIN_TYPE"])
	}
	rightItems := b.itemsOf(f.right, scope)
	switch {
	case f.jtype == "RIGHT" || f.jtype == "FULL" || method == "HASH_JOIN" || method == "MERGE_JOIN" || method == "PUSH_BROADCAST_HASH_JOIN":
		return b.hashJoin(f, l, scope, conds, onConds, needed, cc, method, rightItems)
	}
	// an apply join: the right side runs for each row of the left
	outerJoin := f.jtype == "LEFT"
	var rconds *[]*cond
	if outerJoin {
		rc := onConds
		rconds = &rc
	} else {
		rconds = conds
	}
	r, err := b.planFrom(f.right, scope, rconds, needed, cc, l)
	if err != nil {
		return nil, err
	}
	out := &frag{items: append(append([]*planItem{}, l.items...), r.items...)}
	switch {
	case inner:
		out.from = l.from + " CROSS JOIN " + r.from
		if f.jtype == "INNER" && len(f.using) > 0 {
			out.from = l.from + " INNER JOIN " + r.from + " USING (" + strings.Join(f.using, ", ") + ")"
		}
		out.conds = append(append([]string{}, l.conds...), r.conds...)
	default:
		out.from = l.from + " LEFT OUTER JOIN " + r.from + b.onSQL(f)
		out.conds = append([]string{}, l.conds...)
		for _, c := range onConds {
			c.applied = true
		}
	}
	name := "Cross Apply"
	if outerJoin {
		name = "Outer Apply"
	}
	if l.dist && r.dist && l.root == r.root && r.op.meta["co_located"] == "true" {
		delete(r.op.meta, "co_located")
		out.op = op(name, nil, rel(l.op, "Input"), rel(r.op, "Map"))
		out.dist, out.root, out.split = true, l.root, l.split
	} else if r.dist {
		delete(r.op.meta, "co_located")
		in := b.gather(l)
		batch := op("Create Batch", nil, rel(in, "Input"))
		bs := op("Scan", map[string]string{"scan_type": "BatchScan", "scan_target": "$v" + fmt.Sprint(b.next)})
		b.next++
		bs.rows = cc.count(l.rowsSQL())
		mapSide := op(name, nil, rel(bs, "Input"), rel(r.op, "Map"))
		mapSide.rows = cc.count(out.rowsSQL())
		dname := "Distributed Cross Apply"
		if outerJoin {
			dname = "Distributed Outer Apply"
		}
		out.op = op(dname, map[string]string{"distribution_table": r.root}, rel(batch, "Input"), rel(mapSide, "Map"))
	} else {
		in := b.gather(l)
		out.op = op(name, nil, rel(in, "Input"), rel(r.op, "Map"))
		out.dist, out.root, out.split = false, "", nil
	}
	out.op.rows = cc.count(out.rowsSQL())
	return out, nil
}

func (b *planBuilder) onSQL(f *sqlFrom) string {
	if f.on != nil {
		return " ON " + b.text(f.on)
	}
	if len(f.using) > 0 {
		return " USING (" + strings.Join(f.using, ", ") + ")"
	}
	return " ON TRUE"
}

func (b *planBuilder) itemsOf(f *sqlFrom, scope *planScope) []*planItem {
	var out []*planItem
	for _, it := range scope.items {
		if it.from.lo >= f.lo && it.from.hi <= f.hi {
			out = append(out, it)
		}
	}
	return out
}

// hashJoin plans a Hash Join (or the join its hint names) of the left
// fragment and the right side, each read on its own.
func (b *planBuilder) hashJoin(f *sqlFrom, l *frag, scope *planScope, conds *[]*cond, onConds []*cond, needed map[*planItem][]string, cc *countCtx, method string, rightItems []*planItem) (*frag, error) {
	inner := f.jtype == "INNER" || f.jtype == "CROSS" || f.jtype == "COMMA"
	var rconds *[]*cond
	if inner {
		rconds = conds
	} else {
		rc := []*cond{}
		rconds = &rc
	}
	r, err := b.planFrom(f.right, scope, rconds, needed, cc, nil)
	if err != nil {
		return nil, err
	}
	out := &frag{items: append(append([]*planItem{}, l.items...), r.items...)}
	var joinConds []*cond
	pick := onConds
	if inner {
		pick = *conds
	}
	in := map[*planItem]bool{}
	for _, it := range out.items {
		in[it] = true
	}
	for _, c := range pick {
		if c.applied || c.subq {
			continue
		}
		all := len(c.items) > 0
		for it := range c.items {
			if !in[it] {
				all = false
			}
		}
		if all {
			joinConds = append(joinConds, c)
		}
	}
	if inner {
		out.from = l.from + " CROSS JOIN " + r.from
		out.conds = append(append([]string{}, l.conds...), r.conds...)
		for _, c := range joinConds {
			out.conds = append(out.conds, b.text(c.e))
			c.applied = true
		}
	} else {
		kw := map[string]string{"LEFT": "LEFT OUTER JOIN", "RIGHT": "RIGHT OUTER JOIN", "FULL": "FULL OUTER JOIN"}[f.jtype]
		out.from = "(" + b.ps.text(f.left.lo, f.left.hi) + ") " + kw + " " + b.itemSQL(f.right) + b.onSQL(f)
		if f.left.kind != fromJoin {
			out.from = l.from + " " + kw + " " + b.itemSQL(f.right) + b.onSQL(f)
		}
		out.conds = nil
		if f.jtype == "LEFT" {
			out.conds = append(out.conds, l.conds...)
		}
		for _, c := range onConds {
			c.applied = true
		}
	}
	jt := map[string]string{"LEFT": "LEFT OUTER", "RIGHT": "RIGHT OUTER", "FULL": "FULL OUTER"}[f.jtype]
	if jt == "" {
		jt = "INNER"
	}
	left, right := b.gather(l), b.gather(r)
	var links []planLink
	switch method {
	case "MERGE_JOIN":
		links = []planLink{rel(b.sortFor(left, l, joinConds, true), ""), rel(b.sortFor(right, r, joinConds, false), "")}
	default:
		links = []planLink{rel(left, "Build"), rel(right, "Probe")}
	}
	for _, c := range joinConds {
		links = append(links, planLink{sc: b.scalar(c.e, out), typ: "Condition"})
	}
	name := map[string]string{"MERGE_JOIN": "Merge Join", "PUSH_BROADCAST_HASH_JOIN": "Push Broadcast Hash Join"}[method]
	if name == "" {
		name = "Hash Join"
	}
	out.op = op(name, map[string]string{"join_type": jt}, links...)
	out.op.rows = cc.count(out.rowsSQL())
	return out, nil
}

// sortFor sorts one side of a Merge Join on its join columns, unless it
// already comes in that order.
func (b *planBuilder) sortFor(o *planOp, f *frag, conds []*cond, left bool) *planOp {
	var cols []string
	for _, c := range conds {
		if c.e.kind != exBinary || c.e.op != "=" {
			continue
		}
		for _, a := range c.e.args {
			if r, ok := b.cols[a]; ok {
				for _, it := range f.items {
					if it == r.item {
						cols = append(cols, r.name)
					}
				}
			}
		}
	}
	if prefixOf(cols, f.keyOrder) {
		return o
	}
	var keys []planLink
	for _, c := range cols {
		keys = append(keys, planLink{sc: &planScalar{name: "Reference", desc: "$" + c + " ASC"}, typ: "Key"})
	}
	s := op("Sort", nil, append([]planLink{rel(o, "")}, keys...)...)
	return s
}

// planLeaf plans one FROM item; bound is the left side of the apply join
// the item is the map side of, whose columns its conditions may read.
func (b *planBuilder) planLeaf(f *sqlFrom, scope *planScope, conds *[]*cond, needed map[*planItem][]string, cc *countCtx, bound *frag) (*frag, error) {
	var it *planItem
	for _, x := range scope.items {
		if x.from == f {
			it = x
		}
	}
	if it == nil {
		return nil, unsupportedSQL{"a FROM item"}
	}
	switch {
	case it.table != nil:
		return b.access(it, scope, conds, needed[it], cc, bound)
	case f.kind == fromUnnest:
		b.resolve(f.arr, scope)
		u := op("Array Unnest", nil, planLink{sc: b.scalar(f.arr, nil), typ: ""})
		out := &frag{op: u, items: []*planItem{it}, from: b.itemSQL(f)}
		if bound != nil {
			out.from = bound.from + " CROSS JOIN " + b.itemSQL(f)
			out.conds = bound.conds
			u.rows = cc.count(out.rowsSQL())
			u.execs = cc.count(bound.rowsSQL())
			out.from, out.conds = b.itemSQL(f), nil
		} else {
			u.rows = cc.count(out.rowsSQL())
		}
		return out, nil
	}
	if q := b.quant[strings.ToLower(f.alias)]; q != nil && f.kind == fromSub {
		return b.recursiveUnion(f, it, q, cc), nil
	}
	if it.view != nil {
		vb := &planBuilder{ps: it.view.ps, schema: b.schema, ctes: map[string]*sqlCTE{}, cols: map[*sqlExpr]colRef{},
			subPlans: map[*sqlExpr]*planOp{}, orderFor: map[*sqlSelect][]sqlOrder{}, next: b.next}
		sub, err := vb.planQuery(it.view.q, nil, cc)
		b.next = vb.next
		if err != nil {
			return nil, err
		}
		return &frag{op: vb.gather(sub), items: []*planItem{it}, from: b.itemSQL(f)}, nil
	}
	// a subquery or a WITH clause's query
	q := f.sub
	if q == nil {
		q = b.ctes[strings.ToLower(f.name[len(f.name)-1])].q
	}
	sub, err := b.planQuery(q, scope.parent, cc)
	if err != nil {
		return nil, err
	}
	o := b.gather(sub)
	return &frag{op: o, items: []*planItem{it}, from: b.itemSQL(f)}, nil
}

// access reads a table: from the table or an index, seeking on its key.
func (b *planBuilder) access(it *planItem, scope *planScope, conds *[]*cond, needed []string, cc *countCtx, bound *frag) (*frag, error) {
	t := it.table
	itemSQL := b.itemSQL(it.from)
	boundItems := map[*planItem]bool{}
	if bound != nil {
		for _, x := range bound.items {
			boundItems[x] = true
		}
	}
	// the conditions it can evaluate: on its own columns, the bound side's,
	// parameters and literals
	var local []*cond
	for _, c := range *conds {
		// a sampled table's rows are sampled before the WHERE clause
		// filters them, so no condition reaches its scan
		if it.from.sample != nil {
			break
		}
		if c.applied || c.subq || !c.items[it] {
			continue
		}
		ok := true
		for x := range c.items {
			if x != it && !boundItems[x] {
				ok = false
			}
		}
		if ok {
			local = append(local, c)
		}
	}
	keyOf := func(ix *planIndex) []keyCol {
		if ix == nil {
			return t.pk
		}
		k := append([]keyCol{}, ix.keys...)
		for _, p := range t.pk {
			found := false
			for _, x := range k {
				if strings.EqualFold(x.name, p.name) {
					found = true
				}
			}
			if !found {
				k = append(k, p)
			}
		}
		return k
	}
	covers := func(ix *planIndex) bool {
		for _, c := range needed {
			if !hasCol(keyColNames(keyOf(ix)), c) && !hasCol(ix.storing, c) {
				return false
			}
		}
		return true
	}
	var ix *planIndex
	if h := it.from.hints["FORCE_INDEX"]; h != "" && !strings.EqualFold(h, "_BASE_TABLE") {
		ix = b.schema.indexes[strings.ToLower(h)]
		if ix == nil {
			return nil, unsupportedSQL{fmt.Sprintf("FORCE_INDEX=%s, an index not in the schema", h)}
		}
	} else if h == "" {
		if seek, _ := b.seek(it, keyOf(nil), local, boundItems); len(seek) == 0 {
			var names []string
			for n := range b.schema.indexes {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				cand := b.schema.indexes[n]
				if !strings.EqualFold(cand.table, t.name) || !covers(cand) {
					continue
				}
				if s, _ := b.seek(it, keyOf(cand), local, boundItems); len(s) > 0 {
					ix = cand
					break
				}
			}
		}
	}
	key := keyOf(ix)
	seekConds, _ := b.seek(it, key, local, boundItems)
	var residual, later []*cond
	for _, c := range local {
		if containsCond(seekConds, c) {
			continue
		}
		if ix != nil && !covers(ix) && !b.readsOnly(c, it, append(keyColNames(key), ix.storing...)) {
			later = append(later, c)
			continue
		}
		residual = append(residual, c)
	}
	for _, c := range seekConds {
		c.applied = true
	}
	for _, c := range residual {
		c.applied = true
	}
	for _, c := range later {
		c.applied = true
	}
	condSQL := func(cs []*cond) []string {
		var out []string
		for _, c := range cs {
			out = append(out, b.text(c.e))
		}
		return out
	}
	// counting: on the map side of an apply, the rows over every execution
	count := func(cs []*cond) string {
		if bound == nil {
			return cc.count("SELECT 1 FROM " + itemSQL + whereSQL(condSQL(cs)))
		}
		return cc.count("SELECT 1 FROM " + bound.from + " CROSS JOIN " + itemSQL + whereSQL(append(append([]string{}, bound.conds...), condSQL(cs)...)))
	}
	execs := ""
	if bound != nil {
		execs = cc.count(bound.rowsSQL())
	}
	if b.dmlTarget == it.from {
		for _, p := range t.pk {
			if !hasCol(needed, p.name) {
				needed = append(append([]string{}, needed...), p.name)
			}
		}
	}
	scanCols := needed
	if ix != nil && !covers(ix) {
		scanCols = nil
		for _, c := range needed {
			if hasCol(keyColNames(key), c) || hasCol(ix.storing, c) {
				scanCols = append(scanCols, c)
			}
		}
		for _, p := range t.pk {
			if !hasCol(scanCols, p.name) {
				scanCols = append(scanCols, p.name)
			}
		}
	}
	target, stype, root := t.name, "TableScan", b.schema.root(t)
	if ix != nil {
		target, stype = ix.name, "IndexScan"
		root = ix.name
		if ix.parent != "" {
			if p := b.schema.table(ix.parent); p != nil {
				root = b.schema.root(p)
			}
		}
	}
	f := &frag{items: []*planItem{it}, from: itemSQL, root: root, dist: true}
	scan := b.scanOp(stype, target, len(seekConds) == 0, scanCols, it)
	scan.rows, scan.execs = count(seekConds), execs
	o := scan
	if len(seekConds) > 0 || len(residual) > 0 {
		links := []planLink{rel(scan, "")}
		if len(seekConds) > 0 {
			links = append(links, planLink{sc: b.condScalar(seekConds, nil), typ: "Seek Condition"})
		}
		if len(residual) > 0 {
			links = append(links, planLink{sc: b.condScalar(residual, nil), typ: "Residual Condition"})
		}
		o = op("Filter Scan", nil, links...)
		o.rows, o.execs = count(append(append([]*cond{}, seekConds...), residual...)), execs
		o.filtered = len(residual) > 0
	}
	f.conds = condSQL(append(append([]*cond{}, seekConds...), residual...))
	for _, k := range key {
		f.keyOrder = append(f.keyOrder, k.name)
	}
	// the split pruning a seek on the root's key allows
	if bound == nil && len(seekConds) > 0 && ix == nil && root == t.name {
		f.split = []*planScalar{b.condScalar(seekConds, nil)}
	}
	// co-location: the map side of an apply whose seek reads the left side
	if bound != nil && bound.dist && bound.root == root {
		for _, c := range seekConds {
			for x := range c.items {
				if boundItems[x] {
					o.meta["co_located"] = "true"
				}
			}
		}
	}
	f.op = o
	if ix != nil && !covers(ix) {
		// the back join: the index's rows, then the base table's by key
		idx := b.gather(f)
		batch := op("Create Batch", nil, rel(idx, "Input"))
		bs := op("Scan", map[string]string{"scan_type": "BatchScan", "scan_target": "$v" + fmt.Sprint(b.next)})
		b.next++
		bs.rows, bs.execs = o.rows, execs
		var restCols []string
		for _, c := range needed {
			if !hasCol(scanCols, c) {
				restCols = append(restCols, c)
			}
		}
		ts := b.scanOp("TableScan", t.name, false, restCols, it)
		ts.rows, ts.execs = o.rows, execs
		var pkDesc []string
		for _, p := range t.pk {
			pkDesc = append(pkDesc, "($"+p.name+"' = $batched_"+p.name+")")
		}
		tlinks := []planLink{rel(ts, ""), {sc: &planScalar{name: "Function", desc: strings.Join(pkDesc, " AND ")}, typ: "Seek Condition"}}
		allConds := append(append(append([]*cond{}, seekConds...), residual...), later...)
		if len(later) > 0 {
			tlinks = append(tlinks, planLink{sc: b.condScalar(later, nil), typ: "Residual Condition"})
		}
		fs := op("Filter Scan", nil, tlinks...)
		fs.rows, fs.execs = count(allConds), execs
		fs.filtered = len(later) > 0
		ca := op("Cross Apply", nil, rel(bs, "Input"), rel(fs, "Map"))
		ca.rows = fs.rows
		dca := op("Distributed Cross Apply", map[string]string{"distribution_table": t.name}, rel(batch, "Input"), rel(ca, "Map"))
		dca.rows = fs.rows
		f.op, f.dist, f.root, f.split, f.keyOrder = dca, false, "", nil, nil
		f.conds = condSQL(allConds)
	}
	if it.from.sample != nil {
		b.sample(f, it, cc, bound)
	}
	return f, nil
}

// sample plans TABLESAMPLE over a table's read (Google's unary operators
// page, Random ID Assign): the rows are gathered, a Random Id Assign adds
// a random number to each, and a Filter on it keeps BERNOULLI's share of
// them, or a Sort Limit on it RESERVOIR's number of rows.
func (b *planBuilder) sample(f *frag, it *planItem, cc *countCtx, bound *frag) {
	sm := it.from.sample
	in := b.gather(f)
	ra := op("Random Id Assign", nil, rel(in, ""))
	size := b.text(sm.size)
	sampled := b.ps.text(it.from.lo, it.from.sampleHi)
	var o *planOp
	if sm.method == "BERNOULLI" {
		o = op("Filter", nil, rel(ra, ""), planLink{sc: &planScalar{name: "Function", desc: "($random_id < (" + size + " / 100))"}, typ: "Condition"})
	} else {
		o = op("Sort Limit", nil, rel(ra, ""), planLink{sc: &planScalar{name: "Reference", desc: "$random_id ASC"}, typ: "Key"},
			planLink{sc: b.scalar(sm.size, nil), typ: "Limit"})
	}
	if bound == nil {
		o.rows = cc.count("SELECT 1 FROM " + sampled)
	} else {
		o.rows = cc.count("SELECT 1 FROM " + bound.from + " CROSS JOIN " + sampled + whereSQL(bound.conds))
		o.execs = cc.count(bound.rowsSQL())
	}
	f.op, f.dist, f.root, f.split, f.keyOrder = o, false, "", nil, nil
	f.from, f.conds = sampled, nil
}

func keyColNames(k []keyCol) []string {
	var out []string
	for _, c := range k {
		out = append(out, c.name)
	}
	return out
}

func containsCond(cs []*cond, c *cond) bool {
	for _, x := range cs {
		if x == c {
			return true
		}
	}
	return false
}

// readsOnly says whether c reads only the given columns of it.
func (b *planBuilder) readsOnly(c *cond, it *planItem, cols []string) bool {
	ok := true
	walkExpr(c.e, func(x *sqlExpr) {
		if r, found := b.cols[x]; found && r.item == it && !hasCol(cols, r.name) {
			ok = false
		}
	})
	return ok
}

func (b *planBuilder) scanOp(stype, target string, full bool, cols []string, it *planItem) *planOp {
	meta := map[string]string{"scan_type": stype, "scan_target": target, "scan_method": "Automatic"}
	if full {
		meta["Full scan"] = "true"
	}
	var links []planLink
	for _, c := range cols {
		links = append(links, planLink{sc: &planScalar{name: "Reference", desc: c}, variable: c})
	}
	s := op("Scan", meta, links...)
	s.scan = true
	return s
}

// seek picks the conditions that bound a scan of key: equalities on a
// prefix of it, then a range on the next column.
func (b *planBuilder) seek(it *planItem, key []keyCol, local []*cond, bound map[*planItem]bool) ([]*cond, int) {
	var out []*cond
	for i, k := range key {
		eq := false
		var rng []*cond
		for _, c := range local {
			kind := b.keyPredicate(c.e, it, k.name)
			switch kind {
			case "eq":
				if !eq {
					out = append(out, c)
					eq = true
				}
			case "range":
				rng = append(rng, c)
			}
		}
		if !eq {
			out = append(out, rng...)
			return out, i
		}
	}
	return out, len(key)
}

// keyPredicate says how e restricts column col of it: "eq" (=, IN, IS
// NULL), "range" (<, <=, >, >=, BETWEEN, STARTS_WITH) or "" when it does
// not, the other side reading neither it nor anything not bound.
func (b *planBuilder) keyPredicate(e *sqlExpr, it *planItem, col string) string {
	isKey := func(x *sqlExpr) bool {
		if x.kind == exFunc && x.op == "()" && len(x.args) == 1 {
			x = x.args[0]
		}
		r, ok := b.cols[x]
		return ok && r.item == it && strings.EqualFold(r.name, col)
	}
	free := func(x *sqlExpr) bool {
		ok := true
		walkExpr(x, func(y *sqlExpr) {
			if r, found := b.cols[y]; found && r.item == it {
				ok = false
			}
			if y.kind == exSubq {
				ok = false
			}
		})
		return ok
	}
	switch {
	case e.kind == exBinary && e.op == "=":
		if isKey(e.args[0]) && free(e.args[1]) || isKey(e.args[1]) && free(e.args[0]) {
			return "eq"
		}
	case e.kind == exBinary && e.op == "IN":
		if isKey(e.args[0]) && free(e.args[1]) {
			return "eq"
		}
	case e.kind == exUnary && e.op == "IS NULL":
		if isKey(e.args[0]) {
			return "eq"
		}
	case e.kind == exBinary && (e.op == "<" || e.op == "<=" || e.op == ">" || e.op == ">="):
		if isKey(e.args[0]) && free(e.args[1]) || isKey(e.args[1]) && free(e.args[0]) {
			return "range"
		}
	case e.kind == exBinary && e.op == "BETWEEN":
		if isKey(e.args[0]) && free(e.args[1]) && free(e.args[2]) {
			return "range"
		}
	case e.kind == exFunc && e.op == "STARTS_WITH" && len(e.args) == 2:
		if isKey(e.args[0]) && free(e.args[1]) {
			return "range"
		}
	case e.kind == exFunc && e.op == "()" && len(e.args) == 1:
		return b.keyPredicate(e.args[0], it, col)
	}
	return ""
}

// --- Scalars -------------------------------------------------------------------

// condScalar is a condition: one conjunct, or their AND.
func (b *planBuilder) condScalar(cs []*cond, f *frag) *planScalar {
	if len(cs) == 1 {
		return b.scalar(cs[0].e, f)
	}
	s := &planScalar{name: "Function"}
	var parts []string
	for _, c := range cs {
		k := b.scalar(c.e, f)
		s.kids = append(s.kids, planLink{sc: k})
		parts = append(parts, "("+k.desc+")")
	}
	s.desc = strings.Join(parts, " AND ")
	return s
}

// scalar is an expression as plan nodes, its columns written $Column as
// Spanner's plans write them.
func (b *planBuilder) scalar(e *sqlExpr, f *frag) *planScalar {
	switch e.kind {
	case exCol:
		if r, ok := b.cols[e]; ok {
			return &planScalar{name: "Reference", desc: "$" + r.name}
		}
		return &planScalar{name: "Reference", desc: b.text(e)}
	case exLit:
		return &planScalar{name: "Constant", desc: b.text(e)}
	case exParam:
		return &planScalar{name: "Parameter", desc: b.text(e)}
	case exSubq:
		name := "Scalar Subquery"
		if e.subKind == "ARRAY" {
			name = "Array Subquery"
		}
		s := &planScalar{name: name, desc: b.render(e)}
		if b.subPlans != nil {
			if sp := b.subPlans[e]; sp != nil {
				s.kids = append(s.kids, rel(sp, ""))
			}
		}
		return s
	}
	s := &planScalar{name: "Function", desc: b.render(e)}
	for _, a := range e.args {
		s.kids = append(s.kids, planLink{sc: b.scalar(a, f)})
	}
	return s
}

// render is e's text with its columns as $Column.
func (b *planBuilder) render(e *sqlExpr) string {
	type rep struct {
		lo, hi int
		s      string
	}
	var reps []rep
	walkExpr(e, func(x *sqlExpr) {
		if r, ok := b.cols[x]; ok {
			reps = append(reps, rep{x.lo, x.hi, "$" + r.name})
		}
	})
	sort.Slice(reps, func(i, j int) bool { return reps[i].lo < reps[j].lo })
	var sb strings.Builder
	pos := b.ps.toks[e.lo].start
	for _, r := range reps {
		if b.ps.toks[r.lo].start < pos {
			continue
		}
		sb.WriteString(b.ps.src[pos:b.ps.toks[r.lo].start])
		sb.WriteString(r.s)
		pos = b.ps.toks[r.hi-1].end
	}
	sb.WriteString(b.ps.src[pos:b.ps.toks[e.hi-1].end])
	return sb.String()
}

// --- The wire form -------------------------------------------------------------

// planNodes numbers the plan's nodes depth first, as Spanner does, and
// returns them with each relational operator's node.
func planNodes(root *planOp) ([]*spannerpb.PlanNode, map[*planOp]*spannerpb.PlanNode) {
	var nodes []*spannerpb.PlanNode
	byOp := map[*planOp]*spannerpb.PlanNode{}
	var addOp func(o *planOp) int32
	var addScalar func(s *planScalar) int32
	links := func(n *spannerpb.PlanNode, ls []planLink) {
		for _, l := range ls {
			var idx int32
			if l.rel != nil {
				idx = addOp(l.rel)
			} else {
				idx = addScalar(l.sc)
			}
			n.ChildLinks = append(n.ChildLinks, &spannerpb.PlanNode_ChildLink{ChildIndex: idx, Type: l.typ, Variable: l.variable})
		}
	}
	addOp = func(o *planOp) int32 {
		n := &spannerpb.PlanNode{Index: int32(len(nodes)), Kind: spannerpb.PlanNode_RELATIONAL, DisplayName: o.name}
		nodes = append(nodes, n)
		byOp[o] = n
		links(n, o.kids)
		meta := map[string]any{}
		for k, v := range o.meta {
			meta[k] = v
		}
		if o.name == "Distributed Union" || o.name == "Distributed Cross Apply" || o.name == "Distributed Outer Apply" {
			for _, l := range n.ChildLinks {
				if nodes[l.ChildIndex].Kind == spannerpb.PlanNode_RELATIONAL && (l.Type == "" || l.Type == "Map") {
					meta["subquery_cluster_node"] = fmt.Sprint(l.ChildIndex)
				}
			}
		}
		n.Metadata, _ = structpb.NewStruct(meta)
		return n.Index
	}
	addScalar = func(s *planScalar) int32 {
		n := &spannerpb.PlanNode{Index: int32(len(nodes)), Kind: spannerpb.PlanNode_SCALAR, DisplayName: s.name,
			ShortRepresentation: &spannerpb.PlanNode_ShortRepresentation{Description: s.desc}}
		nodes = append(nodes, n)
		links(n, s.kids)
		for _, l := range n.ChildLinks {
			if nodes[l.ChildIndex].Kind == spannerpb.PlanNode_RELATIONAL {
				if n.ShortRepresentation.Subqueries == nil {
					n.ShortRepresentation.Subqueries = map[string]int32{}
				}
				n.ShortRepresentation.Subqueries[fmt.Sprint(len(n.ShortRepresentation.Subqueries))] = l.ChildIndex
			}
		}
		return n.Index
	}
	addOp(root)
	return nodes, byOp
}

// recursiveUnion plans a quantified edge pattern's paths as Google's
// binary operators page draws them: a Recursive Union of its base input,
// the paths' first edges (a scan of the edge table), and its recursive
// input, a Cross Apply that joins each row of the previous iteration (a
// Recursive Spool Scan) to the edges leaving the node it reached, seeking
// on the edge table's source key. It runs until the upper bound; a lower
// bound above 1 is a Filter on the paths' length.
func (b *planBuilder) recursiveUnion(f *sqlFrom, it *planItem, q *graphQuant, cc *countCtx) *frag {
	e := q.table
	count := func(lo, hi int) string {
		if lo > hi {
			return "SELECT 0"
		}
		return cc.count(q.pathsSQL(lo, hi))
	}
	var cols []string
	cols = append(cols, e.KeyColumns...)
	scan := func() *planOp {
		var links []planLink
		for _, c := range cols {
			links = append(links, planLink{sc: &planScalar{name: "Reference", desc: c}, variable: c})
		}
		s := op("Scan", map[string]string{"scan_type": "TableScan", "scan_target": e.BaseTableName, "scan_method": "Automatic"}, links...)
		s.scan = true
		return s
	}
	root := e.BaseTableName
	if t := b.schema.table(e.BaseTableName); t != nil {
		root = b.schema.root(t)
	}
	// the edge pattern's own conditions, on each edge it reads
	residual := func(o *planOp) *planOp {
		if len(q.hopConds) == 0 {
			return o
		}
		desc := strings.ReplaceAll(strings.Join(q.hopConds, " AND "), "cb_hop.", "$")
		if o.name != "Filter Scan" {
			return op("Filter Scan", nil, rel(o, ""), planLink{sc: &planScalar{name: "Function", desc: desc}, typ: "Residual Condition"})
		}
		o.kids = append(o.kids, planLink{sc: &planScalar{name: "Function", desc: desc}, typ: "Residual Condition"})
		return o
	}
	// the base: the first edges, or for a lower bound of 0 the start
	// nodes (the paths of no edge)
	first := 1
	var base *planOp
	if q.min == 0 && q.node != nil {
		first = 0
		ns := op("Scan", map[string]string{"scan_type": "TableScan", "scan_target": q.node.BaseTableName, "scan_method": "Automatic", "Full scan": "true"})
		ns.scan = true
		ns.rows = count(0, 0)
		nroot := q.node.BaseTableName
		if t := b.schema.table(q.node.BaseTableName); t != nil {
			nroot = b.schema.root(t)
		}
		base = b.distUnion(ns, &frag{root: nroot})
	} else {
		bs := scan()
		bs.meta["Full scan"] = "true"
		bs.rows = count(1, 1)
		bo := residual(bs)
		if bo != bs {
			// the scan reads every edge; the residual keeps the first ones
			bo.rows, bs.rows = bs.rows, cc.count("SELECT 1 FROM "+e.BaseTableName)
		}
		base = b.distUnion(bo, &frag{root: root})
	}
	spool := op("Scan", map[string]string{"scan_type": "RecursiveSpoolScan", "scan_target": "$" + q.alias})
	spool.rows = count(first, q.max-1)
	spool.execs = fmt.Sprintf("SELECT %d", q.max-first)
	ms := scan()
	ms.rows, ms.execs = count(first+1, q.max), spool.rows
	var seek []string
	for i, c := range e.Source.EdgeCols {
		seek = append(seek, "($"+c+" = $"+q.alias+"."+e.Dest.EdgeCols[i]+")")
	}
	fs := residual(op("Filter Scan", nil, rel(ms, ""), planLink{sc: &planScalar{name: "Function", desc: strings.Join(seek, " AND ")}, typ: "Seek Condition"}))
	fs.rows, fs.execs = ms.rows, spool.rows
	ca := op("Cross Apply", nil, rel(spool, "Input"), rel(b.distUnion(fs, &frag{root: root}), "Map"))
	ca.rows, ca.execs = ms.rows, spool.execs
	ru := op("Recursive Union", map[string]string{"upper_bound": fmt.Sprint(q.max)}, rel(base, "Base"), rel(ca, "Recursive"))
	ru.rows = count(first, q.max)
	o := ru
	if q.min > 1 {
		o = op("Filter", nil, rel(ru, ""), planLink{sc: &planScalar{name: "Function", desc: fmt.Sprintf("($path_length >= %d)", q.min)}, typ: "Condition"})
		o.rows = count(q.min, q.max)
	}
	return &frag{op: o, items: []*planItem{it}, from: b.itemSQL(f)}
}
