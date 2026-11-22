package resourcemanager

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// users.list's query (#2103), as Google's "Search for users" guide
// (https://developers.google.com/workspace/admin/directory/v1/guides/search-users,
// read 2026-11-21) documents it: clauses of field, operator and value,
// separated by spaces and joined by an implicit AND.
//
//   - Operators: "=" an exact match (givenName=Jane does not match "Jane
//     Ann"); ":" contains the whole words, in order (givenName:Jane matches
//     "Jane Ann" but not "Janet"); ":PREFIX*" starts with, for email,
//     givenName and familyName only; ":[MIN,MAX]" a range of numbers or
//     dates, MIN inclusive and MAX exclusive; ">", ">=", "<", "<=" for
//     numbers and dates.
//   - Values: a string in single quotes when it holds whitespace, with
//     \' for an apostrophe; a boolean true or false, with = only; a number
//     with a period as its decimal separator; a date YYYY-MM-DD.
//   - Fields: a bare value (givenName, familyName or email); name (the
//     given and family names); email (including aliases); givenName;
//     familyName; isAdmin; isDelegatedAdmin; isSuspended; isArchived; im;
//     externalId; manager and managerId (the manager's email or ID, direct
//     or up the chain); directManager and directManagerId; address (every
//     address field, ":" only); addressPoBox, addressExtended,
//     addressStreet, addressLocality, addressRegion, addressPostalCode,
//     addressCountry; orgName, orgTitle, orgDepartment, orgDescription,
//     orgCostCenter; phone (= only); orgUnitPath (=, matching every unit
//     under it; admin_view only); isEnrolledIn2Sv; isEnforcedIn2Sv;
//     isGuest; and a custom attribute schemaName.fieldName (no :PREFIX*).
//     A multi-valued field matches when one value does: field:[12,15]
//     wants one value in the range, field>12 field<15 may be met by two.
//
// The guide does not say whether matching ignores case; it does here, as
// email addresses and names compare in the Admin console. The instance
// serves no Directory schemas resource, so every custom attribute in a
// user's customSchemas is searchable (as if indexed), and a range applies
// to values that read as numbers or dates.

type userOp int

const (
	opEq userOp = iota
	opHas
	opPrefix
	opRange
	opGT
	opGE
	opLT
	opLE
)

type userClause struct {
	field    string
	op       userOp
	value    string
	min, max string
}

type userQuery []userClause

// userFieldOps are each built-in field's operators: "=" ":" "*" (prefix)
// and "b" (a boolean, = only).
var userFieldOps = map[string]string{
	"name": "=:", "email": "=:*", "givenName": "=:*", "familyName": "=:*",
	"isAdmin": "b", "isDelegatedAdmin": "b", "isSuspended": "b", "isArchived": "b",
	"im": "=:", "externalId": "=:", "manager": "=", "managerId": "=", "directManager": "=", "directManagerId": "=",
	"address": ":", "addressPoBox": "=:", "addressExtended": "=:", "addressStreet": "=:", "addressLocality": "=:",
	"addressRegion": "=:", "addressPostalCode": "=:", "addressCountry": "=:",
	"orgName": "=:", "orgTitle": "=:", "orgDepartment": "=:", "orgDescription": "=:", "orgCostCenter": "=:",
	"phone": "=", "orgUnitPath": "=", "isEnrolledIn2Sv": "b", "isEnforcedIn2Sv": "b", "isGuest": "b",
}

func badQuery(q, why string) error {
	return apierror.InvalidArgument("Invalid Input: query %q: %s", q, why)
}

// readUserValue reads a value at s: quoted, or up to whitespace.
func readUserValue(s string) (string, string, bool) {
	if strings.HasPrefix(s, "'") {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			switch c := s[i]; {
			case c == '\\' && i+1 < len(s):
				i++
				b.WriteByte(s[i])
			case c == '\'':
				return b.String(), s[i+1:], true
			default:
				b.WriteByte(c)
			}
		}
		return "", "", false
	}
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, "", true
	}
	return s[:i], s[i:], true
}

func parseUserQuery(q string) (userQuery, error) {
	var out userQuery
	rest := strings.TrimSpace(q)
	for rest != "" {
		// The field runs up to an operator; a clause with none is a bare
		// value.
		i := strings.IndexAny(rest, ":=<> '")
		var c userClause
		if i < 0 || rest[i] == ' ' || rest[i] == '\'' {
			v, r, ok := readUserValue(rest)
			if !ok {
				return nil, badQuery(q, "an unterminated quote.")
			}
			c = userClause{field: "", op: opHas, value: v}
			if strings.HasSuffix(v, "*") {
				c.op, c.value = opPrefix, strings.TrimSuffix(v, "*")
			}
			out = append(out, c)
			rest = strings.TrimSpace(r)
			continue
		}
		c.field = rest[:i]
		if c.field == "" {
			return nil, badQuery(q, "a clause has no field.")
		}
		rest = rest[i:]
		switch {
		case strings.HasPrefix(rest, ">="):
			c.op, rest = opGE, rest[2:]
		case strings.HasPrefix(rest, "<="):
			c.op, rest = opLE, rest[2:]
		case strings.HasPrefix(rest, ">"):
			c.op, rest = opGT, rest[1:]
		case strings.HasPrefix(rest, "<"):
			c.op, rest = opLT, rest[1:]
		case strings.HasPrefix(rest, "="):
			c.op, rest = opEq, rest[1:]
		case strings.HasPrefix(rest, ":["):
			end := strings.Index(rest, "]")
			if end < 0 {
				return nil, badQuery(q, "a range without its ].")
			}
			lo, hi, ok := strings.Cut(rest[2:end], ",")
			if !ok {
				return nil, badQuery(q, "a range is [MIN,MAX].")
			}
			c.op, c.min, c.max, rest = opRange, strings.TrimSpace(lo), strings.TrimSpace(hi), rest[end+1:]
		default:
			c.op, rest = opHas, rest[1:]
		}
		if c.op != opRange {
			quoted := strings.HasPrefix(rest, "'")
			v, r, ok := readUserValue(rest)
			if !ok {
				return nil, badQuery(q, "an unterminated quote.")
			}
			c.value, rest = v, r
			if c.op == opHas && strings.HasSuffix(v, "*") && !quoted {
				c.op, c.value = opPrefix, strings.TrimSuffix(v, "*")
			}
		}
		if err := checkUserClause(q, c); err != nil {
			return nil, err
		}
		out = append(out, c)
		rest = strings.TrimSpace(rest)
	}
	return out, nil
}

func checkUserClause(q string, c userClause) error {
	if strings.Contains(c.field, ".") {
		if c.op == opPrefix {
			return badQuery(q, ":PREFIX* is not supported on custom attributes.")
		}
		return nil
	}
	ops, ok := userFieldOps[c.field]
	if !ok {
		return badQuery(q, "unknown field "+c.field+".")
	}
	allowed := false
	switch c.op {
	case opEq:
		allowed = strings.ContainsAny(ops, "=b")
		if ops == "b" && c.value != "true" && c.value != "false" {
			return badQuery(q, c.field+" is a boolean: true or false.")
		}
	case opHas:
		allowed = strings.Contains(ops, ":")
	case opPrefix:
		allowed = strings.Contains(ops, "*")
	}
	if !allowed {
		return badQuery(q, "the operator is not supported for "+c.field+".")
	}
	return nil
}

func str(v any) string { s, _ := v.(string); return s }

func objList(o map[string]any, k string) []map[string]any {
	var out []map[string]any
	if l, ok := o[k].([]any); ok {
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func subValues(o map[string]any, list string, keys ...string) []string {
	var out []string
	for _, m := range objList(o, list) {
		for _, k := range keys {
			if s := str(m[k]); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// managersOf are a user's direct managers' emails (relations of type
// manager).
func managersOf(o map[string]any) []string {
	var out []string
	for _, m := range objList(o, "relations") {
		if str(m["type"]) == "manager" && str(m["value"]) != "" {
			out = append(out, normEmail(str(m["value"])))
		}
	}
	return out
}

// userIndex resolves managers across the customer's users.
type userIndex struct {
	byEmail map[string]*DirectoryUser
}

func newUserIndex(us []*DirectoryUser) *userIndex {
	ix := &userIndex{byEmail: map[string]*DirectoryUser{}}
	for _, u := range us {
		ix.byEmail[u.Email()] = u
		for _, e := range userEmails(u)[1:] {
			ix.byEmail[e] = u
		}
	}
	return ix
}

// chain are the user's managers, direct first, then up the chain.
func (ix *userIndex) chain(u *DirectoryUser, direct bool) []*DirectoryUser {
	var out []*DirectoryUser
	seen := map[string]bool{u.Email(): true}
	next := []*DirectoryUser{u}
	for len(next) > 0 {
		var more []*DirectoryUser
		for _, x := range next {
			for _, e := range managersOf(x.Object) {
				m := ix.byEmail[e]
				if m == nil || seen[m.Email()] {
					continue
				}
				seen[m.Email()] = true
				out = append(out, m)
				more = append(more, m)
			}
		}
		if direct {
			break
		}
		next = more
	}
	return out
}

func userEmails(u *DirectoryUser) []string {
	out := []string{u.Email()}
	if al, ok := u.Object["aliases"].([]any); ok {
		for _, a := range al {
			out = append(out, normEmail(str(a)))
		}
	}
	return out
}

// customValues are a custom attribute's values: a multi-valued field is
// a list of {value} objects.
func customValues(o map[string]any, field string) []string {
	schema, f, _ := strings.Cut(field, ".")
	cs, _ := o["customSchemas"].(map[string]any)
	sc, _ := cs[schema].(map[string]any)
	var out []string
	add := func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case float64:
			out = append(out, strconv.FormatFloat(x, 'f', -1, 64))
		case bool:
			out = append(out, strconv.FormatBool(x))
		case map[string]any:
			if vv, ok := x["value"]; ok {
				switch y := vv.(type) {
				case string:
					out = append(out, y)
				case float64:
					out = append(out, strconv.FormatFloat(y, 'f', -1, 64))
				case bool:
					out = append(out, strconv.FormatBool(y))
				}
			}
		}
	}
	if l, ok := sc[f].([]any); ok {
		for _, x := range l {
			add(x)
		}
	} else if v, ok := sc[f]; ok {
		add(v)
	}
	return out
}

// compareUserValues orders two values as numbers or dates; ok is false
// when they are neither.
func compareUserValues(a, b string) (int, bool) {
	if x, err := strconv.ParseFloat(a, 64); err == nil {
		if y, err := strconv.ParseFloat(b, 64); err == nil {
			switch {
			case x < y:
				return -1, true
			case x > y:
				return 1, true
			}
			return 0, true
		}
	}
	if x, err := time.Parse("2006-01-02", a); err == nil {
		if y, err := time.Parse("2006-01-02", b); err == nil {
			return x.Compare(y), true
		}
	}
	return 0, false
}

func matchValue(c userClause, v string) bool {
	switch c.op {
	case opEq:
		return strings.EqualFold(strings.TrimSpace(v), c.value)
	case opHas:
		return wordsMatch(v, c.value)
	case opPrefix:
		return prefixFold(v, c.value)
	case opRange:
		lo, ok1 := compareUserValues(v, c.min)
		hi, ok2 := compareUserValues(v, c.max)
		return ok1 && ok2 && lo >= 0 && hi < 0
	}
	n, ok := compareUserValues(v, c.value)
	if !ok {
		return false
	}
	switch c.op {
	case opGT:
		return n > 0
	case opGE:
		return n >= 0
	case opLT:
		return n < 0
	}
	return n <= 0
}

func anyValue(c userClause, vs []string) bool {
	for _, v := range vs {
		if matchValue(c, v) {
			return true
		}
	}
	return false
}

func boolField(o map[string]any, k string) string {
	b, _ := o[k].(bool)
	return strconv.FormatBool(b)
}

// matches is whether the user meets every clause.
func (q userQuery) matches(u *DirectoryUser, ix *userIndex) bool {
	o := u.Object
	nm, _ := o["name"].(map[string]any)
	given, family := str(nm["givenName"]), str(nm["familyName"])
	for _, c := range q {
		var vs []string
		switch c.field {
		case "":
			vs = append([]string{given, family}, userEmails(u)...)
		case "name":
			vs = []string{strings.TrimSpace(given + " " + family)}
		case "email":
			vs = userEmails(u)
		case "givenName":
			vs = []string{given}
		case "familyName":
			vs = []string{family}
		case "isAdmin", "isDelegatedAdmin", "isArchived", "isEnrolledIn2Sv", "isEnforcedIn2Sv":
			key := map[string]string{"isArchived": "archived"}[c.field]
			if key == "" {
				key = c.field
			}
			vs = []string{boolField(o, key)}
		case "isSuspended":
			vs = []string{boolField(o, "suspended")}
		case "isGuest":
			vs = []string{boolField(o, "isGuestUser")}
		case "im":
			vs = subValues(o, "ims", "im")
		case "externalId":
			vs = subValues(o, "externalIds", "value")
		case "phone":
			vs = subValues(o, "phones", "value")
		case "manager", "directManager", "managerId", "directManagerId":
			for _, m := range ix.chain(u, strings.HasPrefix(c.field, "direct")) {
				if strings.HasSuffix(c.field, "Id") {
					vs = append(vs, m.ID())
				} else {
					vs = append(vs, userEmails(m)...)
				}
			}
		case "address":
			vs = subValues(o, "addresses", "formatted", "poBox", "extendedAddress", "streetAddress", "locality", "region", "postalCode", "country")
		case "addressPoBox":
			vs = subValues(o, "addresses", "poBox")
		case "addressExtended":
			vs = subValues(o, "addresses", "extendedAddress")
		case "addressStreet":
			vs = subValues(o, "addresses", "streetAddress")
		case "addressLocality":
			vs = subValues(o, "addresses", "locality")
		case "addressRegion":
			vs = subValues(o, "addresses", "region")
		case "addressPostalCode":
			vs = subValues(o, "addresses", "postalCode")
		case "addressCountry":
			vs = subValues(o, "addresses", "country")
		case "orgName":
			vs = subValues(o, "organizations", "name")
		case "orgTitle":
			vs = subValues(o, "organizations", "title")
		case "orgDepartment":
			vs = subValues(o, "organizations", "department")
		case "orgDescription":
			vs = subValues(o, "organizations", "description")
		case "orgCostCenter":
			vs = subValues(o, "organizations", "costCenter")
		case "orgUnitPath":
			p := "/" + strings.Trim(strings.ToLower(c.value), "/")
			got := "/" + strings.Trim(strings.ToLower(u.OrgUnitPath()), "/")
			if !(p == "/" || got == p || strings.HasPrefix(got, p+"/")) {
				return false
			}
			continue
		default:
			vs = customValues(o, c.field)
		}
		if !anyValue(c, vs) {
			return false
		}
	}
	return true
}

// SearchUsers are the users a users.list query finds.
func (r *Registry) SearchUsers(query string) ([]*DirectoryUser, error) {
	q, err := parseUserQuery(query)
	if err != nil {
		return nil, err
	}
	all, err := r.Users()
	if err != nil {
		return nil, err
	}
	ix := newUserIndex(all)
	var out []*DirectoryUser
	for _, u := range all {
		if q.matches(u, ix) {
			out = append(out, u)
		}
	}
	return out, nil
}
