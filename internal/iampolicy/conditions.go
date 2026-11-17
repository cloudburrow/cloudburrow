package iampolicy

// IAM Conditions (#1742), as Google's "Policy versions" and "Overview of
// IAM Conditions" document them:
//
//   - a binding's condition has a required title, an optional description
//     and a required expression, a CEL boolean over the attributes
//     internal/iamcond evaluates; one that does not compile is
//     INVALID_ARGUMENT naming the binding;
//   - a policy with a conditional binding must be version 3 ("Requests for
//     policies with any conditional role bindings must specify version
//     3"); one that is not is INVALID_ARGUMENT;
//   - a reader that asks for version 1 (or none) of a policy with
//     conditional bindings is given version 1, each conditional role
//     renamed ROLE_withcond_HASH and its condition left out; a version 1
//     policy set back with those roles keeps their conditions, so a
//     read-modify-write through a version 1 client does not lose them;
//     one that mixes a condition and such a role is refused with Google's
//     "Policy cannot contain both conditions and condition hashes";
//   - bindings of the same role and the same condition are one binding;
//     at most 20 bindings may give the same role to the same principal
//     under different conditions (IAM quotas page), and an expression has
//     at most 12 logic operators.
//
// Google does not publish the text of these errors (only the hash one
// above); the codes are Google's and the messages say what was wrong.

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

// Condition is a binding's condition.
type Condition struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Expression  string `json:"expression"`
}

// MaxConditionsPerRoleAndPrincipal is Google's limit on the bindings that
// give one role to one principal under different conditions.
const MaxConditionsPerRoleAndPrincipal = 20

// withCond joins a conditional role and its condition's hash in a version
// 1 rendering, as Google's "Policy versions" shows:
// roles/iam.securityReviewer_withcond_58e135cabb940ad9346c.
const withCond = "_withcond_"

func (c *Condition) proto() *expr.Expr {
	return &expr.Expr{Title: c.Title, Description: c.Description, Expression: c.Expression}
}

// hash is the 20 hex digits a version 1 rendering appends to role.
func (c *Condition) hash(role string) string {
	sum := sha256.Sum256([]byte(role + "\x00" + c.Title + "\x00" + c.Description + "\x00" + c.Expression))
	return hex.EncodeToString(sum[:])[:20]
}

// Grants is whether the binding applies under attrs: always without a
// condition, and otherwise when the condition evaluates to true.
func (b Binding) Grants(attrs iamcond.Attributes) bool {
	return b.Condition == nil || iamcond.Eval(b.Condition.Expression, attrs)
}

// checkConditions applies the policy-wide rules: conditions only where
// they are served, version 3 for them, and no mix of conditions and hashes.
func checkConditions(p *iampb.Policy, allowed bool) error {
	cond, hash := -1, -1
	for i, b := range p.GetBindings() {
		if b.GetCondition() != nil && cond < 0 {
			cond = i
		}
		if strings.Contains(b.GetRole(), withCond) && hash < 0 {
			hash = i
		}
	}
	switch {
	case cond >= 0 && !allowed:
		return apierror.Unimplemented("policy.bindings[%d].condition: IAM Conditions are not supported on this resource", cond)
	case cond >= 0 && hash >= 0:
		return apierror.InvalidArgument("Policy cannot contain both conditions and condition hashes")
	case cond >= 0 && p.GetVersion() != 3:
		return apierror.InvalidArgument("policy.bindings[%d].condition: a policy with conditional role bindings must be version 3, not %d", cond, p.GetVersion())
	}
	return nil
}

// binding validates the i'th binding and returns it to store, its
// condition restored from current when its role carries a condition hash.
func binding(i int, b *iampb.Binding, current *Stored) (Binding, error) {
	out := Binding{Role: b.GetRole(), Members: slices.Clone(b.GetMembers())}
	if c := b.GetCondition(); c != nil {
		if c.GetTitle() == "" {
			return Binding{}, apierror.InvalidArgument("policy.bindings[%d].condition.title is required", i)
		}
		if strings.TrimSpace(c.GetExpression()) == "" {
			return Binding{}, apierror.InvalidArgument("policy.bindings[%d].condition.expression is required", i)
		}
		if _, err := iamcond.Compile(c.GetExpression()); err != nil {
			return Binding{}, apierror.InvalidArgument("policy.bindings[%d].condition.expression is invalid: %v", i, err)
		}
		out.Condition = &Condition{Title: c.GetTitle(), Description: c.GetDescription(), Expression: c.GetExpression()}
		return out, nil
	}
	role, h, ok := strings.Cut(b.GetRole(), withCond)
	if !ok {
		return out, nil
	}
	if current != nil {
		for _, cb := range current.Bindings {
			if cb.Role == role && cb.Condition != nil && cb.Condition.hash(role) == h {
				c := *cb.Condition
				out.Role, out.Condition = role, &c
				return out, nil
			}
		}
	}
	return Binding{}, apierror.InvalidArgument("policy.bindings[%d].role %q: the condition hash matches no conditional binding of the current policy; read it with requested_policy_version 3", i, b.GetRole())
}

// mergeBinding adds b to bindings, joining it to a binding of the same role
// and condition.
func mergeBinding(bindings []Binding, b Binding) []Binding {
	for i, have := range bindings {
		if have.Role != b.Role || !sameCondition(have.Condition, b.Condition) {
			continue
		}
		for _, m := range b.Members {
			if !slices.Contains(have.Members, m) {
				bindings[i].Members = append(bindings[i].Members, m)
			}
		}
		return bindings
	}
	return append(bindings, b)
}

func sameCondition(a, b *Condition) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// checkSameRoleAndPrincipal enforces Google's limit of 20 bindings that
// give the same role to the same principal under different conditions.
func checkSameRoleAndPrincipal(bindings []Binding) error {
	n := map[[2]string]int{}
	for _, b := range bindings {
		if b.Condition == nil {
			continue
		}
		for _, m := range b.Members {
			k := [2]string{b.Role, m}
			if n[k]++; n[k] > MaxConditionsPerRoleAndPrincipal {
				return apierror.InvalidArgument("%s is granted %s under more than %d different conditions; a policy allows at most %d",
					m, b.Role, MaxConditionsPerRoleAndPrincipal, MaxConditionsPerRoleAndPrincipal)
			}
		}
	}
	return nil
}
