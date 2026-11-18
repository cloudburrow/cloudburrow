package iamcond

// Lint (#1751) is the IAM API's iamPolicies.lintPolicy over one binding's
// condition, with the eleven validation units Google's "Validate allow
// policies" page lists (docs.cloud.google.com/iam/docs/linting-policies,
// read 2026-11-17), each with the page's severity and, where the page
// shows one, its message and location offset:
//
//   - ConditionCompileCheck (ERROR): the expression does not parse or
//     type-check. A parse error is ANTLR's message, as Google's CEL parser
//     gives it ("ERROR: Parse expression:1:4: token recognition error at:
//     '=f'" and the line, with a caret under the column); when it fails,
//     every other unit is reported NOTICE, "The validation unit is skipped
//     due to absence of a required object: CheckedExpr";
//   - ConditionComplexityCheck (ERROR): more than 12 logical operators;
//   - DateTimeCheck (WARNING): request.time compared with a timestamp that
//     is in the past or malformed, so the comparison is always true or
//     false;
//   - DateTimeRangeCheck (WARNING): a time function compared with a value
//     outside its range (getMonth [0, 11], ...);
//   - DrgGetAttributeDefaultValueCheck (ERROR): a default other than [] for
//     api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', V);
//   - EffectiveTimeRangeCheck (WARNING): comparisons of one time function,
//     joined by &&, whose ranges do not meet (or, joined by ||, cover its
//     whole range);
//   - HasOnlyListConstCheck and HasOnlyListLengthCheck (ERROR): hasOnly()
//     of a list that is not all constants, or of more than 10 elements;
//   - ResourceServiceLiteralCheck and ResourceTypeLiteralCheck (WARNING):
//     resource.service or resource.type compared with a value Google's
//     "Resource attributes for IAM Conditions" does not list;
//   - RestrictedAttributesCheck (WARNING): request.auth.access_levels, the
//     attribute the page names as restricted.
//
// Results are sorted by severity, highest first, as the reference says.

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The validation units, in the order Google's page lists them.
const (
	UnitConditionCompile       = "ConditionCompileCheck"
	UnitConditionComplexity    = "ConditionComplexityCheck"
	UnitDateTime               = "DateTimeCheck"
	UnitDateTimeRange          = "DateTimeRangeCheck"
	UnitDrgGetAttributeDefault = "DrgGetAttributeDefaultValueCheck"
	UnitEffectiveTimeRange     = "EffectiveTimeRangeCheck"
	UnitHasOnlyListConst       = "HasOnlyListConstCheck"
	UnitHasOnlyListLength      = "HasOnlyListLengthCheck"
	UnitResourceServiceLiteral = "ResourceServiceLiteralCheck"
	UnitResourceTypeLiteral    = "ResourceTypeLiteralCheck"
	UnitRestrictedAttributes   = "RestrictedAttributesCheck"
)

// LintUnits are the validation units, in Google's order.
var LintUnits = []string{
	UnitConditionCompile, UnitConditionComplexity, UnitDateTime, UnitDateTimeRange,
	UnitDrgGetAttributeDefault, UnitEffectiveTimeRange, UnitHasOnlyListConst,
	UnitHasOnlyListLength, UnitResourceServiceLiteral, UnitResourceTypeLiteral,
	UnitRestrictedAttributes,
}

// The severities a unit reports, as LintResult.Severity names them.
const (
	SeverityError   = "ERROR"
	SeverityWarning = "WARNING"
	SeverityNotice  = "NOTICE"
)

// LintResult is one finding: Offset is the 0-based character offset in the
// expression, -1 when the unit gives none.
type LintResult struct {
	Unit     string
	Severity string
	Offset   int
	Message  string
}

// skipped is the message of a unit that needs a compiled expression.
const skipped = "The validation unit is skipped due to absence of a required object: CheckedExpr"

// Lint lints a condition's expression at time now.
func Lint(src string, now time.Time) []LintResult {
	runeOff := func(pos int) int {
		if pos < 0 {
			return -1
		}
		if pos > len(src) {
			pos = len(src)
		}
		return utf8.RuneCountInString(src[:pos])
	}
	p := &parser{src: src}
	n, err := p.parse()
	if err == nil {
		err = checkBool(n)
	}
	if err != nil {
		off, msg := compileMessage(src, err)
		out := []LintResult{{Unit: UnitConditionCompile, Severity: SeverityError, Offset: runeOff(off), Message: msg}}
		for _, u := range LintUnits[1:] {
			out = append(out, LintResult{Unit: u, Severity: SeverityNotice, Offset: -1, Message: skipped})
		}
		return out
	}
	l := &linter{now: now}
	if p.logical > MaxLogicalOperators {
		l.add(UnitConditionComplexity, SeverityError, -1, fmt.Sprintf("Logical operators count must not be more than %d", MaxLogicalOperators))
	}
	l.walk(n, "")
	for i := range l.out {
		l.out[i].Offset = runeOff(l.out[i].Offset)
	}
	rank := map[string]int{SeverityError: 0, SeverityWarning: 1, SeverityNotice: 2}
	sort.SliceStable(l.out, func(i, j int) bool {
		a, b := l.out[i], l.out[j]
		if rank[a.Severity] != rank[b.Severity] {
			return rank[a.Severity] < rank[b.Severity]
		}
		return slices.Index(LintUnits, a.Unit) < slices.Index(LintUnits, b.Unit)
	})
	return l.out
}

var columnRE = regexp.MustCompile(` ?\(column (\d+)\)`)

// compileMessage is ConditionCompileCheck's offset (bytes, -1 for none)
// and message for err: a syntax error as Google's parser words it, with
// the line and a caret under the column.
func compileMessage(src string, err error) (int, string) {
	var pos int
	var msg string
	if se, ok := err.(*SyntaxError); ok {
		pos, msg = se.Pos, se.ANTLR
	} else {
		msg = err.Error()
		pos = -1
		if m := columnRE.FindStringSubmatch(msg); m != nil {
			c, _ := strconv.Atoi(m[1])
			pos = c - 1
			msg = strings.TrimSpace(columnRE.ReplaceAllString(msg, ""))
		}
		if pos < 0 {
			return -1, "ERROR: " + msg
		}
	}
	if pos > len(src) {
		pos = len(src)
	}
	start := strings.LastIndexByte(src[:pos], '\n') + 1
	end := strings.IndexByte(src[pos:], '\n')
	if end < 0 {
		end = len(src)
	} else {
		end += pos
	}
	line := strings.Count(src[:pos], "\n") + 1
	col := utf8.RuneCountInString(src[start:pos])
	return pos, fmt.Sprintf("ERROR: Parse expression:%d:%d: %s\n  | %s\n  | %s^", line, col, msg, src[start:end], strings.Repeat(".", col))
}

type linter struct {
	now time.Time
	out []LintResult
}

func (l *linter) add(unit, severity string, pos int, msg string) {
	l.out = append(l.out, LintResult{Unit: unit, Severity: severity, Offset: pos, Message: msg})
}

// children are n's operands.
func children(n node) []node {
	switch x := n.(type) {
	case *selectN:
		return []node{x.x}
	case *list:
		return x.elems
	case *mapLit:
		return append(slices.Clone(x.keys), x.vals...)
	case *index:
		return []node{x.x, x.key}
	case *not:
		return []node{x.x}
	case *logical:
		return []node{x.l, x.r}
	case *binary:
		return []node{x.l, x.r}
	case *call:
		if x.recv != nil {
			return append([]node{x.recv}, x.args...)
		}
		return x.args
	}
	return nil
}

// nodePos is where CEL places a node: a literal or name at its start, a
// field selection at its ".", a call at its "(", a list at its "[", an
// operator at the operator.
func nodePos(n node) int {
	switch x := n.(type) {
	case *lit:
		return x.pos
	case *ident:
		return x.pos
	case *selectN:
		return x.dot
	case *list:
		return x.pos
	case *mapLit:
		return x.pos
	case *index:
		return x.pos
	case *not:
		return x.pos
	case *logical:
		return x.pos
	case *binary:
		return x.pos
	case *call:
		return x.open
	}
	return -1
}

// walk runs the units on n and below; parentOp is the logical operator
// of n's parent, so a chain of && or || is examined once, at its top.
func (l *linter) walk(n node, parentOp string) {
	switch x := n.(type) {
	case *logical:
		if x.op != parentOp {
			l.timeRanges(x)
		}
		l.walk(x.l, x.op)
		l.walk(x.r, x.op)
		return
	case *binary:
		l.dateTime(x)
		l.dateTimeRange(x)
		l.literal(x)
	case *call:
		l.hasOnly(x)
		if pos, ok := drgDefaultCall(x); ok {
			l.add(UnitDrgGetAttributeDefault, SeverityError, pos,
				"api.getAttribute call on 'iam.googleapis.com/modifiedGrantsByRole' can only accept empty list ('[]') as default value.")
		}
	case *selectN:
		if a := x.attr(); restrictedAttributes[a] {
			l.add(UnitRestrictedAttributes, SeverityWarning, x.dot,
				"Condition attribute `"+a+"` is restricted or unsupported. Please check https://cloud.google.com/iam/docs/conditions-overview for the full list of supported attributes")
		}
	}
	for _, c := range children(n) {
		l.walk(c, "")
	}
}

// restrictedAttributes are the attributes RestrictedAttributesCheck
// warns of: Google's page names request.auth.access_levels.
var restrictedAttributes = map[string]bool{"request.auth.access_levels": true}

// drgDefault is the position of the first
// api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', V) whose V
// is not [], which setIamPolicy refuses.
func drgDefault(n node) (int, bool) {
	if c, ok := n.(*call); ok {
		if pos, ok := drgDefaultCall(c); ok {
			return pos, true
		}
	}
	for _, c := range children(n) {
		if pos, ok := drgDefault(c); ok {
			return pos, true
		}
	}
	return 0, false
}

func drgDefaultCall(c *call) (int, bool) {
	if c.namespaceFn() != "api.getAttribute" || len(c.args) != 2 || c.argString(0) != modifiedGrantsByRole {
		return 0, false
	}
	if l, ok := c.args[1].(*list); ok && len(l.elems) == 0 {
		return 0, false
	}
	return nodePos(c.args[1]), true
}

const modifiedGrantsByRole = "iam.googleapis.com/modifiedGrantsByRole"

// hasOnly runs HasOnlyListLengthCheck and HasOnlyListConstCheck.
func (l *linter) hasOnly(c *call) {
	if c.fn != "hasOnly" || c.recv != nil && c.namespaceFn() != "" || c.recv == nil || len(c.args) != 1 {
		return
	}
	arg := c.args[0]
	lst, ok := arg.(*list)
	if ok && len(lst.elems) > 10 {
		l.add(UnitHasOnlyListLength, SeverityError, c.open, "The list argument to hasOnly() cannot have more than 10 elements")
	}
	bad := -1
	if !ok {
		bad = nodePos(arg)
	} else {
		for _, e := range lst.elems {
			if _, isLit := e.(*lit); !isLit {
				bad = nodePos(e)
				break
			}
		}
	}
	if bad >= 0 {
		l.add(UnitHasOnlyListConst, SeverityError, bad,
			"hasOnly() expects an argument of type list containing only const values, but a non-const expression was found in the list.")
	}
}

// flip is a comparison with its operands swapped.
var flip = map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<=", "==": "==", "!=": "!="}

func isComparison(op string) bool { _, ok := flip[op]; return ok }

// dateTime runs DateTimeCheck on request.time compared with timestamp()
// or date() of a literal.
func (l *linter) dateTime(b *binary) {
	if !isComparison(b.op) {
		return
	}
	op, other := b.op, b.r
	if !isAttr(b.l, "request.time") {
		if !isAttr(b.r, "request.time") {
			return
		}
		op, other = flip[b.op], b.l
	}
	c, ok := other.(*call)
	if !ok || c.recv != nil || (c.fn != "timestamp" && c.fn != "date") || len(c.args) != 1 {
		return
	}
	arg, ok := c.args[0].(*lit)
	if !ok {
		return
	}
	s, _ := arg.v.(string)
	layout := time.RFC3339Nano
	if c.fn == "date" {
		layout = "2006-01-02"
	}
	t, err := time.Parse(layout, s)
	if err != nil {
		l.add(UnitDateTime, SeverityWarning, arg.pos, fmt.Sprintf("Invalid date time value parsed from %q; condition is effectively False.", s))
		return
	}
	if !t.Before(l.now) {
		return
	}
	effect := "False"
	if op == ">" || op == ">=" || op == "!=" {
		effect = "True"
	}
	l.add(UnitDateTime, SeverityWarning, arg.pos, fmt.Sprintf("Ineffective date time value %s parsed from \"%s\"; condition is effectively %s. Time expired already.", isoformat(t), s, effect))
}

// isoformat writes t as Python's datetime.isoformat does, as Google's
// message shows it ("2000-01-01T00:00:00+00:00").
func isoformat(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + t.Format("-07:00")
}

func isAttr(n node, attr string) bool {
	s, ok := n.(*selectN)
	return ok && s.attr() == attr
}

// timeRange is each time function's valid values (Google's attribute
// reference); getFullYear has no bound.
var timeRange = map[string][2]int64{
	"getDate":         {1, 31},
	"getDayOfMonth":   {0, 30},
	"getDayOfWeek":    {0, 6},
	"getDayOfYear":    {0, 365},
	"getHours":        {0, 23},
	"getMilliseconds": {0, 999},
	"getMinutes":      {0, 59},
	"getMonth":        {0, 11},
	"getSeconds":      {0, 59},
}

// timeCmp is a time function compared with an integer literal: the
// function, its time zone argument's source, the operator with the
// function on the left, and the value.
type timeCmp struct {
	fn, zone string
	op       string
	v        int64
	litPos   int
}

func asTimeCmp(n node) (timeCmp, bool) {
	b, ok := n.(*binary)
	if !ok || !isComparison(b.op) {
		return timeCmp{}, false
	}
	op, fnSide, valSide := b.op, b.l, b.r
	if _, ok := fnSide.(*call); !ok {
		op, fnSide, valSide = flip[b.op], b.r, b.l
	}
	c, ok := fnSide.(*call)
	if !ok || c.recv == nil {
		return timeCmp{}, false
	}
	if _, ok := timeRange[c.fn]; !ok {
		return timeCmp{}, false
	}
	v, ok := valSide.(*lit)
	if !ok {
		return timeCmp{}, false
	}
	iv, ok := v.v.(int64)
	if !ok {
		return timeCmp{}, false
	}
	zone := ""
	if len(c.args) == 1 {
		zone = c.argString(0)
	}
	return timeCmp{fn: c.fn, zone: zone, op: op, v: iv, litPos: v.pos}, true
}

// dateTimeRange runs DateTimeRangeCheck.
func (l *linter) dateTimeRange(b *binary) {
	tc, ok := asTimeCmp(b)
	if !ok {
		return
	}
	r := timeRange[tc.fn]
	if tc.v < r[0] || tc.v > r[1] {
		l.add(UnitDateTimeRange, SeverityWarning, tc.litPos, fmt.Sprintf("The value being compared to the specified timestamp function (%s) must be in range [%d, %d].", tc.fn, r[0], r[1]))
	}
}

// interval is the values [lo, hi] a comparison allows, within the
// function's range; != gives two.
func (tc timeCmp) intervals() [][2]int64 {
	r := timeRange[tc.fn]
	lo, hi := r[0], r[1]
	switch tc.op {
	case "<":
		hi = min(hi, tc.v-1)
	case "<=":
		hi = min(hi, tc.v)
	case ">":
		lo = max(lo, tc.v+1)
	case ">=":
		lo = max(lo, tc.v)
	case "==":
		lo, hi = max(lo, tc.v), min(hi, tc.v)
	case "!=":
		return [][2]int64{{lo, min(hi, tc.v-1)}, {max(lo, tc.v+1), hi}}
	}
	return [][2]int64{{lo, hi}}
}

// timeRanges runs EffectiveTimeRangeCheck on a chain of && (an empty
// range) or of || (the whole range), over the comparisons of each time
// function and zone that appears at least twice in it.
func (l *linter) timeRanges(top *logical) {
	var terms []node
	var flat func(n node)
	flat = func(n node) {
		if x, ok := n.(*logical); ok && x.op == top.op {
			flat(x.l)
			flat(x.r)
			return
		}
		terms = append(terms, n)
	}
	flat(top)
	type group struct {
		fn   string
		cmps []timeCmp
	}
	var keys []string
	groups := map[string]*group{}
	for _, t := range terms {
		tc, ok := asTimeCmp(t)
		if !ok {
			continue
		}
		k := tc.fn + "\x00" + tc.zone
		if groups[k] == nil {
			groups[k] = &group{fn: tc.fn}
			keys = append(keys, k)
		}
		groups[k].cmps = append(groups[k].cmps, tc)
	}
	var fns []string
	for _, k := range keys {
		g := groups[k]
		if len(g.cmps) < 2 {
			continue
		}
		r := timeRange[g.fn]
		if top.op == "&&" {
			// The values every comparison allows.
			allowed := [][2]int64{{r[0], r[1]}}
			for _, tc := range g.cmps {
				var next [][2]int64
				for _, a := range allowed {
					for _, b := range tc.intervals() {
						if lo, hi := max(a[0], b[0]), min(a[1], b[1]); lo <= hi {
							next = append(next, [2]int64{lo, hi})
						}
					}
				}
				allowed = next
			}
			if len(allowed) == 0 && !slices.Contains(fns, g.fn) {
				fns = append(fns, g.fn)
			}
			continue
		}
		var all [][2]int64
		for _, tc := range g.cmps {
			for _, iv := range tc.intervals() {
				if iv[0] <= iv[1] {
					all = append(all, iv)
				}
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i][0] < all[j][0] })
		next := r[0]
		for _, iv := range all {
			if iv[0] > next {
				break
			}
			next = max(next, iv[1]+1)
		}
		if next > r[1] && !slices.Contains(fns, g.fn) {
			fns = append(fns, g.fn)
		}
	}
	if len(fns) == 0 {
		return
	}
	what := "empty"
	if top.op == "||" {
		what = "full"
	}
	l.add(UnitEffectiveTimeRange, SeverityWarning, -1, fmt.Sprintf("The aggregate of time functions [%s] results in %s ranges.", strings.Join(fns, ", "), what))
}

// literal runs ResourceServiceLiteralCheck and ResourceTypeLiteralCheck on
// resource.service or resource.type compared (== or !=) with a string.
func (l *linter) literal(b *binary) {
	if b.op != "==" && b.op != "!=" {
		return
	}
	for _, side := range [][2]node{{b.l, b.r}, {b.r, b.l}} {
		s, ok := side[0].(*selectN)
		if !ok {
			continue
		}
		v, ok := side[1].(*lit)
		if !ok {
			continue
		}
		str, ok := v.v.(string)
		if !ok {
			continue
		}
		switch a := s.attr(); a {
		case "resource.service":
			if !ResourceServices[str] {
				l.add(UnitResourceServiceLiteral, SeverityWarning, v.pos, "resource.service : "+str+" is not supported. Using this value in condition may lead to unintended consequences. Check user guide at https://cloud.google.com/iam/docs/conditions-resource-attributes#resource_service_values for supported values for resource.service.")
			}
		case "resource.type":
			if !ResourceTypes[str] {
				l.add(UnitResourceTypeLiteral, SeverityWarning, v.pos, "resource.type : "+str+" is not supported. Using this value in condition may lead to unintended consequences. Check user guide at https://cloud.google.com/iam/docs/conditions-resource-attributes#resource_type_values for supported values for resource.type.")
			}
		}
	}
}
