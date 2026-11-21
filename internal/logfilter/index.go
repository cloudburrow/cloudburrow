package logfilter

import (
	"math"
	"strconv"
	"strings"
)

// Custom indexes (#2108): a log bucket's index_configs, which Google says
// change how a query of that bucket compares an indexed field
// (docs.cloud.google.com/logging/docs/analyze/custom-index, read
// 2026-11-21): "When indexing is enabled, filter constraints on a field are
// applied based on the type of the index." A string index makes every
// comparison a string one, an integer index an integer one; "If type
// coercion fails, the data isn't indexed", and "If the query value can't be
// coerced to the index type, the index is ignored". Either way the
// comparison falls back to the field's own type.

// IndexType is a custom index's type.
type IndexType int

const (
	// IndexString is INDEX_TYPE_STRING.
	IndexString IndexType = iota + 1
	// IndexInteger is INDEX_TYPE_INTEGER.
	IndexInteger
)

// Index is a custom index on one field. Applies reports whether the index
// holds a record (Google indexes only entries written after the index was
// made: "changes to custom indexes don't apply to logs retroactively"); nil
// means every record.
type Index struct {
	Type    IndexType
	Applies func(Record) bool
}

// CanonicalPath is a field path as the language resolves it (labels."a/b"
// and json_payload.x become labels.a/b and jsonPayload.x), or an error for
// a field LogEntry does not define. Index lookups use this form.
func CanonicalPath(path string) (string, error) {
	toks, err := lex(path)
	if err != nil {
		return "", err
	}
	p := &parser{toks: toks}
	segs, perr := p.path()
	if perr != nil {
		return "", perr
	}
	if t := p.peek(); t.kind != tEOF {
		return "", syntaxErr(t)
	}
	fi, ferr := resolveField(segs)
	if ferr != nil {
		return "", ferr
	}
	return strings.Join(fi.path, "."), nil
}

// indexedCmp is a comparison on an indexed field.
type indexedCmp struct {
	plain cmpNode
	fi    fieldInfo
	ix    Index
}

func (c indexedCmp) eval(r Record) bool {
	if c.ix.Applies != nil && !c.ix.Applies(r) {
		return c.plain.eval(r)
	}
	if !rhsCoerces(c.plain.rhs, c.plain.op, c.ix.Type) {
		return c.plain.eval(r) // the index is ignored
	}
	raws, found := lookup(r, c.fi)
	if !found {
		return false
	}
	var vals []value
	for _, raw := range raws {
		v, ok := coerce(raw, c.ix.Type)
		if !ok {
			return c.plain.eval(r) // the data isn't indexed
		}
		vals = append(vals, v)
	}
	for _, v := range vals {
		if c.plain.rhs.match(v, c.plain.op) {
			return true
		}
	}
	return false
}

// coerce types a raw value as the index's type.
func coerce(raw any, t IndexType) (value, bool) {
	if raw == nil {
		return value{null: true}, true
	}
	switch t {
	case IndexInteger:
		f, ok := number(raw)
		if !ok || f != math.Trunc(f) || math.IsInf(f, 0) {
			return value{}, false
		}
		return value{k: kNumber, n: f}, true
	default:
		s, ok := leafString(raw)
		return value{k: kString, s: s}, ok
	}
}

// rhsCoerces is whether every query value converts to the index's type.
func rhsCoerces(n rhsNode, op string, t IndexType) bool {
	switch x := n.(type) {
	case rhsAnd:
		for _, c := range x {
			if !rhsCoerces(c, op, t) {
				return false
			}
		}
		return true
	case rhsOr:
		for _, c := range x {
			if !rhsCoerces(c, op, t) {
				return false
			}
		}
		return true
	case rhsNot:
		return rhsCoerces(x.n, op, t)
	case *lit:
		if x.null {
			return true
		}
		if t == IndexInteger {
			if op == "=~" || op == "!~" || op == ":" {
				return false
			}
			_, err := strconv.ParseInt(x.text, 10, 64)
			return err == nil
		}
		return true
	}
	return false
}
