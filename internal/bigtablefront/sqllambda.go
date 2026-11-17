package bigtablefront

import (
	"fmt"
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// The lambda array functions of GoogleSQL for Bigtable (#1704):
// ARRAY_FILTER and ARRAY_TRANSFORM, whose lambda is `e -> expr` or
// `(e, i) -> expr`, i the element's zero-based offset, and ARRAY_OFFSET and
// ARRAY_OFFSETS, which find a value or an `e -> condition`
// (https://cloud.google.com/bigtable/docs/reference/sql/array_functions).
// A lambda is an argument of these four only; its aliases shadow the
// columns in its body, which has no aggregate or window function.
//
// NORMALIZE and NORMALIZE_AND_CASEFOLD (string_functions) take the bare
// mode NFC (the default), NFKC, NFD or NFKD; the case folding is Unicode's
// full folding, before the normalization.
//
// GROUPING, MIN_BY and MAX_BY are GoogleSQL's but not GoogleSQL for
// Bigtable's: neither functions-all nor aggregate_functions lists them, nor
// does query-syntax's GROUP BY take ROLLUP, CUBE or GROUPING SETS (read
// 2026-11-17). They are refused UNIMPLEMENTED, saying so (unlistedErr).

// lambdaParams is how many aliases each lambda function's lambda may take.
var lambdaParams = map[string]int{"ARRAY_FILTER": 2, "ARRAY_TRANSFORM": 2, "ARRAY_OFFSET": 1, "ARRAY_OFFSETS": 1}

// unlisted are GoogleSQL functions GoogleSQL for Bigtable does not list.
var unlisted = map[string]bool{"GROUPING": true, "MIN_BY": true, "MAX_BY": true}

// unlistedErr is answered UNIMPLEMENTED, like unsupportedErr.
type unlistedErr struct{ name string }

func (e unlistedErr) Error() string {
	return "Function " + e.name + " is GoogleSQL's, but not in GoogleSQL for Bigtable: its function reference does not list it"
}

type lambda struct {
	params []string
	body   expr
	base   int        // len(scope.locals) outside the lambda
	t      *btpb.Type // the body's type
}

// lambda parses `alias -> body` or `(alias[, alias]) -> body`, reporting
// whether one was there.
func (p *parser) lambda() (expr, bool, error) {
	var names []string
	isOp := func(n int, o string) bool { t := p.peekAt(n); return t.kind == "op" && t.text == o }
	switch {
	case p.peek().kind == "ident" && isOp(1, "->"):
		names, p.pos = []string{p.peek().text}, p.pos+2
	case isOp(0, "(") && p.peekAt(1).kind == "ident" && isOp(2, ")") && isOp(3, "->"):
		names, p.pos = []string{p.peekAt(1).text}, p.pos+4
	case isOp(0, "(") && p.peekAt(1).kind == "ident" && isOp(2, ",") && p.peekAt(3).kind == "ident" && isOp(4, ")") && isOp(5, "->"):
		names, p.pos = []string{p.peekAt(1).text, p.peekAt(3).text}, p.pos+6
	default:
		return nil, false, nil
	}
	if len(names) == 2 && strings.EqualFold(names[0], names[1]) {
		return nil, true, fmt.Errorf("Duplicate lambda argument name %s", names[1])
	}
	body, err := p.expr()
	if err != nil {
		return nil, true, err
	}
	return &lambda{params: names, body: body}, true, nil
}

func (l *lambda) check(*scope) (*btpb.Type, error) {
	return nil, fmt.Errorf("Syntax error: a lambda is allowed only as an argument of ARRAY_FILTER, ARRAY_TRANSFORM, ARRAY_OFFSET or ARRAY_OFFSETS")
}

// checkIn checks l as the next argument of c, whose first is the array.
func (l *lambda) checkIn(c *call, s *scope) (*btpb.Type, error) {
	n, ok := lambdaParams[c.name]
	if !ok || len(c.argTypes) != 1 {
		return l.check(s)
	}
	at := c.argTypes[0]
	elem := int64Type
	if at != nil {
		a, ok := at.GetKind().(*btpb.Type_ArrayType)
		if !ok {
			return nil, fmt.Errorf("No matching signature for function %s for argument types: %s, LAMBDA", c.name, kind(at))
		}
		elem = a.ArrayType.GetElementType()
	}
	if len(l.params) > n {
		return nil, fmt.Errorf("No matching signature for function %s: its lambda takes %d argument(s), not %d", c.name, n, len(l.params))
	}
	inner := s.noAggs("a lambda body")
	inner.wins = nil
	inner.locals = append([]column{}, s.locals...)
	l.base = len(s.locals)
	inner.locals = append(inner.locals, column{name: l.params[0], t: elem})
	if len(l.params) == 2 {
		inner.locals = append(inner.locals, column{name: l.params[1], t: int64Type})
	}
	t, err := l.body.check(inner)
	if err != nil {
		return nil, err
	}
	l.t = t
	return t, nil
}

// lambdaValue is a lambda bound to the row it is called in.
type lambdaValue struct {
	l *lambda
	e *env
}

func (l *lambda) eval(e *env) (any, error) { return &lambdaValue{l: l, e: e}, nil }
func (l *lambda) String() string {
	return "(" + strings.Join(l.params, ",") + ")->" + l.body.String()
}

func (v *lambdaValue) call(elem any, i int) (any, error) {
	e := *v.e
	e.locals = append(append([]any{}, v.e.locals[:v.l.base]...), elem)
	if len(v.l.params) == 2 {
		e.locals = append(e.locals, int64(i))
	}
	return v.l.body.eval(&e)
}

// checkLocal resolves c to a lambda alias, or a field of one, if one is
// in scope by that name.
func (c *colRef) checkLocal(s *scope) (*btpb.Type, bool, error) {
	name := c.name
	if c.qualifier != "" {
		name = c.qualifier
	}
	for i := len(s.locals) - 1; i >= 0; i-- {
		if !strings.EqualFold(s.locals[i].name, name) {
			continue
		}
		c.local = i + 1
		t := s.locals[i].t
		if c.qualifier == "" {
			c.t = t
			return t, true, nil
		}
		st, ok := t.GetKind().(*btpb.Type_StructType)
		if !ok {
			return nil, true, fmt.Errorf("Cannot access field %s on a value with type %s", c.name, kind(t))
		}
		for j, f := range st.StructType.GetFields() {
			if strings.EqualFold(f.GetFieldName(), c.name) {
				c.field, c.t = j+1, f.GetType()
				return c.t, true, nil
			}
		}
		return nil, true, fmt.Errorf("Field name %s does not exist in %s", c.name, kind(t))
	}
	return nil, false, nil
}

func (c *colRef) evalLocal(e *env) any {
	v := e.locals[c.local-1]
	if c.field > 0 && v != nil {
		return v.([]any)[c.field-1]
	}
	if c.field > 0 {
		return nil
	}
	return v
}

// normModeType marks NORMALIZE's bare mode argument.
var normModeType = &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{Encoding: &btpb.Type_String_Encoding{}}}}

var normModes = map[string]bool{"NFC": true, "NFKC": true, "NFD": true, "NFKD": true}

func normForm(mode string) norm.Form {
	switch mode {
	case "NFKC":
		return norm.NFKC
	case "NFD":
		return norm.NFD
	case "NFKD":
		return norm.NFKD
	}
	return norm.NFC
}

func init() {
	lambdaArg := func(c *call, i int) (*lambda, bool) {
		if i >= len(c.args) {
			return nil, false
		}
		l, ok := c.args[i].(*lambda)
		return l, ok
	}
	elemOf := func(t *btpb.Type) *btpb.Type {
		if t == nil {
			return int64Type
		}
		return t.GetArrayType().GetElementType()
	}
	// findSig types ARRAY_OFFSET and ARRAY_OFFSETS's element_to_find: a
	// BOOL lambda, or a value comparable with the elements.
	findSig := func(c *call, ts []*btpb.Type) error {
		if len(ts) < 2 || !argOK(ts[0], "ARRAY") {
			return sigErr(c, ts)
		}
		if l, ok := lambdaArg(c, 1); ok {
			if !isKind(l.t, "BOOL") {
				return fmt.Errorf("No matching signature for function %s: its lambda must return BOOL, not %s", c.name, kind(l.t))
			}
			return nil
		}
		el := elemOf(ts[0])
		if ts[1] != nil && !sameType(el, ts[1]) && !(isNumeric(el) && isNumeric(ts[1])) || !groupable(el) {
			return sigErr(c, ts)
		}
		return nil
	}
	// matches reports whether elem is what vs[1] finds.
	matches := func(c *call, vs []any, elem any, i int) (bool, error) {
		if lv, ok := vs[1].(*lambdaValue); ok {
			r, err := lv.call(elem, i)
			return r == true, err
		}
		if elem == nil {
			return false, nil
		}
		cmp, ok := compare(elem, vs[1])
		return ok && cmp == 0, nil
	}
	add := map[string]*fnDef{
		"ARRAY_FILTER": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			l, ok := lambdaArg(c, 1)
			if len(ts) != 2 || !argOK(ts[0], "ARRAY") || !ok {
				return nil, sigErr(c, ts)
			}
			if !isKind(l.t, "BOOL") {
				return nil, fmt.Errorf("No matching signature for function ARRAY_FILTER: its lambda must return BOOL, not %s", kind(l.t))
			}
			return arrayType(elemOf(ts[0])), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			out := []any{}
			for i, x := range vs[0].([]any) {
				keep, err := vs[1].(*lambdaValue).call(x, i)
				if err != nil {
					return nil, err
				}
				if keep == true {
					out = append(out, x)
				}
			}
			return out, nil
		}},
		"ARRAY_TRANSFORM": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			l, ok := lambdaArg(c, 1)
			if len(ts) != 2 || !argOK(ts[0], "ARRAY") || !ok {
				return nil, sigErr(c, ts)
			}
			if isKind(l.t, "ARRAY<") {
				return nil, fmt.Errorf("No matching signature for function ARRAY_TRANSFORM: an array of arrays is not supported")
			}
			return arrayType(l.t), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			in := vs[0].([]any)
			out := make([]any, len(in))
			for i, x := range in {
				v, err := vs[1].(*lambdaValue).call(x, i)
				if err != nil {
					return nil, err
				}
				out[i] = v
			}
			return out, nil
		}},
		"ARRAY_OFFSET": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) < 2 || len(ts) > 3 {
				return nil, sigErr(c, ts)
			}
			if err := findSig(c, ts); err != nil {
				return nil, err
			}
			if len(ts) == 3 && !argOK(ts[2], "STRING") {
				return nil, sigErr(c, ts)
			}
			if l, ok := c.args[len(c.args)-1].(*literal); ok && len(ts) == 3 && l.v != nil {
				if m := strings.ToUpper(l.v.(string)); m != "FIRST" && m != "LAST" {
					return nil, fmt.Errorf("The third argument of ARRAY_OFFSET must be 'FIRST' or 'LAST', not '%s'", l.v)
				}
			}
			return int64Type, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			a := vs[0].([]any)
			last := false
			if len(vs) == 3 {
				switch strings.ToUpper(vs[2].(string)) {
				case "FIRST":
				case "LAST":
					last = true
				default:
					return nil, fmt.Errorf("The third argument of ARRAY_OFFSET must be 'FIRST' or 'LAST', not '%s'", vs[2])
				}
			}
			for k := range a {
				i := k
				if last {
					i = len(a) - 1 - k
				}
				ok, err := matches(c, vs, a[i], i)
				if err != nil {
					return nil, err
				}
				if ok {
					return int64(i), nil
				}
			}
			return nil, nil
		}},
		"ARRAY_OFFSETS": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 2 {
				return nil, sigErr(c, ts)
			}
			if err := findSig(c, ts); err != nil {
				return nil, err
			}
			return arrayType(int64Type), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			out := []any{}
			for i, x := range vs[0].([]any) {
				ok, err := matches(c, vs, x, i)
				if err != nil {
					return nil, err
				}
				if ok {
					out = append(out, int64(i))
				}
			}
			return out, nil
		}},
	}
	normSig := func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
		if len(ts) < 1 || len(ts) > 2 || !argOK(ts[0], "STRING") || ts[0] == normModeType {
			return nil, sigErr(c, ts)
		}
		if len(ts) == 2 {
			if l, ok := c.args[1].(*literal); !ok || l.t != normModeType {
				return nil, fmt.Errorf("No matching signature for function %s: the normalization mode is NFC, NFKC, NFD or NFKD", c.name)
			}
		}
		return stringType, nil
	}
	normalize := func(fold bool) func(c *call, vs []any, _ *env) (any, error) {
		return func(c *call, vs []any, _ *env) (any, error) {
			s, mode := vs[0].(string), "NFC"
			if len(vs) == 2 {
				mode = vs[1].(string)
			}
			if fold {
				s = cases.Fold().String(s)
			}
			return normForm(mode).String(s), nil
		}
	}
	add["NORMALIZE"] = &fnDef{sig: normSig, run: normalize(false)}
	add["NORMALIZE_AND_CASEFOLD"] = &fnDef{sig: normSig, run: normalize(true)}
	for k, v := range add {
		functions[k] = v
	}
}
