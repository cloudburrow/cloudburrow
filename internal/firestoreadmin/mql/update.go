package mql

// Firestore with MongoDB compatibility (#1703): update documents (the
// update operators Google lists as supported, and replacements) and
// upserts.

import (
	"math"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// isOperatorUpdate reports whether u is a document of update operators
// rather than a replacement.
func isOperatorUpdate(u bson.D) bool {
	return len(u) > 0 && strings.HasPrefix(u[0].Key, "$")
}

// applyUpdate is doc with update u applied; inserting says the document
// is an upsert's new one, which $setOnInsert applies to.
func applyUpdate(doc bson.D, u any, inserting bool) (bson.D, error) {
	id, hasID := lookupE(doc, "_id")
	var out bson.D
	switch x := u.(type) {
	case bson.A:
		cur := []bson.D{cloneDoc(doc)}
		for _, st := range x {
			sd, ok := st.(bson.D)
			if !ok || len(sd) != 1 {
				return nil, badValue("Each element of the 'pipeline' array must be an object")
			}
			switch sd[0].Key {
			case "$addFields", "$set", "$project", "$unset", "$replaceRoot", "$replaceWith":
			default:
				return nil, merr(72, "InvalidOptions", "%s is not allowed to be used within an update", sd[0].Key)
			}
			var err error
			if cur, err = runStage(nil, cur, sd[0]); err != nil {
				return nil, err
			}
		}
		out = cur[0]
	case bson.D:
		if !isOperatorUpdate(x) {
			for _, e := range x {
				if strings.HasPrefix(e.Key, "$") {
					return nil, merr(52, "DollarPrefixedFieldName", "The dollar ($) prefixed field '%s' in '%s' is not allowed in the context of an update's replacement document.", e.Key, e.Key)
				}
			}
			out = cloneDoc(x)
			if hasID {
				if nid, ok := lookupE(out, "_id"); ok && !valuesEqual(nid, id) {
					return nil, merr(66, "ImmutableField", "After applying the update, the (immutable) field '_id' was found to have been altered to _id: %v", nid)
				}
				out = append(bson.D{{Key: "_id", Value: id}}, removeKey(out, "_id")...)
			}
			return out, nil
		}
		out = cloneDoc(doc)
		for _, op := range x {
			fields, ok := op.Value.(bson.D)
			if !ok {
				return nil, merr(9, "FailedToParse", "Modifiers operate on fields but we found type %s instead. For example: {$mod: {<field>: ...}} not {%s: %v}", bsonTypeName(op.Value), op.Key, op.Value)
			}
			for _, f := range fields {
				if err := applyOp(&out, op.Key, f.Key, f.Value, inserting); err != nil {
					return nil, err
				}
			}
		}
	default:
		return nil, merr(9, "FailedToParse", "Update argument must be either an object or an array")
	}
	if hasID {
		nid, ok := lookupE(out, "_id")
		if !ok || !valuesEqual(nid, id) {
			return nil, merr(66, "ImmutableField", "Performing an update on the path '_id' would modify the immutable field '_id'")
		}
	}
	return out, nil
}

func applyOp(d *bson.D, op, path string, v any, inserting bool) error {
	if strings.Contains(path, "$") {
		return badValue("The positional operators ($, $[] and $[<identifier>]) are not supported by CloudBurrow: %s", path)
	}
	parts := strings.Split(path, ".")
	cur, exists := getPath(*d, path)
	switch op {
	case "$set":
		return setPath(d, parts, cloneValue(v))
	case "$setOnInsert":
		if inserting {
			return setPath(d, parts, cloneValue(v))
		}
		return nil
	case "$unset":
		unsetPath(d, parts)
		return nil
	case "$inc", "$mul":
		if !isNumber(v) {
			return merr(14, "TypeMismatch", "Cannot %s with non-numeric argument: {%s: %v}", strings.TrimPrefix(op, "$"), path, v)
		}
		if !exists || cur == nil && !exists {
			if op == "$mul" {
				v = mulNumbers(zeroLike(v), v)
			}
			return setPath(d, parts, v)
		}
		if !isNumber(cur) {
			return merr(14, "TypeMismatch", "Cannot apply %s to a value of non-numeric type. {_id: ...} has the field '%s' of non-numeric type %s", op, path, bsonTypeName(cur))
		}
		if op == "$inc" {
			return setPath(d, parts, addNumbers(cur, v))
		}
		return setPath(d, parts, mulNumbers(cur, v))
	case "$min", "$max":
		if !exists {
			return setPath(d, parts, cloneValue(v))
		}
		c := compareValues(v, cur)
		if (op == "$min" && c < 0) || (op == "$max" && c > 0) {
			return setPath(d, parts, cloneValue(v))
		}
		return nil
	case "$rename":
		to, ok := v.(string)
		if !ok || to == "" {
			return badValue("The 'to' field for $rename must be a string: %s: %v", path, v)
		}
		if !exists {
			return nil
		}
		unsetPath(d, parts)
		return setPath(d, strings.Split(to, "."), cur)
	case "$currentDate":
		now := time.Now().UTC()
		var val any = bson.NewDateTimeFromTime(now)
		if td, ok := v.(bson.D); ok {
			switch lookup(td, "$type") {
			case "timestamp":
				val = bson.Timestamp{T: uint32(now.Unix()), I: 1}
			case "date":
			default:
				return badValue("The '$type' string field is required to be 'date' or 'timestamp': {$currentDate: {field : {$type: 'date'}}}")
			}
		} else if _, ok := v.(bool); !ok {
			return badValue("%s is not valid type for $currentDate. Please use a boolean ('true') or a $type expression ({$type: 'timestamp/date'}).", bsonTypeName(v))
		}
		return setPath(d, parts, val)
	case "$push", "$addToSet":
		var arr bson.A
		if exists {
			a, ok := cur.(bson.A)
			if !ok {
				return merr(2, "BadValue", "The field '%s' must be an array but is of type %s in document {_id: ...}", path, bsonTypeName(cur))
			}
			arr = append(bson.A{}, a...)
		}
		each := bson.A{v}
		var mods bson.D
		if vd, ok := v.(bson.D); ok && len(vd) > 0 && vd[0].Key == "$each" {
			e, ok := vd[0].Value.(bson.A)
			if !ok {
				return badValue("The argument to $each in %s must be an array but it was of type: %s", op, bsonTypeName(vd[0].Value))
			}
			each, mods = e, vd[1:]
		}
		if op == "$addToSet" {
			if len(mods) > 0 {
				return badValue("Found unexpected fields after $each in $addToSet: %v", mods)
			}
			for _, e := range each {
				found := false
				for _, x := range arr {
					if valuesEqual(x, e) {
						found = true
						break
					}
				}
				if !found {
					arr = append(arr, cloneValue(e))
				}
			}
			return setPath(d, parts, arr)
		}
		pos := len(arr)
		if p, ok := lookupE(mods, "$position"); ok {
			n, _ := numberOf(p)
			pos = int(n)
			if pos < 0 {
				pos = len(arr) + pos
				if pos < 0 {
					pos = 0
				}
			}
			if pos > len(arr) {
				pos = len(arr)
			}
		}
		next := append(bson.A{}, arr[:pos]...)
		for _, e := range each {
			next = append(next, cloneValue(e))
		}
		next = append(next, arr[pos:]...)
		if s, ok := lookupE(mods, "$sort"); ok {
			if err := sortArray(next, s); err != nil {
				return err
			}
		}
		if s, ok := lookupE(mods, "$slice"); ok {
			var err error
			if next, err = sliceArray(next, s); err != nil {
				return err
			}
		}
		return setPath(d, parts, next)
	case "$pop":
		if !exists {
			return nil
		}
		a, ok := cur.(bson.A)
		if !ok {
			return merr(14, "TypeMismatch", "Path '%s' contains an element of non-array type '%s'", path, bsonTypeName(cur))
		}
		if len(a) == 0 {
			return nil
		}
		if n, _ := numberOf(v); n < 0 {
			return setPath(d, parts, append(bson.A{}, a[1:]...))
		}
		return setPath(d, parts, append(bson.A{}, a[:len(a)-1]...))
	case "$pull", "$pullAll":
		if !exists {
			return nil
		}
		a, ok := cur.(bson.A)
		if !ok {
			return badValue("Cannot apply %s to a non-array value", op)
		}
		var keep bson.A
		for _, e := range a {
			var drop bool
			if op == "$pullAll" {
				list, ok := v.(bson.A)
				if !ok {
					return badValue("$pullAll requires an array argument but was given a %s", bsonTypeName(v))
				}
				for _, x := range list {
					if valuesEqual(e, x) {
						drop = true
					}
				}
			} else {
				m, err := pullMatches(e, v)
				if err != nil {
					return err
				}
				drop = m
			}
			if !drop {
				keep = append(keep, e)
			}
		}
		if keep == nil {
			keep = bson.A{}
		}
		return setPath(d, parts, keep)
	case "$bit":
		ops, ok := v.(bson.D)
		if !ok {
			return badValue("The $bit modifier is not compatible with a %s. You must pass in an embedded document: {$bit: {field: {and/or/xor: #}}", bsonTypeName(v))
		}
		x, _ := intOf(cur)
		if exists && !isIntegral(cur) {
			return badValue("Cannot apply $bit to a value of non-integral type.")
		}
		is32 := true
		if exists {
			_, is32 = cur.(int32)
		}
		for _, o := range ops {
			y, ok := intOf(o.Value)
			if !ok {
				return badValue("The $bit modifier field must be an Integer(32/64 bit)")
			}
			if _, ok := o.Value.(int64); ok {
				is32 = false
			}
			switch o.Key {
			case "and":
				x &= y
			case "or":
				x |= y
			case "xor":
				x ^= y
			default:
				return badValue("The $bit modifier only supports 'and', 'or', and 'xor', not '%s'", o.Key)
			}
		}
		if is32 {
			return setPath(d, parts, int32(x))
		}
		return setPath(d, parts, x)
	}
	return merr(9, "FailedToParse", "Unknown modifier: %s. Expected a valid update modifier or pipeline-style update specified as an array", op)
}

func isIntegral(v any) bool { _, ok := intOf(v); return ok }

func pullMatches(e, cond any) (bool, error) {
	if c, ok := cond.(bson.D); ok {
		if _, isOp := isOperatorDoc(c); isOp {
			return matchField([]any{e}, c)
		}
		ed, ok := e.(bson.D)
		if !ok {
			return false, nil
		}
		return matches(ed, c)
	}
	return valuesEqual(e, cond), nil
}

func sortArray(a bson.A, spec any) error {
	if d, ok := spec.(bson.D); ok {
		s, err := parseSort(d)
		if err != nil {
			return err
		}
		sort.SliceStable(a, func(i, j int) bool {
			x, _ := a[i].(bson.D)
			y, _ := a[j].(bson.D)
			return s.less(x, y)
		})
		return nil
	}
	n, ok := numberOf(spec)
	if !ok || (n != 1 && n != -1) {
		return badValue("The $sort is invalid: use 1/-1 to sort the whole element, or {field:1/-1} to sort embedded fields")
	}
	sort.SliceStable(a, func(i, j int) bool {
		c := compareValues(a[i], a[j])
		if n < 0 {
			return c > 0
		}
		return c < 0
	})
	return nil
}

func zeroLike(v any) any {
	switch v.(type) {
	case int32:
		return int32(0)
	case int64:
		return int64(0)
	}
	return float64(0)
}

// addNumbers is a+b with MongoDB's type promotion.
func addNumbers(a, b any) any {
	x, xi := intOf(a)
	y, yi := intOf(b)
	if xi && yi {
		s := x + y
		_, a32 := a.(int32)
		_, b32 := b.(int32)
		if a32 && b32 && s >= math.MinInt32 && s <= math.MaxInt32 {
			return int32(s)
		}
		if (s > x) == (y > 0) || y == 0 {
			return s
		}
		return float64(x) + float64(y)
	}
	f, _ := numberOf(a)
	g, _ := numberOf(b)
	return f + g
}

func mulNumbers(a, b any) any {
	x, xi := intOf(a)
	y, yi := intOf(b)
	if xi && yi {
		p := x * y
		_, a32 := a.(int32)
		_, b32 := b.(int32)
		if x != 0 && p/x != y {
			return float64(x) * float64(y)
		}
		if a32 && b32 && p >= math.MinInt32 && p <= math.MaxInt32 {
			return int32(p)
		}
		return p
	}
	f, _ := numberOf(a)
	g, _ := numberOf(b)
	return f * g
}

// upsertBase is the document an upsert starts from: the query's equality
// conditions.
func upsertBase(q bson.D) bson.D {
	out := bson.D{}
	var add func(q bson.D)
	add = func(q bson.D) {
		for _, e := range q {
			if e.Key == "$and" {
				if arr, ok := e.Value.(bson.A); ok {
					for _, c := range arr {
						if cd, ok := c.(bson.D); ok {
							add(cd)
						}
					}
				}
				continue
			}
			if strings.HasPrefix(e.Key, "$") {
				continue
			}
			v := e.Value
			if ops, ok := isOperatorDoc(v); ok {
				eq, ok := lookupE(ops, "$eq")
				if !ok {
					continue
				}
				v = eq
			}
			if _, isRe := v.(bson.Regex); isRe {
				continue
			}
			_ = setPath(&out, strings.Split(e.Key, "."), cloneValue(v))
		}
	}
	add(q)
	return out
}
