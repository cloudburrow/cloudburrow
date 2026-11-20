package resourcemanager

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A group's security settings (#1959): its member restriction, "Member
// Restriction as defined by CEL expression. Supported restrictions are:
// member.customer_id and member.type. Valid values for member.type are 1,
// 2 and 3. They correspond to USER, SERVICE_ACCOUNT, and GROUP
// respectively. The value for member.customer_id only supports
// groupCustomerId() currently which means the customer id of the group
// will be used for restriction. Supported operators are &&, || and =="
// (MemberRestriction.query,
// https://docs.cloud.google.com/identity/docs/reference/rest/v1/SecuritySettings,
// read 2026-11-20). "If you add member restrictions to a group, then you
// can't directly add members that violate those restrictions. You might do
// so indirectly, through nesting"
// (https://knowledge.workspace.google.com/admin/groups/restrict-group-membership):
// memberships.create refuses a direct member the query excludes, and each
// membership's MEMBER role reports its restrictionEvaluations. The
// restriction is evaluated at once, never EVALUATING.

// Member types, as member.type numbers them.
const (
	memberTypeUser           = 1
	memberTypeServiceAccount = 2
	memberTypeGroup          = 3
)

// Restriction states (RestrictionEvaluation.state and
// MembershipRoleRestrictionEvaluation.state).
const (
	RestrictionCompliant        = "COMPLIANT"
	RestrictionForwardCompliant = "FORWARD_COMPLIANT"
	RestrictionNonCompliant     = "NON_COMPLIANT"
)

// memberFacts is what a restriction reads of a member: its type and
// whether it is an entity of the group's customer.
type memberFacts struct {
	typ        int
	inCustomer bool
}

// restriction is a parsed member restriction: an or of ands of
// comparisons.
type restriction struct {
	or [][]restrictionTerm
}

type restrictionTerm struct {
	// typ is the member.type compared (0 for member.customer_id ==
	// groupCustomerId()).
	typ int
	// sub is a parenthesized expression, when set.
	sub *restriction
}

func (x *restriction) allows(f memberFacts) bool {
	for _, and := range x.or {
		ok := true
	terms:
		for _, t := range and {
			switch {
			case t.sub != nil:
				ok = t.sub.allows(f)
			case t.typ == 0:
				ok = f.inCustomer
			default:
				ok = f.typ == t.typ
			}
			if !ok {
				break terms
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// allFacts is every kind of member there is, for comparing two
// restrictions.
var allFacts = []memberFacts{{1, true}, {1, false}, {2, true}, {2, false}, {3, true}, {3, false}}

// within is whether every member x allows, y allows too.
func (x *restriction) within(y *restriction) bool {
	for _, f := range allFacts {
		if x.allows(f) && !y.allows(f) {
			return false
		}
	}
	return true
}

// parseMemberRestriction reads a member restriction query; an empty one
// is no restriction (nil).
func parseMemberRestriction(q string) (*restriction, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	toks, err := lexRestriction(q)
	if err != nil {
		return nil, err
	}
	p := &restrictionParser{q: q, toks: toks}
	out, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.i != len(p.toks) {
		return nil, p.fail("unexpected %q", p.toks[p.i])
	}
	return out, nil
}

func lexRestriction(q string) ([]string, error) {
	var toks []string
	rs := []rune(q)
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(' || c == ')':
			toks = append(toks, string(c))
			i++
		case c == '&' || c == '|' || c == '=':
			if i+1 >= len(rs) || rs[i+1] != c {
				return nil, apierror.InvalidArgument("Invalid member restriction query %q: the supported operators are &&, || and ==.", q)
			}
			toks = append(toks, string([]rune{c, c}))
			i += 2
		case unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '.':
			j := i
			for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || rs[j] == '_' || rs[j] == '.') {
				j++
			}
			// groupCustomerId() is one token.
			if j+1 < len(rs) && rs[j] == '(' && rs[j+1] == ')' {
				j += 2
			}
			toks = append(toks, string(rs[i:j]))
			i = j
		default:
			return nil, apierror.InvalidArgument("Invalid member restriction query %q: unexpected %q.", q, string(c))
		}
	}
	return toks, nil
}

type restrictionParser struct {
	q    string
	toks []string
	i    int
}

func (p *restrictionParser) fail(format string, a ...any) error {
	return apierror.InvalidArgument("Invalid member restriction query %q: %s. Supported restrictions are member.type == 1, 2 or 3 (USER, SERVICE_ACCOUNT, GROUP) and member.customer_id == groupCustomerId(), joined by && and ||.",
		p.q, fmt.Sprintf(format, a...))
}

func (p *restrictionParser) peek() string {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return ""
}

func (p *restrictionParser) expr() (*restriction, error) {
	out := &restriction{}
	for {
		var and []restrictionTerm
		for {
			t, err := p.term()
			if err != nil {
				return nil, err
			}
			and = append(and, t)
			if p.peek() != "&&" {
				break
			}
			p.i++
		}
		out.or = append(out.or, and)
		if p.peek() != "||" {
			return out, nil
		}
		p.i++
	}
}

func (p *restrictionParser) term() (restrictionTerm, error) {
	switch tok := p.peek(); tok {
	case "":
		return restrictionTerm{}, p.fail("it ends early")
	case "(":
		p.i++
		sub, err := p.expr()
		if err != nil {
			return restrictionTerm{}, err
		}
		if p.peek() != ")" {
			return restrictionTerm{}, p.fail("a ( is not closed")
		}
		p.i++
		return restrictionTerm{sub: sub}, nil
	}
	if p.i+2 >= len(p.toks) {
		return restrictionTerm{}, p.fail("%q is not a comparison", p.peek())
	}
	l, op, r := p.toks[p.i], p.toks[p.i+1], p.toks[p.i+2]
	if op != "==" {
		return restrictionTerm{}, p.fail("%q is not a comparison", l)
	}
	p.i += 3
	if r == "member.type" || r == "member.customer_id" {
		l, r = r, l
	}
	switch l {
	case "member.type":
		switch r {
		case "1":
			return restrictionTerm{typ: memberTypeUser}, nil
		case "2":
			return restrictionTerm{typ: memberTypeServiceAccount}, nil
		case "3":
			return restrictionTerm{typ: memberTypeGroup}, nil
		}
		return restrictionTerm{}, p.fail("member.type is 1, 2 or 3, not %s", r)
	case "member.customer_id":
		if r != "groupCustomerId()" {
			return restrictionTerm{}, p.fail("member.customer_id only supports groupCustomerId(), not %s", r)
		}
		return restrictionTerm{}, nil
	}
	return restrictionTerm{}, p.fail("%s is not a supported restriction", l)
}

// memberFactsLocked is what a restriction reads of email: its type, and
// whether it is the customer's. A group here is the customer's; a user is
// when it is an account of the customer's domain; a service account when
// its project is in the organization.
func (r *Registry) memberFactsLocked(email string) (memberFacts, error) {
	typ, err := r.memberTypeLocked(email)
	if err != nil {
		return memberFacts{}, err
	}
	switch typ {
	case "GROUP":
		return memberFacts{typ: memberTypeGroup, inCustomer: true}, nil
	case "SERVICE_ACCOUNT":
		return memberFacts{typ: memberTypeServiceAccount, inCustomer: r.projectInOrgLocked(saProject(email))}, nil
	}
	return memberFacts{typ: memberTypeUser, inCustomer: InDomain(email)}, nil
}

// saProject is the project of a user-managed service account,
// NAME@PROJECT.iam.gserviceaccount.com ("" for another kind).
func saProject(email string) string {
	_, d, _ := strings.Cut(email, "@")
	p, ok := strings.CutSuffix(d, ".iam.gserviceaccount.com")
	if !ok {
		return ""
	}
	return p
}

// projectInOrgLocked is whether the project id has a parent: a folder of
// the organization or the organization itself.
func (r *Registry) projectInOrgLocked(id string) bool {
	if id == "" {
		return false
	}
	b, err := r.st.Get(keyPrefix + id)
	if err != nil {
		return false
	}
	var p Project
	return json.Unmarshal(b, &p) == nil && p.Parent != ""
}

// checkRestrictionLocked refuses a direct member g's restriction excludes.
func (r *Registry) checkRestrictionLocked(g *Group, email string) error {
	x, err := parseMemberRestriction(g.MemberRestriction)
	if err != nil || x == nil {
		return err
	}
	f, err := r.memberFactsLocked(email)
	if err != nil {
		return err
	}
	if !x.allows(f) {
		return apierror.FailedPrecondition("The member %s violates the member restriction of %s (%s): a member the restriction excludes cannot be added directly.",
			email, g.Name(), g.MemberRestriction)
	}
	return nil
}

// memberEvaluationLocked is a membership's restriction state under g's
// restriction x: COMPLIANT when the member and, for a group, every member
// under it adheres; FORWARD_COMPLIANT for a group that holds a member
// violating x but whose own restriction admits nothing x excludes;
// NON_COMPLIANT otherwise.
func (r *Registry) memberEvaluationLocked(x *restriction, email string) (string, error) {
	f, err := r.memberFactsLocked(email)
	if err != nil {
		return "", err
	}
	if !x.allows(f) {
		return RestrictionNonCompliant, nil
	}
	if f.typ != memberTypeGroup {
		return RestrictionCompliant, nil
	}
	inner, err := r.groupByEmailLocked(email)
	if err != nil || inner == nil {
		return RestrictionCompliant, err
	}
	ok, err := r.transitivelyAllowedLocked(x, inner, map[string]bool{})
	if err != nil {
		return "", err
	}
	if ok {
		return RestrictionCompliant, nil
	}
	if own, err := parseMemberRestriction(inner.MemberRestriction); err == nil && own != nil && own.within(x) {
		return RestrictionForwardCompliant, nil
	}
	return RestrictionNonCompliant, nil
}

// transitivelyAllowedLocked is whether x allows every member of g,
// directly or through nested groups.
func (r *Registry) transitivelyAllowedLocked(x *restriction, g *Group, seen map[string]bool) (bool, error) {
	if seen[g.Email] {
		return true, nil
	}
	seen[g.Email] = true
	for _, m := range g.Members {
		f, err := r.memberFactsLocked(m.Email)
		if err != nil {
			return false, err
		}
		if !x.allows(f) {
			return false, nil
		}
		if f.typ == memberTypeGroup {
			inner, err := r.groupByEmailLocked(m.Email)
			if err != nil {
				return false, err
			}
			if inner != nil {
				ok, err := r.transitivelyAllowedLocked(x, inner, seen)
				if err != nil || !ok {
					return ok, err
				}
			}
		}
	}
	return true, nil
}

// groupEvaluationLocked is g's restriction state: COMPLIANT when every
// transitive member adheres; FORWARD_COMPLIANT when the only violations
// are under nested groups that admit nothing new the restriction excludes;
// NON_COMPLIANT otherwise.
func (r *Registry) groupEvaluationLocked(g *Group, x *restriction) (string, error) {
	state := RestrictionCompliant
	for _, m := range g.Members {
		s, err := r.memberEvaluationLocked(x, m.Email)
		if err != nil {
			return "", err
		}
		switch s {
		case RestrictionNonCompliant:
			return RestrictionNonCompliant, nil
		case RestrictionForwardCompliant:
			state = RestrictionForwardCompliant
		}
	}
	return state, nil
}

// SecuritySettings is a group's security settings: its member
// restriction query and the restriction's state ("" for none).
type SecuritySettings struct {
	Name       string
	Query      string
	Evaluation string
}

// GetSecuritySettings reads groups/{g}/securitySettings.
func (r *Registry) GetSecuritySettings(name string) (SecuritySettings, error) {
	gname, ok := strings.CutSuffix(name, "/securitySettings")
	if !ok {
		return SecuritySettings{}, apierror.InvalidArgument("Invalid name %q: it must be groups/{group}/securitySettings.", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupFromNameLocked(gname)
	if err != nil {
		return SecuritySettings{}, err
	}
	return r.securitySettingsLocked(g)
}

func (r *Registry) securitySettingsLocked(g *Group) (SecuritySettings, error) {
	out := SecuritySettings{Name: g.Name() + "/securitySettings", Query: g.MemberRestriction}
	x, err := parseMemberRestriction(g.MemberRestriction)
	if err != nil || x == nil {
		return out, nil
	}
	if out.Evaluation, err = r.groupEvaluationLocked(g, x); err != nil {
		return SecuritySettings{}, err
	}
	return out, nil
}

// UpdateSecuritySettings sets a group's member restriction query; mask
// "May only contain the following field: member_restriction.query".
func (r *Registry) UpdateSecuritySettings(name, query string, mask []string) (SecuritySettings, error) {
	gname, ok := strings.CutSuffix(name, "/securitySettings")
	if !ok {
		return SecuritySettings{}, apierror.InvalidArgument("Invalid name %q: it must be groups/{group}/securitySettings.", name)
	}
	if len(mask) == 0 {
		return SecuritySettings{}, apierror.InvalidArgument("Request contains an invalid argument: updateMask is required; it may only contain member_restriction.query.")
	}
	for _, f := range mask {
		switch f {
		case "member_restriction.query", "memberRestriction.query":
		default:
			return SecuritySettings{}, apierror.InvalidArgument("Invalid updateMask field %q: it may only contain member_restriction.query.", f)
		}
	}
	if _, err := parseMemberRestriction(query); err != nil {
		return SecuritySettings{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupFromNameLocked(gname)
	if err != nil {
		return SecuritySettings{}, err
	}
	g.MemberRestriction = strings.TrimSpace(query)
	g.UpdateTime = r.now().UTC()
	if err := r.putGroupLocked(g); err != nil {
		return SecuritySettings{}, err
	}
	return r.securitySettingsLocked(g)
}

// MemberEvaluation is the restriction state of g's membership of email
// ("" when g has no restriction).
func (r *Registry) MemberEvaluation(g *Group, email string) string {
	x, err := parseMemberRestriction(g.MemberRestriction)
	if err != nil || x == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.memberEvaluationLocked(x, email)
	if err != nil {
		return ""
	}
	return s
}
