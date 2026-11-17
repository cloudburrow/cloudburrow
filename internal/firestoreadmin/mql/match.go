package mql

// Firestore with MongoDB compatibility (#1703): the MongoDB Query
// Language's comparison order, query filters, sorts and projections,
// evaluated over a collection's documents.

import (
	"bytes"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// missingT marks a path that names no value.
type missingT struct{}

// lookupE is d's field key.
func lookupE(d bson.D, key string) (any, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

func lookup(d bson.D, key string) any {
	v, _ := lookupE(d, key)
	return v
}

// numberOf is v as a float64 when v is a number.
func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case float64:
		return x, true
	case bson.Decimal128:
		f, _, err := big.ParseFloat(x.String(), 10, 64, big.ToNearestEven)
		if err != nil {
			if x.IsNaN() {
				return math.NaN(), true
			}
			if x.IsInf() > 0 {
				return math.Inf(1), true
			}
			if x.IsInf() < 0 {
				return math.Inf(-1), true
			}
			return 0, false
		}
		r, _ := f.Float64()
		return r, true
	}
	return 0, false
}

func isNumber(v any) bool { _, ok := numberOf(v); return ok }

// typeRank is v's place in MongoDB's comparison order of BSON types.
func typeRank(v any) int {
	switch v.(type) {
	case bson.MinKey:
		return 1
	case nil, bson.Null, missingT, bson.Undefined:
		return 2
	case int32, int64, int, float64, bson.Decimal128:
		return 3
	case string, bson.Symbol:
		return 4
	case bson.D:
		return 5
	case bson.A:
		return 6
	case bson.Binary:
		return 7
	case bson.ObjectID:
		return 8
	case bool:
		return 9
	case bson.DateTime:
		return 10
	case bson.Timestamp:
		return 11
	case bson.Regex:
		return 12
	case bson.MaxKey:
		return 14
	}
	return 13
}

// compareValues orders two BSON values as MongoDB does.
func compareValues(a, b any) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		return cmpInt(ra, rb)
	}
	switch x := a.(type) {
	case int32, int64, int, float64, bson.Decimal128:
		return compareNumbers(a, b)
	case string:
		return strings.Compare(x, stringOf(b))
	case bson.Symbol:
		return strings.Compare(string(x), stringOf(b))
	case bson.D:
		y := b.(bson.D)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := cmpInt(typeRank(x[i].Value), typeRank(y[i].Value)); c != 0 {
				return c
			}
			if c := strings.Compare(x[i].Key, y[i].Key); c != 0 {
				return c
			}
			if c := compareValues(x[i].Value, y[i].Value); c != 0 {
				return c
			}
		}
		return cmpInt(len(x), len(y))
	case bson.A:
		y := b.(bson.A)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := compareValues(x[i], y[i]); c != 0 {
				return c
			}
		}
		return cmpInt(len(x), len(y))
	case bson.Binary:
		y := b.(bson.Binary)
		if c := cmpInt(len(x.Data), len(y.Data)); c != 0 {
			return c
		}
		if c := cmpInt(int(x.Subtype), int(y.Subtype)); c != 0 {
			return c
		}
		return bytes.Compare(x.Data, y.Data)
	case bson.ObjectID:
		y := b.(bson.ObjectID)
		return bytes.Compare(x[:], y[:])
	case bool:
		y := b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		}
		return 1
	case bson.DateTime:
		return cmpInt64(int64(x), int64(b.(bson.DateTime)))
	case bson.Timestamp:
		return x.Compare(b.(bson.Timestamp))
	case bson.Regex:
		y := b.(bson.Regex)
		if c := strings.Compare(x.Pattern, y.Pattern); c != 0 {
			return c
		}
		return strings.Compare(x.Options, y.Options)
	}
	return 0
}

func stringOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bson.Symbol:
		return string(x)
	}
	return ""
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func intOf(v any) (int64, bool) {
	switch x := v.(type) {
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	}
	return 0, false
}

func compareNumbers(a, b any) int {
	if x, ok := intOf(a); ok {
		if y, ok := intOf(b); ok {
			return cmpInt64(x, y)
		}
	}
	x, _ := numberOf(a)
	y, _ := numberOf(b)
	switch {
	case math.IsNaN(x) && math.IsNaN(y):
		return 0
	case math.IsNaN(x):
		return -1
	case math.IsNaN(y):
		return 1
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// valuesEqual is MongoDB's equality of two values.
func valuesEqual(a, b any) bool {
	return typeRank(a) == typeRank(b) && compareValues(a, b) == 0
}

// pathValues is every value path names in v, arrays of documents
// traversed as MongoDB does; a branch that names nothing is missingT.
func pathValues(v any, parts []string) []any {
	if len(parts) == 0 {
		return []any{v}
	}
	switch x := v.(type) {
	case bson.D:
		f, ok := lookupE(x, parts[0])
		if !ok {
			return []any{missingT{}}
		}
		return pathValues(f, parts[1:])
	case bson.A:
		var out []any
		if i, err := strconv.Atoi(parts[0]); err == nil && i >= 0 {
			if i < len(x) {
				out = append(out, pathValues(x[i], parts[1:])...)
			}
			return out
		}
		for _, e := range x {
			if d, ok := e.(bson.D); ok {
				out = append(out, pathValues(d, parts)...)
			}
		}
		if len(out) == 0 {
			out = append(out, missingT{})
		}
		return out
	}
	return []any{missingT{}}
}

// getPath is the single value a dotted path names in d, as an
// aggregation expression or a sort reads it.
func getPath(v any, path string) (any, bool) {
	cur := v
	for _, p := range strings.Split(path, ".") {
		switch x := cur.(type) {
		case bson.D:
			f, ok := lookupE(x, p)
			if !ok {
				return nil, false
			}
			cur = f
		case bson.A:
			if i, err := strconv.Atoi(p); err == nil {
				if i < 0 || i >= len(x) {
					return nil, false
				}
				cur = x[i]
				continue
			}
			var out bson.A
			for _, e := range x {
				if g, ok := getPath(e, p); ok {
					out = append(out, g)
				}
			}
			cur = out
		default:
			return nil, false
		}
	}
	return cur, true
}

// matches reports whether doc matches the query filter.
func matches(doc bson.D, filter bson.D) (bool, error) {
	for _, e := range filter {
		ok, err := matchTop(doc, e)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func matchTop(doc bson.D, e bson.E) (bool, error) {
	switch e.Key {
	case "$and", "$or", "$nor":
		arr, ok := e.Value.(bson.A)
		if !ok || len(arr) == 0 {
			return false, badValue("%s must be a nonempty array", e.Key)
		}
		for _, c := range arr {
			cd, ok := c.(bson.D)
			if !ok {
				return false, badValue("$or/$and/$nor entries need to be full objects")
			}
			m, err := matches(doc, cd)
			if err != nil {
				return false, err
			}
			switch {
			case e.Key == "$and" && !m:
				return false, nil
			case e.Key == "$or" && m:
				return true, nil
			case e.Key == "$nor" && m:
				return false, nil
			}
		}
		return e.Key != "$or", nil
	case "$expr":
		v, err := evalExpr(e.Value, doc, nil)
		if err != nil {
			return false, err
		}
		return truthy(v), nil
	case "$comment":
		return false, badValue("unknown top level operator: $comment. The $comment operator is not supported.")
	case "$text", "$where", "$jsonSchema":
		return false, badValue("unknown top level operator: %s. The %s operator is not supported.", e.Key, e.Key)
	}
	if strings.HasPrefix(e.Key, "$") {
		return false, badValue("unknown top level operator: %s", e.Key)
	}
	return matchField(pathValues(doc, strings.Split(e.Key, ".")), e.Value)
}

// isOperatorDoc reports whether v is a document of query operators.
func isOperatorDoc(v any) (bson.D, bool) {
	d, ok := v.(bson.D)
	if !ok || len(d) == 0 || !strings.HasPrefix(d[0].Key, "$") {
		return nil, false
	}
	return d, true
}

func matchField(cands []any, cond any) (bool, error) {
	if ops, ok := isOperatorDoc(cond); ok {
		var opts string
		if o, ok := lookupE(ops, "$options"); ok {
			opts, _ = o.(string)
		}
		for _, op := range ops {
			if op.Key == "$options" {
				continue
			}
			m, err := matchOp(cands, op.Key, op.Value, opts)
			if err != nil || !m {
				return false, err
			}
		}
		return true, nil
	}
	if re, ok := cond.(bson.Regex); ok {
		return matchOp(cands, "$regex", re, "")
	}
	return matchOp(cands, "$eq", cond, "")
}

// anyValue reports whether pred holds for a candidate or, a candidate
// being an array, for one of its elements.
func anyValue(cands []any, pred func(any) bool) bool {
	for _, c := range cands {
		if pred(c) {
			return true
		}
		if a, ok := c.(bson.A); ok {
			for _, e := range a {
				if pred(e) {
					return true
				}
			}
		}
	}
	return false
}

func eqMatch(cands []any, v any) bool {
	if v == nil {
		for _, c := range cands {
			if _, ok := c.(missingT); ok {
				return true
			}
		}
	}
	return anyValue(cands, func(c any) bool {
		if _, ok := c.(missingT); ok {
			return false
		}
		return valuesEqual(c, v)
	})
}

func inMatch(cands []any, v any) (bool, error) {
	arr, ok := v.(bson.A)
	if !ok {
		return false, badValue("$in needs an array")
	}
	for _, x := range arr {
		if re, ok := x.(bson.Regex); ok {
			r, err := compileRegex(re.Pattern, re.Options)
			if err != nil {
				return false, err
			}
			if anyValue(cands, func(c any) bool { s, ok := c.(string); return ok && r.MatchString(s) }) {
				return true, nil
			}
			continue
		}
		if eqMatch(cands, x) {
			return true, nil
		}
	}
	return false, nil
}

func compileRegex(pattern, options string) (*regexp.Regexp, error) {
	flags := ""
	for _, o := range options {
		switch o {
		case 'i', 'm', 's':
			flags += string(o)
		case 'u':
		case 'x':
			return nil, badValue("The x regular expression option is not supported by CloudBurrow.")
		default:
			return nil, badValue("invalid flag in regex options: %c", o)
		}
	}
	if flags != "" {
		pattern = "(?" + flags + ")" + pattern
	}
	r, err := regexp.Compile(pattern)
	if err != nil {
		return nil, merr(51091, "Location51091", "Regular expression is invalid: %v", err)
	}
	return r, nil
}

var typeAliases = map[string]int{"double": 1, "string": 2, "object": 3, "array": 4, "binData": 5, "undefined": 6, "objectId": 7,
	"bool": 8, "date": 9, "null": 10, "regex": 11, "dbPointer": 12, "javascript": 13, "symbol": 14, "javascriptWithScope": 15,
	"int": 16, "timestamp": 17, "long": 18, "decimal": 19, "minKey": -1, "maxKey": 127}

func typeMatches(v any, t any) (bool, error) {
	if s, ok := t.(string); ok {
		if s == "number" {
			return isNumber(v), nil
		}
		n, ok := typeAliases[s]
		if !ok {
			return false, badValue("Unknown type name alias: %s", s)
		}
		return typeAliases[bsonTypeName(v)] == n, nil
	}
	n, ok := numberOf(t)
	if !ok {
		return false, badValue("type must be represented as a number or a string")
	}
	return typeAliases[bsonTypeName(v)] == int(n), nil
}

func matchOp(cands []any, op string, v any, opts string) (bool, error) {
	switch op {
	case "$eq":
		return eqMatch(cands, v), nil
	case "$ne":
		return !eqMatch(cands, v), nil
	case "$gt", "$gte", "$lt", "$lte":
		return anyValue(cands, func(c any) bool {
			if _, ok := c.(missingT); ok {
				if v != nil {
					return false
				}
				c = nil
			}
			// Comparisons hold within a type bracket alone.
			if typeRank(c) != typeRank(v) {
				_, minK := v.(bson.MinKey)
				_, maxK := v.(bson.MaxKey)
				if !minK && !maxK {
					return false
				}
			}
			r := compareValues(c, v)
			switch op {
			case "$gt":
				return r > 0
			case "$gte":
				return r >= 0
			case "$lt":
				return r < 0
			}
			return r <= 0
		}), nil
	case "$in":
		return inMatch(cands, v)
	case "$nin":
		m, err := inMatch(cands, v)
		return !m, err
	case "$exists":
		want := truthy(v)
		has := false
		for _, c := range cands {
			if _, ok := c.(missingT); !ok {
				has = true
			}
		}
		return has == want, nil
	case "$type":
		var types bson.A
		if a, ok := v.(bson.A); ok {
			types = a
		} else {
			types = bson.A{v}
		}
		var ferr error
		m := false
		for _, c := range cands {
			if _, ok := c.(missingT); ok {
				continue
			}
			for _, t := range types {
				if s, _ := t.(string); s == "array" {
					if _, ok := c.(bson.A); ok {
						m = true
					}
					continue
				}
				vals := []any{c}
				if a, ok := c.(bson.A); ok {
					vals = a
				}
				for _, x := range vals {
					ok, err := typeMatches(x, t)
					if err != nil {
						ferr = err
					}
					m = m || ok
				}
			}
		}
		return m, ferr
	case "$size":
		n, ok := numberOf(v)
		if !ok || n != math.Trunc(n) || n < 0 {
			return false, badValue("Failed to parse $size. Expected a non-negative whole number")
		}
		for _, c := range cands {
			if a, ok := c.(bson.A); ok && len(a) == int(n) {
				return true, nil
			}
		}
		return false, nil
	case "$all":
		arr, ok := v.(bson.A)
		if !ok {
			return false, badValue("$all needs an array")
		}
		if len(arr) == 0 {
			return false, nil
		}
		for _, x := range arr {
			if em, ok := x.(bson.D); ok && len(em) == 1 && em[0].Key == "$elemMatch" {
				m, err := matchOp(cands, "$elemMatch", em[0].Value, "")
				if err != nil || !m {
					return false, err
				}
				continue
			}
			if !eqMatch(cands, x) {
				return false, nil
			}
		}
		return true, nil
	case "$elemMatch":
		cond, ok := v.(bson.D)
		if !ok {
			return false, badValue("$elemMatch needs an Object")
		}
		_, opDoc := isOperatorDoc(cond)
		for _, c := range cands {
			a, ok := c.(bson.A)
			if !ok {
				continue
			}
			for _, e := range a {
				var m bool
				var err error
				if opDoc && !strings.HasPrefix(cond[0].Key, "$and") && !strings.HasPrefix(cond[0].Key, "$or") && !strings.HasPrefix(cond[0].Key, "$nor") {
					m, err = matchField([]any{e}, cond)
				} else if d, ok := e.(bson.D); ok {
					m, err = matches(d, cond)
				}
				if err != nil {
					return false, err
				}
				if m {
					return true, nil
				}
			}
		}
		return false, nil
	case "$mod":
		arr, ok := v.(bson.A)
		if !ok || len(arr) != 2 {
			return false, badValue("malformed mod, needs to be an array of two numbers")
		}
		d, ok1 := numberOf(arr[0])
		r, ok2 := numberOf(arr[1])
		if !ok1 || !ok2 || math.Trunc(d) == 0 {
			return false, badValue("divisor cannot be 0")
		}
		return anyValue(cands, func(c any) bool {
			f, ok := numberOf(c)
			return ok && int64(math.Trunc(f))%int64(math.Trunc(d)) == int64(math.Trunc(r))
		}), nil
	case "$regex":
		var pattern string
		switch x := v.(type) {
		case string:
			pattern = x
		case bson.Regex:
			pattern = x.Pattern
			if opts == "" {
				opts = x.Options
			}
		default:
			return false, badValue("$regex has to be a string")
		}
		r, err := compileRegex(pattern, opts)
		if err != nil {
			return false, err
		}
		return anyValue(cands, func(c any) bool {
			switch s := c.(type) {
			case string:
				return r.MatchString(s)
			case bson.Symbol:
				return r.MatchString(string(s))
			}
			return false
		}), nil
	case "$not":
		switch x := v.(type) {
		case bson.D:
			if _, ok := isOperatorDoc(x); !ok {
				return false, badValue("$not needs a regex or a document")
			}
			m, err := matchField(cands, x)
			return !m, err
		case bson.Regex:
			m, err := matchOp(cands, "$regex", x, "")
			return !m, err
		}
		return false, badValue("$not needs a regex or a document")
	case "$bitsAllClear", "$bitsAllSet", "$bitsAnyClear", "$bitsAnySet", "$jsonSchema", "$where", "$text", "$comment",
		"$geoWithin", "$geoIntersects", "$near", "$nearSphere":
		return false, badValue("unknown operator: %s. The %s operator is not supported.", op, op)
	}
	return false, badValue("unknown operator: %s", op)
}

// truthy is MongoDB's boolean value of v.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil, missingT, bson.Undefined, bson.Null:
		return false
	case bool:
		return x
	}
	if f, ok := numberOf(v); ok {
		return f != 0
	}
	return true
}

// sortKey is the value a document sorts by on path: an array sorts by its
// least element ascending and its greatest descending.
func sortKey(d bson.D, path string, desc bool) any {
	vals := pathValues(d, strings.Split(path, "."))
	var best any = missingT{}
	first := true
	consider := func(v any) {
		if first {
			best, first = v, false
			return
		}
		c := compareValues(v, best)
		if (!desc && c < 0) || (desc && c > 0) {
			best = v
		}
	}
	for _, v := range vals {
		if a, ok := v.(bson.A); ok {
			if len(a) == 0 {
				consider(bson.Undefined{})
			}
			for _, e := range a {
				consider(e)
			}
			continue
		}
		consider(v)
	}
	return best
}

// sortSpec is a parsed sort document.
type sortSpec []struct {
	path string
	desc bool
}

func parseSort(v any) (sortSpec, error) {
	if v == nil {
		return nil, nil
	}
	d, ok := v.(bson.D)
	if !ok {
		return nil, badValue("sort must be an object")
	}
	var out sortSpec
	for _, e := range d {
		if _, ok := e.Value.(bson.D); ok {
			return nil, badValue("$meta sorts are not supported by CloudBurrow: %s", e.Key)
		}
		n, ok := numberOf(e.Value)
		if !ok || (n != 1 && n != -1) {
			return nil, badValue("$sort key ordering must be 1 (for ascending) or -1 (for descending)")
		}
		out = append(out, struct {
			path string
			desc bool
		}{e.Key, n < 0})
	}
	return out, nil
}

func (s sortSpec) less(a, b bson.D) bool {
	for _, k := range s {
		c := compareValues(sortKey(a, k.path, k.desc), sortKey(b, k.path, k.desc))
		if k.desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
	}
	return false
}

// project applies a find projection to doc.
func project(doc bson.D, proj bson.D) (bson.D, error) {
	if len(proj) == 0 {
		return doc, nil
	}
	include := -1
	idIncluded := true
	var exprs bson.D
	for _, e := range proj {
		if e.Key == "_id" {
			if d, ok := e.Value.(bson.D); ok && len(d) > 0 {
				exprs = append(exprs, e)
				continue
			}
			idIncluded = truthy(e.Value)
			continue
		}
		switch x := e.Value.(type) {
		case bson.D:
			if len(x) == 1 && (x[0].Key == "$slice" || x[0].Key == "$elemMatch") {
				exprs = append(exprs, e)
				if include == -1 && x[0].Key == "$elemMatch" {
					include = 1
				}
				continue
			}
			exprs = append(exprs, e)
			include = 1
			continue
		case string:
			exprs = append(exprs, e)
			include = 1
			continue
		}
		if strings.HasSuffix(e.Key, ".$") {
			return nil, badValue("The positional projection operator is not supported by CloudBurrow: %s", e.Key)
		}
		inc := 0
		if truthy(e.Value) {
			inc = 1
		}
		if include != -1 && include != inc {
			return nil, merr(31254, "Location31254", "Cannot do exclusion on field %s in inclusion projection", e.Key)
		}
		include = inc
	}
	var out bson.D
	if include == 1 {
		id, hasID := lookupE(doc, "_id")
		if idIncluded && hasID {
			out = bson.D{{Key: "_id", Value: id}}
		}
		for _, e := range proj {
			if e.Key == "_id" || !truthy(e.Value) {
				continue
			}
			if _, isDoc := e.Value.(bson.D); isDoc {
				continue
			}
			if _, isStr := e.Value.(string); isStr {
				continue
			}
			out = includePath(out, doc, strings.Split(e.Key, "."))
		}
	} else {
		out = cloneDoc(doc)
		for _, e := range proj {
			if e.Key == "_id" {
				if !idIncluded {
					out = removeKey(out, "_id")
				}
				continue
			}
			if _, isDoc := e.Value.(bson.D); isDoc {
				continue
			}
			unsetPath(&out, strings.Split(e.Key, "."))
		}
	}
	for _, e := range exprs {
		x, _ := e.Value.(bson.D)
		if len(x) == 1 && x[0].Key == "$slice" {
			v, ok := getPath(doc, e.Key)
			a, isArr := v.(bson.A)
			if ok && isArr {
				s, err := sliceArray(a, x[0].Value)
				if err != nil {
					return nil, err
				}
				if include != 1 {
					out = removeKey(out, e.Key)
				}
				_ = setPath(&out, strings.Split(e.Key, "."), s)
			}
			continue
		}
		if len(x) == 1 && x[0].Key == "$elemMatch" {
			v, _ := getPath(doc, e.Key)
			a, _ := v.(bson.A)
			for _, el := range a {
				if m, _ := matchOp([]any{bson.A{el}}, "$elemMatch", x[0].Value, ""); m {
					out = setKey(out, e.Key, bson.A{el})
					break
				}
			}
			continue
		}
		v, err := evalExpr(e.Value, doc, nil)
		if err != nil {
			return nil, err
		}
		out = setKey(out, e.Key, v)
	}
	return out, nil
}

func sliceArray(a bson.A, spec any) (bson.A, error) {
	if arr, ok := spec.(bson.A); ok && len(arr) == 2 {
		skip, _ := numberOf(arr[0])
		n, _ := numberOf(arr[1])
		s := int(skip)
		if s < 0 {
			s = len(a) + s
			if s < 0 {
				s = 0
			}
		}
		if s > len(a) {
			s = len(a)
		}
		e := s + int(n)
		if e > len(a) {
			e = len(a)
		}
		return append(bson.A{}, a[s:e]...), nil
	}
	n, ok := numberOf(spec)
	if !ok {
		return nil, badValue("$slice only supports numbers and [skip, limit] arrays")
	}
	k := int(n)
	if k >= 0 {
		if k > len(a) {
			k = len(a)
		}
		return append(bson.A{}, a[:k]...), nil
	}
	if -k > len(a) {
		k = -len(a)
	}
	return append(bson.A{}, a[len(a)+k:]...), nil
}

// includePath copies the value at parts of src into dst.
func includePath(dst bson.D, src any, parts []string) bson.D {
	sd, ok := src.(bson.D)
	if !ok {
		return dst
	}
	v, ok := lookupE(sd, parts[0])
	if !ok {
		return dst
	}
	if len(parts) == 1 {
		return setKey(dst, parts[0], v)
	}
	switch x := v.(type) {
	case bson.D:
		sub, _ := lookup(dst, parts[0]).(bson.D)
		return setKey(dst, parts[0], includePath(sub, x, parts[1:]))
	case bson.A:
		existing, _ := lookup(dst, parts[0]).(bson.A)
		var out bson.A
		for i, e := range x {
			if _, isDoc := e.(bson.D); !isDoc {
				continue
			}
			var prev bson.D
			if i < len(existing) {
				prev, _ = existing[i].(bson.D)
			}
			out = append(out, includePath(prev, e, parts[1:]))
		}
		if out == nil {
			out = bson.A{}
		}
		return setKey(dst, parts[0], out)
	}
	return dst
}

func setKey(d bson.D, key string, v any) bson.D {
	for i := range d {
		if d[i].Key == key {
			d[i].Value = v
			return d
		}
	}
	return append(d, bson.E{Key: key, Value: v})
}

func removeKey(d bson.D, key string) bson.D {
	out := d[:0:0]
	for _, e := range d {
		if e.Key != key {
			out = append(out, e)
		}
	}
	return out
}

// cloneValue is a deep copy of a BSON value.
func cloneValue(v any) any {
	switch x := v.(type) {
	case bson.D:
		return cloneDoc(x)
	case bson.A:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = cloneValue(e)
		}
		return out
	}
	return v
}

func cloneDoc(d bson.D) bson.D {
	out := make(bson.D, len(d))
	for i, e := range d {
		out[i] = bson.E{Key: e.Key, Value: cloneValue(e.Value)}
	}
	return out
}

// setPath sets the value at a dotted path, creating documents on the way.
func setPath(d *bson.D, parts []string, v any) error {
	if len(parts) == 1 {
		*d = setKey(*d, parts[0], v)
		return nil
	}
	cur, ok := lookupE(*d, parts[0])
	if !ok {
		sub := bson.D{}
		if err := setPath(&sub, parts[1:], v); err != nil {
			return err
		}
		*d = setKey(*d, parts[0], sub)
		return nil
	}
	switch x := cur.(type) {
	case bson.D:
		if err := setPath(&x, parts[1:], v); err != nil {
			return err
		}
		*d = setKey(*d, parts[0], x)
		return nil
	case bson.A:
		i, err := strconv.Atoi(parts[1])
		if err != nil || i < 0 {
			return merr(28, "PathNotViable", "Cannot create field '%s' in element {%s: [...]}", parts[1], parts[0])
		}
		for len(x) <= i {
			x = append(x, nil)
		}
		if len(parts) == 2 {
			x[i] = v
		} else {
			el, isDoc := x[i].(bson.D)
			if x[i] == nil {
				el, isDoc = bson.D{}, true
			}
			if !isDoc {
				return merr(28, "PathNotViable", "Cannot create field '%s' in element {%s: %v}", parts[2], parts[1], x[i])
			}
			if err := setPath(&el, parts[2:], v); err != nil {
				return err
			}
			x[i] = el
		}
		*d = setKey(*d, parts[0], x)
		return nil
	}
	return merr(28, "PathNotViable", "Cannot create field '%s' in element {%s: %v}", parts[1], parts[0], cur)
}

// unsetPath removes the value at a dotted path; an array element is set
// to null, as MongoDB's $unset does.
func unsetPath(d *bson.D, parts []string) {
	if len(parts) == 1 {
		*d = removeKey(*d, parts[0])
		return
	}
	cur, ok := lookupE(*d, parts[0])
	if !ok {
		return
	}
	switch x := cur.(type) {
	case bson.D:
		unsetPath(&x, parts[1:])
		*d = setKey(*d, parts[0], x)
	case bson.A:
		i, err := strconv.Atoi(parts[1])
		if err != nil || i < 0 || i >= len(x) {
			return
		}
		if len(parts) == 2 {
			x[i] = nil
		} else if el, ok := x[i].(bson.D); ok {
			unsetPath(&el, parts[2:])
			x[i] = el
		}
	}
}
