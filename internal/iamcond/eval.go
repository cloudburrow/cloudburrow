package iamcond

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type node interface {
	check() (ty, error)
	eval(a *Attributes) (any, error)
}

var errUnset = errors.New("attribute not available on this resource")

type lit struct {
	v any
	t ty
}

func (n *lit) check() (ty, error)            { return n.t, nil }
func (n *lit) eval(*Attributes) (any, error) { return n.v, nil }

type ident struct {
	name string
	pos  int
}

func (n *ident) check() (ty, error) {
	switch n.name {
	case "api", "destination", "principal", "request", "resource", "compute":
		return "", fmt.Errorf("%s is not an attribute by itself (column %d); name one of its fields or functions", n.name, n.pos+1)
	}
	return "", fmt.Errorf("undeclared reference to %q (column %d)", n.name, n.pos+1)
}
func (n *ident) eval(*Attributes) (any, error) { return nil, errUnset }

// selectN is an attribute: resource.name, resource.type, resource.service,
// request.time, request.path, request.host, request.auth.access_levels,
// destination.ip or destination.port.
type selectN struct {
	x     node
	field string
	pos   int
}

func (n *selectN) attr() string {
	switch x := n.x.(type) {
	case *ident:
		return x.name + "." + n.field
	case *selectN:
		if a := x.attr(); a != "" {
			return a + "." + n.field
		}
	}
	return ""
}

// attrTypes are the attributes a role binding's condition may read.
var attrTypes = map[string]ty{
	"resource.name":              tyString,
	"resource.type":              tyString,
	"resource.service":           tyString,
	"request.time":               tyTimestamp,
	"request.path":               tyString,
	"request.host":               tyString,
	"request.auth.access_levels": tyList,
	"destination.ip":             tyString,
	"destination.port":           tyInt,
}

func (n *selectN) check() (ty, error) {
	a := n.attr()
	if t, ok := attrTypes[a]; ok {
		return t, nil
	}
	switch {
	case a == "request.auth":
		return "", fmt.Errorf("request.auth has no value of its own (column %d); the attribute is request.auth.access_levels", n.pos+1)
	case a == "principal.type" || a == "principal.subject":
		return "", fmt.Errorf("%s is a Principal Access Boundary policy attribute; an allow policy's role binding cannot use it (column %d)", a, n.pos+1)
	case a == "":
		return "", fmt.Errorf("unsupported field selection .%s (column %d)", n.field, n.pos+1)
	}
	return "", fmt.Errorf("undefined field '%s' (column %d): %s is not an IAM Conditions attribute", n.field, n.pos+1, a)
}

func (n *selectN) eval(a *Attributes) (any, error) {
	var s string
	switch n.attr() {
	case "resource.name":
		s = a.ResourceName
	case "resource.type":
		s = a.ResourceType
	case "resource.service":
		s = a.ResourceService
	case "request.time":
		if a.RequestTime.IsZero() {
			return time.Now().UTC(), nil
		}
		return a.RequestTime.UTC(), nil
	case "request.path":
		s = a.RequestPath
	case "request.host":
		s = a.RequestHost
	case "destination.ip":
		s = a.DestinationIP
	case "destination.port":
		if a.DestinationPort == 0 {
			return nil, errUnset
		}
		return a.DestinationPort, nil
	case "request.auth.access_levels":
		if a.AccessLevels == nil {
			return nil, errUnset
		}
		return strList(a.AccessLevels), nil
	}
	if s == "" {
		return nil, errUnset
	}
	return s, nil
}

type list struct{ elems []node }

func (n *list) check() (ty, error) {
	for _, e := range n.elems {
		if _, err := e.check(); err != nil {
			return "", err
		}
	}
	return tyList, nil
}

func (n *list) eval(a *Attributes) (any, error) {
	out := make([]any, len(n.elems))
	for i, e := range n.elems {
		v, err := e.eval(a)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

type not struct{ x node }

func (n *not) check() (ty, error) {
	t, err := n.x.check()
	if err != nil {
		return "", err
	}
	if t != tyBool {
		return "", fmt.Errorf("found no matching overload for '!_' applied to (%s)", t)
	}
	return tyBool, nil
}

func (n *not) eval(a *Attributes) (any, error) {
	v, err := n.x.eval(a)
	if err != nil {
		return nil, err
	}
	return !v.(bool), nil
}

type logical struct {
	op   string
	l, r node
}

func (n *logical) check() (ty, error) {
	for _, x := range []node{n.l, n.r} {
		t, err := x.check()
		if err != nil {
			return "", err
		}
		if t != tyBool {
			return "", fmt.Errorf("found no matching overload for '_%s_' applied to (%s)", n.op, t)
		}
	}
	return tyBool, nil
}

// eval is CEL's commutative logic: an error on one side is absorbed when
// the other side decides the result.
func (n *logical) eval(a *Attributes) (any, error) {
	l, lerr := n.l.eval(a)
	r, rerr := n.r.eval(a)
	decide := n.op == "||" // true decides ||, false decides &&
	if lerr == nil && l.(bool) == decide {
		return decide, nil
	}
	if rerr == nil && r.(bool) == decide {
		return decide, nil
	}
	if lerr != nil {
		return nil, lerr
	}
	if rerr != nil {
		return nil, rerr
	}
	return !decide, nil
}

type binary struct {
	op   string
	l, r node
	pos  int
}

func (n *binary) check() (ty, error) {
	lt, err := n.l.check()
	if err != nil {
		return "", err
	}
	rt, err := n.r.check()
	if err != nil {
		return "", err
	}
	bad := fmt.Errorf("found no matching overload for '_%s_' applied to (%s, %s) (column %d)", n.op, lt, rt, n.pos+1)
	switch n.op {
	case "==", "!=":
		if lt != rt || lt == tyList {
			return "", bad
		}
		return tyBool, nil
	case "<", "<=", ">", ">=":
		if lt != rt || (lt != tyInt && lt != tyString && lt != tyTimestamp && lt != tyDuration) {
			return "", bad
		}
		return tyBool, nil
	case "in":
		if rt != tyList {
			return "", bad
		}
		return tyBool, nil
	case "+":
		switch {
		case lt == tyTimestamp && rt == tyDuration, lt == tyDuration && rt == tyTimestamp:
			return tyTimestamp, nil
		case lt == tyDuration && rt == tyDuration:
			return tyDuration, nil
		case lt == tyInt && rt == tyInt:
			return tyInt, nil
		case lt == tyString && rt == tyString:
			return tyString, nil
		}
	case "-":
		switch {
		case lt == tyTimestamp && rt == tyDuration:
			return tyTimestamp, nil
		case lt == tyTimestamp && rt == tyTimestamp:
			return tyDuration, nil
		case lt == tyDuration && rt == tyDuration:
			return tyDuration, nil
		case lt == tyInt && rt == tyInt:
			return tyInt, nil
		}
	}
	return "", bad
}

func (n *binary) eval(a *Attributes) (any, error) {
	l, err := n.l.eval(a)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(a)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "==":
		return equal(l, r), nil
	case "!=":
		return !equal(l, r), nil
	case "in":
		for _, e := range r.([]any) {
			if equal(l, e) {
				return true, nil
			}
		}
		return false, nil
	case "<", "<=", ">", ">=":
		c := compare(l, r)
		switch n.op {
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		}
		return c >= 0, nil
	case "+":
		switch lv := l.(type) {
		case time.Time:
			return lv.Add(r.(time.Duration)), nil
		case time.Duration:
			if rt, ok := r.(time.Time); ok {
				return rt.Add(lv), nil
			}
			return lv + r.(time.Duration), nil
		case int64:
			return lv + r.(int64), nil
		case string:
			return lv + r.(string), nil
		}
	case "-":
		switch lv := l.(type) {
		case time.Time:
			if rt, ok := r.(time.Time); ok {
				return lv.Sub(rt), nil
			}
			return lv.Add(-r.(time.Duration)), nil
		case time.Duration:
			return lv - r.(time.Duration), nil
		case int64:
			return lv - r.(int64), nil
		}
	}
	return nil, fmt.Errorf("no overload for %s", n.op)
}

func equal(l, r any) bool {
	if lt, ok := l.(time.Time); ok {
		rt, ok := r.(time.Time)
		return ok && lt.Equal(rt)
	}
	return l == r
}

func compare(l, r any) int {
	switch lv := l.(type) {
	case int64:
		rv := r.(int64)
		return cmp3(lv < rv, lv > rv)
	case string:
		return strings.Compare(lv, r.(string))
	case time.Time:
		return lv.Compare(r.(time.Time))
	case time.Duration:
		rv := r.(time.Duration)
		return cmp3(lv < rv, lv > rv)
	}
	return 0
}

func cmp3(lt, gt bool) int {
	switch {
	case lt:
		return -1
	case gt:
		return 1
	}
	return 0
}

// call is a function: timestamp(), duration(), date(), or a method of
// resource.name or request.time.
type call struct {
	fn   string
	recv node
	args []node
	pos  int
}

// timeGetters are request.time's extraction functions, with what each
// returns of a time in its zone, as Google's attribute reference gives
// their ranges.
var timeGetters = map[string]func(t time.Time) int64{
	"getDate":         func(t time.Time) int64 { return int64(t.Day()) },
	"getDayOfMonth":   func(t time.Time) int64 { return int64(t.Day() - 1) },
	"getDayOfWeek":    func(t time.Time) int64 { return int64(t.Weekday()) },
	"getDayOfYear":    func(t time.Time) int64 { return int64(t.YearDay() - 1) },
	"getFullYear":     func(t time.Time) int64 { return int64(t.Year()) },
	"getHours":        func(t time.Time) int64 { return int64(t.Hour()) },
	"getMilliseconds": func(t time.Time) int64 { return int64(t.Nanosecond() / 1e6) },
	"getMinutes":      func(t time.Time) int64 { return int64(t.Minute()) },
	"getMonth":        func(t time.Time) int64 { return int64(t.Month() - 1) },
	"getSeconds":      func(t time.Time) int64 { return int64(t.Second()) },
}

// namespaceFn is the function a call on a bare namespace names
// (resource.matchTag, api.getAttribute, compute.*), or "".
func (n *call) namespaceFn() string {
	if id, ok := n.recv.(*ident); ok {
		return id.name + "." + n.fn
	}
	return ""
}

// checkNamespace types a call on a bare namespace; api.getAttribute is
// its default's type.
func (n *call) checkNamespace(ns string, ats []ty, sig func() error) (ty, error) {
	strs := func(k int) bool {
		if len(ats) != k {
			return false
		}
		for _, t := range ats {
			if t != tyString {
				return false
			}
		}
		return true
	}
	switch ns {
	case "resource.hasTagKey", "resource.hasTagKeyId":
		if !strs(1) {
			return "", sig()
		}
		return tyBool, nil
	case "resource.matchTag", "resource.matchTagId":
		if !strs(2) {
			return "", sig()
		}
		return tyBool, nil
	case "api.getAttribute":
		if len(ats) != 2 || ats[0] != tyString {
			return "", sig()
		}
		if l, ok := n.args[1].(*list); ok && len(l.elems) != 0 && n.argString(0) == "iam.googleapis.com/modifiedGrantsByRole" {
			return "", fmt.Errorf("api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', ...)'s default must be [] (column %d)", n.pos+1)
		}
		return ats[1], nil
	case "compute.isForwardingRuleCreationOperation":
		if len(ats) != 0 {
			return "", sig()
		}
		return tyBool, nil
	case "compute.matchLoadBalancingSchemes":
		if len(ats) != 1 || ats[0] != tyList {
			return "", sig()
		}
		return tyBool, nil
	}
	return "", fmt.Errorf("undeclared reference to '%s' (column %d)", ns, n.pos+1)
}

// argString is the i'th argument when it is a string literal.
func (n *call) argString(i int) string {
	if l, ok := n.args[i].(*lit); ok {
		if s, ok := l.v.(string); ok {
			return s
		}
	}
	return ""
}

func (n *call) check() (ty, error) {
	var rt ty
	ns := n.namespaceFn()
	if n.recv != nil && ns == "" {
		t, err := n.recv.check()
		if err != nil {
			return "", err
		}
		rt = t
	}
	var ats []ty
	for _, a := range n.args {
		t, err := a.check()
		if err != nil {
			return "", err
		}
		ats = append(ats, t)
	}
	sig := func() error {
		var parts []string
		if n.recv != nil {
			parts = append(parts, string(rt))
		}
		for _, t := range ats {
			parts = append(parts, string(t))
		}
		return fmt.Errorf("found no matching overload for '%s' applied to (%s) (column %d)", n.fn, strings.Join(parts, ", "), n.pos+1)
	}
	one := func(t ty) bool { return len(ats) == 1 && ats[0] == t }
	if ns != "" {
		return n.checkNamespace(ns, ats, sig)
	}
	switch {
	case n.recv == nil && (n.fn == "timestamp" || n.fn == "date"):
		if !one(tyString) {
			return "", sig()
		}
		return tyTimestamp, nil
	case n.recv == nil && n.fn == "duration":
		if !one(tyString) {
			return "", sig()
		}
		return tyDuration, nil
	case n.recv != nil && (n.fn == "startsWith" || n.fn == "endsWith"):
		if rt != tyString || !one(tyString) {
			return "", sig()
		}
		return tyBool, nil
	case n.recv != nil && n.fn == "extract":
		if rt != tyString || !one(tyString) {
			return "", sig()
		}
		if lit, ok := n.args[0].(*lit); ok {
			if err := checkTemplate(lit.v.(string)); err != nil {
				return "", err
			}
		}
		return tyString, nil
	case n.recv != nil && n.fn == "hasOnly":
		if rt != tyList || !one(tyList) {
			return "", sig()
		}
		return tyBool, nil
	case n.recv != nil && timeGetters[n.fn] != nil:
		if rt != tyTimestamp || len(ats) > 1 || (len(ats) == 1 && ats[0] != tyString) {
			return "", sig()
		}
		return tyInt, nil
	}
	if n.recv == nil {
		return "", fmt.Errorf("undeclared reference to '%s' (column %d)", n.fn, n.pos+1)
	}
	return "", sig()
}

func (n *call) eval(a *Attributes) (any, error) {
	if ns := n.namespaceFn(); ns != "" {
		return n.evalNamespace(ns, a)
	}
	var recv any
	if n.recv != nil {
		v, err := n.recv.eval(a)
		if err != nil {
			return nil, err
		}
		recv = v
	}
	args := make([]any, len(n.args))
	for i, x := range n.args {
		v, err := x.eval(a)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	switch n.fn {
	case "timestamp":
		return time.Parse(time.RFC3339Nano, args[0].(string))
	case "date":
		return time.Parse("2006-01-02", args[0].(string))
	case "duration":
		return parseDuration(args[0].(string))
	case "startsWith":
		return strings.HasPrefix(recv.(string), args[0].(string)), nil
	case "endsWith":
		return strings.HasSuffix(recv.(string), args[0].(string)), nil
	case "extract":
		return extract(recv.(string), args[0].(string))
	case "hasOnly":
		for _, v := range recv.([]any) {
			found := false
			for _, w := range args[0].([]any) {
				if equal(v, w) {
					found = true
					break
				}
			}
			if !found {
				return false, nil
			}
		}
		return true, nil
	}
	t := recv.(time.Time)
	if len(args) == 1 {
		loc, err := zone(args[0].(string))
		if err != nil {
			return nil, err
		}
		t = t.In(loc)
	}
	return timeGetters[n.fn](t), nil
}

// zone is a time zone argument: an IANA name, or a UTC offset such as
// "+05:30" or "-08:00".
func zone(z string) (*time.Location, error) {
	if len(z) == 6 && (z[0] == '+' || z[0] == '-') && z[3] == ':' {
		h, err1 := strconv.Atoi(z[1:3])
		m, err2 := strconv.Atoi(z[4:6])
		if err1 == nil && err2 == nil {
			off := h*3600 + m*60
			if z[0] == '-' {
				off = -off
			}
			return time.FixedZone(z, off), nil
		}
	}
	return time.LoadLocation(z)
}

// parseDuration is duration()'s argument: seconds followed by "s", as
// Google's reference documents ("90s"), fractions allowed.
func parseDuration(s string) (time.Duration, error) {
	num, ok := strings.CutSuffix(s, "s")
	if !ok || num == "" {
		return 0, fmt.Errorf("duration %q is not a number of seconds followed by s", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("duration %q is not a number of seconds followed by s", s)
	}
	return time.Duration(f * float64(time.Second)), nil
}

// checkTemplate is extract()'s template: exactly one {name} placeholder.
func checkTemplate(tpl string) error {
	open := strings.Count(tpl, "{")
	if open != 1 || strings.Count(tpl, "}") != 1 || strings.Index(tpl, "{") > strings.Index(tpl, "}") {
		return fmt.Errorf("extract template %q must contain exactly one {placeholder}", tpl)
	}
	return nil
}

// extract is resource.name.extract(template), as Google's reference
// describes it: the text the template's placeholder matches, found by the
// text before the placeholder and ended by the text after it (or the end
// of the name when the placeholder ends the template); "" when the name
// does not contain the text before it.
func extract(name, tpl string) (string, error) {
	if err := checkTemplate(tpl); err != nil {
		return "", err
	}
	i := strings.Index(tpl, "{")
	j := strings.Index(tpl, "}")
	before, after := tpl[:i], tpl[j+1:]
	k := strings.Index(name, before)
	if k < 0 {
		return "", nil
	}
	rest := name[k+len(before):]
	if after == "" {
		return rest, nil
	}
	e := strings.Index(rest, after)
	if e < 0 {
		return "", nil
	}
	return rest[:e], nil
}

// evalNamespace evaluates resource.*, api.* and compute.* functions.
func (n *call) evalNamespace(ns string, a *Attributes) (any, error) {
	args := make([]any, len(n.args))
	for i, x := range n.args {
		v, err := x.eval(a)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	switch ns {
	case "resource.hasTagKey", "resource.hasTagKeyId", "resource.matchTag", "resource.matchTagId":
		if !a.TagsKnown {
			return nil, errUnset
		}
		for _, t := range a.Tags {
			var ok bool
			switch ns {
			case "resource.hasTagKey":
				ok = t.NamespacedKey == args[0]
			case "resource.hasTagKeyId":
				ok = t.KeyID == args[0]
			case "resource.matchTag":
				ok = t.NamespacedKey == args[0] && t.ShortValue == args[1]
			case "resource.matchTagId":
				ok = t.KeyID == args[0] && t.ValueID == args[1]
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case "api.getAttribute":
		v, ok := a.API[args[0].(string)]
		if !ok {
			return args[1], nil
		}
		if l, ok := v.([]string); ok {
			return strList(l), nil
		}
		return v, nil
	case "compute.isForwardingRuleCreationOperation":
		if a.ForwardingRule == nil {
			return nil, errUnset
		}
		return a.ForwardingRule.Creation, nil
	case "compute.matchLoadBalancingSchemes":
		if a.ForwardingRule == nil {
			return nil, errUnset
		}
		for _, s := range a.ForwardingRule.Schemes {
			for _, w := range args[0].([]any) {
				if s == w {
					return true, nil
				}
			}
		}
		return false, nil
	}
	return nil, fmt.Errorf("no function %s", ns)
}

func strList(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}
