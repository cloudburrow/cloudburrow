package bigtablefront

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The plan and run of a query (#1511): the FROM source's columns and rows
// (a table read through the emulator, with the temporal arguments
// applied, or a view's own query run), then WHERE, GROUP BY and the
// aggregates, DISTINCT, ORDER BY _key and LIMIT, as the reference orders
// them.

// source is what FROM names: its columns and rows.
type source struct {
	cols []column
	rows [][]any
	// refused are columns the emulator's SQL cannot read, and why.
	refused map[string]string
}

// plan is a checked query.
type plan struct {
	q        *query
	src      *source
	items    []selectItem
	cols     []*btpb.ColumnMetadata
	aggs     []*call
	grouped  bool
	groupBy  []expr
	keyIndex int // the _key column of the source, or -1
}

func (s *scope) noAggs(where string) *scope {
	c := *s
	c.aggs = nil
	c.noAggOK = where
	return &c
}

func (c *call) checkAgg(s *scope) (*btpb.Type, error) {
	if s.aggs == nil {
		where := s.noAggOK
		if where == "" {
			where = "here"
		}
		return nil, fmt.Errorf("Aggregate function %s not allowed in %s", c.name, where)
	}
	if s.inAgg {
		return nil, fmt.Errorf("Aggregations of aggregations are not allowed")
	}
	if c.name == "MIN_BY" || c.name == "MAX_BY" {
		return nil, unsupportedErr{"the function " + c.name}
	}
	inner := *s
	inner.inAgg = true
	var ts []*btpb.Type
	for _, a := range c.args {
		t, err := a.check(&inner)
		if err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	c.argTypes = ts
	n := len(ts)
	if c.star {
		c.t = int64Type
	} else {
		bad := func() (*btpb.Type, error) { return nil, sigErr(c, ts) }
		switch c.name {
		case "COUNT":
			if n != 1 {
				return bad()
			}
			c.t = int64Type
		case "COUNTIF":
			if n != 1 || !isKind(ts[0], "BOOL") {
				return bad()
			}
			c.t = int64Type
		case "SUM":
			if n != 1 || !isNumeric(ts[0]) {
				return bad()
			}
			c.t = int64Type
			if !isKind(ts[0], "INT64") {
				c.t = float64Type
			}
		case "AVG":
			if n != 1 || !isNumeric(ts[0]) {
				return bad()
			}
			c.t = float64Type
		case "MIN", "MAX":
			if n != 1 || isKind(ts[0], "MAP<", "STRUCT<", "ARRAY<") {
				return bad()
			}
			c.t = ts[0]
		case "ANY_VALUE":
			if n != 1 {
				return bad()
			}
			c.t = ts[0]
		case "LOGICAL_AND", "LOGICAL_OR":
			if n != 1 || !isKind(ts[0], "BOOL") {
				return bad()
			}
			c.t = boolType
		case "BIT_AND", "BIT_OR", "BIT_XOR":
			if n != 1 || !isKind(ts[0], "INT64") {
				return bad()
			}
			c.t = int64Type
		case "ARRAY_AGG":
			if n != 1 || isKind(ts[0], "ARRAY<") {
				return bad()
			}
			c.t = arrayType(ts[0])
		case "STRING_AGG":
			if n < 1 || n > 2 || !isKind(ts[0], "STRING", "BYTES") || n == 2 && !assignable(ts[1], ts[0]) {
				return bad()
			}
			c.t = ts[0]
		}
		if c.distinct && !groupable(ts[0]) {
			return nil, fmt.Errorf("Aggregate functions with DISTINCT cannot be used with arguments of type %s", kind(ts[0]))
		}
	}
	c.isAgg = true
	c.agg = len(*s.aggs)
	*s.aggs = append(*s.aggs, c)
	return c.t, nil
}

// aggregate computes c over rows.
func (c *call) aggregate(rows [][]any, base env) (any, error) {
	if c.star {
		return int64(len(rows)), nil
	}
	var vals []any
	seen := map[string]bool{}
	var sep any
	for _, r := range rows {
		e := base
		e.row = r
		v, err := c.args[0].eval(&e)
		if err != nil {
			return nil, err
		}
		if len(c.args) == 2 && sep == nil {
			if sep, err = c.args[1].eval(&e); err != nil {
				return nil, err
			}
		}
		if v == nil && (c.name != "ARRAY_AGG" || c.ignoreNulls) {
			continue
		}
		if c.distinct {
			k := groupKey(v)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		vals = append(vals, v)
	}
	switch c.name {
	case "COUNT":
		return int64(len(vals)), nil
	case "COUNTIF":
		n := int64(0)
		for _, v := range vals {
			if v == true {
				n++
			}
		}
		return n, nil
	case "SUM", "AVG":
		if len(vals) == 0 {
			return nil, nil
		}
		if c.name == "SUM" && isKind(c.t, "INT64") {
			var s int64
			for _, v := range vals {
				r, err := arith("+", s, v, true)
				if err != nil {
					return nil, err
				}
				s = r.(int64)
			}
			return s, nil
		}
		f := 0.0
		for _, v := range vals {
			f += toFloat(v)
		}
		if c.name == "AVG" {
			return f / float64(len(vals)), nil
		}
		return f, nil
	case "MIN", "MAX":
		var best any
		for _, v := range vals {
			if fv, ok := v.(float64); ok && math.IsNaN(fv) {
				return math.NaN(), nil
			}
			if best == nil {
				best = v
				continue
			}
			cmp, _ := compare(v, best)
			if c.name == "MIN" && cmp < 0 || c.name == "MAX" && cmp > 0 {
				best = v
			}
		}
		return best, nil
	case "ANY_VALUE":
		if len(vals) == 0 {
			return nil, nil
		}
		return vals[0], nil
	case "LOGICAL_AND", "LOGICAL_OR":
		if len(vals) == 0 {
			return nil, nil
		}
		and := c.name == "LOGICAL_AND"
		for _, v := range vals {
			if v.(bool) != and {
				return !and, nil
			}
		}
		return and, nil
	case "BIT_AND", "BIT_OR", "BIT_XOR":
		if len(vals) == 0 {
			return nil, nil
		}
		acc := vals[0].(int64)
		for _, v := range vals[1:] {
			switch c.name {
			case "BIT_AND":
				acc &= v.(int64)
			case "BIT_OR":
				acc |= v.(int64)
			default:
				acc ^= v.(int64)
			}
		}
		return acc, nil
	case "ARRAY_AGG":
		if len(vals) == 0 {
			return nil, nil
		}
		for _, v := range vals {
			if v == nil {
				return nil, fmt.Errorf("Array cannot have a null element; error in writing field")
			}
		}
		return vals, nil
	case "STRING_AGG":
		if len(vals) == 0 {
			return nil, nil
		}
		d := ","
		if sep != nil {
			d = string(asBytes(sep))
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = string(asBytes(v))
		}
		return text(strings.Join(parts, d), isKind(c.t, "BYTES")), nil
	}
	return nil, unsupportedErr{"the aggregate " + c.name}
}

// walk calls f on e and every expression under it, not descending into
// aggregates' arguments when f returns false.
func walk(e expr, f func(expr) bool) {
	if e == nil || !f(e) {
		return
	}
	switch x := e.(type) {
	case *index:
		walk(x.x, f)
		walk(x.k, f)
	case *field:
		walk(x.x, f)
	case *arrayLit:
		for _, a := range x.elems {
			walk(a, f)
		}
	case *interval:
		walk(x.n, f)
	case *unaryOp:
		walk(x.x, f)
	case *binaryOp:
		walk(x.l, f)
		walk(x.r, f)
	case *isExpr:
		walk(x.x, f)
	case *inExpr:
		walk(x.x, f)
		walk(x.array, f)
		for _, a := range x.list {
			walk(a, f)
		}
	case *caseExpr:
		walk(x.operand, f)
		for i := range x.whens {
			walk(x.whens[i], f)
			walk(x.thens[i], f)
		}
		walk(x.els, f)
	case *call:
		for _, a := range x.args {
			walk(a, f)
		}
	case *cast:
		walk(x.x, f)
	case *extract:
		walk(x.x, f)
		walk(x.tz, f)
	}
}

// constant is whether e reads no column, so it can be evaluated once.
func constant(e expr) bool {
	ok := true
	walk(e, func(x expr) bool {
		switch c := x.(type) {
		case *colRef:
			ok = false
		case *call:
			if c.isAgg || aggregates[c.name] {
				ok = false
			}
		}
		return ok
	})
	return ok
}

// covered checks that e reads columns only through the group keys.
func covered(e expr, keys map[string]bool) error {
	var err error
	walk(e, func(x expr) bool {
		if err != nil {
			return false
		}
		if keys[x.String()] {
			return false
		}
		switch c := x.(type) {
		case *call:
			if c.isAgg {
				return false
			}
		case *colRef:
			err = fmt.Errorf("SELECT list expression references column %s which is neither grouped nor aggregated", c.name)
			return false
		}
		return true
	})
	return err
}

// build checks q against its source.
func build(q *query, src *source, params map[string]*btpb.Type) (*plan, error) {
	p := &plan{q: q, src: src, keyIndex: -1}
	for i, c := range src.cols {
		if c.name == "_key" {
			p.keyIndex = i
		}
	}
	base := &scope{cols: src.cols, alias: q.alias, params: params}
	refusedCheck := func(e expr) error {
		var err error
		walk(e, func(x expr) bool {
			if c, ok := x.(*colRef); ok && err == nil {
				if why, bad := src.refused[c.name]; bad {
					err = unsupportedErr{why}
				}
			}
			return err == nil
		})
		return err
	}
	if q.where != nil {
		if err := refusedCheck(q.where); err != nil {
			return nil, err
		}
		t, err := q.where.check(base.noAggs("WHERE clause"))
		if err != nil {
			return nil, err
		}
		if !isKind(t, "BOOL") {
			return nil, fmt.Errorf("WHERE clause should return type BOOL, but returns %s", kind(t))
		}
	}
	// Expand *.
	for _, it := range q.items {
		if !it.star {
			p.items = append(p.items, it)
			continue
		}
		if it.alias != "" && it.alias != q.alias {
			return nil, fmt.Errorf("Unrecognized name: %s", it.alias)
		}
		if q.from == "" {
			return nil, fmt.Errorf("SELECT * must have a FROM clause")
		}
		for _, c := range src.cols {
			if _, bad := src.refused[c.name]; bad {
				continue
			}
			p.items = append(p.items, selectItem{e: &colRef{name: c.name}, alias: c.name})
		}
	}
	aggScope := *base
	aggScope.aggs = &p.aggs
	for i := range p.items {
		if err := refusedCheck(p.items[i].e); err != nil {
			return nil, err
		}
		t, err := p.items[i].e.check(&aggScope)
		if err != nil {
			return nil, err
		}
		name := p.items[i].alias
		if name == "" {
			switch x := p.items[i].e.(type) {
			case *colRef:
				name = x.name
			case *field:
				name = x.name
			}
		}
		p.cols = append(p.cols, &btpb.ColumnMetadata{Name: name, Type: t})
	}
	// GROUP BY: an expression, a SELECT alias or a 1-based ordinal.
	keys := map[string]bool{}
	for _, g := range q.groupBy {
		if l, ok := g.(*literal); ok {
			n, isInt := l.v.(int64)
			if !isInt || n < 1 || n > int64(len(p.items)) {
				return nil, fmt.Errorf("GROUP BY ordinal %v is out of range", l.v)
			}
			if _, isAgg := p.items[n-1].e.(*call); isAgg && p.items[n-1].e.(*call).isAgg {
				return nil, fmt.Errorf("GROUP BY ordinal %d refers to an aggregate", n)
			}
			g = p.items[n-1].e
		} else if c, ok := g.(*colRef); ok && c.qualifier == "" {
			inSource := false
			for _, col := range src.cols {
				inSource = inSource || col.name == c.name
			}
			for _, it := range p.items {
				if it.alias == c.name && !inSource {
					g = it.e
				}
			}
		}
		if err := refusedCheck(g); err != nil {
			return nil, err
		}
		t, err := g.check(base.noAggs("GROUP BY clause"))
		if err != nil {
			return nil, err
		}
		if !groupable(t) {
			return nil, fmt.Errorf("Grouping by expressions of type %s is not allowed", kind(t))
		}
		p.groupBy = append(p.groupBy, g)
		keys[g.String()] = true
	}
	p.grouped = len(p.groupBy) > 0 || len(p.aggs) > 0
	if p.grouped {
		for _, it := range p.items {
			if err := covered(it.e, keys); err != nil {
				return nil, err
			}
		}
	}
	if q.distinct {
		for _, c := range p.cols {
			if !groupable(c.Type) {
				return nil, fmt.Errorf("Column %s of type %s cannot be used in SELECT DISTINCT", c.Name, kind(c.Type))
			}
		}
	}
	if q.orderBy {
		if p.keyIndex < 0 {
			return nil, fmt.Errorf("ORDER BY: %s has no _key column", q.from)
		}
		if p.grouped && !keys[(&colRef{name: "_key"}).String()] {
			return nil, fmt.Errorf("ORDER BY clause expression references column _key which is neither grouped nor aggregated")
		}
	}
	if q.limit != nil {
		t, err := q.limit.check(base.noAggs("LIMIT"))
		if err != nil {
			return nil, err
		}
		if !isKind(t, "INT64") || !constant(q.limit) {
			return nil, fmt.Errorf("LIMIT expects an integer literal or parameter")
		}
	}
	return p, nil
}

// run evaluates the plan, returning its rows.
func (p *plan) run(base env) ([][]any, error) {
	rows := p.src.rows
	if p.q.where != nil {
		var kept [][]any
		for _, r := range rows {
			e := base
			e.row = r
			v, err := p.q.where.eval(&e)
			if err != nil {
				return nil, err
			}
			if v == true {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if p.q.orderBy {
		sorted := append([][]any{}, rows...)
		k := p.keyIndex
		sort.SliceStable(sorted, func(i, j int) bool {
			c, _ := compare(sorted[i][k], sorted[j][k])
			return c < 0
		})
		rows = sorted
	}
	limit := int64(-1)
	if p.q.limit != nil {
		v, err := p.q.limit.eval(&base)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, fmt.Errorf("Limit requires non-null count argument")
		}
		if limit = v.(int64); limit < 0 {
			return nil, fmt.Errorf("Limit requires non-negative count argument")
		}
	}
	project := func(e env) ([]any, error) {
		out := make([]any, len(p.items))
		for i, it := range p.items {
			v, err := it.e.eval(&e)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	var out [][]any
	seen := map[string]bool{}
	emit := func(r []any) bool {
		if p.q.distinct {
			k := groupKey(r)
			if seen[k] {
				return true
			}
			seen[k] = true
		}
		out = append(out, r)
		return limit < 0 || int64(len(out)) < limit
	}
	if limit == 0 {
		return nil, nil
	}
	if !p.grouped {
		for _, r := range rows {
			e := base
			e.row = r
			v, err := project(e)
			if err != nil {
				return nil, err
			}
			if !emit(v) {
				break
			}
		}
		return out, nil
	}
	type group struct{ rows [][]any }
	var order []string
	groups := map[string]*group{}
	for _, r := range rows {
		e := base
		e.row = r
		var kv []any
		for _, g := range p.groupBy {
			v, err := g.eval(&e)
			if err != nil {
				return nil, err
			}
			kv = append(kv, v)
		}
		k := groupKey(kv)
		gr, ok := groups[k]
		if !ok {
			gr = &group{}
			groups[k] = gr
			order = append(order, k)
		}
		gr.rows = append(gr.rows, r)
	}
	if len(p.groupBy) == 0 && len(order) == 0 {
		// Aggregates over no rows: one row.
		order = []string{""}
		groups[""] = &group{}
	}
	for _, k := range order {
		gr := groups[k]
		e := base
		if len(gr.rows) > 0 {
			e.row = gr.rows[0]
		} else {
			e.row = make([]any, len(p.src.cols))
		}
		e.aggs = make([]any, len(p.aggs))
		for i, a := range p.aggs {
			v, err := a.aggregate(gr.rows, base)
			if err != nil {
				return nil, err
			}
			e.aggs[i] = v
		}
		v, err := project(e)
		if err != nil {
			return nil, err
		}
		if !emit(v) {
			break
		}
	}
	return out, nil
}

// --- values on the wire ---

// encode is v, of type t, as a result Value.
func encode(v any, t *btpb.Type) *btpb.Value {
	if v == nil {
		return &btpb.Value{}
	}
	switch k := t.GetKind().(type) {
	case *btpb.Type_BytesType:
		return &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: asBytes(v)}}
	case *btpb.Type_StringType:
		return &btpb.Value{Kind: &btpb.Value_StringValue{StringValue: string(asBytes(v))}}
	case *btpb.Type_Int64Type:
		return &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: v.(int64)}}
	case *btpb.Type_Float32Type, *btpb.Type_Float64Type:
		return &btpb.Value{Kind: &btpb.Value_FloatValue{FloatValue: toFloat(v)}}
	case *btpb.Type_BoolType:
		return &btpb.Value{Kind: &btpb.Value_BoolValue{BoolValue: v.(bool)}}
	case *btpb.Type_TimestampType:
		return &btpb.Value{Kind: &btpb.Value_TimestampValue{TimestampValue: timestamppb.New(v.(time.Time))}}
	case *btpb.Type_DateType:
		d := v.(time.Time)
		return &btpb.Value{Kind: &btpb.Value_DateValue{DateValue: &date.Date{Year: int32(d.Year()), Month: int32(d.Month()), Day: int32(d.Day())}}}
	case *btpb.Type_ArrayType:
		var vs []*btpb.Value
		for _, e := range v.([]any) {
			vs = append(vs, encode(e, k.ArrayType.GetElementType()))
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: vs}}}
	case *btpb.Type_MapType:
		m := v.(*mapValue)
		var vs []*btpb.Value
		for i := range m.keys {
			vs = append(vs, &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: []*btpb.Value{
				encode(m.keys[i], k.MapType.GetKeyType()), encode(m.vals[i], k.MapType.GetValueType()),
			}}}})
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: vs}}}
	case *btpb.Type_StructType:
		var vs []*btpb.Value
		for i, f := range v.([]any) {
			vs = append(vs, encode(f, k.StructType.GetFields()[i].GetType()))
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: vs}}}
	}
	return &btpb.Value{}
}

// paramTypeOK is whether the reference takes a parameter of type t.
func paramTypeOK(t *btpb.Type) bool {
	switch t.GetKind().(type) {
	case *btpb.Type_BytesType, *btpb.Type_StringType, *btpb.Type_Int64Type, *btpb.Type_Float32Type,
		*btpb.Type_Float64Type, *btpb.Type_BoolType, *btpb.Type_TimestampType, *btpb.Type_DateType:
		return true
	case *btpb.Type_ArrayType:
		e := t.GetArrayType().GetElementType()
		return !isKind(e, "ARRAY<") && paramTypeOK(e)
	}
	return false
}

// decode is a bound parameter's value, of type t.
func decode(v *btpb.Value, t *btpb.Type) (any, error) {
	switch k := v.GetKind().(type) {
	case nil:
		return nil, nil
	case *btpb.Value_BytesValue:
		if isKind(t, "BYTES") {
			return k.BytesValue, nil
		}
	case *btpb.Value_StringValue:
		if isKind(t, "STRING") {
			return k.StringValue, nil
		}
	case *btpb.Value_IntValue:
		if isKind(t, "INT64") {
			return k.IntValue, nil
		}
	case *btpb.Value_FloatValue:
		if isKind(t, "FLOAT32", "FLOAT64") {
			return k.FloatValue, nil
		}
	case *btpb.Value_BoolValue:
		if isKind(t, "BOOL") {
			return k.BoolValue, nil
		}
	case *btpb.Value_TimestampValue:
		if isKind(t, "TIMESTAMP") {
			return k.TimestampValue.AsTime(), nil
		}
	case *btpb.Value_DateValue:
		if isKind(t, "DATE") {
			return time.Date(int(k.DateValue.GetYear()), time.Month(k.DateValue.GetMonth()), int(k.DateValue.GetDay()), 0, 0, 0, 0, time.UTC), nil
		}
	case *btpb.Value_ArrayValue:
		if isKind(t, "ARRAY<") {
			et := t.GetArrayType().GetElementType()
			out := make([]any, 0, len(k.ArrayValue.GetValues()))
			for _, e := range k.ArrayValue.GetValues() {
				d, err := decode(e, et)
				if err != nil {
					return nil, err
				}
				out = append(out, d)
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("the value does not match the parameter's type %s", kind(t))
}

// --- the table as a source ---

// temporal is the FROM table's temporal arguments, evaluated.
type temporal struct {
	history                      bool
	asOf, after, afterEq, before *time.Time
	latestN                      int64
}

func temporalArgs(args []tableArg, params map[string]*btpb.Type, base env) (temporal, error) {
	var t temporal
	s := &scope{params: params, noAggOK: "a table argument"}
	seen := map[string]bool{}
	for _, a := range args {
		if seen[a.name] {
			return t, fmt.Errorf("The named argument %s is given twice", a.name)
		}
		seen[a.name] = true
		at, err := a.e.check(s)
		if err != nil {
			return t, err
		}
		want := map[string]string{"with_history": "BOOL", "as_of": "TIMESTAMP", "after": "TIMESTAMP",
			"after_or_equal": "TIMESTAMP", "before": "TIMESTAMP", "latest_n": "INT64"}[a.name]
		if want == "" {
			return t, fmt.Errorf("Unknown named argument %s: the table arguments are with_history, as_of, after, after_or_equal, before and latest_n", a.name)
		}
		if !isKind(at, want) {
			return t, fmt.Errorf("The named argument %s takes %s, not %s", a.name, want, kind(at))
		}
		if !constant(a.e) {
			return t, fmt.Errorf("The named argument %s must be a constant", a.name)
		}
		if base.params == nil && a.name != "with_history" {
			continue // prepared: the parameters are bound at execution
		}
		v, err := a.e.eval(&base)
		if err != nil {
			if a.name == "with_history" {
				return t, fmt.Errorf("with_history must be TRUE or FALSE, not a parameter: it fixes the families' types")
			}
			return t, err
		}
		if v == nil {
			return t, fmt.Errorf("The named argument %s must not be NULL", a.name)
		}
		switch a.name {
		case "with_history":
			t.history = v.(bool)
		case "latest_n":
			if t.latestN = v.(int64); t.latestN < 1 {
				return t, fmt.Errorf("latest_n must be at least 1")
			}
		default:
			ts := v.(time.Time)
			switch a.name {
			case "as_of":
				t.asOf = &ts
			case "after":
				t.after = &ts
			case "after_or_equal":
				t.afterEq = &ts
			case "before":
				t.before = &ts
			}
		}
	}
	if !t.history {
		for _, n := range []string{"after", "after_or_equal", "before", "latest_n"} {
			if seen[n] {
				return t, fmt.Errorf("The table argument %s requires with_history => TRUE", n)
			}
		}
	}
	return t, nil
}

// tableColumns is the table's columns: _key, and a map per family.
func tableColumns(families map[string]*familyInfo, t temporal) ([]column, map[string]string) {
	cols := []column{{"_key", bytesType}}
	refused := map[string]string{}
	ids := make([]string, 0, len(families))
	for id := range families {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ft := familyType
		if t.history {
			ft = historyType
		}
		if families[id].typed {
			refused[id] = "reading the aggregate column family " + id
		}
		cols = append(cols, column{id, ft})
	}
	return cols, refused
}

type familyInfo struct{ typed bool }

// tableRows is the table's rows as the columns tableColumns names.
func tableRows(rows []row, cols []column, t temporal) [][]any {
	idx := map[string]int{}
	for i, c := range cols {
		idx[c.name] = i
	}
	out := make([][]any, 0, len(rows))
	for _, r := range rows {
		vals := make([]any, len(cols))
		vals[0] = r.key
		// family -> qualifier -> cells, newest first.
		type qcells struct {
			q     []byte
			cells []cell
		}
		byFam := map[string][]*qcells{}
		for _, c := range r.cells {
			ts := time.UnixMicro(c.ts).UTC()
			if t.asOf != nil && ts.After(*t.asOf) || t.after != nil && !ts.After(*t.after) ||
				t.afterEq != nil && ts.Before(*t.afterEq) || t.before != nil && !ts.Before(*t.before) {
				continue
			}
			qs := byFam[c.family]
			var cur *qcells
			for _, q := range qs {
				if string(q.q) == string(c.qualifier) {
					cur = q
				}
			}
			if cur == nil {
				cur = &qcells{q: c.qualifier}
				byFam[c.family] = append(qs, cur)
			}
			cur.cells = append(cur.cells, c)
		}
		for i := 1; i < len(cols); i++ {
			m := &mapValue{}
			qs := byFam[cols[i].name]
			sort.Slice(qs, func(a, b int) bool { return string(qs[a].q) < string(qs[b].q) })
			for _, q := range qs {
				sort.SliceStable(q.cells, func(a, b int) bool { return q.cells[a].ts > q.cells[b].ts })
				m.keys = append(m.keys, q.q)
				if !t.history {
					m.vals = append(m.vals, q.cells[0].value)
					continue
				}
				cs := q.cells
				if t.latestN > 0 && int64(len(cs)) > t.latestN {
					cs = cs[:t.latestN]
				}
				hist := make([]any, len(cs))
				for j, c := range cs {
					hist[j] = []any{time.UnixMicro(c.ts).UTC(), c.value}
				}
				m.vals = append(m.vals, hist)
			}
			if len(m.keys) > 0 {
				vals[i] = m // an unused family is NULL (the reference)
			}
		}
		_ = idx
		out = append(out, vals)
	}
	return out
}
