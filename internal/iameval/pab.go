package iameval

// Principal access boundary (PAB) policies (IAM v3, #2057), as "Principal
// access boundary policies" describes them
// (https://docs.cloud.google.com/iam/docs/principal-access-boundary-policies,
// read 2026-11-20):
//
//   - a PAB policy lists the Resource Manager resources (projects, folders,
//     organizations) a principal is eligible to access, each with its
//     descendants; it is bound to principal sets by policy bindings;
//   - a principal subject to no PAB policy can access any resource; one
//     subject to at least one PAB policy that can block the permission is
//     blocked unless one of those policies makes the resource eligible:
//     the policies are additive;
//   - a policy blocks only the permissions its enforcement version covers
//     (pabpermissions.tsv.gz, below); "latest", and a policy without a
//     version, use the default version;
//   - a binding's condition (principal.type and principal.subject only)
//     that is true or cannot be evaluated enforces the policy; false does
//     not;
//   - PAB policies block or do not block; they never grant.
//
// The principal sets a caller is in ("Supported principal sets", same page):
// a workforce pool's identities are in the pool's set and the
// organization's; a workload pool's in the pool's and in its project's,
// folders' and organization's; a service account in its project's, folders'
// and organization's; a user of a Google Workspace domain (DomainCustomers)
// in the domain's set (//iam.googleapis.com/locations/global/workspace/ID,
// the ID taken to be the customer ID: UNVERIFIED, re-read 2026-11-20) and
// the organization's.
//
// Evaluation order, as "Policy types" gives it
// (https://docs.cloud.google.com/iam/docs/policy-types, read 2026-11-20):
// IAM checks PAB eligibility first, then deny policies, then allow
// policies; it evaluates them all and compiles the result, so a block by
// any of the first two refuses whatever the allow policies grant.
//
// The permissions each enforcement version can block are Google's "Permissions
// that Principal Access Boundary policies can block"
// (https://docs.cloud.google.com/iam/docs/pab-blocked-permissions, read
// 2026-11-20), embedded as pabpermissions.tsv.gz: one line per pattern,
// VERSION, ROW, "block" or "except", the pattern (SERVICE_FQDN/RESOURCE.VERB,
// either part "*", or SERVICE_FQDN/* for all), and the service's name. A
// row's exceptions apply to that row's patterns only, and each version
// includes the rows of the versions before it.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"strconv"
	"strings"
	"sync"

	iamv3 "cloud.google.com/go/iam/apiv3/iampb"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

//go:embed pabpermissions.tsv.gz
var pabPermissionsGz []byte

// DefaultEnforcementVersion is the default enforcement version, which
// "latest" and a policy without a version use ("The current default
// enforcement version is 4", read 2026-11-20).
const DefaultEnforcementVersion = 4

// MaxEnforcementVersion is the newest enforcement version.
const MaxEnforcementVersion = 4

type pabRow struct {
	version       int
	block, except []string
}

var (
	pabOnce sync.Once
	pabRows []pabRow
)

func pabCatalog() []pabRow {
	pabOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(pabPermissionsGz))
		if err != nil {
			return
		}
		byRow := map[string]int{}
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if len(f) < 4 {
				continue
			}
			v, err := strconv.Atoi(f[0])
			if err != nil {
				continue
			}
			i, ok := byRow[f[1]]
			if !ok {
				i = len(pabRows)
				byRow[f[1]] = i
				pabRows = append(pabRows, pabRow{version: v})
			}
			if f[2] == "except" {
				pabRows[i].except = append(pabRows[i].except, f[3])
			} else {
				pabRows[i].block = append(pabRows[i].block, f[3])
			}
		}
	})
	return pabRows
}

// EnforcementVersion is the version an enforcementVersion names: "" and
// "latest" the default, "1" to "4" themselves; ok is false for anything
// else.
func EnforcementVersion(v string) (int, bool) {
	switch v {
	case "", "latest":
		return DefaultEnforcementVersion, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxEnforcementVersion || strconv.Itoa(n) != v {
		return 0, false
	}
	return n, true
}

// pabPatternMatches is whether a blocked-permissions pattern names the v2
// permission p.
func pabPatternMatches(pattern, p string) bool {
	psvc, prest, ok := strings.Cut(pattern, "/")
	svc, rest, ok2 := strings.Cut(p, "/")
	if !ok || !ok2 || psvc != svc {
		return false
	}
	if prest == "*" {
		return true
	}
	pres, pverb, ok := cutLast(prest, ".")
	res, verb, ok2 := cutLast(rest, ".")
	if !ok || !ok2 {
		return prest == rest
	}
	return (pres == "*" || pres == res) && (pverb == "*" || pverb == verb)
}

// PABBlockable is whether a policy at enforcement version (as its
// enforcementVersion field gives it) can block the v1 permission.
func PABBlockable(permission, version string) bool {
	v, ok := EnforcementVersion(version)
	if !ok {
		return false
	}
	p := V2Permission(permission)
	for _, row := range pabCatalog() {
		if row.version > v {
			continue
		}
		matched := false
		for _, b := range row.block {
			if pabPatternMatches(b, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		excepted := false
		for _, x := range row.except {
			if pabPatternMatches(x, p) {
				excepted = true
				break
			}
		}
		if !excepted {
			return true
		}
	}
	return false
}

// Bound is one PAB policy binding with the policy it binds.
type Bound struct {
	Binding *iamv3.PolicyBinding
	// Policy is nil when the bound policy does not exist.
	Policy *iamv3.PrincipalAccessBoundaryPolicy
}

// PABSource is a Source that stores PAB policies and their bindings.
type PABSource interface {
	// BoundaryBindings are the PAB policy bindings whose target is the
	// principal set (a full resource name: a project by ID or number).
	BoundaryBindings(principalSet string) []Bound
	// BoundaryOrganization is the organization ("organizations/N") whose
	// principal set holds the instance's Workspace users and workforce
	// pools.
	BoundaryOrganization() string
}

// PAB access states, in Policy Troubleshooter v3's words.
const (
	PABAllowed     = "PAB_ACCESS_STATE_ALLOWED"
	PABNotAllowed  = "PAB_ACCESS_STATE_NOT_ALLOWED"
	PABNotEnforced = "PAB_ACCESS_STATE_NOT_ENFORCED"
)

// BoundaryResult explains one PAB policy bound to the caller.
type BoundaryResult struct {
	Binding *iamv3.PolicyBinding
	Policy  *iamv3.PrincipalAccessBoundaryPolicy
	// PrincipalSet is the binding's target the caller is in.
	PrincipalSet string
	// Condition is "true", "false" or "error" ("" without one).
	Condition string
	// Blockable is whether the policy's enforcement version covers the
	// permission.
	Blockable bool
	// Resource is the rule resource that makes the checked resource
	// eligible, "" for none.
	Resource string
	// State is PABAllowed, PABNotAllowed or PABNotEnforced.
	State string
}

const crmFull = "//cloudresourcemanager.googleapis.com/"

// principal type values a binding's condition reads.
const (
	TypeWorkspace = "iam.googleapis.com/WorkspaceIdentity"
	TypeWorkforce = "iam.googleapis.com/WorkforcePoolIdentity"
	TypeWorkload  = "iam.googleapis.com/WorkloadPoolIdentity"
	TypeSA        = "iam.googleapis.com/ServiceAccount"
)

// PrincipalSets are the PAB principal sets email is in, its principal.type
// and its principal.subject.
func (e *Evaluator) PrincipalSets(email string) (sets []string, typ, subject string) {
	email = strings.TrimSpace(email)
	if email == "" || e.Source == nil {
		return nil, "", ""
	}
	org := ""
	if ps, ok := e.Source.(PABSource); ok {
		org = ps.BoundaryOrganization()
	}
	hierarchy := func(project string) []string {
		var out []string
		for _, a := range e.Source.Ancestry(project) {
			out = append(out, crmFull+a)
		}
		return out
	}
	l := strings.ToLower(email)
	switch {
	case strings.HasPrefix(l, "principal://iam.googleapis.com/locations/global/workforcepools/"):
		rest := email[len("principal://iam.googleapis.com/locations/global/workforcePools/"):]
		pool, sub, ok := strings.Cut(rest, "/subject/")
		if !ok {
			return nil, "", ""
		}
		sets = []string{"//iam.googleapis.com/locations/global/workforcePools/" + pool}
		if org != "" {
			sets = append(sets, crmFull+org)
		}
		return sets, TypeWorkforce, sub
	case strings.HasPrefix(l, "principal://iam.googleapis.com/projects/"):
		rest := email[len("principal://iam.googleapis.com/projects/"):]
		number, rest, _ := strings.Cut(rest, "/")
		poolPath, sub, ok := strings.Cut(rest, "/subject/")
		if !ok || !strings.Contains(poolPath, "/workloadIdentityPools/") {
			return nil, "", ""
		}
		sets = append([]string{"//iam.googleapis.com/projects/" + number + "/" + poolPath}, hierarchy(number)...)
		return sets, TypeWorkload, sub
	case strings.HasPrefix(l, "principal://"), strings.HasPrefix(l, "principalset://"):
		return nil, "", ""
	}
	_, domain, _ := strings.Cut(l, "@")
	if strings.HasSuffix(domain, ".iam.gserviceaccount.com") {
		project := strings.TrimSuffix(domain, ".iam.gserviceaccount.com")
		return hierarchy(project), TypeSA, l
	}
	for d, c := range DomainCustomers {
		if strings.EqualFold(d, domain) {
			sets = append(sets, "//iam.googleapis.com/locations/global/workspace/"+c)
			if org != "" {
				sets = append(sets, crmFull+org)
			}
			return sets, TypeWorkspace, l
		}
	}
	return nil, "", ""
}

// ancestry is req's resource's hierarchy, the deepest first.
func (e *Evaluator) ancestry(req Request) []string {
	if req.Project != "" {
		return e.Source.Ancestry(req.Project)
	}
	var anc []string
	for _, a := range req.Own {
		if !strings.HasPrefix(a.Resource, "//") {
			anc = append(anc, a.Resource)
		}
	}
	return anc
}

// canonicalHierarchy is a rule's resource as the ancestry names it (a
// project by its ID).
func (e *Evaluator) canonicalHierarchy(full string) string {
	r := strings.TrimPrefix(full, crmFull)
	if p, ok := strings.CutPrefix(r, "projects/"); ok {
		if anc := e.Source.Ancestry(p); len(anc) > 0 {
			return anc[0]
		}
	}
	return r
}

// boundary evaluates the PAB policies bound to req's principal: blocked
// when at least one of them applies and can block the permission and none
// makes the resource eligible.
func (e *Evaluator) boundary(req Request) (bool, []BoundaryResult) {
	ps, ok := e.Source.(PABSource)
	if !ok {
		return false, nil
	}
	sets, typ, subject := e.PrincipalSets(req.Email)
	if len(sets) == 0 {
		return false, nil
	}
	anc := map[string]bool{}
	for _, a := range e.ancestry(req) {
		anc[a] = true
	}
	attrs := iamcond.Attributes{PrincipalType: typ, PrincipalSubject: subject}
	applies, eligible := false, false
	var out []BoundaryResult
	seen := map[string]bool{}
	for _, set := range sets {
		for _, b := range ps.BoundaryBindings(set) {
			if b.Policy == nil || seen[b.Binding.GetName()] {
				continue
			}
			seen[b.Binding.GetName()] = true
			r := BoundaryResult{Binding: b.Binding, Policy: b.Policy, PrincipalSet: set, State: PABNotEnforced}
			if c := b.Binding.GetCondition(); c != nil && strings.TrimSpace(c.GetExpression()) != "" {
				r.Condition = denyCondError
				if x, err := iamcond.CompilePrincipal(c.GetExpression()); err == nil {
					if v, err := x.Evaluate(attrs); err == nil {
						r.Condition = denyCondFalse
						if v {
							r.Condition = denyCondTrue
						}
					}
				}
			}
			r.Blockable = PABBlockable(req.Permission, b.Policy.GetDetails().GetEnforcementVersion())
			if r.Condition == denyCondFalse || !r.Blockable {
				out = append(out, r)
				continue
			}
			applies = true
			for _, rule := range b.Policy.GetDetails().GetRules() {
				if rule.GetEffect() != iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW {
					continue
				}
				for _, res := range rule.GetResources() {
					if r.Resource == "" && anc[e.canonicalHierarchy(res)] {
						r.Resource = res
					}
				}
			}
			r.State = PABNotAllowed
			if r.Resource != "" {
				r.State = PABAllowed
				eligible = true
			}
			out = append(out, r)
		}
	}
	return applies && !eligible, out
}
