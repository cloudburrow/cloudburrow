package mql

// Firestore with MongoDB compatibility (#1703): aggregation pipelines,
// their stages and expressions, evaluated over a collection's documents.
// Google limits a pipeline to 250 stages and does not support $merge or
// $out (behavior differences).

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const mongoMaxStages = 250

// aggEnv is what a pipeline's stages read beside their input: another
// collection, for $lookup.
type aggEnv struct {
	load func(coll string) ([]bson.D, error)
}

func runPipeline(env *aggEnv, docs []bson.D, pipeline bson.A) ([]bson.D, error) {
	if len(pipeline) > mongoMaxStages {
		return nil, merr(7749501, "Location7749501", "Aggregations are limited to %d stages; this pipeline has %d.", mongoMaxStages, len(pipeline))
	}
	for _, st := range pipeline {
		sd, ok := st.(bson.D)
		if !ok || len(sd) != 1 {
			return nil, merr(40323, "Location40323", "A pipeline stage specification object must contain exactly one field.")
		}
		var err error
		if docs, err = runStage(env, docs, sd[0]); err != nil {
			return nil, err
		}
	}
	return docs, nil
}

func runStage(env *aggEnv, docs []bson.D, st bson.E) ([]bson.D, error) {
	switch st.Key {
	case "$match":
		f, ok := st.Value.(bson.D)
		if !ok {
			return nil, badValue("the match filter must be an expression in an object")
		}
		var out []bson.D
		for _, d := range docs {
			m, err := matches(d, f)
			if err != nil {
				return nil, err
			}
			if m {
				out = append(out, d)
			}
		}
		return out, nil
	case "$project":
		spec, ok := st.Value.(bson.D)
		if !ok || len(spec) == 0 {
			return nil, merr(40177, "Location40177", "$project specification must be an object and have at least one field")
		}
		out := make([]bson.D, 0, len(docs))
		for _, d := range docs {
			p, err := project(d, spec)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, nil
	case "$addFields", "$set":
		spec, ok := st.Value.(bson.D)
		if !ok {
			return nil, merr(40272, "Location40272", "%s specification stage must be an object", st.Key)
		}
		out := make([]bson.D, 0, len(docs))
		for _, d := range docs {
			n := cloneDoc(d)
			for _, e := range spec {
				v, err := evalExpr(e.Value, d, nil)
				if err != nil {
					return nil, err
				}
				if _, rm := v.(removeT); rm {
					unsetPath(&n, strings.Split(e.Key, "."))
					continue
				}
				if err := setPath(&n, strings.Split(e.Key, "."), v); err != nil {
					return nil, err
				}
			}
			out = append(out, n)
		}
		return out, nil
	case "$unset":
		var fields []string
		switch x := st.Value.(type) {
		case string:
			fields = []string{x}
		case bson.A:
			for _, f := range x {
				s, ok := f.(string)
				if !ok {
					return nil, merr(31120, "Location31120", "$unset specification must be a string or an array containing only string values")
				}
				fields = append(fields, s)
			}
		default:
			return nil, merr(31002, "Location31002", "$unset specification must be a string or an array")
		}
		out := make([]bson.D, 0, len(docs))
		for _, d := range docs {
			n := cloneDoc(d)
			for _, f := range fields {
				unsetPath(&n, strings.Split(f, "."))
			}
			out = append(out, n)
		}
		return out, nil
	case "$sort":
		s, err := parseSort(st.Value)
		if err != nil {
			return nil, err
		}
		if len(s) == 0 {
			return nil, merr(15976, "Location15976", "$sort stage must have at least one sort key")
		}
		out := append([]bson.D(nil), docs...)
		sort.SliceStable(out, func(i, j int) bool { return s.less(out[i], out[j]) })
		return out, nil
	case "$skip", "$limit":
		n, ok := numberOf(st.Value)
		if !ok || n < 0 || n != math.Trunc(n) || (st.Key == "$limit" && n == 0) {
			return nil, merr(15958, "Location15958", "invalid argument to %s stage: %v", st.Key, st.Value)
		}
		if st.Key == "$skip" {
			if int(n) >= len(docs) {
				return nil, nil
			}
			return docs[int(n):], nil
		}
		if int(n) < len(docs) {
			return docs[:int(n)], nil
		}
		return docs, nil
	case "$count":
		name, ok := st.Value.(string)
		if !ok || name == "" || strings.HasPrefix(name, "$") || strings.Contains(name, ".") {
			return nil, merr(40156, "Location40156", "the count field must be a non-empty string without $ or .")
		}
		if len(docs) == 0 {
			return nil, nil
		}
		return []bson.D{{{Key: name, Value: int32(len(docs))}}}, nil
	case "$group":
		return groupStage(docs, st.Value)
	case "$sortByCount":
		g, err := groupStage(docs, bson.D{{Key: "_id", Value: st.Value}, {Key: "count", Value: bson.D{{Key: "$sum", Value: int32(1)}}}})
		if err != nil {
			return nil, err
		}
		sort.SliceStable(g, func(i, j int) bool { return compareValues(lookup(g[i], "count"), lookup(g[j], "count")) > 0 })
		return g, nil
	case "$unwind":
		return unwindStage(docs, st.Value)
	case "$replaceRoot", "$replaceWith":
		expr := st.Value
		if st.Key == "$replaceRoot" {
			d, ok := st.Value.(bson.D)
			nr, has := lookupE(d, "newRoot")
			if !ok || !has {
				return nil, merr(40231, "Location40231", "no newRoot specified for the $replaceRoot stage")
			}
			expr = nr
		}
		out := make([]bson.D, 0, len(docs))
		for _, d := range docs {
			v, err := evalExpr(expr, d, nil)
			if err != nil {
				return nil, err
			}
			nd, ok := v.(bson.D)
			if !ok {
				return nil, merr(40228, "Location40228", "'newRoot' expression must evaluate to an object, but resulting value was of type %s", bsonTypeName(v))
			}
			out = append(out, nd)
		}
		return out, nil
	case "$lookup":
		return lookupStage(env, docs, st.Value)
	case "$facet":
		spec, ok := st.Value.(bson.D)
		if !ok {
			return nil, merr(40169, "Location40169", "the $facet specification must be a non-empty object")
		}
		res := bson.D{}
		for _, f := range spec {
			p, ok := f.Value.(bson.A)
			if !ok {
				return nil, merr(40170, "Location40170", "arguments to $facet must be arrays")
			}
			for _, s := range p {
				if sd, ok := s.(bson.D); ok && len(sd) == 1 && (sd[0].Key == "$sample" || sd[0].Key == "$out" || sd[0].Key == "$merge" || sd[0].Key == "$facet") {
					return nil, merr(40600, "Location40600", "%s is not allowed to be used within a $facet stage", sd[0].Key)
				}
			}
			r, err := runPipeline(env, docs, p)
			if err != nil {
				return nil, err
			}
			arr := bson.A{}
			for _, d := range r {
				arr = append(arr, d)
			}
			res = append(res, bson.E{Key: f.Key, Value: arr})
		}
		return []bson.D{res}, nil
	case "$out", "$merge":
		return nil, merr(40324, "Location40324", "The %s stage is not supported.", st.Key)
	}
	return nil, merr(40324, "Location40324", "Unrecognized pipeline stage name: '%s'", st.Key)
}

func groupStage(docs []bson.D, spec any) ([]bson.D, error) {
	sd, ok := spec.(bson.D)
	idExpr, hasID := lookupE(sd, "_id")
	if !ok || !hasID {
		return nil, merr(15955, "Location15955", "a group specification must include an _id")
	}
	type acc struct {
		op   string
		expr any
	}
	var accs []struct {
		name string
		acc
	}
	for _, e := range sd {
		if e.Key == "_id" {
			continue
		}
		ad, ok := e.Value.(bson.D)
		if !ok || len(ad) != 1 {
			return nil, merr(40234, "Location40234", "The field '%s' must be an accumulator object", e.Key)
		}
		switch ad[0].Key {
		case "$sum", "$avg", "$min", "$max", "$first", "$last", "$push", "$addToSet", "$count", "$mergeObjects":
		default:
			return nil, merr(15952, "Location15952", "unknown group operator '%s'", ad[0].Key)
		}
		accs = append(accs, struct {
			name string
			acc
		}{e.Key, acc{ad[0].Key, ad[0].Value}})
	}
	type group struct {
		id   any
		vals [][]any
	}
	var groups []*group
	for _, d := range docs {
		id, err := evalExpr(idExpr, d, nil)
		if err != nil {
			return nil, err
		}
		if _, m := id.(missingT); m {
			id = nil
		}
		var g *group
		for _, x := range groups {
			if valuesEqual(x.id, id) {
				g = x
				break
			}
		}
		if g == nil {
			g = &group{id: id, vals: make([][]any, len(accs))}
			groups = append(groups, g)
		}
		for i, a := range accs {
			var v any = int32(1)
			if a.op != "$count" {
				if v, err = evalExpr(a.expr, d, nil); err != nil {
					return nil, err
				}
			}
			g.vals[i] = append(g.vals[i], v)
		}
	}
	out := make([]bson.D, 0, len(groups))
	for _, g := range groups {
		row := bson.D{{Key: "_id", Value: g.id}}
		for i, a := range accs {
			row = append(row, bson.E{Key: a.name, Value: accumulate(a.op, g.vals[i])})
		}
		out = append(out, row)
	}
	return out, nil
}

func accumulate(op string, vals []any) any {
	present := vals[:0:0]
	for _, v := range vals {
		if _, m := v.(missingT); !m {
			present = append(present, v)
		}
	}
	switch op {
	case "$sum", "$count":
		var s any = int32(0)
		for _, v := range present {
			if isNumber(v) {
				s = addNumbers(s, v)
			}
		}
		return s
	case "$avg":
		var s float64
		n := 0
		for _, v := range present {
			if f, ok := numberOf(v); ok {
				s += f
				n++
			}
		}
		if n == 0 {
			return nil
		}
		return s / float64(n)
	case "$min", "$max":
		var best any
		for _, v := range present {
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			c := compareValues(v, best)
			if (op == "$min" && c < 0) || (op == "$max" && c > 0) {
				best = v
			}
		}
		return best
	case "$first":
		if len(vals) == 0 {
			return nil
		}
		if _, m := vals[0].(missingT); m {
			return nil
		}
		return vals[0]
	case "$last":
		if len(vals) == 0 {
			return nil
		}
		if _, m := vals[len(vals)-1].(missingT); m {
			return nil
		}
		return vals[len(vals)-1]
	case "$push":
		out := bson.A{}
		return append(out, present...)
	case "$addToSet":
		out := bson.A{}
		for _, v := range present {
			dup := false
			for _, x := range out {
				if valuesEqual(x, v) {
					dup = true
				}
			}
			if !dup {
				out = append(out, v)
			}
		}
		return out
	case "$mergeObjects":
		out := bson.D{}
		for _, v := range present {
			if d, ok := v.(bson.D); ok {
				for _, e := range d {
					out = setKey(out, e.Key, e.Value)
				}
			}
		}
		return out
	}
	return nil
}

func unwindStage(docs []bson.D, spec any) ([]bson.D, error) {
	var path, idx string
	preserve := false
	switch x := spec.(type) {
	case string:
		path = x
	case bson.D:
		path, _ = lookup(x, "path").(string)
		idx, _ = lookup(x, "includeArrayIndex").(string)
		preserve = truthy(lookup(x, "preserveNullAndEmptyArrays"))
	}
	if !strings.HasPrefix(path, "$") {
		return nil, merr(28818, "Location28818", "path option to $unwind stage should be prefixed with a '$': %s", path)
	}
	field := path[1:]
	var out []bson.D
	for _, d := range docs {
		v, ok := getPath(d, field)
		arr, isArr := v.(bson.A)
		switch {
		case !ok || v == nil || (isArr && len(arr) == 0):
			if preserve {
				n := cloneDoc(d)
				if idx != "" {
					n = setKey(n, idx, nil)
				}
				out = append(out, n)
			}
		case !isArr:
			n := cloneDoc(d)
			if idx != "" {
				n = setKey(n, idx, nil)
			}
			out = append(out, n)
		default:
			for i, e := range arr {
				n := cloneDoc(d)
				_ = setPath(&n, strings.Split(field, "."), cloneValue(e))
				if idx != "" {
					n = setKey(n, idx, int64(i))
				}
				out = append(out, n)
			}
		}
	}
	return out, nil
}

func lookupStage(env *aggEnv, docs []bson.D, spec any) ([]bson.D, error) {
	sd, ok := spec.(bson.D)
	from, _ := lookup(sd, "from").(string)
	local, _ := lookup(sd, "localField").(string)
	foreign, _ := lookup(sd, "foreignField").(string)
	as, _ := lookup(sd, "as").(string)
	if _, p := lookupE(sd, "pipeline"); p {
		return nil, badValue("$lookup with a pipeline is not supported by CloudBurrow.")
	}
	if !ok || from == "" || local == "" || foreign == "" || as == "" || env == nil || env.load == nil {
		return nil, merr(9, "FailedToParse", "$lookup requires from, localField, foreignField and as")
	}
	other, err := env.load(from)
	if err != nil {
		return nil, err
	}
	out := make([]bson.D, 0, len(docs))
	for _, d := range docs {
		lv := pathValues(d, strings.Split(local, "."))
		var keys []any
		for _, v := range lv {
			if a, ok := v.(bson.A); ok {
				keys = append(keys, a...)
			} else if _, m := v.(missingT); m {
				keys = append(keys, nil)
			} else {
				keys = append(keys, v)
			}
		}
		matched := bson.A{}
		for _, o := range other {
			fv := pathValues(o, strings.Split(foreign, "."))
			for _, k := range keys {
				if eqMatch(fv, k) {
					matched = append(matched, o)
					break
				}
			}
		}
		n := cloneDoc(d)
		_ = setPath(&n, strings.Split(as, "."), matched)
		out = append(out, n)
	}
	return out, nil
}

// removeT is $$REMOVE.
type removeT struct{}

func exprErr(op string) error {
	return merr(168, "InvalidPipelineOperator", "Unrecognized expression '%s'", op)
}

// evalExpr evaluates an aggregation expression against doc ($$ROOT and
// $$CURRENT) and vars.
func evalExpr(expr any, doc bson.D, vars map[string]any) (any, error) {
	switch x := expr.(type) {
	case string:
		if strings.HasPrefix(x, "$$") {
			name := x[2:]
			rest := ""
			if i := strings.Index(name, "."); i >= 0 {
				name, rest = name[:i], name[i+1:]
			}
			var base any
			switch name {
			case "ROOT", "CURRENT":
				base = doc
			case "REMOVE":
				return removeT{}, nil
			case "NOW":
				base = bson.NewDateTimeFromTime(time.Now())
			default:
				v, ok := vars[name]
				if !ok {
					return nil, merr(17276, "Location17276", "Use of undefined variable: %s", name)
				}
				base = v
			}
			if rest == "" {
				return base, nil
			}
			v, ok := getPath(base, rest)
			if !ok {
				return missingT{}, nil
			}
			return v, nil
		}
		if strings.HasPrefix(x, "$") {
			v, ok := getPath(doc, x[1:])
			if !ok {
				return missingT{}, nil
			}
			return v, nil
		}
		return x, nil
	case bson.A:
		out := make(bson.A, 0, len(x))
		for _, e := range x {
			v, err := evalExpr(e, doc, vars)
			if err != nil {
				return nil, err
			}
			if _, m := v.(missingT); m {
				v = nil
			}
			out = append(out, v)
		}
		return out, nil
	case bson.D:
		if len(x) == 1 && strings.HasPrefix(x[0].Key, "$") {
			return evalOp(x[0].Key, x[0].Value, doc, vars)
		}
		out := bson.D{}
		for _, e := range x {
			if strings.HasPrefix(e.Key, "$") {
				return nil, exprErr(e.Key)
			}
			v, err := evalExpr(e.Value, doc, vars)
			if err != nil {
				return nil, err
			}
			if _, m := v.(missingT); m {
				continue
			}
			if _, r := v.(removeT); r {
				continue
			}
			out = append(out, bson.E{Key: e.Key, Value: v})
		}
		return out, nil
	}
	return expr, nil
}

// args evaluates an operator's arguments, a single one or an array.
func args(v any, doc bson.D, vars map[string]any) ([]any, error) {
	arr, ok := v.(bson.A)
	if !ok {
		arr = bson.A{v}
	}
	out := make([]any, len(arr))
	for i, a := range arr {
		r, err := evalExpr(a, doc, vars)
		if err != nil {
			return nil, err
		}
		if _, m := r.(missingT); m {
			r = nil
		}
		out[i] = r
	}
	return out, nil
}

func nullish(v any) bool {
	switch v.(type) {
	case nil, missingT, bson.Undefined, bson.Null:
		return true
	}
	return false
}

func evalOp(op string, arg any, doc bson.D, vars map[string]any) (any, error) {
	switch op {
	case "$literal":
		return arg, nil
	case "$cond":
		var c, t, f any
		if d, ok := arg.(bson.D); ok {
			c, t, f = lookup(d, "if"), lookup(d, "then"), lookup(d, "else")
		} else if a, ok := arg.(bson.A); ok && len(a) == 3 {
			c, t, f = a[0], a[1], a[2]
		} else {
			return nil, badValue("$cond needs if, then and else")
		}
		cv, err := evalExpr(c, doc, vars)
		if err != nil {
			return nil, err
		}
		if truthy(cv) {
			return evalExpr(t, doc, vars)
		}
		return evalExpr(f, doc, vars)
	case "$switch":
		d, _ := arg.(bson.D)
		branches, _ := lookup(d, "branches").(bson.A)
		for _, b := range branches {
			bd, _ := b.(bson.D)
			cv, err := evalExpr(lookup(bd, "case"), doc, vars)
			if err != nil {
				return nil, err
			}
			if truthy(cv) {
				return evalExpr(lookup(bd, "then"), doc, vars)
			}
		}
		if def, ok := lookupE(d, "default"); ok {
			return evalExpr(def, doc, vars)
		}
		return nil, merr(40066, "Location40066", "$switch could not find a matching branch for an input, and no default was specified.")
	case "$let":
		d, _ := arg.(bson.D)
		vd, _ := lookup(d, "vars").(bson.D)
		nv := map[string]any{}
		for k, v := range vars {
			nv[k] = v
		}
		for _, e := range vd {
			v, err := evalExpr(e.Value, doc, vars)
			if err != nil {
				return nil, err
			}
			nv[e.Key] = v
		}
		return evalExpr(lookup(d, "in"), doc, nv)
	case "$filter", "$map":
		d, _ := arg.(bson.D)
		in, err := evalExpr(lookup(d, "input"), doc, vars)
		if err != nil {
			return nil, err
		}
		if nullish(in) {
			return nil, nil
		}
		arr, ok := in.(bson.A)
		if !ok {
			return nil, badValue("input to %s must be an array not %s", op, bsonTypeName(in))
		}
		as, _ := lookup(d, "as").(string)
		if as == "" {
			as = "this"
		}
		body := lookup(d, "cond")
		if op == "$map" {
			body = lookup(d, "in")
		}
		out := bson.A{}
		for _, e := range arr {
			nv := map[string]any{as: e}
			for k, v := range vars {
				if k != as {
					nv[k] = v
				}
			}
			r, err := evalExpr(body, doc, nv)
			if err != nil {
				return nil, err
			}
			if op == "$map" {
				if _, m := r.(missingT); m {
					r = nil
				}
				out = append(out, r)
			} else if truthy(r) {
				out = append(out, e)
			}
		}
		return out, nil
	case "$reduce":
		d, _ := arg.(bson.D)
		in, err := evalExpr(lookup(d, "input"), doc, vars)
		if err != nil {
			return nil, err
		}
		acc, err := evalExpr(lookup(d, "initialValue"), doc, vars)
		if err != nil {
			return nil, err
		}
		arr, _ := in.(bson.A)
		for _, e := range arr {
			nv := map[string]any{"this": e, "value": acc}
			for k, v := range vars {
				if _, ok := nv[k]; !ok {
					nv[k] = v
				}
			}
			if acc, err = evalExpr(lookup(d, "in"), doc, nv); err != nil {
				return nil, err
			}
		}
		return acc, nil
	}
	a, err := args(arg, doc, vars)
	if err != nil {
		return nil, err
	}
	need := func(n int) error {
		if len(a) != n {
			return merr(16020, "Location16020", "Expression %s takes exactly %d arguments. %d were passed in.", op, n, len(a))
		}
		return nil
	}
	switch op {
	case "$add":
		var s any = int32(0)
		var date *bson.DateTime
		for _, v := range a {
			if nullish(v) {
				return nil, nil
			}
			if dt, ok := v.(bson.DateTime); ok {
				date = &dt
				continue
			}
			if !isNumber(v) {
				return nil, merr(16554, "TypeMismatch", "$add only supports numeric or date types, not %s", bsonTypeName(v))
			}
			s = addNumbers(s, v)
		}
		if date != nil {
			f, _ := numberOf(s)
			return bson.DateTime(int64(*date) + int64(math.Round(f))), nil
		}
		return s, nil
	case "$subtract":
		if err := need(2); err != nil {
			return nil, err
		}
		if nullish(a[0]) || nullish(a[1]) {
			return nil, nil
		}
		if x, ok := a[0].(bson.DateTime); ok {
			if y, ok := a[1].(bson.DateTime); ok {
				return int64(x) - int64(y), nil
			}
			f, _ := numberOf(a[1])
			return bson.DateTime(int64(x) - int64(f)), nil
		}
		if !isNumber(a[0]) || !isNumber(a[1]) {
			return nil, merr(16556, "TypeMismatch", "can't $subtract %s from %s", bsonTypeName(a[1]), bsonTypeName(a[0]))
		}
		return addNumbers(a[0], negate(a[1])), nil
	case "$multiply":
		var p any = int32(1)
		for _, v := range a {
			if nullish(v) {
				return nil, nil
			}
			if !isNumber(v) {
				return nil, merr(16555, "TypeMismatch", "$multiply only supports numeric types, not %s", bsonTypeName(v))
			}
			p = mulNumbers(p, v)
		}
		return p, nil
	case "$divide", "$mod", "$pow", "$log":
		if err := need(2); err != nil {
			return nil, err
		}
		if nullish(a[0]) || nullish(a[1]) {
			return nil, nil
		}
		x, ok1 := numberOf(a[0])
		y, ok2 := numberOf(a[1])
		if !ok1 || !ok2 {
			return nil, merr(16609, "TypeMismatch", "%s only supports numeric types, not %s and %s", op, bsonTypeName(a[0]), bsonTypeName(a[1]))
		}
		switch op {
		case "$divide":
			if y == 0 {
				return nil, merr(16608, "Location16608", "can't $divide by zero")
			}
			return x / y, nil
		case "$mod":
			if y == 0 {
				return nil, merr(16610, "Location16610", "can't $mod by zero")
			}
			if xi, ok := intOf(a[0]); ok {
				if yi, ok := intOf(a[1]); ok {
					r := xi % yi
					if _, ok := a[0].(int32); ok {
						if _, ok := a[1].(int32); ok {
							return int32(r), nil
						}
					}
					return r, nil
				}
			}
			return math.Mod(x, y), nil
		case "$pow":
			r := math.Pow(x, y)
			if isIntegral(a[0]) && isIntegral(a[1]) && y >= 0 && math.Abs(r) < 1<<53 {
				if _, ok := a[0].(int32); ok && r >= math.MinInt32 && r <= math.MaxInt32 {
					return int32(r), nil
				}
				return int64(r), nil
			}
			return r, nil
		}
		return math.Log(x) / math.Log(y), nil
	case "$abs", "$ceil", "$floor", "$sqrt", "$exp", "$ln", "$log10", "$trunc", "$round":
		if len(a) == 0 || nullish(a[0]) {
			return nil, nil
		}
		x, ok := numberOf(a[0])
		if !ok {
			return nil, merr(28765, "TypeMismatch", "%s only supports numeric types, not %s", op, bsonTypeName(a[0]))
		}
		switch op {
		case "$abs":
			if i, ok := intOf(a[0]); ok {
				if i < 0 {
					i = -i
				}
				if _, ok := a[0].(int32); ok {
					return int32(i), nil
				}
				return i, nil
			}
			return math.Abs(x), nil
		case "$ceil", "$floor", "$trunc", "$round":
			if isIntegral(a[0]) {
				return a[0], nil
			}
			places := 0.0
			if len(a) > 1 {
				places, _ = numberOf(a[1])
			}
			m := math.Pow(10, places)
			switch op {
			case "$ceil":
				return math.Ceil(x), nil
			case "$floor":
				return math.Floor(x), nil
			case "$trunc":
				return math.Trunc(x*m) / m, nil
			}
			return math.RoundToEven(x*m) / m, nil
		case "$sqrt":
			return math.Sqrt(x), nil
		case "$exp":
			return math.Exp(x), nil
		case "$ln":
			return math.Log(x), nil
		}
		return math.Log10(x), nil
	case "$eq", "$ne", "$gt", "$gte", "$lt", "$lte", "$cmp":
		if err := need(2); err != nil {
			return nil, err
		}
		c := compareValues(a[0], a[1])
		switch op {
		case "$eq":
			return c == 0, nil
		case "$ne":
			return c != 0, nil
		case "$gt":
			return c > 0, nil
		case "$gte":
			return c >= 0, nil
		case "$lt":
			return c < 0, nil
		case "$lte":
			return c <= 0, nil
		}
		return int32(c), nil
	case "$and":
		for _, v := range a {
			if !truthy(v) {
				return false, nil
			}
		}
		return true, nil
	case "$or":
		for _, v := range a {
			if truthy(v) {
				return true, nil
			}
		}
		return false, nil
	case "$not":
		if err := need(1); err != nil {
			return nil, err
		}
		return !truthy(a[0]), nil
	case "$ifNull":
		for _, v := range a {
			if !nullish(v) {
				return v, nil
			}
		}
		return a[len(a)-1], nil
	case "$concat":
		var sb strings.Builder
		for _, v := range a {
			if nullish(v) {
				return nil, nil
			}
			s, ok := v.(string)
			if !ok {
				return nil, merr(16702, "TypeMismatch", "$concat only supports strings, not %s", bsonTypeName(v))
			}
			sb.WriteString(s)
		}
		return sb.String(), nil
	case "$toLower", "$toUpper":
		if len(a) == 0 || nullish(a[0]) {
			return "", nil
		}
		s := fmt.Sprint(a[0])
		if op == "$toLower" {
			return strings.ToLower(s), nil
		}
		return strings.ToUpper(s), nil
	case "$strLenCP", "$strLenBytes":
		s, ok := a[0].(string)
		if !ok {
			return nil, merr(34471, "Location34471", "%s requires a string argument, found: %s", op, bsonTypeName(a[0]))
		}
		if op == "$strLenBytes" {
			return int32(len(s)), nil
		}
		return int32(len([]rune(s))), nil
	case "$substr", "$substrBytes", "$substrCP":
		if err := need(3); err != nil {
			return nil, err
		}
		s, _ := a[0].(string)
		st, _ := numberOf(a[1])
		n, _ := numberOf(a[2])
		if op == "$substrCP" {
			r := []rune(s)
			b, e := clamp(int(st), len(r)), clamp(int(st)+int(n), len(r))
			if n < 0 {
				e = len(r)
			}
			return string(r[b:e]), nil
		}
		b, e := clamp(int(st), len(s)), clamp(int(st)+int(n), len(s))
		if n < 0 {
			e = len(s)
		}
		return s[b:e], nil
	case "$split":
		if err := need(2); err != nil {
			return nil, err
		}
		if nullish(a[0]) {
			return nil, nil
		}
		s, _ := a[0].(string)
		sep, _ := a[1].(string)
		out := bson.A{}
		for _, p := range strings.Split(s, sep) {
			out = append(out, p)
		}
		return out, nil
	case "$size":
		arr, ok := a[0].(bson.A)
		if !ok {
			return nil, merr(17124, "Location17124", "The argument to $size must be an array. Type of argument: %s", bsonTypeName(a[0]))
		}
		return int32(len(arr)), nil
	case "$isArray":
		_, ok := a[0].(bson.A)
		return ok, nil
	case "$arrayElemAt":
		if err := need(2); err != nil {
			return nil, err
		}
		arr, ok := a[0].(bson.A)
		if !ok {
			return nil, nil
		}
		i, _ := numberOf(a[1])
		k := int(i)
		if k < 0 {
			k += len(arr)
		}
		if k < 0 || k >= len(arr) {
			return missingT{}, nil
		}
		return arr[k], nil
	case "$first", "$last":
		arr, ok := a[0].(bson.A)
		if !ok || len(arr) == 0 {
			return missingT{}, nil
		}
		if op == "$first" {
			return arr[0], nil
		}
		return arr[len(arr)-1], nil
	case "$in":
		if err := need(2); err != nil {
			return nil, err
		}
		arr, ok := a[1].(bson.A)
		if !ok {
			return nil, merr(40081, "Location40081", "$in requires an array as a second argument, found: %s", bsonTypeName(a[1]))
		}
		for _, e := range arr {
			if valuesEqual(e, a[0]) {
				return true, nil
			}
		}
		return false, nil
	case "$concatArrays":
		out := bson.A{}
		for _, v := range a {
			if nullish(v) {
				return nil, nil
			}
			arr, ok := v.(bson.A)
			if !ok {
				return nil, merr(28664, "Location28664", "$concatArrays only supports arrays, not %s", bsonTypeName(v))
			}
			out = append(out, arr...)
		}
		return out, nil
	case "$reverseArray":
		arr, ok := a[0].(bson.A)
		if !ok {
			return nil, nil
		}
		out := make(bson.A, len(arr))
		for i, e := range arr {
			out[len(arr)-1-i] = e
		}
		return out, nil
	case "$slice":
		arr, ok := a[0].(bson.A)
		if !ok {
			return nil, nil
		}
		if len(a) == 3 {
			return sliceArray(arr, bson.A{a[1], a[2]})
		}
		return sliceArray(arr, a[1])
	case "$range":
		st, _ := numberOf(a[0])
		en, _ := numberOf(a[1])
		step := 1.0
		if len(a) > 2 {
			step, _ = numberOf(a[2])
		}
		if step == 0 {
			return nil, merr(34449, "Location34449", "$range requires a non-zero step value")
		}
		out := bson.A{}
		for i := st; (step > 0 && i < en) || (step < 0 && i > en); i += step {
			out = append(out, int32(i))
		}
		return out, nil
	case "$sum", "$avg", "$min", "$max":
		vals := a
		if len(a) == 1 {
			if arr, ok := a[0].(bson.A); ok {
				vals = arr
			}
		}
		return accumulate(op, vals), nil
	case "$mergeObjects":
		return accumulate(op, a), nil
	case "$objectToArray":
		d, ok := a[0].(bson.D)
		if !ok {
			return nil, nil
		}
		out := bson.A{}
		for _, e := range d {
			out = append(out, bson.D{{Key: "k", Value: e.Key}, {Key: "v", Value: e.Value}})
		}
		return out, nil
	case "$arrayToObject":
		arr, ok := a[0].(bson.A)
		if !ok {
			return nil, nil
		}
		out := bson.D{}
		for _, e := range arr {
			switch x := e.(type) {
			case bson.D:
				k, _ := lookup(x, "k").(string)
				out = setKey(out, k, lookup(x, "v"))
			case bson.A:
				if len(x) == 2 {
					k, _ := x[0].(string)
					out = setKey(out, k, x[1])
				}
			}
		}
		return out, nil
	case "$type":
		if len(a) == 0 {
			return "missing", nil
		}
		v, err := evalExpr(arg, doc, vars)
		if err != nil {
			return nil, err
		}
		return bsonTypeName(v), nil
	case "$toString":
		if nullish(a[0]) {
			return nil, nil
		}
		switch x := a[0].(type) {
		case string:
			return x, nil
		case bson.ObjectID:
			return x.Hex(), nil
		case bson.DateTime:
			return x.Time().UTC().Format("2006-01-02T15:04:05.000Z"), nil
		}
		return fmt.Sprint(a[0]), nil
	case "$toInt", "$toLong", "$toDouble":
		if nullish(a[0]) {
			return nil, nil
		}
		f, ok := numberOf(a[0])
		if b, isB := a[0].(bool); isB {
			f, ok = 0, true
			if b {
				f = 1
			}
		}
		if !ok {
			return nil, merr(241, "ConversionFailure", "Unsupported conversion from %s to %s in $convert with no onError value", bsonTypeName(a[0]), strings.TrimPrefix(op, "$to"))
		}
		switch op {
		case "$toInt":
			return int32(f), nil
		case "$toLong":
			return int64(f), nil
		}
		return f, nil
	}
	return nil, exprErr(op)
}

func clamp(i, n int) int {
	if i < 0 {
		return 0
	}
	if i > n {
		return n
	}
	return i
}

func negate(v any) any {
	switch x := v.(type) {
	case int32:
		if x != math.MinInt32 {
			return -x
		}
		return -int64(x)
	case int64:
		return -x
	}
	f, _ := numberOf(v)
	return -f
}
