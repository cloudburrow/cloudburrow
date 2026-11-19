package run

import (
	"context"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Organization policies (#1890), enforced whatever --iam-enforcement, as on
// Google Cloud: gcp.resourceLocations on creating a resource in
// projects/{p}/locations/{l}, and iam.allowedPolicyMemberDomains on
// setIamPolicy. Nothing is refused when no policy is set (or no source is
// in use, as in unit tests).

// checkOrgLocation refuses creating a resource under parent
// (projects/{p}/locations/{l}) in a location gcp.resourceLocations does
// not allow, FAILED_PRECONDITION with Google's documented message.
func checkOrgLocation(ctx context.Context, parent string) error {
	project, location, err := resource.ParseLocation(parent)
	if err != nil {
		return nil
	}
	if err := orgpolicy.CheckLocations(ctx, "projects/"+project, "projects/"+project, location); err != nil {
		return apierror.FailedPrecondition("%s", err.Error())
	}
	return nil
}

// policyMembers are p's members, across its bindings.
func policyMembers(p *iampb.Policy) []string {
	var out []string
	for _, b := range p.GetBindings() {
		out = append(out, b.GetMembers()...)
	}
	return out
}

// storedProto is cur as a policy, nil for none.
func storedProto(cur *iampolicy.Stored) *iampb.Policy {
	if cur == nil {
		return nil
	}
	return cur.Proto()
}

// checkOrgMembers refuses a setIamPolicy on name that adds a member
// iam.allowedPolicyMemberDomains does not permit, FAILED_PRECONDITION
// with Google's message.
func checkOrgMembers(ctx context.Context, name string, before, after *iampb.Policy) error {
	if err := orgpolicy.CheckMembers(ctx, orgpolicy.ProjectOf(name), policyMembers(before), policyMembers(after)); err != nil {
		return apierror.FailedPrecondition("%s", err.Error())
	}
	return nil
}
