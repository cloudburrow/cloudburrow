package bigtablefront

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/protobuf/proto"
)

// The expression tree of GoogleSQL for Bigtable (#1511): each node is
// checked once against the query's scope (check), which fixes its type,
// the type PrepareQuery reports in ResultSetMetadata, and then evaluated
// per row (eval). Values are nil (NULL), []byte, string, int64, float64
// (FLOAT32 and FLOAT64), bool, time.Time (TIMESTAMP, and DATE at UTC
// midnight), []any (ARRAY, and STRUCT by field) and *mapValue.

type mapValue struct {
	keys, vals []any
}

func (m *mapValue) get(k any) (any, bool) {
	for i, key := range m.keys {
		if c, ok := compare(key, k); ok && c == 0 {
			return m.vals[i], true
		}
	}
	return nil, false
}

var (
	bytesType     = &btpb.Type{Kind: &btpb.Type_BytesType{BytesType: &btpb.Type_Bytes{}}}
	stringType    = &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{}}}
	int64Type     = &btpb.Type{Kind: &btpb.Type_Int64Type{Int64Type: &btpb.Type_Int64{}}}
	float32Type   = &btpb.Type{Kind: &btpb.Type_Float32Type{Float32Type: &btpb.Type_Float32{}}}
	float64Type   = &btpb.Type{Kind: &btpb.Type_Float64Type{Float64Type: &btpb.Type_Float64{}}}
	boolType      = &btpb.Type{Kind: &btpb.Type_BoolType{BoolType: &btpb.Type_Bool{}}}
	timestampType = &btpb.Type{Kind: &btpb.Type_TimestampType{TimestampType: &btpb.Type_Timestamp{}}}
	dateType      = &btpb.Type{Kind: &btpb.Type_DateType{DateType: &btpb.Type_Date{}}}
	familyType    = mapType(bytesType, bytesType)
	// cellType is a cell with history: STRUCT<timestamp TIMESTAMP, value BYTES>.
	cellType    = structType([]string{"timestamp", "value"}, []*btpb.Type{timestampType, bytesType})
	historyType = mapType(bytesType, arrayType(cellType))
	// datePartType marks a bare date part argument (DAY, HOUR, ...).
	datePartType = &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{Encoding: &btpb.Type_String_Encoding{}}}}
)

func mapType(k, v *btpb.Type) *btpb.Type {
	return &btpb.Type{Kind: &btpb.Type_MapType{MapType: &btpb.Type_Map{KeyType: k, ValueType: v}}}
}

func arrayType(e *btpb.Type) *btpb.Type {
	return &btpb.Type{Kind: &btpb.Type_ArrayType{ArrayType: &btpb.Type_Array{ElementType: e}}}
}

func structType(names []string, types []*btpb.Type) *btpb.Type {
	s := &btpb.Type_Struct{}
	for i, n := range names {
		s.Fields = append(s.Fields, &btpb.Type_Struct_Field{FieldName: n, Type: types[i]})
	}
	return &btpb.Type{Kind: &btpb.Type_StructType{StructType: s}}
}

// kind is a type's name, as errors spell it.
func kind(t *btpb.Type) string {
	switch k := t.GetKind().(type) {
	case nil:
		if t == nil {
			return "NULL"
		}
	case *btpb.Type_BytesType:
		return "BYTES"
	case *btpb.Type_StringType:
		return "STRING"
	case *btpb.Type_Int64Type:
		return "INT64"
	case *btpb.Type_Float32Type:
		return "FLOAT32"
	case *btpb.Type_Float64Type:
		return "FLOAT64"
	case *btpb.Type_BoolType:
		return "BOOL"
	case *btpb.Type_TimestampType:
		return "TIMESTAMP"
	case *btpb.Type_DateType:
		return "DATE"
	case *btpb.Type_ArrayType:
		return "ARRAY<" + kind(k.ArrayType.GetElementType()) + ">"
	case *btpb.Type_MapType:
		return "MAP<" + kind(k.MapType.GetKeyType()) + ", " + kind(k.MapType.GetValueType()) + ">"
	case *btpb.Type_StructType:
		var fs []string
		for _, f := range k.StructType.GetFields() {
			fs = append(fs, f.GetFieldName()+" "+kind(f.GetType()))
		}
		return "STRUCT<" + strings.Join(fs, ", ") + ">"
	}
	return "UNKNOWN"
}

func isKind(t *btpb.Type, names ...string) bool {
	k := kind(t)
	for _, n := range names {
		if k == n || strings.HasSuffix(n, "<") && strings.HasPrefix(k, n) {
			return true
		}
	}
	return false
}

func isNumeric(t *btpb.Type) bool { return isKind(t, "INT64", "FLOAT32", "FLOAT64") }

func sameType(a, b *btpb.Type) bool { return proto.Equal(a, b) }

// parseTypeName is the type a CAST names.
func parseTypeName(n string) (*btpb.Type, error) {
	switch n {
	case "BYTES":
		return bytesType, nil
	case "STRING":
		return stringType, nil
	case "INT64":
		return int64Type, nil
	case "FLOAT32":
		return float32Type, nil
	case "FLOAT64":
		return float64Type, nil
	case "BOOL":
		return boolType, nil
	case "TIMESTAMP":
		return timestampType, nil
	case "DATE":
		return dateType, nil
	}
	if strings.HasPrefix(n, "ARRAY<") {
		e, err := parseTypeName(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(n, "ARRAY<"), ">")))
		if err != nil {
			return nil, err
		}
		return arrayType(e), nil
	}
	return nil, unsupportedErr{"the type " + n}
}

// column is one column of the FROM source.
type column struct {
	name string
	t    *btpb.Type
}

// scope is what an expression is checked against.
type scope struct {
	cols    []column
	alias   string
	params  map[string]*btpb.Type
	aggs    *[]*call // the aggregates found, when they are allowed
	inAgg   bool
	noAggOK string // why aggregates are not allowed here, if they are not
}

type env struct {
	row    []any
	aggs   []any
	params map[string]any
	now    time.Time
}

type expr interface {
	check(s *scope) (*btpb.Type, error)
	eval(e *env) (any, error)
	String() string
}

// --- leaves ---

type literal struct {
	v any
	t *btpb.Type // nil for NULL
}

func (l *literal) check(*scope) (*btpb.Type, error) {
	if l.t == nil {
		return int64Type, nil // an untyped NULL is INT64, as in GoogleSQL
	}
	return l.t, nil
}
func (l *literal) eval(*env) (any, error) { return l.v, nil }
func (l *literal) String() string         { return fmt.Sprintf("%#v", l.v) }

type param struct {
	name string
	t    *btpb.Type
}

func (p *param) check(s *scope) (*btpb.Type, error) {
	t, ok := s.params[p.name]
	if !ok {
		return nil, fmt.Errorf("Query parameter '%s' not found", p.name)
	}
	p.t = t
	return t, nil
}
func (p *param) eval(e *env) (any, error) {
	v, ok := e.params[p.name]
	if !ok {
		return nil, fmt.Errorf("Query parameter '%s' is not bound", p.name)
	}
	return v, nil
}
func (p *param) String() string { return "@" + p.name }

type colRef struct {
	name, qualifier string
	quoted          bool
	idx             int
	t               *btpb.Type
}

func (c *colRef) check(s *scope) (*btpb.Type, error) {
	if c.qualifier != "" && c.qualifier != s.alias {
		return nil, fmt.Errorf("Unrecognized name: %s", c.qualifier)
	}
	for i, col := range s.cols {
		if col.name == c.name {
			c.idx, c.t = i, col.t
			return col.t, nil
		}
	}
	if c.name == "_timestamp" {
		return nil, unsupportedErr{"_timestamp (UNPACK)"}
	}
	return nil, fmt.Errorf("Unrecognized name: %s", c.name)
}
func (c *colRef) eval(e *env) (any, error) { return e.row[c.idx], nil }
func (c *colRef) String() string           { return "col:" + c.name }

// --- composites ---

type index struct {
	x, k expr
	mode string // "key", "offset", "ordinal", "safe_offset", "safe_ordinal"
	safe bool
}

func (ix *index) check(s *scope) (*btpb.Type, error) {
	xt, err := ix.x.check(s)
	if err != nil {
		return nil, err
	}
	kt, err := ix.k.check(s)
	if err != nil {
		return nil, err
	}
	switch k := xt.GetKind().(type) {
	case *btpb.Type_MapType:
		if ix.mode != "key" {
			return nil, fmt.Errorf("a map is subscripted by its key, not %s", strings.ToUpper(ix.mode))
		}
		if !assignable(kt, k.MapType.GetKeyType()) {
			return nil, fmt.Errorf("a map with %s keys cannot be subscripted by %s", kind(k.MapType.GetKeyType()), kind(kt))
		}
		return k.MapType.GetValueType(), nil
	case *btpb.Type_ArrayType:
		if !isKind(kt, "INT64") {
			return nil, fmt.Errorf("an array subscript must be INT64, not %s", kind(kt))
		}
		if ix.mode == "key" {
			ix.mode = "offset"
		}
		return k.ArrayType.GetElementType(), nil
	}
	return nil, fmt.Errorf("a value of type %s cannot be subscripted", kind(xt))
}

func (ix *index) eval(e *env) (any, error) {
	x, err := ix.x.eval(e)
	if err != nil || x == nil {
		return nil, err
	}
	k, err := ix.k.eval(e)
	if err != nil || k == nil {
		return nil, err
	}
	switch xv := x.(type) {
	case *mapValue:
		v, _ := xv.get(k)
		return v, nil
	case []any:
		n := k.(int64)
		if strings.HasSuffix(ix.mode, "ordinal") {
			n--
		}
		if n < 0 || n >= int64(len(xv)) {
			if strings.HasPrefix(ix.mode, "safe") {
				return nil, nil
			}
			return nil, fmt.Errorf("Array index %d is out of bounds", k)
		}
		return xv[n], nil
	}
	return nil, nil
}
func (ix *index) String() string { return ix.x.String() + "[" + ix.mode + ":" + ix.k.String() + "]" }

type field struct {
	x    expr
	name string
	idx  int
}

func (f *field) check(s *scope) (*btpb.Type, error) {
	xt, err := f.x.check(s)
	if err != nil {
		return nil, err
	}
	st, ok := xt.GetKind().(*btpb.Type_StructType)
	if !ok {
		return nil, fmt.Errorf("Cannot access field %s on a value with type %s", f.name, kind(xt))
	}
	for i, fl := range st.StructType.GetFields() {
		if strings.EqualFold(fl.GetFieldName(), f.name) {
			f.idx = i
			return fl.GetType(), nil
		}
	}
	return nil, fmt.Errorf("Field name %s does not exist in %s", f.name, kind(xt))
}
func (f *field) eval(e *env) (any, error) {
	x, err := f.x.eval(e)
	if err != nil || x == nil {
		return nil, err
	}
	return x.([]any)[f.idx], nil
}
func (f *field) String() string { return f.x.String() + "." + f.name }

type arrayLit struct {
	elems []expr
	t     *btpb.Type
}

func (a *arrayLit) check(s *scope) (*btpb.Type, error) {
	var et *btpb.Type
	for _, e := range a.elems {
		t, err := e.check(s)
		if err != nil {
			return nil, err
		}
		if l, ok := e.(*literal); ok && l.t == nil {
			continue
		}
		if et == nil {
			et = t
		} else if u, ok := unify(et, t); ok {
			et = u
		} else {
			return nil, fmt.Errorf("Array elements of types {%s, %s} do not have a common supertype", kind(et), kind(t))
		}
	}
	if et == nil {
		et = int64Type
	}
	a.t = arrayType(et)
	return a.t, nil
}
func (a *arrayLit) eval(e *env) (any, error) {
	out := make([]any, 0, len(a.elems))
	et := a.t.GetArrayType().GetElementType()
	for _, x := range a.elems {
		v, err := x.eval(e)
		if err != nil {
			return nil, err
		}
		out = append(out, coerce(v, et))
	}
	return out, nil
}
func (a *arrayLit) String() string {
	var p []string
	for _, e := range a.elems {
		p = append(p, e.String())
	}
	return "[" + strings.Join(p, ",") + "]"
}

type interval struct {
	n    expr
	unit string
}

func (iv *interval) check(s *scope) (*btpb.Type, error) {
	return nil, fmt.Errorf("INTERVAL is allowed only as an argument of TIMESTAMP_ADD and TIMESTAMP_SUB")
}
func (iv *interval) eval(*env) (any, error) { return nil, nil }
func (iv *interval) String() string         { return "INTERVAL " + iv.n.String() + " " + iv.unit }

// --- operators ---

type unaryOp struct {
	op string
	x  expr
	t  *btpb.Type
}

func (u *unaryOp) check(s *scope) (*btpb.Type, error) {
	t, err := u.x.check(s)
	if err != nil {
		return nil, err
	}
	switch u.op {
	case "NOT":
		if !isKind(t, "BOOL") {
			return nil, fmt.Errorf("NOT takes BOOL, not %s", kind(t))
		}
	case "-":
		if !isNumeric(t) {
			return nil, fmt.Errorf("No matching signature for operator - for argument type %s", kind(t))
		}
	case "~":
		if !isKind(t, "INT64", "BYTES") {
			return nil, fmt.Errorf("No matching signature for operator ~ for argument type %s", kind(t))
		}
	}
	u.t = t
	return t, nil
}
func (u *unaryOp) eval(e *env) (any, error) {
	v, err := u.x.eval(e)
	if err != nil || v == nil {
		return nil, err
	}
	switch u.op {
	case "NOT":
		return !v.(bool), nil
	case "-":
		switch n := v.(type) {
		case int64:
			if n == math.MinInt64 {
				return nil, fmt.Errorf("int64 overflow: -%d", n)
			}
			return -n, nil
		case float64:
			return -n, nil
		}
	case "~":
		switch n := v.(type) {
		case int64:
			return ^n, nil
		case []byte:
			out := make([]byte, len(n))
			for i, b := range n {
				out[i] = ^b
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("bad operand for %s", u.op)
}
func (u *unaryOp) String() string { return u.op + "(" + u.x.String() + ")" }

type binaryOp struct {
	op   string
	l, r expr
	t    *btpb.Type
	like *regexp.Regexp // a LIKE with a constant pattern
}

// assignable is whether a value of type from can be used as to: equal, an
// INT64 as a FLOAT, or a STRING as BYTES (which the reference says is
// implicit).
func assignable(from, to *btpb.Type) bool {
	if from == nil || sameType(from, to) {
		return true
	}
	if isKind(from, "INT64") && isKind(to, "FLOAT32", "FLOAT64") || isKind(from, "FLOAT32") && isKind(to, "FLOAT64") {
		return true
	}
	return isKind(from, "STRING") && isKind(to, "BYTES")
}

// unify is the common supertype of a and b.
func unify(a, b *btpb.Type) (*btpb.Type, bool) {
	switch {
	case assignable(a, b):
		return b, true
	case assignable(b, a):
		return a, true
	}
	return nil, false
}

func (b *binaryOp) check(s *scope) (*btpb.Type, error) {
	lt, err := b.l.check(s)
	if err != nil {
		return nil, err
	}
	rt, err := b.r.check(s)
	if err != nil {
		return nil, err
	}
	if l, ok := b.l.(*literal); ok && l.t == nil {
		lt = rt
	}
	if r, ok := b.r.(*literal); ok && r.t == nil {
		rt = lt
	}
	bad := func() (*btpb.Type, error) {
		return nil, fmt.Errorf("No matching signature for operator %s for argument types: %s, %s", b.op, kind(lt), kind(rt))
	}
	switch b.op {
	case "AND", "OR":
		if !isKind(lt, "BOOL") || !isKind(rt, "BOOL") {
			return bad()
		}
		b.t = boolType
	case "=", "!=", "<", "<=", ">", ">=":
		if _, ok := unify(lt, rt); !ok || isKind(lt, "MAP<", "STRUCT<") || (b.op != "=" && b.op != "!=" && isKind(lt, "ARRAY<")) {
			return bad()
		}
		b.t = boolType
	case "LIKE":
		u, ok := unify(lt, rt)
		if !ok || !isKind(u, "STRING", "BYTES") {
			return bad()
		}
		if r, ok := b.r.(*literal); ok && r.v != nil {
			b.like, err = likeRegexp(r.v)
			if err != nil {
				return nil, err
			}
		}
		b.t = boolType
	case "+", "-", "*", "/":
		if !isNumeric(lt) || !isNumeric(rt) {
			if (b.op == "+" || b.op == "-") && isKind(lt, "TIMESTAMP") {
				return nil, fmt.Errorf("use TIMESTAMP_ADD or TIMESTAMP_SUB to add an interval to a TIMESTAMP")
			}
			return bad()
		}
		switch {
		case b.op == "/" || isKind(lt, "FLOAT64") || isKind(rt, "FLOAT64"):
			b.t = float64Type
		case isKind(lt, "FLOAT32") || isKind(rt, "FLOAT32"):
			b.t = float64Type
		default:
			b.t = int64Type
		}
	case "||":
		u, ok := unify(lt, rt)
		if !ok || !isKind(u, "STRING", "BYTES", "ARRAY<") {
			return bad()
		}
		b.t = u
	case "&", "|", "^", "<<", ">>":
		if isKind(lt, "INT64") && isKind(rt, "INT64") || isKind(lt, "BYTES") && (isKind(rt, "BYTES") || b.op == "<<" || b.op == ">>") {
			b.t = lt
		} else {
			return bad()
		}
	}
	return b.t, nil
}

func (b *binaryOp) eval(e *env) (any, error) {
	l, err := b.l.eval(e)
	if err != nil {
		return nil, err
	}
	// AND and OR use three-valued logic and stop early.
	switch b.op {
	case "AND":
		if l == false {
			return false, nil
		}
		r, err := b.r.eval(e)
		if err != nil {
			return nil, err
		}
		if r == false {
			return false, nil
		}
		if l == nil || r == nil {
			return nil, nil
		}
		return true, nil
	case "OR":
		if l == true {
			return true, nil
		}
		r, err := b.r.eval(e)
		if err != nil {
			return nil, err
		}
		if r == true {
			return true, nil
		}
		if l == nil || r == nil {
			return nil, nil
		}
		return false, nil
	}
	r, err := b.r.eval(e)
	if err != nil || l == nil || r == nil {
		return nil, err
	}
	switch b.op {
	case "=", "!=", "<", "<=", ">", ">=":
		if la, ok := l.([]any); ok {
			eq := equalValues(la, r)
			return eq == (b.op == "="), nil
		}
		c, ok := compare(l, r)
		if !ok {
			return nil, nil // NaN
		}
		switch b.op {
		case "=":
			return c == 0, nil
		case "!=":
			return c != 0, nil
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		}
		return c >= 0, nil
	case "LIKE":
		re := b.like
		if re == nil {
			if re, err = likeRegexp(r); err != nil {
				return nil, err
			}
		}
		return re.Match(asBytes(l)), nil
	case "||":
		switch lv := l.(type) {
		case string:
			if rs, ok := r.(string); ok && isKind(b.t, "STRING") {
				return lv + rs, nil
			}
			return append([]byte(lv), asBytes(r)...), nil
		case []byte:
			return append(append([]byte{}, lv...), asBytes(r)...), nil
		case []any:
			return append(append([]any{}, lv...), r.([]any)...), nil
		}
	case "+", "-", "*", "/":
		return arith(b.op, l, r, isKind(b.t, "INT64"))
	case "&", "|", "^", "<<", ">>":
		return bitwise(b.op, l, r)
	}
	return nil, fmt.Errorf("bad operands for %s", b.op)
}
func (b *binaryOp) String() string { return "(" + b.l.String() + " " + b.op + " " + b.r.String() + ")" }

func arith(op string, l, r any, integer bool) (any, error) {
	if integer {
		a, b := l.(int64), r.(int64)
		switch op {
		case "+":
			s := a + b
			if (s > a) != (b > 0) {
				return nil, fmt.Errorf("int64 overflow: %d + %d", a, b)
			}
			return s, nil
		case "-":
			s := a - b
			if (s < a) != (b > 0) {
				return nil, fmt.Errorf("int64 overflow: %d - %d", a, b)
			}
			return s, nil
		case "*":
			if a != 0 && b != 0 {
				s := a * b
				if s/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
					return nil, fmt.Errorf("int64 overflow: %d * %d", a, b)
				}
				return s, nil
			}
			return int64(0), nil
		}
	}
	a, b := toFloat(l), toFloat(r)
	switch op {
	case "+":
		return a + b, nil
	case "-":
		return a - b, nil
	case "*":
		return a * b, nil
	}
	if b == 0 {
		return nil, fmt.Errorf("division by zero: %v / %v", a, b)
	}
	return a / b, nil
}

func bitwise(op string, l, r any) (any, error) {
	if a, ok := l.(int64); ok {
		b := r.(int64)
		switch op {
		case "&":
			return a & b, nil
		case "|":
			return a | b, nil
		case "^":
			return a ^ b, nil
		case "<<":
			if b < 0 {
				return nil, fmt.Errorf("Bitwise shift by negative offset")
			}
			if b >= 64 {
				return int64(0), nil
			}
			return a << uint(b), nil
		}
		if b < 0 {
			return nil, fmt.Errorf("Bitwise shift by negative offset")
		}
		if b >= 64 {
			return int64(0), nil
		}
		return int64(uint64(a) >> uint(b)), nil
	}
	a := l.([]byte)
	if op == "<<" || op == ">>" {
		return nil, unsupportedErr{"shifting BYTES"}
	}
	b := r.([]byte)
	if len(a) != len(b) {
		return nil, fmt.Errorf("Bitwise binary operator for BYTES requires equal length of the inputs. Got %d bytes on the left hand side and %d bytes on the right hand side.", len(a), len(b))
	}
	out := make([]byte, len(a))
	for i := range a {
		switch op {
		case "&":
			out[i] = a[i] & b[i]
		case "|":
			out[i] = a[i] | b[i]
		default:
			out[i] = a[i] ^ b[i]
		}
	}
	return out, nil
}

func likeRegexp(pat any) (*regexp.Regexp, error) {
	p := asBytes(pat)
	var b strings.Builder
	b.WriteString(`(?s)\A`)
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		case '\\':
			if i+1 == len(p) {
				return nil, fmt.Errorf("LIKE pattern ends with a backslash")
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

type isExpr struct {
	x    expr
	not  bool
	what string
}

func (i *isExpr) check(s *scope) (*btpb.Type, error) {
	t, err := i.x.check(s)
	if err != nil {
		return nil, err
	}
	if i.what != "NULL" && !isKind(t, "BOOL") {
		return nil, fmt.Errorf("IS %s takes BOOL, not %s", i.what, kind(t))
	}
	return boolType, nil
}
func (i *isExpr) eval(e *env) (any, error) {
	v, err := i.x.eval(e)
	if err != nil {
		return nil, err
	}
	var r bool
	switch i.what {
	case "NULL":
		r = v == nil
		// An unused family, an empty map, reads as NULL (the reference).
		if m, ok := v.(*mapValue); ok && len(m.keys) == 0 {
			r = true
		}
	case "TRUE":
		r = v == true
	case "FALSE":
		r = v == false
	}
	return r != i.not, nil
}
func (i *isExpr) String() string { return fmt.Sprintf("is(%s,%v,%s)", i.x, i.not, i.what) }

type inExpr struct {
	x     expr
	list  []expr
	array expr
	not   bool
}

func (in *inExpr) check(s *scope) (*btpb.Type, error) {
	xt, err := in.x.check(s)
	if err != nil {
		return nil, err
	}
	if in.array != nil {
		at, err := in.array.check(s)
		if err != nil {
			return nil, err
		}
		if !isKind(at, "ARRAY<") {
			return nil, fmt.Errorf("IN UNNEST takes an ARRAY, not %s", kind(at))
		}
		if _, ok := unify(xt, at.GetArrayType().GetElementType()); !ok {
			return nil, fmt.Errorf("No matching signature for IN UNNEST: %s, %s", kind(xt), kind(at))
		}
		return boolType, nil
	}
	for _, e := range in.list {
		t, err := e.check(s)
		if err != nil {
			return nil, err
		}
		if _, ok := unify(xt, t); !ok {
			if l, isLit := e.(*literal); !isLit || l.t != nil {
				return nil, fmt.Errorf("No matching signature for operator IN for argument types %s and {%s}", kind(xt), kind(t))
			}
		}
	}
	return boolType, nil
}
func (in *inExpr) eval(e *env) (any, error) {
	x, err := in.x.eval(e)
	if err != nil {
		return nil, err
	}
	var vals []any
	if in.array != nil {
		a, err := in.array.eval(e)
		if err != nil {
			return nil, err
		}
		vals, _ = a.([]any)
	} else {
		for _, l := range in.list {
			v, err := l.eval(e)
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
		}
	}
	if x == nil {
		if len(vals) == 0 {
			return in.not, nil
		}
		return nil, nil
	}
	sawNull := false
	for _, v := range vals {
		if v == nil {
			sawNull = true
			continue
		}
		if c, ok := compare(x, v); ok && c == 0 {
			return !in.not, nil
		}
	}
	if sawNull {
		return nil, nil
	}
	return in.not, nil
}
func (in *inExpr) String() string { return fmt.Sprintf("in(%s,%v,%v)", in.x, in.list, in.not) }

type caseExpr struct {
	operand     expr
	whens, thens []expr
	els         expr
	t           *btpb.Type
}

func (c *caseExpr) check(s *scope) (*btpb.Type, error) {
	var ot *btpb.Type
	if c.operand != nil {
		t, err := c.operand.check(s)
		if err != nil {
			return nil, err
		}
		ot = t
	}
	results := append([]expr{}, c.thens...)
	if c.els != nil {
		results = append(results, c.els)
	}
	for _, w := range c.whens {
		t, err := w.check(s)
		if err != nil {
			return nil, err
		}
		if ot != nil {
			if _, ok := unify(ot, t); !ok {
				return nil, fmt.Errorf("CASE: WHEN of type %s does not match %s", kind(t), kind(ot))
			}
		} else if !isKind(t, "BOOL") {
			return nil, fmt.Errorf("CASE: WHEN takes BOOL, not %s", kind(t))
		}
	}
	t, err := commonType(s, results, "CASE")
	c.t = t
	return t, err
}

// commonType checks es and returns their supertype, NULLs aside.
func commonType(s *scope, es []expr, what string) (*btpb.Type, error) {
	var out *btpb.Type
	for _, e := range es {
		t, err := e.check(s)
		if err != nil {
			return nil, err
		}
		if l, ok := e.(*literal); ok && l.t == nil {
			continue
		}
		if out == nil {
			out = t
			continue
		}
		u, ok := unify(out, t)
		if !ok {
			return nil, fmt.Errorf("No matching signature for %s: %s and %s have no common supertype", what, kind(out), kind(t))
		}
		out = u
	}
	if out == nil {
		out = int64Type
	}
	return out, nil
}

func (c *caseExpr) eval(e *env) (any, error) {
	var ov any
	if c.operand != nil {
		v, err := c.operand.eval(e)
		if err != nil {
			return nil, err
		}
		ov = v
	}
	for i, w := range c.whens {
		wv, err := w.eval(e)
		if err != nil {
			return nil, err
		}
		match := false
		if c.operand != nil {
			if ov != nil && wv != nil {
				cmp, ok := compare(ov, wv)
				match = ok && cmp == 0
			}
		} else {
			match = wv == true
		}
		if match {
			v, err := c.thens[i].eval(e)
			return coerce(v, c.t), err
		}
	}
	if c.els != nil {
		v, err := c.els.eval(e)
		return coerce(v, c.t), err
	}
	return nil, nil
}
func (c *caseExpr) String() string { return fmt.Sprintf("case(%v,%v,%v,%v)", c.operand, c.whens, c.thens, c.els) }

// --- values ---

func asBytes(v any) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	}
	return nil
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return math.NaN()
}

// coerce converts v to type t where that is implicit.
func coerce(v any, t *btpb.Type) any {
	switch x := v.(type) {
	case int64:
		if isKind(t, "FLOAT32", "FLOAT64") {
			return float64(x)
		}
	case string:
		if isKind(t, "BYTES") {
			return []byte(x)
		}
	}
	return v
}

// compare orders two non-NULL values of comparable types; ok is false for
// a NaN.
func compare(a, b any) (int, bool) {
	switch x := a.(type) {
	case []byte:
		return bytes.Compare(x, asBytes(b)), true
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y), true
		}
		return bytes.Compare([]byte(x), asBytes(b)), true
	case int64:
		if y, ok := b.(int64); ok {
			switch {
			case x < y:
				return -1, true
			case x > y:
				return 1, true
			}
			return 0, true
		}
	case bool:
		y := b.(bool)
		switch {
		case x == y:
			return 0, true
		case !x:
			return -1, true
		}
		return 1, true
	case time.Time:
		y := b.(time.Time)
		return x.Compare(y), true
	}
	fa, fb := toFloat(a), toFloat(b)
	if math.IsNaN(fa) || math.IsNaN(fb) {
		return 0, false
	}
	switch {
	case fa < fb:
		return -1, true
	case fa > fb:
		return 1, true
	}
	return 0, true
}

func equalValues(a []any, b any) bool {
	bb, ok := b.([]any)
	if !ok || len(a) != len(bb) {
		return false
	}
	for i := range a {
		if a[i] == nil || bb[i] == nil {
			if a[i] != bb[i] {
				return false
			}
			continue
		}
		if c, ok := compare(a[i], bb[i]); !ok || c != 0 {
			return false
		}
	}
	return true
}

// groupKey is a value's identity for GROUP BY, DISTINCT and COUNT(DISTINCT).
func groupKey(v any) string {
	switch x := v.(type) {
	case nil:
		return "n"
	case []byte:
		return "b" + string(x)
	case string:
		return "s" + x
	case int64:
		return fmt.Sprintf("i%d", x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return fmt.Sprintf("i%d", int64(x))
		}
		return fmt.Sprintf("f%v", x)
	case bool:
		return fmt.Sprintf("t%v", x)
	case time.Time:
		return fmt.Sprintf("d%d", x.UnixNano())
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = groupKey(e)
		}
		return "a[" + strings.Join(parts, "\x00") + "]"
	case *mapValue:
		return "m" + groupKey(x.keys) + groupKey(x.vals)
	}
	return fmt.Sprintf("%v", v)
}

// groupable is whether GROUP BY and DISTINCT take a value of type t.
func groupable(t *btpb.Type) bool {
	return !isKind(t, "MAP<", "FLOAT32", "FLOAT64") && (!isKind(t, "ARRAY<") || groupable(t.GetArrayType().GetElementType())) &&
		(!isKind(t, "STRUCT<") || func() bool {
			for _, f := range t.GetStructType().GetFields() {
				if !groupable(f.GetType()) {
					return false
				}
			}
			return true
		}())
}

// sortValues sorts vs ascending, NULLs first.
func sortValues(vs []any) {
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i] == nil {
			return vs[j] != nil
		}
		if vs[j] == nil {
			return false
		}
		c, _ := compare(vs[i], vs[j])
		return c < 0
	})
}
