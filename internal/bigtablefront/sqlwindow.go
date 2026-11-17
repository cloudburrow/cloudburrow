package bigtablefront

import (
	"fmt"
	"math"
	"sort"
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// Window function calls (#1704), as GoogleSQL for Bigtable's reference
// documents them (https://cloud.google.com/bigtable/docs/reference/sql/window-function-calls):
//
//	function_name ( [ argument_list ] ) OVER { named_window | ( [ window_specification ] ) }
//	window_specification:
//	  [ named_window ] [ PARTITION BY expr [, ...] ]
//	  [ ORDER BY expr [ { ASC | DESC } ] [, ...] ] [ { ROWS | RANGE } frame ]
//	WINDOW named_window AS { named_window | ( [ window_specification ] ) } [, ...]
//
// They are evaluated after GROUP BY and the aggregates, over the rows (or
// groups) the SELECT list is evaluated on, and never reorder them. The
// functions are the aggregates (aggregate_functions,
// statistical_aggregate_functions, approximate_aggregate_functions), the
// numbering functions ROW_NUMBER, RANK, DENSE_RANK, PERCENT_RANK, CUME_DIST
// and NTILE (numbering_functions) and the navigation functions
// FIRST_VALUE, LAST_VALUE, NTH_VALUE, LAG, LEAD, PERCENTILE_CONT and
// PERCENTILE_DISC (navigation_functions). Without ORDER BY the frame is the
// whole partition; with ORDER BY and no frame clause it is RANGE BETWEEN
// UNBOUNDED PRECEDING AND CURRENT ROW, as the reference says. Rows that tie
// on the ORDER BY keys keep the order they were read in (by row key);
// NULL keys sort first in ascending order.

type orderKey struct {
	e    expr
	desc bool
}

const (
	unboundedPreceding = iota
	preceding
	currentRow
	following
	unboundedFollowing
)

type frameBound struct {
	kind int
	n    expr
}

type windowFrame struct {
	rows       bool
	start, end frameBound
}

type windowSpec struct {
	name      string // a named window this one builds on
	partition []expr
	order     []orderKey
	frame     *windowFrame
}

var numbering = map[string]bool{"ROW_NUMBER": true, "RANK": true, "DENSE_RANK": true, "PERCENT_RANK": true, "CUME_DIST": true, "NTILE": true}
var navigation = map[string]bool{"FIRST_VALUE": true, "LAST_VALUE": true, "NTH_VALUE": true, "LAG": true, "LEAD": true,
	"PERCENTILE_CONT": true, "PERCENTILE_DISC": true}

// over reads a window function call's OVER clause, if there is one.
func (c *call) over(p *parser) (expr, error) {
	if !p.keyword("OVER") {
		if numbering[c.name] || navigation[c.name] {
			return nil, fmt.Errorf("Analytic function %s must have an OVER clause", c.name)
		}
		return c, nil
	}
	if c.distinct {
		return nil, fmt.Errorf("DISTINCT is not allowed for analytic function %s", c.name)
	}
	if t := p.peek(); t.kind == "ident" || t.kind == "qident" {
		p.pos++
		c.win = &windowSpec{name: t.text}
		return c, nil
	}
	if err := p.expect("("); err != nil {
		return nil, err
	}
	w, err := p.windowSpecification()
	if err != nil {
		return nil, err
	}
	c.win = w
	return c, p.expect(")")
}

func (p *parser) windowSpecification() (*windowSpec, error) {
	w := &windowSpec{}
	if t := p.peek(); (t.kind == "ident" || t.kind == "qident") && !reserved[strings.ToUpper(t.text)] {
		p.pos++
		w.name = t.text
	}
	if p.keyword("PARTITION") {
		if !p.keyword("BY") {
			return nil, fmt.Errorf("Syntax error: expected BY after PARTITION")
		}
		if w.name != "" {
			return nil, fmt.Errorf("If a window has a PARTITION BY, it cannot reference a named window")
		}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			w.partition = append(w.partition, e)
			if !p.op(",") {
				break
			}
		}
	}
	if p.keyword("ORDER") {
		if !p.keyword("BY") {
			return nil, fmt.Errorf("Syntax error: expected BY after ORDER")
		}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			k := orderKey{e: e}
			if p.keyword("DESC") {
				k.desc = true
			} else {
				p.keyword("ASC")
			}
			w.order = append(w.order, k)
			if !p.op(",") {
				break
			}
		}
	}
	if p.isKeyword("ROWS") || p.isKeyword("RANGE") {
		f := &windowFrame{rows: strings.EqualFold(p.next().text, "ROWS")}
		bound := func() (frameBound, error) {
			switch {
			case p.keyword("UNBOUNDED"):
				if p.keyword("PRECEDING") {
					return frameBound{kind: unboundedPreceding}, nil
				}
				if p.keyword("FOLLOWING") {
					return frameBound{kind: unboundedFollowing}, nil
				}
				return frameBound{}, fmt.Errorf("Syntax error: expected PRECEDING or FOLLOWING after UNBOUNDED")
			case p.keyword("CURRENT"):
				if !p.keyword("ROW") {
					return frameBound{}, fmt.Errorf("Syntax error: expected ROW after CURRENT")
				}
				return frameBound{kind: currentRow}, nil
			}
			n, err := p.additive()
			if err != nil {
				return frameBound{}, err
			}
			if p.keyword("PRECEDING") {
				return frameBound{kind: preceding, n: n}, nil
			}
			if p.keyword("FOLLOWING") {
				return frameBound{kind: following, n: n}, nil
			}
			return frameBound{}, fmt.Errorf("Syntax error: expected PRECEDING or FOLLOWING")
		}
		var err error
		if p.keyword("BETWEEN") {
			if f.start, err = bound(); err != nil {
				return nil, err
			}
			if !p.keyword("AND") {
				return nil, fmt.Errorf("Syntax error: expected AND in the window frame")
			}
			if f.end, err = bound(); err != nil {
				return nil, err
			}
		} else {
			if f.start, err = bound(); err != nil {
				return nil, err
			}
			f.end = frameBound{kind: currentRow}
		}
		if f.start.kind == unboundedFollowing || f.end.kind == unboundedPreceding || f.end.kind < f.start.kind {
			return nil, fmt.Errorf("Invalid window frame: its start must not be after its end")
		}
		w.frame = f
	}
	return w, nil
}

// windowClause reads WINDOW name AS spec [, ...], after the WINDOW keyword.
func (p *parser) windowClause(q *query) error {
	q.windows = map[string]*windowSpec{}
	for {
		t := p.next()
		if t.kind != "ident" && t.kind != "qident" {
			return fmt.Errorf("Syntax error: expected a window name in the WINDOW clause")
		}
		if !p.keyword("AS") {
			return fmt.Errorf("Syntax error: expected AS after the window name %s", t.text)
		}
		var w *windowSpec
		if n := p.peek(); n.kind == "ident" || n.kind == "qident" {
			p.pos++
			w = &windowSpec{name: n.text}
		} else {
			if err := p.expect("("); err != nil {
				return err
			}
			var err error
			if w, err = p.windowSpecification(); err != nil {
				return err
			}
			if err := p.expect(")"); err != nil {
				return err
			}
		}
		key := strings.ToLower(t.text)
		if _, dup := q.windows[key]; dup {
			return fmt.Errorf("Duplicate window name %s", t.text)
		}
		if w.name != "" {
			if _, ok := q.windows[strings.ToLower(w.name)]; !ok {
				return fmt.Errorf("Unrecognized window alias %s: a named window must be defined before it is referenced", w.name)
			}
		}
		q.windows[key] = w
		if !p.op(",") {
			return nil
		}
	}
}

// resolve is w with the named windows it builds on applied, as the
// reference's rules for a named window in a window specification say.
func (w *windowSpec) resolve(windows map[string]*windowSpec, depth int) (*windowSpec, error) {
	if w.name == "" {
		return w, nil
	}
	if depth > 16 {
		return nil, fmt.Errorf("Named windows nest too deep")
	}
	base, ok := windows[strings.ToLower(w.name)]
	if !ok {
		return nil, fmt.Errorf("Unrecognized window alias %s", w.name)
	}
	b, err := base.resolve(windows, depth+1)
	if err != nil {
		return nil, err
	}
	out := &windowSpec{partition: b.partition, order: b.order, frame: b.frame}
	if len(w.partition) > 0 {
		return nil, fmt.Errorf("If a window has a PARTITION BY, it cannot reference a named window")
	}
	if len(w.order) > 0 {
		if len(b.order) > 0 {
			return nil, fmt.Errorf("If a window inherits an ORDER BY from its referenced window, it cannot have an inlined ORDER BY")
		}
		out.order = w.order
	}
	if w.frame != nil {
		if b.frame != nil {
			return nil, fmt.Errorf("A window cannot inherit a window frame from its referenced window")
		}
		out.frame = w.frame
	}
	return out, nil
}

// checkWindow types a window function call.
func (c *call) checkWindow(s *scope) (*btpb.Type, error) {
	if s.wins == nil {
		where := s.noAggOK
		if where == "" {
			where = "here"
		}
		if s.inAgg {
			where = "an aggregate function's arguments"
		}
		return nil, fmt.Errorf("Analytic function %s not allowed in %s", c.name, where)
	}
	w, err := c.win.resolve(s.windows, 0)
	if err != nil {
		return nil, err
	}
	c.win = w
	inner := *s
	inner.wins = nil
	inner.noAggOK = ""
	for _, e := range w.partition {
		t, err := e.check(&inner)
		if err != nil {
			return nil, err
		}
		if !groupable(t) || isKind(t, "FLOAT32", "FLOAT64") {
			return nil, fmt.Errorf("Partitioning by expressions of type %s is not allowed", kind(t))
		}
	}
	for _, o := range w.order {
		t, err := o.e.check(&inner)
		if err != nil {
			return nil, err
		}
		if isKind(t, "ARRAY<", "STRUCT<", "MAP<") {
			return nil, fmt.Errorf("Ordering by expressions of type %s is not allowed", kind(t))
		}
	}
	if f := w.frame; f != nil {
		if numbering[c.name] || c.name == "LAG" || c.name == "LEAD" || c.name == "PERCENTILE_CONT" || c.name == "PERCENTILE_DISC" {
			return nil, fmt.Errorf("Window framing clause is not allowed for analytic function %s", c.name)
		}
		for _, b := range []frameBound{f.start, f.end} {
			if b.n == nil {
				continue
			}
			t, err := b.n.check(&inner)
			if err != nil {
				return nil, err
			}
			if !constant(b.n) || (f.rows && !isKind(t, "INT64")) || !isNumeric(t) {
				return nil, fmt.Errorf("Window frame offset must be a constant, non-negative integer or parameter")
			}
		}
		if !f.rows && (f.start.n != nil || f.end.n != nil) {
			if len(w.order) != 1 {
				return nil, fmt.Errorf("A RANGE window frame with an offset must have exactly one ORDER BY expression")
			}
			t, _ := w.order[0].e.check(&inner)
			if !isNumeric(t) {
				return nil, fmt.Errorf("A RANGE window frame with an offset must be ordered by a numeric expression")
			}
		}
		if len(w.order) == 0 && !(f.start.kind == unboundedPreceding && f.end.kind == unboundedFollowing) {
			return nil, fmt.Errorf("Analytic function %s with a window frame requires an ORDER BY in its window", c.name)
		}
	}
	argTypes := func() ([]*btpb.Type, error) {
		var ts []*btpb.Type
		for _, a := range c.args {
			t, err := a.check(&inner)
			if err != nil {
				return nil, err
			}
			ts = append(ts, t)
		}
		c.argTypes = ts
		return ts, nil
	}
	needOrder := func() error {
		if len(w.order) == 0 {
			return fmt.Errorf("Analytic function %s must have an ORDER BY in its window", c.name)
		}
		return nil
	}
	switch {
	case numbering[c.name]:
		ts, err := argTypes()
		if err != nil {
			return nil, err
		}
		if c.name != "ROW_NUMBER" {
			if err := needOrder(); err != nil {
				return nil, err
			}
		}
		c.t = int64Type
		switch c.name {
		case "NTILE":
			if len(ts) != 1 || !isKind(ts[0], "INT64") || !constant(c.args[0]) {
				return nil, fmt.Errorf("NTILE takes one constant INT64 argument")
			}
		case "PERCENT_RANK", "CUME_DIST":
			c.t = float64Type
			fallthrough
		default:
			if len(ts) != 0 {
				return nil, sigErr(c, ts)
			}
		}
	case navigation[c.name]:
		ts, err := argTypes()
		if err != nil {
			return nil, err
		}
		if len(ts) == 0 {
			return nil, sigErr(c, ts)
		}
		c.t = ts[0]
		switch c.name {
		case "FIRST_VALUE", "LAST_VALUE":
			if len(ts) != 1 {
				return nil, sigErr(c, ts)
			}
		case "NTH_VALUE":
			if len(ts) != 2 || !isKind(ts[1], "INT64") || !constant(c.args[1]) {
				return nil, fmt.Errorf("NTH_VALUE takes a value and a constant INT64")
			}
		case "LAG", "LEAD":
			if err := needOrder(); err != nil {
				return nil, err
			}
			if len(ts) > 3 || len(ts) >= 2 && (!isKind(ts[1], "INT64") || !constant(c.args[1])) {
				return nil, fmt.Errorf("%s's offset must be a non-negative integer literal or parameter", c.name)
			}
			if len(ts) == 3 {
				if !constant(c.args[2]) {
					return nil, fmt.Errorf("%s's default must be a constant expression", c.name)
				}
				if l, ok := c.args[2].(*literal); !(ok && l.t == nil) && !sameType(ts[2], ts[0]) && !assignable(ts[2], ts[0]) &&
					!(isKind(ts[2], "INT64") && isKind(ts[0], "FLOAT32", "FLOAT64")) {
					return nil, fmt.Errorf("%s's default of type %s does not coerce to %s", c.name, kind(ts[2]), kind(ts[0]))
				}
			}
		case "PERCENTILE_CONT", "PERCENTILE_DISC":
			if len(w.order) > 0 {
				return nil, fmt.Errorf("%s cannot have an ORDER BY in its window", c.name)
			}
			if len(ts) != 2 || !isNumeric(ts[1]) || !constant(c.args[1]) {
				return nil, fmt.Errorf("%s takes a value and a FLOAT64 literal percentile in [0, 1]", c.name)
			}
			if c.name == "PERCENTILE_CONT" {
				if !isNumeric(ts[0]) {
					return nil, sigErr(c, ts)
				}
				c.t = float64Type
			} else if isKind(ts[0], "ARRAY<", "STRUCT<", "MAP<") {
				return nil, sigErr(c, ts)
			}
		}
	case aggregates[c.name]:
		if c.name == "MIN_BY" || c.name == "MAX_BY" {
			return nil, unsupportedErr{"the function " + c.name}
		}
		// The aggregate's own typing, with no GROUP BY slot taken.
		tmp := inner
		var none []*call
		tmp.aggs = &none
		tmp.inAgg = false
		t, err := c.checkAgg(&tmp)
		if err != nil {
			return nil, err
		}
		c.isAgg = false
		c.t = t
	default:
		for _, d := range documented {
			if d == c.name {
				return nil, fmt.Errorf("%s is not an analytic function: it cannot have an OVER clause", c.name)
			}
		}
		return nil, fmt.Errorf("Function not found: %s", c.name)
	}
	c.winSlot = len(*s.wins)
	*s.wins = append(*s.wins, c)
	return c.t, nil
}

// window computes c, a window function call, for each of inputs.
func (c *call) window(inputs []env) ([]any, error) {
	w := c.win
	out := make([]any, len(inputs))
	// Partitions, in the order their first row was read.
	var order []string
	parts := map[string][]int{}
	keys := make([][]any, len(inputs))
	for i := range inputs {
		e := inputs[i]
		var pk []any
		for _, x := range w.partition {
			v, err := x.eval(&e)
			if err != nil {
				return nil, err
			}
			pk = append(pk, v)
		}
		k := groupKey(pk)
		if _, ok := parts[k]; !ok {
			order = append(order, k)
		}
		parts[k] = append(parts[k], i)
		for _, o := range w.order {
			v, err := o.e.eval(&e)
			if err != nil {
				return nil, err
			}
			keys[i] = append(keys[i], v)
		}
	}
	cmpKeys := func(a, b int) int {
		for j, o := range w.order {
			x, y := keys[a][j], keys[b][j]
			var r int
			switch {
			case x == nil && y == nil:
			case x == nil:
				r = -1
			case y == nil:
				r = 1
			default:
				r, _ = compare(x, y)
			}
			if o.desc {
				r = -r
			}
			if r != 0 {
				return r
			}
		}
		return 0
	}
	for _, k := range order {
		idx := parts[k]
		sort.SliceStable(idx, func(a, b int) bool { return cmpKeys(idx[a], idx[b]) < 0 })
		if err := c.windowPartition(inputs, idx, cmpKeys, keys, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// windowPartition fills out for the rows idx of one partition, in order.
func (c *call) windowPartition(inputs []env, idx []int, cmpKeys func(a, b int) int, keys [][]any, out []any) error {
	n := len(idx)
	w := c.win
	// peers: the first and last position of each position's peer group.
	first, last := make([]int, n), make([]int, n)
	for i := 0; i < n; i++ {
		if i > 0 && cmpKeys(idx[i-1], idx[i]) == 0 {
			first[i] = first[i-1]
		} else {
			first[i] = i
		}
	}
	for i := n - 1; i >= 0; i-- {
		if i < n-1 && cmpKeys(idx[i+1], idx[i]) == 0 {
			last[i] = last[i+1]
		} else {
			last[i] = i
		}
	}
	constInt := func(e expr) (int64, error) {
		env0 := inputs[idx[0]]
		v, err := e.eval(&env0)
		if err != nil {
			return 0, err
		}
		if v == nil {
			return 0, fmt.Errorf("%s: the argument must not be NULL", c.name)
		}
		if f, ok := v.(float64); ok {
			return int64(f), nil
		}
		return v.(int64), nil
	}
	argAt := func(pos, arg int) (any, error) {
		e := inputs[idx[pos]]
		return c.args[arg].eval(&e)
	}
	// frame is the positions [lo, hi] of position i's window frame.
	frame := func(i int) (int, int, error) {
		f := w.frame
		if f == nil {
			if len(w.order) == 0 {
				return 0, n - 1, nil
			}
			return 0, last[i], nil
		}
		at := func(b frameBound, isEnd bool) (int, error) {
			switch b.kind {
			case unboundedPreceding:
				return 0, nil
			case unboundedFollowing:
				return n - 1, nil
			case currentRow:
				if f.rows {
					return i, nil
				}
				if isEnd {
					return last[i], nil
				}
				return first[i], nil
			}
			env0 := inputs[idx[0]]
			v, err := b.n.eval(&env0)
			if err != nil {
				return 0, err
			}
			if v == nil || toFloat(v) < 0 {
				return 0, fmt.Errorf("Window frame offset must be non-negative and not NULL")
			}
			if f.rows {
				off := int(v.(int64))
				if b.kind == preceding {
					return i - off, nil
				}
				return i + off, nil
			}
			// RANGE: the rows whose key is within the offset of this row's.
			off := toFloat(v)
			cur := keys[idx[i]][0]
			if cur == nil {
				if isEnd {
					return last[i], nil
				}
				return first[i], nil
			}
			target := toFloat(cur)
			sign := 1.0
			if b.kind == preceding {
				sign = -1
			}
			if w.order[0].desc {
				sign = -sign
			}
			target += sign * off
			within := func(j int) bool {
				kv := keys[idx[j]][0]
				if kv == nil {
					return false
				}
				x := toFloat(kv)
				if isEnd {
					if w.order[0].desc {
						return x >= target
					}
					return x <= target
				}
				if w.order[0].desc {
					return x <= target
				}
				return x >= target
			}
			if isEnd {
				p := -1
				for j := 0; j < n; j++ {
					if within(j) {
						p = j
					}
				}
				return p, nil
			}
			for j := 0; j < n; j++ {
				if within(j) {
					return j, nil
				}
			}
			return n, nil
		}
		lo, err := at(f.start, false)
		if err != nil {
			return 0, 0, err
		}
		hi, err := at(f.end, true)
		if err != nil {
			return 0, 0, err
		}
		if lo < 0 {
			lo = 0
		}
		if hi > n-1 {
			hi = n - 1
		}
		return lo, hi, nil
	}
	switch c.name {
	case "ROW_NUMBER":
		for i := range idx {
			out[idx[i]] = int64(i + 1)
		}
		return nil
	case "RANK":
		for i := range idx {
			out[idx[i]] = int64(first[i] + 1)
		}
		return nil
	case "DENSE_RANK":
		r := int64(0)
		for i := range idx {
			if first[i] == i {
				r++
			}
			out[idx[i]] = r
		}
		return nil
	case "PERCENT_RANK":
		for i := range idx {
			if n == 1 {
				out[idx[i]] = 0.0
				continue
			}
			out[idx[i]] = float64(first[i]) / float64(n-1)
		}
		return nil
	case "CUME_DIST":
		for i := range idx {
			out[idx[i]] = float64(last[i]+1) / float64(n)
		}
		return nil
	case "NTILE":
		k, err := constInt(c.args[0])
		if err != nil {
			return err
		}
		if k <= 0 {
			return fmt.Errorf("NTILE's argument must be positive, not %d", k)
		}
		base, rem := int64(n)/k, int64(n)%k
		for i := range idx {
			p := int64(i)
			var b int64
			if p < rem*(base+1) {
				b = p/(base+1) + 1
			} else {
				b = rem + (p-rem*(base+1))/base + 1
			}
			out[idx[i]] = b
		}
		return nil
	case "LAG", "LEAD":
		off := int64(1)
		if len(c.args) >= 2 {
			var err error
			if off, err = constInt(c.args[1]); err != nil {
				return err
			}
			if off < 0 {
				return fmt.Errorf("%s's offset must not be negative", c.name)
			}
		}
		for i := range idx {
			j := i - int(off)
			if c.name == "LEAD" {
				j = i + int(off)
			}
			var v any
			var err error
			if j >= 0 && j < n {
				v, err = argAt(j, 0)
			} else if len(c.args) == 3 {
				v, err = argAt(i, 2)
				v = coerce(v, c.t)
			}
			if err != nil {
				return err
			}
			out[idx[i]] = v
		}
		return nil
	case "PERCENTILE_CONT", "PERCENTILE_DISC":
		pv, err := argAt(0, 1)
		if err != nil {
			return err
		}
		if pv == nil || toFloat(pv) < 0 || toFloat(pv) > 1 {
			return fmt.Errorf("%s's percentile must be in [0, 1]", c.name)
		}
		pc := toFloat(pv)
		var vals []any
		for i := range idx {
			v, err := argAt(i, 0)
			if err != nil {
				return err
			}
			if v != nil {
				vals = append(vals, v)
			}
		}
		sort.SliceStable(vals, func(a, b int) bool { r, _ := compare(vals[a], vals[b]); return r < 0 })
		var res any
		if m := len(vals); m > 0 {
			if c.name == "PERCENTILE_CONT" {
				pos := pc * float64(m-1)
				lo, hi := math.Floor(pos), math.Ceil(pos)
				a, b := toFloat(vals[int(lo)]), toFloat(vals[int(hi)])
				res = a + (pos-lo)*(b-a)
			} else {
				res = vals[m-1]
				for j := range vals {
					if float64(j+1)/float64(m) >= pc {
						res = vals[j]
						break
					}
				}
			}
		}
		for i := range idx {
			out[idx[i]] = res
		}
		return nil
	}
	for i := range idx {
		lo, hi, err := frame(i)
		if err != nil {
			return err
		}
		var span []int
		for j := lo; j <= hi; j++ {
			span = append(span, j)
		}
		switch c.name {
		case "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE":
			nth := int64(1)
			if c.name == "NTH_VALUE" {
				if nth, err = constInt(c.args[1]); err != nil {
					return err
				}
				if nth < 1 {
					return fmt.Errorf("NTH_VALUE's position must be at least 1")
				}
			}
			if c.name == "LAST_VALUE" {
				for a, b := 0, len(span)-1; a < b; a, b = a+1, b-1 {
					span[a], span[b] = span[b], span[a]
				}
			}
			var v any
			seen := int64(0)
			for _, j := range span {
				x, err := argAt(j, 0)
				if err != nil {
					return err
				}
				if x == nil && c.ignoreNulls {
					continue
				}
				if seen++; seen == nth {
					v = x
					break
				}
			}
			out[idx[i]] = v
		default:
			envs := make([]env, len(span))
			for a, j := range span {
				envs[a] = inputs[idx[j]]
			}
			v, err := c.aggregate(envs)
			if err != nil {
				return err
			}
			out[idx[i]] = v
		}
	}
	return nil
}
