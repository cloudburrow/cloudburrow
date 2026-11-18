package mql

// Firestore with MongoDB compatibility (#1738): the positional update
// operators ($, $[] and $[<identifier>]) with arrayFilters, and the
// positional projection ($), which Google's supported features page (8.0)
// lists as supported. They follow MongoDB's rules: $ is the first element
// of the array the query matched, $[] every element, $[<identifier>] the
// elements the identifier's array filter matches.

import (
	"regexp"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// UpdateContext is what an update's positional operators read: the
// update's query, and its arrayFilters.
type UpdateContext struct {
	Query        bson.D
	ArrayFilters bson.A
}

var arrayFilterIDRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)

// parseArrayFilters is filters by identifier.
func parseArrayFilters(af bson.A) (map[string]bson.D, error) {
	out := map[string]bson.D{}
	for _, v := range af {
		f, ok := v.(bson.D)
		if !ok {
			return nil, merr(14, "TypeMismatch", "Each array filter must be an object")
		}
		if len(f) == 0 {
			return nil, merr(9, "FailedToParse", "Cannot use an expression without a top-level field name in arrayFilters")
		}
		id := ""
		for _, e := range f {
			top := e.Key
			if strings.HasPrefix(top, "$") {
				// $and, $or, $nor: the identifier is their fields'.
				arr, _ := e.Value.(bson.A)
				for _, c := range arr {
					if cd, ok := c.(bson.D); ok && len(cd) > 0 {
						top = cd[0].Key
						break
					}
				}
			}
			top, _, _ = strings.Cut(top, ".")
			if id == "" {
				id = top
			} else if id != top {
				return nil, merr(9, "FailedToParse", "Error parsing array filter :: caused by :: Expected a single top-level field name, found '%s' and '%s'", id, top)
			}
		}
		if !arrayFilterIDRe.MatchString(id) {
			return nil, badValue("Error parsing array filter :: caused by :: The top-level field name must be an alphanumeric string beginning with a lowercase letter, found '%s'", id)
		}
		if _, dup := out[id]; dup {
			return nil, merr(9, "FailedToParse", "Found multiple array filters with the same top-level field name %s", id)
		}
		out[id] = f
	}
	return out, nil
}

// queryConds are the query's conditions on path or below it, through $and.
func queryConds(q bson.D, path string) bson.D {
	var out bson.D
	for _, e := range q {
		if e.Key == "$and" {
			arr, _ := e.Value.(bson.A)
			for _, c := range arr {
				if cd, ok := c.(bson.D); ok {
					out = append(out, queryConds(cd, path)...)
				}
			}
			continue
		}
		if e.Key == path || strings.HasPrefix(e.Key, path+".") {
			out = append(out, e)
		}
	}
	return out
}

// positionalIndex is the index of the first element of the array at path
// in doc that the query's conditions on that array match.
func positionalIndex(doc bson.D, q bson.D, path string) (int, error) {
	conds := queryConds(q, path)
	notFound := badValue("The positional operator did not find the match needed from the query.")
	if len(conds) == 0 {
		return 0, notFound
	}
	v, ok := getPath(doc, path)
	arr, isArr := v.(bson.A)
	if !ok || !isArr {
		return 0, notFound
	}
	for i, e := range arr {
		one := cloneDoc(doc)
		if err := setPath(&one, strings.Split(path, "."), bson.A{e}); err != nil {
			return 0, err
		}
		m, err := matches(one, conds)
		if err != nil {
			return 0, err
		}
		if m {
			return i, nil
		}
	}
	return 0, notFound
}

// expandPositional is the concrete paths a path with positional operators
// names in doc. used records the array filters it applied.
func expandPositional(doc bson.D, path string, uc *UpdateContext, filters map[string]bson.D, used map[string]bool, inserting bool) ([]string, error) {
	parts := strings.Split(path, ".")
	var walk func(prefix []string, cur any, rest []string) ([]string, error)
	walk = func(prefix []string, cur any, rest []string) ([]string, error) {
		if len(rest) == 0 {
			return []string{strings.Join(prefix, ".")}, nil
		}
		p := rest[0]
		here := strings.Join(prefix, ".")
		switch {
		case p == "$":
			if inserting {
				return nil, badValue("The positional operator did not find the match needed from the query.")
			}
			var q bson.D
			if uc != nil {
				q = uc.Query
			}
			i, err := positionalIndex(doc, q, here)
			if err != nil {
				return nil, err
			}
			arr, _ := cur.(bson.A)
			return walk(append(prefix, strconv.Itoa(i)), arr[i], rest[1:])
		case p == "$[]" || (strings.HasPrefix(p, "$[") && strings.HasSuffix(p, "]")):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "$["), "]")
			var f bson.D
			if id != "" {
				var ok bool
				if f, ok = filters[id]; !ok {
					return nil, badValue("No array filter found for identifier '%s' in path '%s'", id, path)
				}
				used[id] = true
			}
			if inserting {
				return nil, nil
			}
			arr, ok := cur.(bson.A)
			if !ok {
				if cur == nil {
					return nil, badValue("The path '%s' must exist in the document in order to apply array updates.", here)
				}
				return nil, badValue("Cannot apply array updates to non-array element %s: %v", here, cur)
			}
			var out []string
			for i, e := range arr {
				if f != nil {
					m, err := matches(bson.D{{Key: id, Value: e}}, f)
					if err != nil {
						return nil, err
					}
					if !m {
						continue
					}
				}
				sub, err := walk(append(append([]string{}, prefix...), strconv.Itoa(i)), e, rest[1:])
				if err != nil {
					return nil, err
				}
				out = append(out, sub...)
			}
			return out, nil
		}
		var next any
		switch x := cur.(type) {
		case bson.D:
			next = lookup(x, p)
		case bson.A:
			if i, err := strconv.Atoi(p); err == nil && i >= 0 && i < len(x) {
				next = x[i]
			}
		}
		if _, missing := next.(missingT); missing {
			next = nil
		}
		return walk(append(prefix, p), next, rest[1:])
	}
	return walk(nil, doc, parts)
}

// projectPositional is doc and proj with each "path.$" of proj resolved:
// the array at path holds only the first element the query matched.
func projectPositional(doc bson.D, proj bson.D, q bson.D) (bson.D, bson.D, error) {
	n := 0
	for _, e := range proj {
		if strings.HasSuffix(e.Key, ".$") {
			n++
		}
	}
	if n == 0 {
		return doc, proj, nil
	}
	if n > 1 {
		return nil, nil, badValue("Cannot specify more than one positional projection per query.")
	}
	doc = cloneDoc(doc)
	out := bson.D{}
	for _, e := range proj {
		if !strings.HasSuffix(e.Key, ".$") {
			out = append(out, e)
			continue
		}
		path := strings.TrimSuffix(e.Key, ".$")
		if !truthy(e.Value) {
			return nil, nil, merr(31395, "Location31395", "positional projection cannot be used with exclusion")
		}
		if len(queryConds(q, path)) == 0 {
			return nil, nil, badValue("Executor error during find command :: caused by :: positional operator '.$' couldn't find a matching element in the array")
		}
		i, err := positionalIndex(doc, q, path)
		if err != nil {
			return nil, nil, badValue("Executor error during find command :: caused by :: positional operator '.$' couldn't find a matching element in the array")
		}
		v, _ := getPath(doc, path)
		if err := setPath(&doc, strings.Split(path, "."), bson.A{v.(bson.A)[i]}); err != nil {
			return nil, nil, err
		}
		out = append(out, bson.E{Key: path, Value: int32(1)})
	}
	return doc, out, nil
}

// ProjectQuery is Project, with the positional projection resolved by the
// query that matched doc.
func ProjectQuery(doc bson.D, proj bson.D, q bson.D) (bson.D, error) {
	d, p, err := projectPositional(doc, proj, q)
	if err != nil {
		return nil, err
	}
	return project(d, p)
}

// ApplyUpdateWith is ApplyUpdate, with the positional operators resolved
// by uc.
func ApplyUpdateWith(doc bson.D, u any, inserting bool, uc *UpdateContext) (bson.D, error) {
	return applyUpdateCtx(doc, u, inserting, uc)
}
