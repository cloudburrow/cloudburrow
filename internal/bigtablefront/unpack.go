package bigtablefront

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// UNPACK (#1704), the built-in table function of GoogleSQL for Bigtable
// (https://cloud.google.com/bigtable/docs/reference/sql/table-functions-built-in#unpack):
//
//	UNPACK(TABLE table(with_history => true))
//	UNPACK((SELECT ... FROM table(with_history => true) ...))
//
// "takes a input table and expands it so that each input timestamped
// value is expanded into its own row representing that timestamp. The
// timestamps are moved to a separate _timestamp column." As the reference
// lists:
//   - a column family MAP<key, ARRAY<STRUCT<timestamp, value>>> becomes
//     MAP<key, value>, holding the qualifiers that have a value at the row's
//     timestamp;
//   - a column qualifier ARRAY<STRUCT<timestamp, value>> becomes value,
//     NULL where it has none at the row's timestamp;
//   - other columns are unchanged, repeated on each of the row's rows;
//   - an input row with no timestamps is one row, its _timestamp NULL;
//   - ORDER BY inside the input query is ignored.
//
// Values sharing a timestamp across the selected columns land in one row
// (the reference's Dept2 row at 19:00 holds both expenses and sales). The
// reference fixes no order for the expanded rows; the emulator lists each
// input row's timestamps newest first, as Bigtable keeps cells.

// unpackInput reads UNPACK's parenthesised argument, after the UNPACK name.
func (p *parser) unpackInput() (*query, error) {
	start := p.pos
	if !p.op("(") {
		return nil, fmt.Errorf("Syntax error: expected ( after UNPACK")
	}
	depth := 1
	end := p.pos
	for ; end < len(p.toks) && depth > 0; end++ {
		if p.toks[end].kind == "op" {
			switch p.toks[end].text {
			case "(":
				depth++
			case ")":
				depth--
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("Syntax error: expected ) to close UNPACK")
	}
	inner := p.toks[start+1 : end-1] // between UNPACK's parentheses
	p.pos = end
	var toks []token
	switch {
	case len(inner) > 0 && inner[0].kind == "ident" && strings.EqualFold(inner[0].text, "TABLE"):
		// TABLE t(args) is the table itself: SELECT * FROM t(args).
		toks = append([]token{{"ident", "SELECT"}, {"op", "*"}, {"ident", "FROM"}}, inner[1:]...)
	case len(inner) >= 2 && inner[0].text == "(" && inner[len(inner)-1].text == ")":
		toks = inner[1 : len(inner)-1]
		if len(toks) == 0 || toks[0].kind != "ident" || !strings.EqualFold(toks[0].text, "SELECT") {
			return nil, fmt.Errorf("Syntax error: UNPACK takes a subquery, UNPACK((SELECT ...)), or a table, UNPACK(TABLE t(with_history => true))")
		}
	default:
		return nil, fmt.Errorf("Syntax error: UNPACK takes a subquery, UNPACK((SELECT ...)), or a table, UNPACK(TABLE t(with_history => true))")
	}
	q, err := parseTokens(toks)
	if err != nil {
		return nil, fmt.Errorf("UNPACK: %w", err)
	}
	// "ORDER BY within the UNPACK subquery is ignored."
	q.orderBy = false
	return q, nil
}

// unpackKind is how UNPACK transforms a column: 0 unchanged, 1 a family
// of timestamped values, 2 a qualifier's timestamped values.
func unpackKind(t *btpb.Type) (int, *btpb.Type) {
	cellValue := func(a *btpb.Type) *btpb.Type {
		at, ok := a.GetKind().(*btpb.Type_ArrayType)
		if !ok {
			return nil
		}
		st, ok := at.ArrayType.GetElementType().GetKind().(*btpb.Type_StructType)
		if !ok {
			return nil
		}
		fs := st.StructType.GetFields()
		if len(fs) != 2 || fs[0].GetFieldName() != "timestamp" || !isKind(fs[0].GetType(), "TIMESTAMP") || fs[1].GetFieldName() != "value" {
			return nil
		}
		return fs[1].GetType()
	}
	if m, ok := t.GetKind().(*btpb.Type_MapType); ok {
		if v := cellValue(m.MapType.GetValueType()); v != nil {
			return 1, mapType(m.MapType.GetKeyType(), v)
		}
		return 0, t
	}
	if v := cellValue(t); v != nil {
		return 2, v
	}
	return 0, t
}

// unpackSource is UNPACK's table: the input query's columns transformed,
// and _timestamp.
func (q *sqlFront) unpackSource(ctx context.Context, instance string, qu *query, params map[string]*btpb.Type, base env, read bool, depth int) (*source, error) {
	if len(qu.args) > 0 {
		return nil, fmt.Errorf("UNPACK takes no table arguments")
	}
	in, err := q.compileParsed(ctx, instance, qu.unpack, params, base.params, read, depth+1)
	if err != nil {
		return nil, err
	}
	src := &source{}
	kinds := make([]int, len(in.cols))
	for i, c := range in.cols {
		var t *btpb.Type
		kinds[i], t = unpackKind(c.GetType())
		src.cols = append(src.cols, column{c.GetName(), t})
	}
	src.cols = append(src.cols, column{"_timestamp", timestampType})
	if !read {
		return src, nil
	}
	rows, err := in.run(base)
	if err != nil {
		return nil, err
	}
	src.rows = unpackRows(rows, kinds)
	return src, nil
}

// unpackRows expands each input row into a row per timestamp.
func unpackRows(rows [][]any, kinds []int) [][]any {
	var out [][]any
	for _, r := range rows {
		seen := map[int64]bool{}
		var stamps []time.Time
		note := func(h any) {
			cells, _ := h.([]any)
			for _, c := range cells {
				ts := c.([]any)[0].(time.Time)
				if !seen[ts.UnixNano()] {
					seen[ts.UnixNano()] = true
					stamps = append(stamps, ts)
				}
			}
		}
		for i, k := range kinds {
			switch k {
			case 1:
				if m, ok := r[i].(*mapValue); ok {
					for _, h := range m.vals {
						note(h)
					}
				}
			case 2:
				note(r[i])
			}
		}
		if len(stamps) == 0 {
			o := make([]any, len(kinds)+1)
			for i, k := range kinds {
				if k == 0 {
					o[i] = r[i]
				}
			}
			out = append(out, o)
			continue
		}
		sort.Slice(stamps, func(a, b int) bool { return stamps[a].After(stamps[b]) })
		at := func(h any, ts time.Time) (any, bool) {
			cells, _ := h.([]any)
			for _, c := range cells {
				cv := c.([]any)
				if cv[0].(time.Time).Equal(ts) {
					return cv[1], true
				}
			}
			return nil, false
		}
		for _, ts := range stamps {
			o := make([]any, len(kinds)+1)
			for i, k := range kinds {
				switch k {
				case 0:
					o[i] = r[i]
				case 1:
					m, ok := r[i].(*mapValue)
					if !ok {
						continue
					}
					nm := &mapValue{}
					for j, h := range m.vals {
						if v, ok := at(h, ts); ok {
							nm.keys = append(nm.keys, m.keys[j])
							nm.vals = append(nm.vals, v)
						}
					}
					if len(nm.keys) > 0 {
						o[i] = nm
					}
				case 2:
					o[i], _ = at(r[i], ts)
				}
			}
			o[len(kinds)] = ts
			out = append(out, o)
		}
	}
	return out
}
