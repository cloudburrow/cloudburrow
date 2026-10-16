package secrets

// List filters (#1228), as docs.cloud.google.com/secret-manager/docs/filtering
// documents them; documented, not measured against Google:
//
//   - a restriction is FIELD OP VALUE, OP one of = != < > <= >= and :
//     (":" is a case-insensitive substring match; FIELD:* means the field
//     is set);
//   - restrictions combine with AND (or a space), OR and NOT (or "-"); OR
//     binds tighter than AND, and parentheses group;
//   - a value may be quoted, and a parenthesised value list such as
//     state:(ENABLED OR DISABLED) matches any of its values;
//   - times are RFC 3339 (2021-01-01T06:00:00Z) or a date (2021-07-31,
//     midnight UTC); durations are seconds such as 259200s.
//
// Fields are a secret's name, labels.KEY, annotations.KEY, create_time,
// expire_time, replication.automatic,
// replication.user_managed.replicas.location, topics, rotation,
// rotation.next_rotation_time, rotation.rotation_period and
// version_destroy_ttl (#1278), plus the documented ones no secret here can
// carry (customer-managed encryption, version aliases), which are never set; and a version's name, state,
// create_time and destroy_time. Anything else, including gcloud's "~"
// regular expressions, which the API itself does not take, is refused
// UNIMPLEMENTED naming it, never ignored: an ignored filter would return
// everything and look like a filter that matched it all.

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// filterValue is one field of a resource as a filter sees it. A field that
// is not set has set=false; a repeated field has several strs.
type filterValue struct {
	set  bool
	strs []string
	t    time.Time
	d    time.Duration
	kind byte // 's' string, 't' time, 'd' duration
}

// filterNode is a parsed filter: a restriction, or a combination of nodes.
type filterNode struct {
	op       string // "AND", "OR", "NOT" or "" for a restriction
	children []*filterNode

	field  string
	cmp    string
	values []string // a value list is OR-ed
}

// listFilter is a compiled filter; nil matches everything.
type listFilter struct {
	root   *filterNode
	lookup func(r any, field string) (filterValue, bool)
}

// matches reports whether r passes the filter.
func (f *listFilter) matches(r any) bool {
	if f == nil {
		return true
	}
	return f.eval(f.root, r)
}

func (f *listFilter) eval(n *filterNode, r any) bool {
	switch n.op {
	case "AND":
		for _, c := range n.children {
			if !f.eval(c, r) {
				return false
			}
		}
		return true
	case "OR":
		for _, c := range n.children {
			if f.eval(c, r) {
				return true
			}
		}
		return false
	case "NOT":
		return !f.eval(n.children[0], r)
	}
	v, _ := f.lookup(r, n.field)
	if n.cmp == "!=" {
		return !restrictionMatches(v, "=", n.values)
	}
	return restrictionMatches(v, n.cmp, n.values)
}

func restrictionMatches(v filterValue, cmp string, values []string) bool {
	for _, want := range values {
		if valueMatches(v, cmp, want) {
			return true
		}
	}
	return false
}

func valueMatches(v filterValue, cmp, want string) bool {
	if cmp == ":" && want == "*" {
		return v.set
	}
	if !v.set {
		return false
	}
	switch v.kind {
	case 't':
		if cmp == ":" {
			return strings.Contains(strings.ToLower(v.t.UTC().Format(time.RFC3339Nano)), strings.ToLower(want))
		}
		w, _ := parseFilterTime(want)
		return compare(v.t.Compare(w), cmp)
	case 'd':
		w, _ := parseFilterDuration(want)
		c := 0
		if v.d < w {
			c = -1
		} else if v.d > w {
			c = 1
		}
		if cmp == ":" {
			cmp = "="
		}
		return compare(c, cmp)
	}
	for _, s := range v.strs {
		if cmp == ":" {
			if strings.Contains(strings.ToLower(s), strings.ToLower(want)) {
				return true
			}
			continue
		}
		if compare(strings.Compare(s, want), cmp) {
			return true
		}
	}
	return false
}

func compare(c int, cmp string) bool {
	switch cmp {
	case "=":
		return c == 0
	case "<":
		return c < 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	case ">=":
		return c >= 0
	}
	return false
}

func parseFilterTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

func parseFilterDuration(s string) (time.Duration, error) {
	if !strings.HasSuffix(s, "s") {
		return 0, strconv.ErrSyntax
	}
	secs, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// --- fields --------------------------------------------------------------

// unsetSecretFields are documented filter fields of a Secret that this
// service refuses to create (checkCreatable), so no secret ever has them.
var unsetSecretFields = map[string]bool{
	"version_aliases": true,
}

// cmekFilterFields are the documented filter fields of a secret's
// customer-managed keys (#1453), in both casings, each mapped to whether it
// names automatic replication's key.
var cmekFilterFields = map[string]bool{
	"replication.user_managed.replicas.customerManagedEncryption":                false,
	"replication.user_managed.replicas.customerManagedEncryption.kmsKeyName":     false,
	"replication.user_managed.replicas.customer_managed_encryption":              false,
	"replication.user_managed.replicas.customer_managed_encryption.kms_key_name": false,
	"replication.automatic.customerManagedEncryption":                            true,
	"replication.automatic.customer_managed_encryption":                          true,
	"replication.automatic.customerManagedEncryption.kmsKeyName":                 true,
	"replication.automatic.customer_managed_encryption.kms_key_name":             true,
}

// cmekFilterValue is a secret's keys for a cmekFilterFields field.
func cmekFilterValue(s Secret, automatic bool) filterValue {
	if automatic != (s.Replication != "user-managed") {
		return filterValue{kind: 's'}
	}
	var keys []string
	for _, k := range s.KMSKeys {
		if k != "" {
			keys = append(keys, k)
		}
	}
	return strValue(keys...)
}

// secretFieldKind is the value kind of a field, for validating a value
// when the filter is parsed rather than silently matching nothing.
func secretFieldKind(field string) (byte, bool) {
	switch {
	case field == "name", field == "replication.automatic", field == "replication.user_managed.replicas.location",
		strings.HasPrefix(field, "labels."), strings.HasPrefix(field, "annotations."),
		field == "labels", field == "annotations", field == "topics", field == "topics.name":
		return 's', true
	case field == "create_time", field == "expire_time", field == "rotation.next_rotation_time":
		return 't', true
	case field == "rotation":
		return 's', true
	case field == "rotation.rotation_period", field == "version_destroy_ttl":
		return 'd', true
	case unsetSecretFields[field]:
		return 's', true
	}
	if _, ok := cmekFilterFields[field]; ok {
		return 's', true
	}
	return 0, false
}

func versionFieldKind(field string) (byte, bool) {
	switch field {
	case "name", "state":
		return 's', true
	case "create_time", "destroy_time":
		return 't', true
	}
	return 0, false
}

func timeValue(t time.Time) filterValue {
	return filterValue{set: !t.IsZero(), t: t, kind: 't'}
}

func strValue(s ...string) filterValue {
	return filterValue{set: len(s) > 0, strs: s, kind: 's'}
}

func mapValue(m map[string]string, field, prefix string) filterValue {
	if field == strings.TrimSuffix(prefix, ".") {
		// labels:KEY: the map has the key.
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		return strValue(keys...)
	}
	v, ok := m[strings.TrimPrefix(field, prefix)]
	if !ok {
		return filterValue{kind: 's'}
	}
	return strValue(v)
}

func secretField(r any, field string) (filterValue, bool) {
	s := r.(Secret)
	switch {
	case field == "name":
		return strValue(s.Name), true
	case field == "labels" || strings.HasPrefix(field, "labels."):
		return mapValue(s.Labels, field, "labels."), true
	case field == "annotations" || strings.HasPrefix(field, "annotations."):
		return mapValue(s.Annotations, field, "annotations."), true
	case field == "create_time":
		return timeValue(s.Created), true
	case field == "expire_time":
		return timeValue(s.Expire), true
	case field == "replication.automatic":
		if s.Replication == "user-managed" {
			return filterValue{kind: 's'}, true
		}
		return filterValue{set: true, strs: []string{""}, kind: 's'}, true
	case field == "replication.user_managed.replicas.location":
		return strValue(s.ReplicaLocations...), true
	case field == "topics" || field == "topics.name":
		return strValue(s.Topics...), true
	// The rotation and the destruction delay (#1278).
	case field == "rotation":
		if s.NextRotation.IsZero() && s.RotationPeriod == 0 {
			return filterValue{kind: 's'}, true
		}
		return filterValue{set: true, strs: []string{""}, kind: 's'}, true
	case field == "rotation.next_rotation_time":
		return timeValue(s.NextRotation), true
	case field == "rotation.rotation_period":
		return filterValue{set: s.RotationPeriod > 0, d: s.RotationPeriod, kind: 'd'}, true
	case field == "version_destroy_ttl":
		return filterValue{set: s.VersionDestroyTTL > 0, d: s.VersionDestroyTTL, kind: 'd'}, true
	}
	if automatic, ok := cmekFilterFields[field]; ok {
		return cmekFilterValue(s, automatic), true
	}
	return filterValue{}, unsetSecretFields[field]
}

func versionField(r any, field string) (filterValue, bool) {
	v := r.(Version)
	switch field {
	case "name":
		return strValue(v.Name), true
	case "state":
		return strValue(string(v.State)), true
	case "create_time":
		return timeValue(v.Created), true
	case "destroy_time":
		return timeValue(v.Destroyed), true
	}
	return filterValue{}, false
}

// parseSecretFilter compiles a ListSecrets filter; an empty one is nil.
func parseSecretFilter(filter string) (*listFilter, error) {
	return parseFilter(filter, "secret", secretFieldKind, secretField)
}

// parseVersionFilter compiles a ListSecretVersions filter.
func parseVersionFilter(filter string) (*listFilter, error) {
	return parseFilter(filter, "secret version", versionFieldKind, versionField)
}

func parseFilter(filter, what string, kindOf func(string) (byte, bool), lookup func(any, string) (filterValue, bool)) (*listFilter, error) {
	if strings.TrimSpace(filter) == "" {
		return nil, nil
	}
	toks, err := lexFilter(filter)
	if err != nil {
		return nil, err
	}
	p := &filterParser{toks: toks, filter: filter, what: what, kindOf: kindOf}
	root, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, apierror.InvalidArgument("list filter %q: unexpected %q", filter, p.toks[p.pos].text)
	}
	return &listFilter{root: root, lookup: lookup}, nil
}

// --- lexer and parser ----------------------------------------------------

type filterToken struct {
	text   string
	quoted bool
}

func lexFilter(s string) ([]filterToken, error) {
	var out []filterToken
	// After an operator the value runs to a space or parenthesis, so a time
	// such as 2021-01-01T06:00:00Z keeps its colons; a value list keeps
	// that mode until it closes.
	afterOp, inList := false, false
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '(' || c == ')':
			out = append(out, filterToken{text: string(c)})
			if c == '(' && afterOp {
				inList = true
			} else if c == ')' {
				inList = false
			}
			afterOp = false
			i++
		case (afterOp || inList) && c != '"' && c != '\'':
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && s[j] != '(' && s[j] != ')' {
				j++
			}
			out = append(out, filterToken{text: s[i:j]})
			afterOp = false
			i = j
		case c == '"' || c == '\'':
			j := i + 1
			var b strings.Builder
			for j < len(s) && s[j] != c {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, apierror.InvalidArgument("list filter %q has an unterminated quoted string", s)
			}
			out = append(out, filterToken{text: b.String(), quoted: true})
			afterOp = false
			i = j + 1
		case strings.ContainsRune("=!<>:~", rune(c)):
			op := string(c)
			if i+1 < len(s) && c != '=' && c != ':' && (s[i+1] == '=' || (c == '!' && s[i+1] == '~')) {
				op += string(s[i+1])
			}
			out = append(out, filterToken{text: op})
			afterOp = true
			i += len(op)
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && !strings.ContainsRune("()=!<>:~\"'", rune(s[j])) {
				j++
			}
			out = append(out, filterToken{text: s[i:j]})
			i = j
		}
	}
	return out, nil
}

type filterParser struct {
	toks   []filterToken
	pos    int
	filter string
	what   string
	kindOf func(string) (byte, bool)
}

func (p *filterParser) peek() (filterToken, bool) {
	if p.pos >= len(p.toks) {
		return filterToken{}, false
	}
	return p.toks[p.pos], true
}

func (p *filterParser) keyword(word string) bool {
	t, ok := p.peek()
	return ok && !t.quoted && t.text == word
}

// parseAnd is a sequence of OR-terms joined by AND or juxtaposition.
func (p *filterParser) parseAnd() (*filterNode, error) {
	var terms []*filterNode
	for {
		t, ok := p.peek()
		if !ok || (!t.quoted && t.text == ")") {
			break
		}
		if p.keyword("AND") {
			if len(terms) == 0 {
				return nil, apierror.InvalidArgument("list filter %q: AND needs an expression on each side", p.filter)
			}
			p.pos++
			continue
		}
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		terms = append(terms, n)
	}
	if len(terms) == 0 {
		return nil, apierror.InvalidArgument("list filter %q: expected an expression", p.filter)
	}
	if len(terms) == 1 {
		return terms[0], nil
	}
	return &filterNode{op: "AND", children: terms}, nil
}

func (p *filterParser) parseOr() (*filterNode, error) {
	first, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	terms := []*filterNode{first}
	for p.keyword("OR") {
		p.pos++
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		terms = append(terms, n)
	}
	if len(terms) == 1 {
		return first, nil
	}
	return &filterNode{op: "OR", children: terms}, nil
}

func (p *filterParser) parseUnary() (*filterNode, error) {
	t, ok := p.peek()
	if !ok {
		return nil, apierror.InvalidArgument("list filter %q ends where an expression was expected", p.filter)
	}
	if !t.quoted && (t.text == "NOT" || t.text == "-" || (strings.HasPrefix(t.text, "-") && len(t.text) > 1)) {
		if t.text == "NOT" || t.text == "-" {
			p.pos++
		} else {
			p.toks[p.pos].text = t.text[1:]
		}
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &filterNode{op: "NOT", children: []*filterNode{n}}, nil
	}
	if !t.quoted && t.text == "(" {
		p.pos++
		n, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		if !p.keyword(")") {
			return nil, apierror.InvalidArgument("list filter %q has an unclosed parenthesis", p.filter)
		}
		p.pos++
		return n, nil
	}
	return p.parseRestriction()
}

func (p *filterParser) parseRestriction() (*filterNode, error) {
	ft := p.toks[p.pos]
	field := ft.text
	p.pos++
	opTok, ok := p.peek()
	if !ok || opTok.quoted || !isFilterOp(opTok.text) {
		return nil, apierror.Unimplemented("list filter %q: %q is not a FIELD OPERATOR VALUE restriction; "+
			"CloudBurrow's Secret Manager implements the documented filter syntax only "+
			"(docs.cloud.google.com/secret-manager/docs/filtering)", p.filter, field)
	}
	op := opTok.text
	p.pos++
	if op == "~" || op == "!~" || op == "!" {
		return nil, apierror.Unimplemented("list filter %q: operator %q is not implemented; regular expressions are a gcloud "+
			"client-side feature that the Secret Manager API does not take", p.filter, op)
	}
	kind, known := p.kindOf(field)
	if !known {
		return nil, apierror.Unimplemented("list filter %q: field %q is not a %s field CloudBurrow's Secret Manager can filter on",
			p.filter, field, p.what)
	}
	var values []string
	if p.keyword("(") {
		p.pos++
		for {
			t, ok := p.peek()
			if !ok {
				return nil, apierror.InvalidArgument("list filter %q has an unclosed value list", p.filter)
			}
			p.pos++
			if !t.quoted && t.text == ")" {
				break
			}
			if !t.quoted && (t.text == "OR") {
				continue
			}
			if !t.quoted && (t.text == "AND" || t.text == "NOT") {
				return nil, apierror.Unimplemented("list filter %q: %s inside a value list is not implemented; only OR is", p.filter, t.text)
			}
			values = append(values, t.text)
		}
		if len(values) == 0 {
			return nil, apierror.InvalidArgument("list filter %q has an empty value list", p.filter)
		}
	} else {
		t, ok := p.peek()
		if !ok || (!t.quoted && (t.text == ")" || t.text == "(")) {
			return nil, apierror.InvalidArgument("list filter %q: %s%s needs a value", p.filter, field, op)
		}
		p.pos++
		values = []string{t.text}
	}
	for _, v := range values {
		if v == "*" {
			if op != ":" {
				return nil, apierror.InvalidArgument("list filter %q: * is a value only with \":\" (%s:* means %s is set)", p.filter, field, field)
			}
			continue
		}
		switch kind {
		case 't':
			if op == ":" {
				continue
			}
			if _, err := parseFilterTime(v); err != nil {
				return nil, apierror.InvalidArgument("list filter %q: %s takes an RFC 3339 time such as 2021-01-01T06:00:00Z, got %q", p.filter, field, v)
			}
		case 'd':
			if _, err := parseFilterDuration(v); err != nil {
				return nil, apierror.InvalidArgument("list filter %q: %s takes a duration in seconds such as 259200s, got %q", p.filter, field, v)
			}
		}
	}
	return &filterNode{field: field, cmp: op, values: values}, nil
}

func isFilterOp(s string) bool {
	switch s {
	case "=", "!=", "<", ">", "<=", ">=", ":", "~", "!~", "!":
		return true
	}
	return false
}

// filterList keeps the items f matches.
func filterList[T any](items []T, f *listFilter) []T {
	if f == nil {
		return items
	}
	out := items[:0:0]
	for _, it := range items {
		if f.matches(it) {
			out = append(out, it)
		}
	}
	return out
}

// listTotal is a list's total_size: Google reports none (0) when a filter
// is set, per the filtering page.
func listTotal(n int, f *listFilter) int32 {
	if f != nil {
		return 0
	}
	return int32(n)
}

// ListSecretsFiltered is ListSecrets with a ListSecrets filter applied, for
// callers outside the API such as the console (#1228).
func (s *Store) ListSecretsFiltered(project, filter string) ([]Secret, error) {
	f, err := parseSecretFilter(filter)
	if err != nil {
		return nil, err
	}
	all, err := s.ListSecrets(project)
	if err != nil {
		return nil, err
	}
	return filterList(all, f), nil
}
