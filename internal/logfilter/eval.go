package logfilter

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type node interface{ eval(Record) bool }

type andNode []node
type orNode []node
type notNode struct{ n node }
type funcNode func(Record) bool

func (a andNode) eval(r Record) bool {
	for _, n := range a {
		if !n.eval(r) {
			return false
		}
	}
	return true
}

func (o orNode) eval(r Record) bool {
	for _, n := range o {
		if n.eval(r) {
			return true
		}
	}
	return false
}

func (n notNode) eval(r Record) bool  { return !n.n.eval(r) }
func (f funcNode) eval(r Record) bool { return f(r) }

// value is one typed field value.
type value struct {
	k    vkind
	s    string
	n    float64
	b    bool
	t    time.Time
	d    time.Duration
	null bool
	obj  any // a nested object or list, for ":" (some subfield has it)
}

// SeverityLevels are LogSeverity's names and numbers.
var SeverityLevels = map[string]int{
	"DEFAULT": 0, "DEBUG": 100, "INFO": 200, "NOTICE": 300, "WARNING": 400,
	"ERROR": 500, "CRITICAL": 600, "ALERT": 700, "EMERGENCY": 800,
}

func severityName(n int) string {
	for k, v := range SeverityLevels {
		if v == n {
			return k
		}
	}
	return strconv.Itoa(n)
}

// lit is a right-hand value, converted up front to each type it might be
// compared as.
type lit struct {
	text   string
	quoted bool
	null   bool
	fold   string
	re     *regexp.Regexp
	num    float64
	numOK  bool
	b      bool
	bOK    bool
	t      time.Time
	tOK    bool
	sev    int
	sevOK  bool
	d      time.Duration
	dOK    bool
}

func newLit(t token) *lit {
	v := t.val
	l := &lit{text: v, quoted: t.kind == tString, fold: fold(v)}
	if !l.quoted && v == "NULL_VALUE" {
		l.null = true
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		l.num, l.numOK = f, true
	} else if lv := strings.ToLower(v); lv == "nan" || lv == "infinity" || lv == "-infinity" {
		l.num, l.numOK = map[string]float64{"nan": math.NaN(), "infinity": math.Inf(1), "-infinity": math.Inf(-1)}[lv], true
	}
	switch strings.ToLower(v) {
	case "true":
		l.b, l.bOK = true, true
	case "false":
		l.bOK = true
	}
	l.t, l.tOK = parseTime(v)
	if n, ok := SeverityLevels[strings.ToUpper(v)]; ok {
		l.sev, l.sevOK = n, true
	} else if n, err := strconv.Atoi(v); err == nil {
		l.sev, l.sevOK = n, true
	}
	if d, err := time.ParseDuration(v); err == nil {
		l.d, l.dOK = d, true
	}
	return l
}

var timeLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700", "2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02"}

// parseTime reads RFC 3339 or ISO 8601; a time with no zone, and a date
// alone (midnight), are UTC.
func parseTime(s string) (time.Time, bool) {
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// fold is the language's case-insensitive form of a string (NFKC_CF).
func fold(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	return norm.NFKC.String(cases.Fold().String(norm.NFKC.String(s)))
}

// rhsNode is a comparison's right side: one value, or values joined by NOT,
// OR and AND, each compared with the field (field = ("a" OR "b")).
type rhsNode interface{ match(v value, op string) bool }

type rhsAnd []rhsNode
type rhsOr []rhsNode
type rhsNot struct{ n rhsNode }

func (a rhsAnd) match(v value, op string) bool {
	for _, n := range a {
		if !n.match(v, op) {
			return false
		}
	}
	return true
}

func (o rhsOr) match(v value, op string) bool {
	for _, n := range o {
		if n.match(v, op) {
			return true
		}
	}
	return false
}

func (n rhsNot) match(v value, op string) bool { return !n.n.match(v, op) }

func ord(c int, op string) bool {
	switch op {
	case "=", ":":
		return c == 0
	case "!=":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

func cmp[T int | float64 | int64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// match compares one field value with the literal; a literal that does not
// convert to the field's type fails the comparison, as the language says.
func (l *lit) match(v value, op string) bool {
	if l.null || v.null {
		if l.null && v.null {
			return op == "=" || op == ":"
		}
		if l.null {
			return op == "!="
		}
		return false
	}
	if op == "=~" || op == "!~" {
		if v.k != kString {
			return false
		}
		return l.re.MatchString(v.s) == (op == "=~")
	}
	switch v.k {
	case kString:
		got := fold(v.s)
		if op == ":" {
			return strings.Contains(got, l.fold)
		}
		return ord(strings.Compare(got, l.fold), op)
	case kNumber:
		if !l.numOK {
			return false
		}
		if math.IsNaN(v.n) || math.IsNaN(l.num) {
			return op == "!="
		}
		return ord(cmp(v.n, l.num), op)
	case kBool:
		if !l.bOK {
			return false
		}
		a, b := 0, 0
		if v.b {
			a = 1
		}
		if l.b {
			b = 1
		}
		return ord(cmp(a, b), op)
	case kTime:
		if !l.tOK {
			return false
		}
		return ord(v.t.Compare(l.t), op)
	case kSeverity:
		if !l.sevOK {
			return false
		}
		return ord(cmp(int(v.n), l.sev), op)
	case kDuration:
		if !l.dOK {
			return false
		}
		return ord(cmp(int64(v.d), int64(l.d)), op)
	}
	// An object or list: ":" asks whether some value inside has it.
	if op != ":" {
		return false
	}
	found := false
	walkLeaves(v.obj, nil, func(_ []string, x any) bool {
		if s, ok := leafString(x); ok && strings.Contains(fold(s), l.fold) {
			found = true
		}
		return !found
	})
	return found
}

// lhsExpr is a comparison's left side.
type lhsExpr interface {
	// values are the field's values; exists is false when the field is
	// missing (every comparison fails); explicit is whether the entry has it
	// (":*"), not merely as a default.
	values(Record) (vals []value, exists, explicit bool)
}

type pathLHS struct{ fi fieldInfo }

func (p pathLHS) values(r Record) ([]value, bool, bool) {
	raws, found := lookup(r, p.fi)
	if !found {
		if p.fi.free || p.fi.typ == kAny {
			return nil, false, false
		}
		return []value{zero(p.fi.typ)}, true, false
	}
	var out []value
	for _, raw := range raws {
		if v, ok := toValue(raw, p.fi.typ); ok {
			out = append(out, v)
		}
	}
	return out, true, true
}

func zero(k vkind) value {
	v := value{k: k}
	if k == kTime {
		v.t = time.Unix(0, 0).UTC()
	}
	return v
}

// lookup walks a path through the record; repeated fields yield each element.
func lookup(r Record, fi fieldInfo) ([]any, bool) {
	top, ok := r.Field(fi.path[0])
	if !ok || top == nil && len(fi.path) > 1 {
		return nil, false
	}
	cur := []any{top}
	for _, seg := range fi.path[1:] {
		var next []any
		for _, c := range flatten(cur) {
			if v, ok := child(c, seg, fi.proto); ok {
				next = append(next, v)
			}
		}
		if len(next) == 0 {
			return nil, false
		}
		cur = next
	}
	return flatten(cur), true
}

func flatten(vs []any) []any {
	var out []any
	for _, v := range vs {
		if l, ok := v.([]any); ok {
			out = append(out, flatten(l)...)
			continue
		}
		out = append(out, v)
	}
	return out
}

func child(c any, seg string, ci bool) (any, bool) {
	switch m := c.(type) {
	case map[string]any:
		if v, ok := m[seg]; ok {
			return v, true
		}
		if ci {
			for k, v := range m {
				if strings.EqualFold(k, seg) {
					return v, true
				}
			}
		}
	case map[string]string:
		if v, ok := m[seg]; ok {
			return v, true
		}
	}
	return nil, false
}

// toValue types a raw JSON-shaped value as its field's type.
func toValue(raw any, k vkind) (value, bool) {
	if raw == nil {
		return value{null: true}, true
	}
	switch k {
	case kString:
		s, ok := leafString(raw)
		return value{k: kString, s: s}, ok
	case kNumber:
		f, ok := number(raw)
		return value{k: kNumber, n: f}, ok
	case kBool:
		switch b := raw.(type) {
		case bool:
			return value{k: kBool, b: b}, true
		case string:
			pb, err := strconv.ParseBool(b)
			return value{k: kBool, b: pb}, err == nil
		}
		return value{}, false
	case kTime:
		switch t := raw.(type) {
		case time.Time:
			return value{k: kTime, t: t}, true
		case string:
			pt, ok := parseTime(t)
			return value{k: kTime, t: pt}, ok
		}
		return value{}, false
	case kSeverity:
		if s, ok := raw.(string); ok {
			if n, ok := SeverityLevels[strings.ToUpper(s)]; ok {
				return value{k: kSeverity, n: float64(n)}, true
			}
		}
		f, ok := number(raw)
		return value{k: kSeverity, n: f}, ok
	case kDuration:
		switch d := raw.(type) {
		case time.Duration:
			return value{k: kDuration, d: d}, true
		case string:
			pd, err := time.ParseDuration(d)
			return value{k: kDuration, d: pd}, err == nil
		}
		return value{}, false
	}
	switch x := raw.(type) {
	case string:
		return value{k: kString, s: x}, true
	case bool:
		return value{k: kBool, b: x}, true
	case time.Time:
		return value{k: kTime, t: x}, true
	case time.Duration:
		return value{k: kDuration, d: x}, true
	case map[string]any, map[string]string, []any:
		return value{obj: x}, true
	}
	if f, ok := number(raw); ok {
		return value{k: kNumber, n: f}, true
	}
	return value{}, false
}

func number(raw any) (float64, bool) {
	switch n := raw.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// leafString is a scalar's text.
func leafString(x any) (string, bool) {
	switch v := x.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano), true
	case nil, map[string]any, map[string]string, []any:
		return "", false
	}
	if f, ok := number(x); ok {
		return strconv.FormatFloat(f, 'f', -1, 64), true
	}
	return fmt.Sprint(x), true
}

// walkLeaves visits every scalar under x with its path; visit returns false
// to stop.
func walkLeaves(x any, path []string, visit func([]string, any) bool) bool {
	switch v := x.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !walkLeaves(v[k], append(path, k), visit) {
				return false
			}
		}
		return true
	case map[string]string:
		for k, s := range v {
			if !visit(append(path, k), s) {
				return false
			}
		}
		return true
	case []any:
		for _, e := range v {
			if !walkLeaves(e, path, visit) {
				return false
			}
		}
		return true
	}
	return visit(path, x)
}

type cmpNode struct {
	lhs lhsExpr
	op  string
	rhs rhsNode
}

func (c cmpNode) eval(r Record) bool {
	vals, exists, _ := c.lhs.values(r)
	if !exists {
		return false
	}
	for _, v := range vals {
		if c.rhs.match(v, c.op) {
			return true
		}
	}
	return false
}

type presenceNode struct{ lhs lhsExpr }

func (p presenceNode) eval(r Record) bool {
	_, _, explicit := p.lhs.values(r)
	return explicit
}

// globalNode is a bare value: some field of the entry has it.
type globalNode struct{ l *lit }

func (g globalNode) eval(r Record) bool {
	return g.l.match(value{obj: r.Root()}, ":")
}

type castLHS struct {
	inner lhsExpr
	to    string
	loc   *time.Location
}

func (c castLHS) values(r Record) ([]value, bool, bool) {
	vals, exists, explicit := c.inner.values(r)
	var out []value
	for _, v := range vals {
		if cv, ok := castValue(v, c.to, c.loc); ok {
			out = append(out, cv)
		}
	}
	return out, exists, explicit
}

func valueString(v value, loc *time.Location) (string, bool) {
	switch {
	case v.null:
		return "", false
	case v.k == kString:
		return v.s, true
	case v.k == kNumber:
		return strconv.FormatFloat(v.n, 'f', -1, 64), true
	case v.k == kBool:
		return strconv.FormatBool(v.b), true
	case v.k == kTime:
		return v.t.In(loc).Format(time.RFC3339Nano), true
	case v.k == kSeverity:
		return severityName(int(v.n)), true
	case v.k == kDuration:
		return strconv.FormatFloat(v.d.Seconds(), 'f', -1, 64) + "s", true
	}
	b, err := json.Marshal(v.obj)
	return string(b), err == nil
}

func castValue(v value, to string, loc *time.Location) (value, bool) {
	switch to {
	case "STRING":
		s, ok := valueString(v, loc)
		return value{k: kString, s: s}, ok
	case "INT64", "FLOAT64":
		var f float64
		switch v.k {
		case kNumber, kSeverity:
			f = v.n
		case kBool:
			if v.b {
				f = 1
			}
		case kString:
			n, err := strconv.ParseFloat(strings.TrimSpace(v.s), 64)
			if err != nil {
				return value{}, false
			}
			f = n
		default:
			return value{}, false
		}
		if to == "INT64" {
			f = math.Trunc(f)
		}
		return value{k: kNumber, n: f}, true
	case "BOOL":
		switch v.k {
		case kBool:
			return v, true
		case kNumber:
			return value{k: kBool, b: v.n != 0}, true
		case kString:
			b, err := strconv.ParseBool(strings.TrimSpace(v.s))
			return value{k: kBool, b: b}, err == nil
		}
		return value{}, false
	case "TIMESTAMP":
		switch v.k {
		case kTime:
			return v, true
		case kString:
			if t, err := time.ParseInLocation(time.RFC3339Nano, v.s, loc); err == nil {
				return value{k: kTime, t: t}, true
			}
			t, ok := parseTime(v.s)
			return value{k: kTime, t: t}, ok
		}
		return value{}, false
	default: // DURATION
		switch v.k {
		case kDuration:
			return v, true
		case kString:
			d, err := time.ParseDuration(v.s)
			return value{k: kDuration, d: d}, err == nil
		case kNumber:
			return value{k: kDuration, d: time.Duration(v.n * float64(time.Second))}, true
		}
		return value{}, false
	}
}

type extractLHS struct {
	inner lhsExpr
	re    *regexp.Regexp
}

func (x extractLHS) values(r Record) ([]value, bool, bool) {
	vals, exists, explicit := x.inner.values(r)
	var out []value
	for _, v := range vals {
		s, ok := valueString(v, time.UTC)
		if !ok {
			continue
		}
		if m := x.re.FindStringSubmatch(s); m != nil {
			out = append(out, value{k: kString, s: m[1]})
		}
	}
	return out, exists, explicit
}

// logIDNode is log_id("ID"): the entry's log ID, not URL-encoded.
type logIDNode struct{ id string }

func (l logIDNode) eval(r Record) bool {
	raw, _ := r.Field("logName")
	name, _ := raw.(string)
	_, enc, ok := strings.Cut(name, "/logs/")
	if !ok {
		return false
	}
	if id, err := url.PathUnescape(enc); err == nil {
		enc = id
	}
	return enc == l.id
}

// sampleNode is sample(FIELD, FRACTION): a deterministic hash of the field
// keeps that fraction of entries; an entry missing the field is excluded.
type sampleNode struct {
	fi       fieldInfo
	fraction float64
}

func (s sampleNode) eval(r Record) bool {
	vals, exists, _ := pathLHS{s.fi}.values(r)
	if !exists || len(vals) == 0 {
		return false
	}
	str, _ := valueString(vals[0], time.UTC)
	h := fnv.New64a()
	h.Write([]byte(str))
	return float64(h.Sum64())/math.Exp2(64) < s.fraction
}

func parseNet(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ipNode is ip_in_net(FIELD, SUBNET): the field's address or range lies in
// the subnet; a missing or malformed address is false.
type ipNode struct {
	fi     fieldInfo
	subnet netip.Prefix
}

func (n ipNode) eval(r Record) bool {
	vals, exists, _ := pathLHS{n.fi}.values(r)
	if !exists {
		return false
	}
	for _, v := range vals {
		s, ok := valueString(v, time.UTC)
		if !ok {
			continue
		}
		p, err := parseNet(strings.TrimSpace(s))
		if err != nil || p.Addr().Is4() != n.subnet.Addr().Is4() {
			continue
		}
		if p.Bits() >= n.subnet.Bits() && n.subnet.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// searchNode is SEARCH([FIELD,] "QUERY"): every token of the query, and every
// `backquoted phrase`, appears among the text's tokens, ignoring case;
// tokens are whole words, not substrings.
type searchNode struct {
	field   *fieldInfo
	words   []string
	phrases [][]string
}

func textTokens(s string) []string {
	return strings.FieldsFunc(fold(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func newSearch(field *fieldInfo, q string) searchNode {
	n := searchNode{field: field}
	for i := 0; i < len(q); {
		if q[i] == '`' {
			j := strings.IndexByte(q[i+1:], '`')
			if j < 0 {
				j = len(q) - i - 1
			}
			if ph := textTokens(q[i+1 : i+1+j]); len(ph) > 0 {
				n.phrases = append(n.phrases, ph)
			}
			i += j + 2
			continue
		}
		j := strings.IndexByte(q[i:], '`')
		if j < 0 {
			j = len(q) - i
		}
		n.words = append(n.words, textTokens(q[i:i+j])...)
		i += j
	}
	return n
}

func (n searchNode) eval(r Record) bool {
	var texts []string
	collect := func(path []string, x any) bool {
		if len(path) > 0 {
			if t, ok := schema[strings.Join(path, ".")]; ok && t != kString && t != kAny && t != free {
				return true
			}
		}
		if s, ok := x.(string); ok {
			texts = append(texts, s)
		}
		return true
	}
	if n.field == nil {
		walkLeaves(r.Root(), nil, collect)
	} else {
		raws, ok := lookup(r, *n.field)
		if !ok {
			return false
		}
		for _, x := range raws {
			walkLeaves(x, append([]string{}, n.field.path...), collect)
		}
	}
	have := map[string]bool{}
	var toks [][]string
	for _, s := range texts {
		ts := textTokens(s)
		toks = append(toks, ts)
		for _, t := range ts {
			have[t] = true
		}
	}
	for _, w := range n.words {
		if !have[w] {
			return false
		}
	}
	for _, ph := range n.phrases {
		if !hasPhrase(toks, ph) {
			return false
		}
	}
	return len(n.words)+len(n.phrases) > 0
}

func hasPhrase(texts [][]string, ph []string) bool {
	for _, ts := range texts {
		for i := 0; i+len(ph) <= len(ts); i++ {
			ok := true
			for j := range ph {
				if ts[i+j] != ph[j] {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}
