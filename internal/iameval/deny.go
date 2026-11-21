package iameval

// Deny policies (IAM v2, #1886), as "Deny policies" describes them
// (https://docs.cloud.google.com/iam/docs/deny-overview, read 2026-11-19):
// IAM checks the deny policies that apply to a resource before its allow
// policies. Those are the deny policies attached to the resource's project,
// its folders and its organization, as they are inherited by everything
// below their attachment point, and each is evaluated on its own. A deny
// rule denies a permission when the permission is one of deniedPermissions
// (a permission or a permission group, svc/resource.* or svc/*.*) and not
// one of exceptionPermissions, the principal is one of deniedPrincipals
// and not one of exceptionPrincipals, and denialCondition, if any, is true
// or cannot be evaluated. A denied permission is refused whatever roles
// the principal holds.
//
// Permissions in deny policies are named SERVICE_FQDN/RESOURCE.VERB; which
// ones can be denied, and their IAM v1 names where those are not
// SERVICE_ID.RESOURCE.VERB, are Google's "Permissions supported in deny
// policies" (https://docs.cloud.google.com/iam/docs/deny-permissions-support,
// read 2026-11-19), embedded as denypermissions.tsv.gz: one permission or
// permission group per line, then a tab and its v1 name when the page
// gives one, then a tab and its aliases.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"strings"
	"sync"

	iamv2 "cloud.google.com/go/iam/apiv2/iampb"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

//go:embed denypermissions.tsv.gz
var denyPermissionsGz []byte

type denyCatalog struct {
	// supported are the permissions and groups that may be named, by
	// name, each mapping to its canonical name (an alias to its permission).
	supported map[string]string
	// v2ByV1 are the v2 names of the v1 permissions whose v2 name is not
	// the default derivation.
	v2ByV1 map[string]string
}

var (
	denyOnce sync.Once
	denyCat  denyCatalog
)

func catalog() denyCatalog {
	denyOnce.Do(func() {
		denyCat = denyCatalog{supported: map[string]string{}, v2ByV1: map[string]string{}}
		zr, err := gzip.NewReader(bytes.NewReader(denyPermissionsGz))
		if err != nil {
			return
		}
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if f[0] == "" {
				continue
			}
			denyCat.supported[f[0]] = f[0]
			if len(f) > 1 && f[1] != "" {
				denyCat.v2ByV1[f[1]] = f[0]
			}
			if len(f) > 2 && f[2] != "" {
				for _, a := range strings.Split(f[2], ",") {
					denyCat.supported[a] = f[0]
				}
			}
		}
	})
	return denyCat
}

// DeniablePermission is whether p (SERVICE_FQDN/RESOURCE.VERB, or a
// permission group) may be named in a deny rule, and its canonical name.
func DeniablePermission(p string) (string, bool) {
	c, ok := catalog().supported[p]
	return c, ok
}

// V2Permission is a v1 permission's name in deny policies
// (secretmanager.versions.access is
// secretmanager.googleapis.com/versions.access).
func V2Permission(v1 string) string {
	if v2, ok := catalog().v2ByV1[v1]; ok {
		return v2
	}
	svc, rest, ok := strings.Cut(v1, ".")
	if !ok {
		return v1
	}
	if svc == "resourcemanager" {
		svc = "cloudresourcemanager"
	}
	return svc + ".googleapis.com/" + rest
}

// Deniable is whether a v1 permission can be denied at all.
func Deniable(v1 string) bool {
	_, ok := DeniablePermission(V2Permission(v1))
	return ok
}

// permissionMatches is whether a rule's permission (or group) names the
// v2 permission p.
func permissionMatches(rule, p string) bool {
	if c, ok := DeniablePermission(rule); ok {
		rule = c
	}
	if rule == p {
		return true
	}
	rsvc, rrest, ok := strings.Cut(rule, "/")
	psvc, prest, ok2 := strings.Cut(p, "/")
	if !ok || !ok2 || rsvc != psvc {
		return false
	}
	if rrest == "*.*" {
		return true
	}
	if res, ok := strings.CutSuffix(rrest, ".*"); ok {
		pres, _, _ := cutLast(prest, ".")
		return pres == res
	}
	return false
}

func cutLast(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// DenyIdentifiers are the deny-policy principal identifiers that name a
// caller with members m (Members' keys): principal://goog/subject/EMAIL
// for a user, principal://iam.googleapis.com/projects/-/serviceAccounts/EMAIL
// for a service account, principalSet://goog/group/GROUP for each group,
// principalSet://goog/cloudIdentityCustomerId/C for a member of a
// customer's domain (DomainCustomers), and principalSet://goog/public:all
// for everyone. Lowercased. A workforce pool's identifiers are never a
// caller's: Workforce Identity Federation is not served (its token
// exchange answers 501), so no caller is one.
func denyIdentifiers(members map[string]bool) map[string]bool {
	out := map[string]bool{"principalset://goog/public:all": true}
	for m := range members {
		switch {
		case strings.HasPrefix(m, "user:"):
			out["principal://goog/subject/"+strings.TrimPrefix(m, "user:")] = true
		case strings.HasPrefix(m, "serviceaccount:"):
			out["principal://iam.googleapis.com/projects/-/serviceaccounts/"+strings.TrimPrefix(m, "serviceaccount:")] = true
		case strings.HasPrefix(m, "group:"):
			out["principalset://goog/group/"+strings.TrimPrefix(m, "group:")] = true
		case strings.HasPrefix(m, "domain:"):
			for d, c := range DomainCustomers {
				if strings.EqualFold(d, strings.TrimPrefix(m, "domain:")) {
					out["principalset://goog/cloudidentitycustomerid/"+strings.ToLower(c)] = true
				}
			}
		case strings.HasPrefix(m, "principal://"), strings.HasPrefix(m, "principalset://"):
			// A federated identity's (#1941) are the same in both.
			out[m] = true
		}
	}
	return out
}

func matchIdentifier(list []string, ids map[string]bool) string {
	for _, p := range list {
		if ids[strings.ToLower(strings.TrimSpace(p))] {
			return p
		}
	}
	return ""
}

// Deny states, in Policy Troubleshooter v3's words.
const (
	Denied        = "DENY_ACCESS_STATE_DENIED"
	NotDenied     = "DENY_ACCESS_STATE_NOT_DENIED"
	DenyUnknown   = "DENY_ACCESS_STATE_UNKNOWN_CONDITIONAL"
	denyCondTrue  = "true"
	denyCondFalse = "false"
	denyCondError = "error"
)

// DenyRuleResult explains one deny rule.
type DenyRuleResult struct {
	Rule *iamv2.DenyRule
	// Permission is the deniedPermissions entry that names the permission,
	// "" for none; ExceptPermission the exceptionPermissions entry.
	Permission, ExceptPermission string
	// Principal is the deniedPrincipals entry that names the caller, ""
	// for none; ExceptPrincipal the exceptionPrincipals entry.
	Principal, ExceptPrincipal string
	// Condition is "true", "false" or "error" ("" without one).
	Condition string
	Denies    bool
}

// DenyPolicyResult explains one deny policy.
type DenyPolicyResult struct {
	// Resource is the attachment point ("projects/p", "folders/n",
	// "organizations/n").
	Resource string
	Policy   *iamv2.Policy
	Rules    []DenyRuleResult
	Denies   bool
}

// DenySource is a Source that stores deny policies.
type DenySource interface {
	// DenyPolicies are the deny policies attached to a hierarchy
	// resource ("projects/ID", "folders/N", "organizations/N").
	DenyPolicies(resource string) []*iamv2.Policy
}

// TagSource is a Source that knows resources' effective tags, which deny
// conditions and allow conditions read.
type TagSource interface {
	// Tags are a resource's effective tags by its full resource name; ok
	// is false when they are not known.
	Tags(fullName string) (tags []iamcond.Tag, ok bool)
}

// deny evaluates the deny policies in req's ancestry.
func (e *Evaluator) deny(req Request, members map[string]bool, attrs iamcond.Attributes) (bool, []DenyPolicyResult) {
	ds, ok := e.Source.(DenySource)
	if !ok || req.Project == "" && len(req.Own) == 0 {
		return false, nil
	}
	var anc []string
	if req.Project != "" {
		anc = e.Source.Ancestry(req.Project)
	} else {
		for _, a := range req.Own {
			if !strings.HasPrefix(a.Resource, "//") {
				anc = append(anc, a.Resource)
			}
		}
	}
	perm := V2Permission(req.Permission)
	ids := denyIdentifiers(members)
	denied := false
	var out []DenyPolicyResult
	for _, r := range anc {
		for _, p := range ds.DenyPolicies(r) {
			pr := DenyPolicyResult{Resource: r, Policy: p}
			for _, rule := range p.GetRules() {
				dr := rule.GetDenyRule()
				if dr == nil {
					continue
				}
				rr := DenyRuleResult{Rule: dr}
				for _, dp := range dr.GetDeniedPermissions() {
					if permissionMatches(dp, perm) {
						rr.Permission = dp
						break
					}
				}
				for _, xp := range dr.GetExceptionPermissions() {
					if permissionMatches(xp, perm) {
						rr.ExceptPermission = xp
						break
					}
				}
				rr.Principal = matchIdentifier(dr.GetDeniedPrincipals(), ids)
				rr.ExceptPrincipal = matchIdentifier(dr.GetExceptionPrincipals(), ids)
				condOK := true
				if c := dr.GetDenialCondition(); c != nil && strings.TrimSpace(c.GetExpression()) != "" {
					rr.Condition = denyCondError
					if x, err := iamcond.Compile(c.GetExpression()); err == nil {
						if v, err := x.Evaluate(attrs); err == nil {
							rr.Condition = denyCondFalse
							if v {
								rr.Condition = denyCondTrue
							}
						}
					}
					// "If the condition evaluates to true or cannot be
					// evaluated, the permission is denied."
					condOK = rr.Condition != denyCondFalse
				}
				rr.Denies = rr.Permission != "" && rr.ExceptPermission == "" && rr.Principal != "" && rr.ExceptPrincipal == "" && condOK
				pr.Denies = pr.Denies || rr.Denies
				pr.Rules = append(pr.Rules, rr)
			}
			denied = denied || pr.Denies
			out = append(out, pr)
		}
	}
	return denied, out
}
