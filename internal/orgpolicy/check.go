package orgpolicy

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

// The constraints CloudBurrow enforces.
const (
	AllowedPolicyMemberDomains     = "iam.allowedPolicyMemberDomains"
	DisableServiceAccountKeyCreate = "iam.disableServiceAccountKeyCreation"
	ManagedDisableKeyCreate        = "iam.managed.disableServiceAccountKeyCreation"
	DisableServiceAccountCreation  = "iam.disableServiceAccountCreation"
	UniformBucketLevelAccess       = "storage.uniformBucketLevelAccess"
	PublicAccessPrevention         = "storage.publicAccessPrevention"
	ResourceLocations              = "gcp.resourceLocations"
)

// Effective is a constraint's effective policy on a resource.
type Effective struct {
	// Spec is the enforced spec, DryRun the dry-run one; nil is none,
	// the constraint's default.
	Spec   *orgpolicypb.PolicySpec
	DryRun *orgpolicypb.PolicySpec
	// Tags are the resource's effective tags, which rule conditions read.
	Tags []iamcond.Tag
}

// Source answers effective policies: the host's Resource Manager
// registry, or a server in the cluster asking the host (Remote).
type Source interface {
	// Effective is constraint (its short name, "gcp.resourceLocations")
	// on resource ("projects/{id or number}", "folders/N",
	// "organizations/N"). A resource the source does not know has none.
	Effective(ctx context.Context, resource, constraint string) (Effective, error)
}

var (
	mu  sync.RWMutex
	src Source
)

// Use makes s the process's source; nil turns enforcement off (what a
// process that never calls Use, such as a unit test, has).
func Use(s Source) {
	mu.Lock()
	defer mu.Unlock()
	src = s
}

func current() Source {
	mu.RLock()
	defer mu.RUnlock()
	return src
}

// effective is constraint's effective policy on resource; none when there
// is no source or it cannot answer, so an unreadable hierarchy never
// refuses a call.
func effective(ctx context.Context, s Source, resource, constraint string) Effective {
	if s == nil || resource == "" {
		return Effective{}
	}
	e, err := s.Effective(ctx, resource, constraint)
	if err != nil {
		return Effective{}
	}
	return e
}

// Violation is a refused request: the constraint and Google's message for
// it. Each API reports it with its own code (FAILED_PRECONDITION over
// gRPC and JSON, Cloud Storage's 412 conditionNotMet).
type Violation struct {
	Constraint string
	Message    string
}

func (v *Violation) Error() string { return v.Message }

func dryRun(resource, constraint string, ok bool) {
	if !ok {
		log.Printf("orgpolicy: dry run: the request on %s would violate constraints/%s", resource, constraint)
	}
}

// Enforced reports whether the boolean constraint is enforced on
// resource. A dry-run spec that would enforce it is logged, never
// enforced.
func Enforced(ctx context.Context, resource, constraint string) bool {
	s := current()
	if s == nil {
		return false
	}
	e := effective(ctx, s, resource, constraint)
	if e.DryRun != nil {
		dryRun(resource, constraint, !BoolEnforced(e.DryRun, e.Tags))
	}
	return BoolEnforced(e.Spec, e.Tags)
}

// SourceSet reports whether a source is in use (enforcement is possible).
func SourceSet() bool { return current() != nil }

// CheckBool is a boolean constraint's refusal, msg, when it is enforced
// on resource; nil otherwise.
func CheckBool(ctx context.Context, resource, constraint, msg string) error {
	if Enforced(ctx, resource, constraint) {
		return &Violation{Constraint: constraint, Message: msg}
	}
	return nil
}

// CheckLocations refuses creating a resource of resource's project in a
// location gcp.resourceLocations does not allow, with Google's documented
// message: "LOCATION_IN_REQUEST violates constraint
// constraints/gcp.resourceLocations on the resource RESOURCE_TESTED."
// ("Restricting Resource Locations", read 2026-11-19). The global
// location is not checked: the constraint does not apply to global
// resources. tested is the resource named in the message.
func CheckLocations(ctx context.Context, resource, tested string, locations ...string) error {
	s := current()
	if s == nil {
		return nil
	}
	e := effective(ctx, s, resource, ResourceLocations)
	if e.Spec == nil && e.DryRun == nil {
		return nil
	}
	for _, loc := range locations {
		if loc == "" || strings.EqualFold(loc, "global") {
			continue
		}
		if e.DryRun != nil {
			dryRun(resource, ResourceLocations, ListAllows(e.DryRun, e.Tags, loc, locationMatches))
		}
		if !ListAllows(e.Spec, e.Tags, loc, locationMatches) {
			return &Violation{Constraint: ResourceLocations,
				Message: fmt.Sprintf("Location %s:%s violates constraint constraints/gcp.resourceLocations on the resource %s.", LocationKind(loc), loc, tested)}
		}
	}
	return nil
}

// MembersMessage is Google's refusal of a member outside the permitted
// customers ("Restrict identities with domain-restricted sharing", read
// 2026-11-19).
const MembersMessage = "One or more users named in the policy do not belong to a permitted customer."

// Customer is the local organization: its directory customer ID, its
// number and its domain: Resource Manager's OrganizationCustomerID,
// OrganizationNumber and OrganizationDomain, which its tests hold equal.
var Customer = struct{ ID, OrgNumber, Domain string }{"C0cloudburrow", "100000000001", "cloudburrow.local"}

// memberInCustomer reports whether member belongs to the local
// organization's customer: a user, group or domain of its domain, a
// service account (every project is the local organization's), or a
// workload identity principal. allUsers and allAuthenticatedUsers belong
// to no customer.
func memberInCustomer(member string) bool {
	kind, id, _ := strings.Cut(member, ":")
	switch kind {
	case "allUsers", "allAuthenticatedUsers":
		return false
	case "deleted", "projectOwner", "projectEditor", "projectViewer":
		return true
	case "serviceAccount":
		return true
	case "user", "group":
		_, domain, ok := strings.Cut(id, "@")
		return ok && strings.EqualFold(domain, Customer.Domain)
	case "domain":
		return strings.EqualFold(id, Customer.Domain)
	case "principal", "principalSet":
		return !strings.HasPrefix(member, "principalSet://goog/public:")
	}
	return false
}

// memberMatches reports whether an iam.allowedPolicyMemberDomains value
// (a customer ID, is:{customer ID}, or
// is:principalSet://cloudresourcemanager.googleapis.com/organizations/N)
// covers member.
func memberMatches(value, member string) bool {
	v := strings.TrimPrefix(value, "is:")
	switch {
	case strings.EqualFold(v, Customer.ID):
	case v == "principalSet://cloudresourcemanager.googleapis.com/organizations/"+Customer.OrgNumber:
	default:
		return false
	}
	return memberInCustomer(member)
}

// CheckMembers refuses an IAM policy on resource (under project, folder
// or organization `under`) whose members added beside before include
// one iam.allowedPolicyMemberDomains does not permit. Members already in
// the policy are not checked again: the constraint applies to grants made
// after it is set.
func CheckMembers(ctx context.Context, under string, before, after []string) error {
	s := current()
	if s == nil {
		return nil
	}
	e := effective(ctx, s, under, AllowedPolicyMemberDomains)
	if e.Spec == nil && e.DryRun == nil {
		return nil
	}
	had := map[string]bool{}
	for _, m := range before {
		had[m] = true
	}
	for _, m := range after {
		if had[m] || strings.HasPrefix(m, "deleted:") {
			continue
		}
		if e.DryRun != nil {
			dryRun(under, AllowedPolicyMemberDomains, ListAllows(e.DryRun, e.Tags, m, memberMatches))
		}
		if !ListAllows(e.Spec, e.Tags, m, memberMatches) {
			return &Violation{Constraint: AllowedPolicyMemberDomains, Message: MembersMessage}
		}
	}
	return nil
}

// ProjectOf is the projects/{p} a resource name lies under
// ("projects/p/locations/l/keyRings/k" is projects/p), or "".
func ProjectOf(name string) string {
	name = strings.TrimPrefix(name, "//")
	if i := strings.Index(name, "projects/"); i >= 0 {
		rest := name[i+len("projects/"):]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[:j]
		}
		if rest != "" && rest != "_" && rest != "-" {
			return "projects/" + rest
		}
	}
	return ""
}
