package resourcemanager

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// Dynamic groups (#2103): a group whose memberships are its query's users,
// "Dynamic groups are groups whose membership is automatically managed
// based on user attributes"
// (https://docs.cloud.google.com/identity/docs/concepts/overview-dynamic-groups,
// read 2026-11-21). The query is evaluated over the customer's users
// (directoryusers.go) each time the group's memberships are read, so a
// change of a user's attributes changes its memberships at once, and IAM,
// which reads a group's members, grants a dynamic group's roles to the
// users its query names. Rules followed, from that page, the attributes
// page (.../how-to/dynamic-groups-attributes), the test page
// (.../how-to/test-query-dynamic-groups) and the Group reference
// (.../reference/rest/v1/groups):
//
//   - the label cloudidentity.googleapis.com/groups.dynamic, and
//     dynamicGroupMetadata.queries with "Only one entry with USER resource";
//   - "You can create up to 500 dynamic groups per customer";
//   - "You cannot add a dynamic group as a member of a regular Google
//     Group", and its memberships are the query's alone (none is created
//     or deleted by hand: the memberships methods refuse them);
//   - the query language: the User resource's attributes in snake_case
//     (user.organizations, user.addresses, user.name.given_name, ...),
//     the exists() macro, ==, = , &&, ||, !, equalsIgnoreCase(),
//     orgUnitId() and userId(); "Any NOT ! query using the exists() macro
//     and using the AND && operator within it" and "Any query using the
//     exists() macro with the NOT ! operator inside it" are refused;
//   - a type sub-field is compared with its enum value (organizations 1
//     work, 2 school, 3 domain-only; relations 12 manager; locations 2
//     desk, the values the attributes page names; any other type keeps
//     its string);
//   - status UP_TO_DATE with statusTime the time of the read, as the
//     memberships are computed then.

const (
	// maxDynamicGroups is "up to 500 dynamic groups per customer".
	maxDynamicGroups = 500
	resourceTypeUser = "USER"
)

// typeEnums are the type sub-fields' enum values the attributes page
// names, per list field.
var typeEnums = map[string]map[string]int64{
	"organizations": {"work": 1, "school": 2, "domain_only": 3, "domain-only": 3},
	"relations":     {"manager": 12},
	"locations":     {"desk": 2},
}

// userListFields are the User resource's list attributes.
var userListFields = []string{"addresses", "locations", "organizations", "relations", "emails", "external_ids",
	"ims", "keywords", "languages", "phones", "websites"}

// snake is s in snake_case.
func snake(s string) string {
	var b strings.Builder
	for i, c := range s {
		if unicode.IsUpper(c) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(c))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func snakeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[snake(k)] = snakeValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = snakeValue(e)
		}
		return out
	default:
		return v
	}
}

// queryUser is the user u as a dynamic group query reads it.
func queryUser(u *DirectoryUser, idOf func(email string) string) map[string]any {
	o := u.Object
	out := map[string]any{}
	for k, v := range o {
		switch k {
		case "customSchemas", "name", "password":
			continue
		}
		out[snake(k)] = snakeValue(v)
	}
	b := func(k string) bool { v, _ := o[k].(bool); return v }
	out["is_2sv_enforced"] = b("isEnforcedIn2Sv")
	out["is_enrolled_in_2sv"] = b("isEnrolledIn2Sv")
	out["is_mailbox_setup"] = b("isMailboxSetup")
	out["change_password_at_next_login"] = b("changePasswordAtNextLogin")
	out["suspended"] = b("suspended")
	out["archived"] = b("archived")
	nm, _ := o["name"].(map[string]any)
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	out["name"] = map[string]any{"given_name": str(nm, "givenName"), "family_name": str(nm, "familyName"), "value": str(nm, "fullName")}
	for _, f := range userListFields {
		l, ok := out[f].([]any)
		if !ok {
			l = []any{}
		}
		for _, e := range l {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if p, ok := m["primary"].(bool); ok && !p {
				// "When a field has a primary value, that value can only be
				// evaluated as a true expression."
				delete(m, "primary")
			}
			if t, ok := m["type"].(string); ok {
				if n, ok := typeEnums[f][t]; ok {
					m["type"] = n
				}
			}
		}
		out[f] = l
	}
	path := u.OrgUnitPath()
	out["org_unit_id"] = OrgUnitID(path)
	var ous []any
	for p := "/" + strings.Trim(path, "/"); ; {
		ous = append(ous, map[string]any{"org_unit_id": OrgUnitID(p)})
		if p == "/" {
			break
		}
		i := strings.LastIndex(p, "/")
		p = p[:i]
		if p == "" {
			p = "/"
		}
	}
	out["org_units"] = ous
	var managers []any
	if rels, ok := o["relations"].([]any); ok {
		for _, e := range rels {
			m, _ := e.(map[string]any)
			if t, _ := m["type"].(string); t == "manager" {
				if id := idOf(normEmail(fmt.Sprint(m["value"]))); id != "" {
					managers = append(managers, map[string]any{"user_id": id})
				}
			}
		}
	}
	if managers == nil {
		managers = []any{}
	}
	out["managers"] = managers
	cs := map[string]any{}
	if c, ok := o["customSchemas"].(map[string]any); ok {
		for schema, fields := range c {
			fm, _ := fields.(map[string]any)
			sm := map[string]any{}
			for k, v := range fm {
				if l, ok := v.([]any); ok {
					// A multi-valued field's values: [{value, type}].
					var vals []any
					for _, e := range l {
						if m, ok := e.(map[string]any); ok {
							vals = append(vals, m["value"])
						} else {
							vals = append(vals, e)
						}
					}
					sm[k] = vals
					continue
				}
				sm[k] = v
			}
			cs[schema] = sm
		}
	}
	out["custom_schemas"] = cs
	return out
}

var (
	dynEnvOnce sync.Once
	dynEnv     *cel.Env
	dynEnvErr  error
)

func dynamicEnv() (*cel.Env, error) {
	dynEnvOnce.Do(func() {
		ident := func(args ...ref.Val) ref.Val { return args[0] }
		dynEnv, dynEnvErr = cel.NewEnv(
			cel.Variable("user", cel.DynType),
			cel.Function("equalsIgnoreCase", cel.MemberOverload("string_equals_ignore_case", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType,
				cel.BinaryBinding(func(a, b ref.Val) ref.Val {
					x, ok1 := a.Value().(string)
					y, ok2 := b.Value().(string)
					if !ok1 || !ok2 {
						return types.False
					}
					return types.Bool(strings.EqualFold(x, y))
				}))),
			cel.Function("orgUnitId", cel.Overload("org_unit_id_string", []*cel.Type{cel.StringType}, cel.StringType,
				cel.FunctionBinding(ident))),
			cel.Function("userId", cel.Overload("user_id_string", []*cel.Type{cel.StringType}, cel.StringType,
				cel.FunctionBinding(ident))),
		)
	})
	return dynEnv, dynEnvErr
}

// normalizeQuery turns the query's single = into ==, outside quotes: the
// attributes page's examples use both.
func normalizeQuery(q string) string {
	var b strings.Builder
	var quote rune
	rs := []rune(q)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if quote != 0 {
			b.WriteRune(c)
			if c == '\\' && i+1 < len(rs) {
				i++
				b.WriteRune(rs[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			b.WriteRune(c)
			continue
		}
		if c == '=' {
			prev := rune(0)
			if i > 0 {
				prev = rs[i-1]
			}
			next := rune(0)
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			if next == '=' {
				b.WriteString("==")
				i++
				continue
			}
			if prev != '!' && prev != '<' && prev != '>' && prev != '=' {
				b.WriteString("==")
				continue
			}
		}
		b.WriteRune(c)
	}
	return b.String()
}

// DynamicQuery is a compiled dynamic group query.
type DynamicQuery struct {
	prg cel.Program
}

// contains is whether e holds a call of fn.
func containsCall(e ast.Expr, fn string) bool {
	found := false
	walkExpr(e, func(x ast.Expr) {
		if x.Kind() == ast.CallKind && x.AsCall().FunctionName() == fn {
			found = true
		}
	})
	return found
}

func walkExpr(e ast.Expr, f func(ast.Expr)) {
	if e == nil {
		return
	}
	f(e)
	switch e.Kind() {
	case ast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			walkExpr(c.Target(), f)
		}
		for _, a := range c.Args() {
			walkExpr(a, f)
		}
	case ast.SelectKind:
		walkExpr(e.AsSelect().Operand(), f)
	case ast.ComprehensionKind:
		c := e.AsComprehension()
		walkExpr(c.IterRange(), f)
		walkExpr(c.LoopStep(), f)
		walkExpr(c.LoopCondition(), f)
		walkExpr(c.Result(), f)
	case ast.ListKind:
		for _, x := range e.AsList().Elements() {
			walkExpr(x, f)
		}
	}
}

// CompileDynamicQuery checks and compiles a query.
func CompileDynamicQuery(q string) (*DynamicQuery, error) {
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("the query is empty")
	}
	env, err := dynamicEnv()
	if err != nil {
		return nil, err
	}
	parsed, iss := env.Parse(normalizeQuery(q))
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	var refused error
	walkExpr(parsed.NativeRep().Expr(), func(x ast.Expr) {
		switch x.Kind() {
		case ast.ComprehensionKind:
			if containsCall(x.AsComprehension().LoopStep(), "!_") {
				refused = fmt.Errorf("a query using the exists() macro with the NOT ! operator inside it is not supported")
			}
		case ast.CallKind:
			if x.AsCall().FunctionName() != "!_" {
				return
			}
			walkExpr(x.AsCall().Args()[0], func(y ast.Expr) {
				if y.Kind() == ast.ComprehensionKind && containsCall(y.AsComprehension().LoopStep(), "_&&_") {
					refused = fmt.Errorf("a NOT ! query using the exists() macro and the AND && operator within it is not supported")
				}
			})
		}
	})
	if refused != nil {
		return nil, refused
	}
	checked, iss := env.Check(parsed)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if t := checked.OutputType(); t != cel.BoolType && t != cel.DynType {
		return nil, fmt.Errorf("the query must be a condition, not a %s", t)
	}
	prg, err := env.Program(checked)
	if err != nil {
		return nil, err
	}
	return &DynamicQuery{prg: prg}, nil
}

// Matches is whether the query names the user; an attribute the user does
// not have makes it false.
func (q *DynamicQuery) Matches(user map[string]any) bool {
	v, _, err := q.prg.Eval(map[string]any{"user": user})
	if err != nil {
		return false
	}
	b, ok := v.Value().(bool)
	return ok && b
}

// dynamicMemberID is the stable membership ID of a user in a dynamic
// group.
func dynamicMemberID(groupID, userID string) string {
	sum := sha256.Sum256([]byte(groupID + "/" + userID))
	return "1" + hex.EncodeToString(sum[:7])
}

// dynamicMembersLocked are the memberships the group's query computes.
func (r *Registry) dynamicMembersLocked(g *Group) []GroupMember {
	q, err := CompileDynamicQuery(g.DynamicQuery)
	if err != nil {
		return nil
	}
	us, err := r.usersLocked()
	if err != nil {
		return nil
	}
	ids := map[string]string{}
	for _, u := range us {
		ids[u.Email()] = u.ID()
	}
	idOf := func(e string) string { return ids[e] }
	var out []GroupMember
	for _, u := range us {
		if !q.Matches(queryUser(u, idOf)) {
			continue
		}
		t := u.userTime()
		if t.Before(g.CreateTime) {
			t = g.CreateTime
		}
		out = append(out, GroupMember{ID: dynamicMemberID(g.ID, u.ID()), Email: u.Email(), Roles: []string{"MEMBER"},
			Type: "USER", CreateTime: t, UpdateTime: t})
	}
	return out
}
